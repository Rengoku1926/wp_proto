# Step 6 -- Redis Pub/Sub for Message Routing

Module: `github.com/Rengoku1926/wp_proto/apps/backend`

**Prerequisites:** Hub, clients, message routing, delivery states (SENT/DELIVERED/READ) all working with direct in-memory routing from previous steps.

---

## 1. Why Redis Pub/Sub?

Up until now, when User A sends a message to User B, the hub does a direct lookup:

```go
if recipient, ok := h.clients[recipientID]; ok {
    recipient.send <- payload
}
```

This has a fundamental limitation: **it only works if both users are connected to the same server process.**

### The single-server illusion

Right now we run one server, so `hub.clients` contains every connected user. But the moment you add a second server behind a load balancer, User A might be on Server 1 and User B on Server 2. Server 1's hub has no idea User B exists.

### How pub/sub fixes this

```
                        Redis
                     +---------+
                     | PUBLISH  |
                     | chat:B   |
                     +----+----+
                          |
          +---------------+---------------+
          |                               |
   Server 1                        Server 2
   (User A connected)              (User B connected)
   hub.clients = {A}              hub.clients = {B}
   subscribed: chat:A             subscribed: chat:B
                                       |
                                  User B receives
                                  the message
```

Each server subscribes to `chat:{userID}` for every user connected to it. When a message needs to reach User B, the sender's server PUBLISHes to `chat:B`. Whichever server holds User B's WebSocket connection is subscribed to that channel and delivers the message.

### Why bother on a single server?

Even with one server, switching to pub/sub now gives us:

1. **Decoupled production from consumption.** The readPump that handles the incoming message does not need a reference to the recipient's client struct. It just publishes to Redis.
2. **Async delivery.** Redis handles the fan-out. The sender's goroutine is not blocked waiting for the recipient's send channel.
3. **Foundation for scaling.** When we add a second server later, zero routing code changes.

### The catch: pub/sub is fire-and-forget

Redis pub/sub does NOT store messages. If nobody is subscribed to `chat:B` when the PUBLISH happens (because User B is offline or between reconnects), the message is **silently dropped**. This is why we still save every message to Postgres first, and why the next step (Step 7) adds an offline buffer that replays missed messages on reconnect.

---

## 2. Pub/Sub Repository -- `internal/repository/pubsub.go`

This is a thin wrapper around the Redis client's PUBLISH and SUBSCRIBE commands.

```go
// internal/repository/pubsub.go
package repository

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// PubSubRepo wraps Redis pub/sub operations for chat message routing.
type PubSubRepo struct {
	rdb *redis.Client
}

// NewPubSubRepo creates a new PubSubRepo.
func NewPubSubRepo(rdb *redis.Client) *PubSubRepo {
	return &PubSubRepo{rdb: rdb}
}

// channel returns the Redis channel name for a given user.
func channel(userID string) string {
	return fmt.Sprintf("chat:%s", userID)
}

// Publish sends data to the Redis channel for the given user.
// Any server that has this user connected (and subscribed) will receive it.
func (r *PubSubRepo) Publish(ctx context.Context, userID string, data []byte) error {
	return r.rdb.Publish(ctx, channel(userID), data).Err()
}

// Subscribe returns a *redis.PubSub handle subscribed to the channel for the
// given user. The caller is responsible for calling Close() on it.
func (r *PubSubRepo) Subscribe(ctx context.Context, userID string) *redis.PubSub {
	return r.rdb.Subscribe(ctx, channel(userID))
}
```

The `channel()` helper keeps the naming convention in one place. Every message destined for User B goes to Redis channel `chat:B`.

---

## 3. Subscribe Loop Per Client -- `internal/handler/client.go` additions

Each client now runs a third goroutine: `subscribeLoop`. It subscribes to `chat:{userID}` and forwards every incoming Redis message into the client's `send` channel, where `writePump` picks it up and writes it to the WebSocket.

```go
// internal/handler/client.go
package handler

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 4096
)

// Client represents a single WebSocket connection.
type Client struct {
	hub    *Hub
	userID string
	conn   *websocket.Conn
	send   chan []byte

	// Redis pub/sub handle for this client's channel.
	sub *redis.PubSub

	pubsubRepo *repository.PubSubRepo
	msgRepo    *repository.MessageRepo
}

// subscribeLoop listens on the Redis channel for this user and forwards
// every message into the client's send channel.
//
// It exits when sub.Channel() is closed (triggered by sub.Close()).
func (c *Client) subscribeLoop() {
	ch := c.sub.Channel()
	for msg := range ch {
		c.send <- []byte(msg.Payload)
	}
	log.Debug().Str("user", c.userID).Msg("subscribeLoop exited")
}

// readPump reads messages from the WebSocket connection.
func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
		// Closing the subscription causes subscribeLoop to exit.
		if c.sub != nil {
			c.sub.Close()
		}
	}()

	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(
				err,
				websocket.CloseGoingAway,
				websocket.CloseNormalClosure,
			) {
				log.Error().Err(err).Str("user", c.userID).Msg("read error")
			}
			break
		}
		c.handleIncoming(raw)
	}
}

// handleIncoming processes a raw WebSocket message.
func (c *Client) handleIncoming(raw []byte) {
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		log.Error().Err(err).Msg("bad envelope")
		return
	}

	switch envelope.Type {
	case "message":
		c.handleMessage(envelope.Data)
	case "state_update":
		c.handleStateUpdate(envelope.Data)
	}
}

// handleMessage processes an incoming chat message.
//
// Flow:
//  1. Parse the message payload.
//  2. Save to Postgres with state=SENT.
//  3. Send SENT acknowledgement back to the sender via Redis pub/sub.
//  4. Publish the message to the recipient's Redis channel.
func (c *Client) handleMessage(data json.RawMessage) {
	var msg struct {
		ID          string `json:"id"`
		RecipientID string `json:"recipient_id"`
		Content     string `json:"content"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Error().Err(err).Msg("bad message payload")
		return
	}

	ctx := context.Background()

	// 1. Save to Postgres (state = SENT).
	if err := c.msgRepo.Save(ctx, msg.ID, c.userID, msg.RecipientID, msg.Content); err != nil {
		log.Error().Err(err).Msg("failed to save message")
		return
	}

	// 2. Publish SENT ack back to sender via Redis.
	sentAck, _ := json.Marshal(map[string]interface{}{
		"type": "state_update",
		"data": map[string]string{
			"message_id": msg.ID,
			"state":      "SENT",
		},
	})
	if err := c.pubsubRepo.Publish(ctx, c.userID, sentAck); err != nil {
		log.Error().Err(err).Msg("failed to publish SENT ack")
	}

	// 3. Publish message to recipient via Redis.
	outgoing, _ := json.Marshal(map[string]interface{}{
		"type": "message",
		"data": map[string]string{
			"id":        msg.ID,
			"sender_id": c.userID,
			"content":   msg.Content,
		},
	})
	if err := c.pubsubRepo.Publish(ctx, msg.RecipientID, outgoing); err != nil {
		log.Error().Err(err).Msg("failed to publish message to recipient")
	}
}

// handleStateUpdate processes DELIVERED / READ acknowledgements.
//
// Flow:
//  1. Parse the state update.
//  2. Update the message state in Postgres.
//  3. Publish the state update to the original sender via Redis.
func (c *Client) handleStateUpdate(data json.RawMessage) {
	var su struct {
		MessageID string `json:"message_id"`
		State     string `json:"state"`
		SenderID  string `json:"sender_id"`
	}
	if err := json.Unmarshal(data, &su); err != nil {
		log.Error().Err(err).Msg("bad state_update payload")
		return
	}

	ctx := context.Background()

	// 1. Update state in Postgres.
	if err := c.msgRepo.UpdateState(ctx, su.MessageID, su.State); err != nil {
		log.Error().Err(err).Msg("failed to update message state")
		return
	}

	// 2. Publish state update to original sender via Redis.
	payload, _ := json.Marshal(map[string]interface{}{
		"type": "state_update",
		"data": map[string]string{
			"message_id": su.MessageID,
			"state":      su.State,
		},
	})
	if err := c.pubsubRepo.Publish(ctx, su.SenderID, payload); err != nil {
		log.Error().Err(err).Msg("failed to publish state update")
	}
}

// writePump writes messages from the send channel to the WebSocket.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// Hub closed the channel.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
```

### CRITICAL: subscription ordering

Notice that the subscription is created **before** the client is added to the hub's connection map. If we did it the other way around:

```
1. Add client to connMap        <-- another goroutine sees client is "online"
2. Someone publishes to chat:B  <-- message goes to Redis
3. Subscribe to chat:B          <-- too late, message already lost
```

By subscribing first, we guarantee no messages are lost between registration and subscription:

```
1. Subscribe to chat:B          <-- listening from this moment
2. Add client to connMap        <-- now "online" and already listening
3. Someone publishes to chat:B  <-- subscribeLoop receives it
```

---

## 4. Updated Message Routing Flow

Here is the complete flow when User A sends a message to User B:

```
User A (browser)                Server                        Redis                    User B (browser)
     |                            |                             |                            |
     |-- WS: {type:"message"} -->|                             |                            |
     |                            |                             |                            |
     |                    readPump parses msg                   |                            |
     |                    Save to Postgres (SENT)               |                            |
     |                            |                             |                            |
     |                            |-- PUBLISH chat:A (SENT ack)->|                           |
     |                            |-- PUBLISH chat:B (message) ->|                           |
     |                            |                             |                            |
     |                            |<- subscribeLoop A receives --|                           |
     |<-- WS: {state:"SENT"} ----|                             |                            |
     |                            |                             |-- subscribeLoop B receives->|
     |                            |                             |                            |
     |                            |                      User B's subscribeLoop              |
     |                            |                      pushes to send chan                  |
     |                            |                             |                            |
     |                            |                             |    writePump picks it up    |
     |                            |                             |<--- WS: {type:"message"} --|
```

Compare this with the old direct routing:

```
OLD: readPump -> hub.clients[recipientID].send <- payload
NEW: readPump -> Redis PUBLISH -> subscribeLoop -> client.send <- payload
```

The sender no longer needs to know anything about the recipient's client struct. It just publishes to a Redis channel.

---

## 5. Updated ACK / State Routing

When User B sends a DELIVERED or READ acknowledgement:

```
User B (browser)                Server                        Redis                    User A (browser)
     |                            |                             |                            |
     |-- WS: {type:"state_update",                             |                            |
     |    data:{message_id:"X",   |                             |                            |
     |    state:"DELIVERED",      |                             |                            |
     |    sender_id:"A"}} ------>|                             |                            |
     |                            |                             |                            |
     |                    readPump parses state_update          |                            |
     |                    Update Postgres: msg X = DELIVERED    |                            |
     |                            |                             |                            |
     |                            |-- PUBLISH chat:A ---------->|                            |
     |                            |   {state_update: DELIVERED} |                            |
     |                            |                             |                            |
     |                            |                             |-- subscribeLoop A receives |
     |                            |                             |          |                  |
     |                            |                             |    push to send chan        |
     |                            |                             |          |                  |
     |                            |                             |<-- WS: {state:"DELIVERED"} |
```

Same pattern: update Postgres, then PUBLISH via Redis. The sender's `subscribeLoop` receives it.

---

## 6. Goroutine Lifecycle Update

Each client now has **3 goroutines**:

| Goroutine       | Started by      | Reads from         | Writes to          | Exit trigger                     |
| --------------- | --------------- | ------------------ | ------------------ | -------------------------------- |
| `readPump`      | HandleWebSocket | WebSocket conn     | Redis (PUBLISH)    | WS read error or close           |
| `writePump`     | HandleWebSocket | `client.send` chan | WebSocket conn     | `send` chan closed by Hub        |
| `subscribeLoop` | HandleWebSocket | Redis subscription | `client.send` chan | `sub.Close()` called by readPump |

### Shutdown sequence

```
1. WebSocket connection drops (or client disconnects)
      |
2. readPump detects read error, exits
      |
      +---> defer: hub.unregister <- c
      |           Hub removes client from connMap
      |           Hub closes client.send channel
      |               |
      |               +---> writePump sees closed channel, exits
      |
      +---> defer: c.sub.Close()
                  |
                  +---> subscribeLoop's ch range ends, exits
```

All three goroutines exit cleanly. No leaked goroutines, no leaked Redis subscriptions.

---

## 7. Updated `hub.go`

The Hub now holds a `*redis.Client` reference that it passes to each client during registration. The Hub itself no longer routes messages -- it only manages connection tracking.

```go
// internal/handler/hub.go
package handler

import (
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
)

// Hub manages active WebSocket clients.
// It no longer routes messages directly -- Redis pub/sub handles that.
// The Hub's job is now limited to:
//   - Tracking which users are connected (connMap)
//   - Cleaning up on disconnect (closing the send channel)
type Hub struct {
	// Redis client, passed to each Client for subscribeLoop.
	rdb *redis.Client

	// PubSub repository for publishing messages.
	pubsubRepo *repository.PubSubRepo

	// Message repository for Postgres operations.
	msgRepo *repository.MessageRepo

	// Connected clients keyed by userID.
	clients map[string]*Client

	// Register channel.
	register chan *Client

	// Unregister channel.
	unregister chan *Client
}

// NewHub creates a new Hub.
func NewHub(rdb *redis.Client, pubsubRepo *repository.PubSubRepo, msgRepo *repository.MessageRepo) *Hub {
	return &Hub{
		rdb:        rdb,
		pubsubRepo: pubsubRepo,
		msgRepo:    msgRepo,
		clients:    make(map[string]*Client),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
}

// Run starts the Hub's main loop.
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client.userID] = client
			log.Info().Str("user", client.userID).Int("total", len(h.clients)).Msg("client registered")

		case client := <-h.unregister:
			if _, ok := h.clients[client.userID]; ok {
				delete(h.clients, client.userID)
				close(client.send)
				log.Info().Str("user", client.userID).Int("total", len(h.clients)).Msg("client unregistered")
			}
		}
	}
}
```

Notice what is **gone** from the Hub: there is no `broadcast` channel, no message routing logic, no recipient lookup. The Hub is now purely a connection registry.

---

## 8. Updated `HandleWebSocket`

The WebSocket handler creates the client, subscribes to Redis, then starts all three goroutines.

```go
// internal/handler/websocket.go
package handler

import (
	"context"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // In production, validate origin.
	},
}

// HandleWebSocket upgrades the HTTP connection and registers the client.
func HandleWebSocket(hub *Hub, w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		http.Error(w, "missing user_id", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error().Err(err).Msg("upgrade failed")
		return
	}

	// CRITICAL ORDER:
	// 1. Create the Redis subscription FIRST.
	// 2. Then register the client with the Hub.
	// This prevents a race where messages are published between
	// registration and subscription.

	ctx := context.Background()
	sub := hub.pubsubRepo.Subscribe(ctx, userID)

	// Verify the subscription is active by waiting for confirmation.
	// This blocks until Redis confirms the SUBSCRIBE command succeeded.
	_, err = sub.Receive(ctx)
	if err != nil {
		log.Error().Err(err).Str("user", userID).Msg("redis subscribe failed")
		conn.Close()
		sub.Close()
		return
	}

	client := &Client{
		hub:        hub,
		userID:     userID,
		conn:       conn,
		send:       make(chan []byte, 256),
		sub:        sub,
		pubsubRepo: hub.pubsubRepo,
		msgRepo:    hub.msgRepo,
	}

	// Register with the Hub (adds to connMap).
	hub.register <- client

	// Start all three goroutines.
	go client.subscribeLoop()
	go client.writePump()
	go client.readPump()
}
```

The key detail: `sub.Receive(ctx)` is called before anything else. This blocks until Redis confirms the subscription is active. Only then do we register the client and start the goroutines.

---

## Complete Flow: ASCII Diagram

Putting it all together, here is the full architecture after this step:

```
+------------------+          +------------------+
|   User A         |          |   User B         |
|   (browser)      |          |   (browser)      |
+--------+---------+          +--------+---------+
         |                             |
    WebSocket                     WebSocket
         |                             |
+--------+---------+          +--------+---------+
| Client A         |          | Client B         |
| - readPump       |          | - readPump       |
| - writePump      |          | - writePump      |
| - subscribeLoop  |          | - subscribeLoop  |
+--------+---------+          +--------+---------+
         |                             |
         |    +-------------------+    |
         |    |                   |    |
         +--->|    Redis          |<---+
         |    |  pub/sub          |    |
         |    |                   |    |
         |    | chat:A  chat:B    |    |
         |    +-------------------+    |
         |                             |
         |    +-------------------+    |
         +--->|   Postgres        |<---+
              |   (messages)      |
              +-------------------+

    readPump writes to: Postgres + Redis PUBLISH
    subscribeLoop reads from: Redis SUBSCRIBE
    writePump reads from: client.send channel
```

---

## Acceptance Test

### Setup

Make sure Redis is running:

```bash
redis-cli ping
# PONG
```

### Monitor Redis traffic

In a separate terminal:

```bash
redis-cli MONITOR
```

### Test with two browser tabs

1. Open two browser tabs to your chat UI.
2. Connect as User A (`ws://localhost:8080/ws?user_id=alice`).
3. Connect as User B (`ws://localhost:8080/ws?user_id=bob`).

### Verify subscriptions

In the `redis-cli MONITOR` output, you should see:

```
"SUBSCRIBE" "chat:alice"
"SUBSCRIBE" "chat:bob"
```

### Send a message from Alice to Bob

Send from Alice's WebSocket:

```json
{
  "type": "message",
  "data": {
    "id": "msg-001",
    "recipient_id": "bob",
    "content": "Hello Bob!"
  }
}
```

### Verify Redis MONITOR output

You should see two PUBLISH commands:

```
"PUBLISH" "chat:alice" "{\"type\":\"state_update\",\"data\":{\"message_id\":\"msg-001\",\"state\":\"SENT\"}}"
"PUBLISH" "chat:bob"   "{\"type\":\"message\",\"data\":{\"id\":\"msg-001\",\"sender_id\":\"alice\",\"content\":\"Hello Bob!\"}}"
```

The first PUBLISH is the SENT acknowledgement back to Alice. The second is the message delivery to Bob.

### Verify Alice receives SENT ack

Alice's WebSocket should receive:

```json
{
  "type": "state_update",
  "data": {
    "message_id": "msg-001",
    "state": "SENT"
  }
}
```

### Verify Bob receives the message

Bob's WebSocket should receive:

```json
{
  "type": "message",
  "data": {
    "id": "msg-001",
    "sender_id": "alice",
    "content": "Hello Bob!"
  }
}
```

### Verify state updates flow through Redis

Send a DELIVERED ack from Bob:

```json
{
  "type": "state_update",
  "data": {
    "message_id": "msg-001",
    "state": "DELIVERED",
    "sender_id": "alice"
  }
}
```

Redis MONITOR should show:

```
"PUBLISH" "chat:alice" "{\"type\":\"state_update\",\"data\":{\"message_id\":\"msg-001\",\"state\":\"DELIVERED\"}}"
```

Alice's WebSocket receives the DELIVERED state update.

### Verify cleanup on disconnect

Close Bob's browser tab. Redis MONITOR should show:

```
"UNSUBSCRIBE" "chat:bob"
```

This confirms `sub.Close()` was called in readPump's defer, and the subscribeLoop exited cleanly.

---

## What Changed From Step 5

| Component          | Before (Step 5)                     | After (Step 6)                           |
| ------------------ | ----------------------------------- | ---------------------------------------- |
| Message routing    | `hub.clients[id].send <- payload`   | `pubsubRepo.Publish(ctx, id, payload)`   |
| State routing      | `hub.clients[senderID].send <- ack` | `pubsubRepo.Publish(ctx, senderID, ack)` |
| Client goroutines  | readPump, writePump (2)             | readPump, writePump, subscribeLoop (3)   |
| Hub responsibility | Route messages + manage connections | Manage connections only                  |
| New files          | --                                  | `internal/repository/pubsub.go`          |
| Modified files     | --                                  | `client.go`, `hub.go`, `websocket.go`    |

---

## Next Step

Messages now flow through Redis pub/sub, but if the recipient is offline when the PUBLISH happens, the message is lost from the real-time path (it is still saved in Postgres). Step 7 adds an **offline message buffer** that detects when a user comes online and replays any messages they missed.
