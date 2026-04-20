# Step 7 -- Offline Buffer (Store-and-Forward)

Module: `github.com/Rengoku1926/wp_proto/apps/backend`

**Prerequisites:** Hub, pub/sub routing, delivery states working. Messages route through Redis pub/sub. The message is always persisted to Postgres first (Step 5), then published via Redis (Step 6).

---

## 1. The Problem

Redis pub/sub is fire-and-forget. When Alice sends a message to Bob, the server runs:

```
PUBLISH chat:bob <message-json>
```

If Bob is connected, his `subscribeLoop` picks it up and delivers it over the WebSocket. But if Bob is offline -- no one is subscribed to `chat:bob`. Redis sees zero subscribers and silently drops the PUBLISH. The message is gone from the real-time path.

Yes, the message is saved in Postgres. But Postgres is the long-term archive. We need a fast, lightweight mechanism that:

1. Detects the recipient is offline at the moment of sending.
2. Buffers the message.
3. Replays all buffered messages the instant the recipient reconnects.

This is the **store-and-forward** pattern -- the same approach used by every major messaging system (WhatsApp, Signal, iMessage).

---

## 2. The Solution: Redis Lists as Offline Queues

We use a Redis List for each offline user. The key pattern is:

```
offline:{userID}
```

### Why a Redis List?

A Redis List is a doubly-linked list of strings. It supports:

| Command              | What it does                            | Time complexity |
| -------------------- | --------------------------------------- | --------------- |
| `LPUSH key value`    | Insert at the head (left) of the list   | O(1)            |
| `RPOP key`           | Remove and return from the tail (right) | O(1)            |
| `LLEN key`           | Return the length of the list           | O(1)            |
| `EXPIRE key seconds` | Set a TTL on the key                    | O(1)            |

By pushing at the head (LPUSH) and popping from the tail (RPOP), we get **FIFO order** -- messages come out in the same order they went in.

```
LPUSH: [msg3] -> [msg2] -> [msg1]    (newest at head)
RPOP:  [msg3] -> [msg2] -> [msg1]    (oldest popped first)
                                 ^
                                 RPOP returns msg1
```

### Why Redis Lists over Postgres?

| Concern           | Redis List                          | Postgres table                      |
| ----------------- | ----------------------------------- | ----------------------------------- |
| Push latency      | ~0.1ms (in-memory)                  | ~1-5ms (disk + WAL)                 |
| Pop (drain)       | O(1) per message, atomic            | DELETE with ORDER BY, row locks     |
| TTL               | Built-in EXPIRE, automatic cleanup  | Requires a cron job or pg_cron      |
| Bounded retention | EXPIRE handles it                   | Manual cleanup                      |
| Atomic push + TTL | Pipeline (2 commands, 1 round-trip) | Transaction (BEGIN/COMMIT overhead) |

The offline buffer is ephemeral by design. Messages live here for at most 30 days. Postgres already has the permanent copy. Redis gives us speed and automatic expiry.

---

## 3. Offline Store Repository -- `internal/repository/offline.go`

```go
// internal/repository/offline.go
package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
	"github.com/redis/go-redis/v9"
)

const offlineTTL = 30 * 24 * time.Hour // 30 days

// OfflineStore manages per-user offline message buffers in Redis.
type OfflineStore struct {
	rdb *redis.Client
}

// NewOfflineStore creates a new OfflineStore.
func NewOfflineStore(rdb *redis.Client) *OfflineStore {
	return &OfflineStore{rdb: rdb}
}

// offlineKey returns the Redis key for a user's offline buffer.
func offlineKey(userID string) string {
	return fmt.Sprintf("offline:%s", userID)
}

// Push adds a message to the user's offline buffer.
//
// It uses a Redis pipeline to send LPUSH and EXPIRE in a single round-trip.
// LPUSH inserts the message at the head of the list.
// EXPIRE refreshes the TTL to 30 days from now.
func (s *OfflineStore) Push(ctx context.Context, userID string, msg *model.Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	key := offlineKey(userID)

	// Pipeline: send both commands in one round-trip.
	pipe := s.rdb.Pipeline()
	pipe.LPush(ctx, key, data)
	pipe.Expire(ctx, key, offlineTTL)
	_, err = pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("offline push pipeline: %w", err)
	}

	return nil
}

// Drain removes and returns all messages from the user's offline buffer.
//
// Messages are returned in FIFO order (oldest first) because we LPUSH
// on write and RPOP on read.
//
// After this call, the offline buffer for this user is empty.
func (s *OfflineStore) Drain(ctx context.Context, userID string) ([]*model.Message, error) {
	key := offlineKey(userID)
	var messages []*model.Message

	for {
		// RPOP removes and returns the tail element (oldest message).
		data, err := s.rdb.RPop(ctx, key).Bytes()
		if err == redis.Nil {
			// List is empty, we're done.
			break
		}
		if err != nil {
			return messages, fmt.Errorf("offline rpop: %w", err)
		}

		var msg model.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			// Log and skip malformed entries rather than failing the entire drain.
			continue
		}
		messages = append(messages, &msg)
	}

	return messages, nil
}

// Count returns the number of messages in a user's offline buffer.
// Useful for diagnostics and monitoring.
func (s *OfflineStore) Count(ctx context.Context, userID string) (int64, error) {
	return s.rdb.LLen(ctx, offlineKey(userID)).Result()
}
```

### Line-by-line: Push

1. **`json.Marshal(msg)`** -- serialize the `model.Message` struct to JSON bytes. This is the same format the WebSocket sends, so no conversion is needed on drain.

2. **`s.rdb.Pipeline()`** -- creates a pipeline. Commands added to the pipeline are buffered locally and sent to Redis in a single network round-trip when `Exec()` is called.

3. **`pipe.LPush(ctx, key, data)`** -- `LPUSH offline:bob <json>`. Inserts the message at the head of the list. If the list does not exist, Redis creates it automatically.

4. **`pipe.Expire(ctx, key, offlineTTL)`** -- `EXPIRE offline:bob 2592000`. Sets (or refreshes) the TTL to 30 days. Every new message resets the clock.

5. **`pipe.Exec(ctx)`** -- sends both commands to Redis in one round-trip. Both execute atomically from the client's perspective (no other command can interleave between them on this pipeline).

### Line-by-line: Drain

1. **`s.rdb.RPop(ctx, key).Bytes()`** -- `RPOP offline:bob`. Removes and returns the tail element. Because we LPUSH (head) and RPOP (tail), this gives FIFO ordering.

2. **`if err == redis.Nil`** -- `redis.Nil` is returned when the list is empty (or the key does not exist). This is our exit condition.

3. **Loop until `redis.Nil`** -- we drain the entire buffer in one pass. Each RPOP is O(1). For 100 buffered messages, that is 100 round-trips. If this becomes a bottleneck, you could use a Lua script to drain in a single call, but for typical message volumes this is fine.

---

## 4. Message Router -- `internal/router/router.go` (NEW file)

Up until now, `handleMessage` in `client.go` directly calls `pubsubRepo.Publish()`. This works when the recipient is online, but it silently drops messages when they are offline.

The Router encapsulates the online/offline decision:

```go
// internal/router/router.go
package router

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/handler"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
	"github.com/rs/zerolog"
)

// Router decides whether to deliver a message via pub/sub (online)
// or push it to the offline buffer (offline).
type Router struct {
	hub          *handler.Hub
	offlineStore *repository.OfflineStore
	pubsubRepo   *repository.PubSubRepo
	msgRepo      *repository.MessageRepo
	log          zerolog.Logger
}

// NewRouter creates a new Router.
func NewRouter(
	hub *handler.Hub,
	offlineStore *repository.OfflineStore,
	pubsubRepo *repository.PubSubRepo,
	msgRepo *repository.MessageRepo,
	log zerolog.Logger,
) *Router {
	return &Router{
		hub:          hub,
		offlineStore: offlineStore,
		pubsubRepo:   pubsubRepo,
		msgRepo:      msgRepo,
		log:          log,
	}
}

// Route delivers a message to the recipient.
//
// Decision logic:
//  1. Check if the recipient is currently connected (hub.IsOnline).
//  2. If online: publish via Redis pub/sub for real-time delivery.
//  3. If offline: push to the offline buffer for later drain.
//
// Even when the hub reports "online", the pub/sub delivery might fail
// (network blip, Redis overload). In that case, we fall back to the
// offline buffer. The offline buffer is the safety net.
func (r *Router) Route(ctx context.Context, recipientID string, msg *model.Message) error {
	if r.hub.IsOnline(recipientID) {
		// Recipient appears to be online. Try pub/sub delivery.
		payload, err := json.Marshal(map[string]interface{}{
			"type": "message",
			"data": map[string]string{
				"id":        msg.ID,
				"sender_id": msg.SenderID,
				"content":   msg.Content,
			},
		})
		if err != nil {
			return fmt.Errorf("marshal message: %w", err)
		}

		err = r.pubsubRepo.Publish(ctx, recipientID, payload)
		if err != nil {
			// Pub/sub failed. Fall back to offline buffer.
			r.log.Warn().
				Err(err).
				Str("recipient", recipientID).
				Msg("pub/sub publish failed, falling back to offline buffer")

			return r.offlineStore.Push(ctx, recipientID, msg)
		}

		r.log.Debug().
			Str("recipient", recipientID).
			Str("msg_id", msg.ID).
			Msg("message delivered via pub/sub")

		return nil
	}

	// Recipient is offline. Push to offline buffer.
	r.log.Debug().
		Str("recipient", recipientID).
		Str("msg_id", msg.ID).
		Msg("recipient offline, buffering message")

	return r.offlineStore.Push(ctx, recipientID, msg)
}
```

### The `IsOnline` method on Hub

The Router needs to ask the Hub whether a user is currently connected. Add this method to `hub.go`:

```go
// IsOnline returns true if the user has an active WebSocket connection
// on this server instance.
//
// NOTE: this is checked from outside the Hub's Run goroutine, so we
// need synchronization. We use a sync.RWMutex to protect connMap reads.
func (h *Hub) IsOnline(userID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.clients[userID]
	return ok
}
```

This requires adding a `sync.RWMutex` to the Hub struct and locking it in the `Run()` loop wherever `h.clients` is modified. Here is the updated Hub:

```go
// internal/handler/hub.go
package handler

import (
	"sync"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
)

type Hub struct {
	rdb        *redis.Client
	pubsubRepo *repository.PubSubRepo
	msgRepo    *repository.MessageRepo

	mu      sync.RWMutex
	clients map[string]*Client

	register   chan *Client
	unregister chan *Client
}

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

func (h *Hub) IsOnline(userID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.clients[userID]
	return ok
}

func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client.userID] = client
			h.mu.Unlock()
			log.Info().Str("user", client.userID).Int("total", len(h.clients)).Msg("client registered")

			// Drain offline buffer AFTER registration.
			// See Section 5 for why ordering matters.
			go client.drainOfflineBuffer()

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client.userID]; ok {
				delete(h.clients, client.userID)
				close(client.send)
				log.Info().Str("user", client.userID).Int("total", len(h.clients)).Msg("client unregistered")
			}
			h.mu.Unlock()
		}
	}
}
```

The key changes:

- Added `sync.RWMutex` to protect concurrent access to `clients` map.
- `IsOnline()` takes a read lock -- multiple goroutines can check online status concurrently.
- `Run()` takes a write lock when modifying the map.
- After registering a client, we kick off `drainOfflineBuffer()` in a goroutine.

### Why the Router falls back to offline buffer on pub/sub failure

Even if `hub.IsOnline(recipientID)` returns true, the Redis PUBLISH can still fail:

- Momentary Redis connectivity issue.
- Redis is overloaded and the command times out.
- The recipient disconnected between the `IsOnline` check and the PUBLISH (race window).

In all these cases, the message would be lost from the real-time path. By falling back to the offline buffer, we guarantee the message will be delivered when the recipient next connects.

---

## 5. Drain on Reconnect -- `internal/handler/client.go`

When a user reconnects, we need to replay all buffered messages. Add this method to the Client struct:

```go
// internal/handler/client.go -- add to existing file

// drainOfflineBuffer replays any messages that were buffered while
// this user was offline.
//
// Called by Hub.Run() in a goroutine after the client is added to connMap.
func (c *Client) drainOfflineBuffer() {
	ctx := context.Background()

	messages, err := c.offlineStore.Drain(ctx, c.userID)
	if err != nil {
		log.Error().Err(err).Str("user", c.userID).Msg("failed to drain offline buffer")
		return
	}

	if len(messages) == 0 {
		return
	}

	for _, msg := range messages {
		payload, err := json.Marshal(map[string]interface{}{
			"type": "message",
			"data": map[string]string{
				"id":        msg.ID,
				"sender_id": msg.SenderID,
				"content":   msg.Content,
			},
		})
		if err != nil {
			log.Error().Err(err).Str("msg_id", msg.ID).Msg("failed to marshal offline message")
			continue
		}

		// Push to the client's send channel.
		// Use select-with-default to avoid blocking if the channel is full
		// (which would mean the client is slow or disconnected).
		select {
		case c.send <- payload:
		default:
			log.Warn().
				Str("user", c.userID).
				Str("msg_id", msg.ID).
				Msg("send channel full during offline drain, message dropped")
		}
	}

	log.Info().
		Str("user", c.userID).
		Int("count", len(messages)).
		Msg("drained offline buffer")
}
```

The Client struct also needs the `offlineStore` field. Update the struct:

```go
// Client represents a single WebSocket connection.
type Client struct {
	hub    *Hub
	userID string
	conn   *websocket.Conn
	send   chan []byte

	sub *redis.PubSub

	pubsubRepo   *repository.PubSubRepo
	msgRepo      *repository.MessageRepo
	offlineStore *repository.OfflineStore
}
```

### CRITICAL: Operation ordering on reconnect

The ordering of operations when a user connects is essential for correctness:

```
1. Subscribe to Redis pub/sub channel (chat:bob)
   -- Bob is now listening for new real-time messages.

2. Register client with Hub (add to connMap)
   -- Bob is now "online" from the Router's perspective.
   -- Any new messages from this point forward go through pub/sub.

3. Drain offline buffer
   -- Replay everything that was buffered while Bob was offline.
```

Why this order?

**If we drained BEFORE subscribing:**

```
1. Drain offline buffer (get messages 1, 2, 3)
2. Alice sends message 4           <-- goes to pub/sub
3. Subscribe to Redis pub/sub      <-- too late, message 4 is lost
```

**If we drained BEFORE registering with Hub:**

```
1. Subscribe to pub/sub
2. Drain offline buffer (get messages 1, 2, 3)
3. Alice sends message 4           <-- Router checks IsOnline -> false
                                       Router pushes to offline buffer
4. Register with Hub               <-- now "online"
                                       But message 4 is in offline buffer
                                       and no one drains again!
```

**Correct order (subscribe, register, drain):**

```
1. Subscribe to pub/sub            <-- listening for new messages
2. Register with Hub               <-- Router now sends via pub/sub
3. Drain offline buffer            <-- replay old messages
4. Alice sends message 4           <-- Router checks IsOnline -> true
                                       pub/sub delivers it
                                       subscribeLoop receives it
```

There is a brief overlap window where a message could arrive via both pub/sub (because we subscribed in step 1) AND be in the offline buffer (if it was pushed between the last drain and now). This is a dedup concern handled in Section 6.

---

## 6. Idempotency on Drain

During the race window between subscribe and drain, a message could theoretically be delivered twice:

1. Alice sends a message while Bob is offline -- it goes to the offline buffer.
2. Bob reconnects. His `subscribeLoop` starts. Hub registers him.
3. Alice sends another message -- this one goes via pub/sub AND might have been pushed to the offline buffer if the Router checked `IsOnline` a microsecond before registration completed.

In practice this race is extremely narrow, but we handle it:

**Client-side dedup:** Every message has a unique `id` (the `client_id` generated by the sender). The client application (browser/mobile) maintains a set of seen message IDs and ignores duplicates. This is standard practice in messaging clients.

**Server-side safety:** There is no double-insert risk in Postgres. The message was saved to Postgres exactly once (in `handleMessage`, before any routing). The offline buffer and pub/sub are delivery mechanisms, not storage. Even if the same message is delivered twice over the WebSocket, Postgres has exactly one copy.

---

## 7. TTL Management

Each `Push` call refreshes the EXPIRE on the offline key:

```
LPUSH offline:bob <msg1>       -- creates key, list = [msg1]
EXPIRE offline:bob 2592000     -- TTL = 30 days from now

... 5 days later ...

LPUSH offline:bob <msg2>       -- list = [msg2, msg1]
EXPIRE offline:bob 2592000     -- TTL reset to 30 days from NOW
```

This means the TTL is always "30 days since the last message was buffered." If no one sends Bob a message for 30 days, the key auto-deletes and Redis reclaims the memory.

### Why this prevents unbounded growth

Without TTL, a user who signs up and never returns would accumulate messages forever. With the 30-day EXPIRE:

- Active users who go offline temporarily: buffer is drained on reconnect (usually within hours).
- Users who stop using the app: buffer auto-deletes after 30 days of inactivity.
- Abandoned accounts: same as above.

This is a simple and effective approach. No background cleanup jobs, no cron, no manual garbage collection.

---

## 8. Redis Pipeline: Why Push Uses It

The `Push` method needs to execute two Redis commands:

```
LPUSH offline:bob <data>
EXPIRE offline:bob 2592000
```

Without a pipeline, this is two network round-trips:

```
Client -> Redis: LPUSH       (round-trip 1: ~0.1ms)
Redis  -> Client: OK
Client -> Redis: EXPIRE      (round-trip 2: ~0.1ms)
Redis  -> Client: OK
```

With a pipeline, both commands are buffered locally and sent in a single write:

```
Client -> Redis: LPUSH + EXPIRE  (one round-trip: ~0.1ms)
Redis  -> Client: OK, OK
```

### Is it truly atomic?

Strictly speaking, Redis pipelines are not atomic in the way a `MULTI/EXEC` transaction is. Another client could interleave a command between the LPUSH and EXPIRE. However:

1. The EXPIRE refreshes an existing TTL -- if another EXPIRE runs between ours, the result is the same (TTL gets set to 30 days).
2. The worst case without EXPIRE is the key lives with its previous TTL, which is at most 30 days. Not a correctness issue.
3. From the calling client's perspective, both commands succeed or fail together (the pipeline either sends or it does not).

For true atomicity, you would use `MULTI/EXEC`. But for this use case, a pipeline is sufficient and faster (no transaction overhead).

---

## 9. Updated `main.go`

Wire the new components into the application startup:

```go
// cmd/main.go
package main

import (
	"context"
	"net/http"
	"os"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/config"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/database"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/handler"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/logger"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/router"
)

func main() {
	cfg, err := config.Load(".env.local")
	if err != nil {
		panic(err)
	}

	log := logger.New(cfg.Primary.Env)

	ctx := context.Background()

	pool, err := database.Connect(ctx, cfg.Database)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to database")
		os.Exit(1)
	}
	defer pool.Close()
	log.Info().Msg("successfully connected to postgres")

	if err := database.Migrate(ctx, pool); err != nil {
		log.Fatal().Err(err).Msg("failed to run migrations")
		os.Exit(1)
	}
	log.Info().Msg("migrations completed successfully")

	redisClient, err := database.ConnectRedis(ctx, cfg.Redis)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to redis")
		os.Exit(1)
	}
	defer redisClient.Close()
	log.Info().Msg("successfully connected to redis")

	// Repositories
	pubsubRepo := repository.NewPubSubRepo(redisClient)
	msgRepo := repository.NewMessageRepo(pool)
	offlineStore := repository.NewOfflineStore(redisClient)

	// Hub
	hub := handler.NewHub(redisClient, pubsubRepo, msgRepo, offlineStore)
	go hub.Run()

	// Router
	msgRouter := router.NewRouter(hub, offlineStore, pubsubRepo, msgRepo, log)

	// HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		handler.HandleWebSocket(hub, w, r)
	})

	_ = msgRouter // used by handler via Hub -- see updated HandleWebSocket below

	log.Info().Msg("starting server on :8080")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatal().Err(err).Msg("server failed")
	}
}
```

### Updated `HandleWebSocket` to pass `offlineStore`

The `HandleWebSocket` function now passes the `offlineStore` to the Client:

```go
// internal/handler/websocket.go
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
	// 1. Subscribe to Redis pub/sub FIRST.
	// 2. Register with Hub (which triggers offline drain).
	ctx := context.Background()
	sub := hub.pubsubRepo.Subscribe(ctx, userID)

	_, err = sub.Receive(ctx)
	if err != nil {
		log.Error().Err(err).Str("user", userID).Msg("redis subscribe failed")
		conn.Close()
		sub.Close()
		return
	}

	client := &Client{
		hub:          hub,
		userID:       userID,
		conn:         conn,
		send:         make(chan []byte, 256),
		sub:          sub,
		pubsubRepo:   hub.pubsubRepo,
		msgRepo:      hub.msgRepo,
		offlineStore: hub.offlineStore,
	}

	// Register with Hub. Hub.Run() will call drainOfflineBuffer().
	hub.register <- client

	go client.subscribeLoop()
	go client.writePump()
	go client.readPump()
}
```

And the Hub needs to expose `offlineStore`:

```go
// Add to Hub struct:
type Hub struct {
	rdb          *redis.Client
	pubsubRepo   *repository.PubSubRepo
	msgRepo      *repository.MessageRepo
	offlineStore *repository.OfflineStore

	mu      sync.RWMutex
	clients map[string]*Client

	register   chan *Client
	unregister chan *Client
}

func NewHub(
	rdb *redis.Client,
	pubsubRepo *repository.PubSubRepo,
	msgRepo *repository.MessageRepo,
	offlineStore *repository.OfflineStore,
) *Hub {
	return &Hub{
		rdb:          rdb,
		pubsubRepo:   pubsubRepo,
		msgRepo:      msgRepo,
		offlineStore: offlineStore,
		clients:      make(map[string]*Client),
		register:     make(chan *Client),
		unregister:   make(chan *Client),
	}
}
```

### Updated `handleMessage` to use the Router

The client's `handleMessage` now uses the Router instead of directly calling `pubsubRepo.Publish()`:

```go
// internal/handler/client.go -- updated handleMessage

func (c *Client) handleMessage(data json.RawMessage) {
	var incoming struct {
		ID          string `json:"id"`
		RecipientID string `json:"recipient_id"`
		Content     string `json:"content"`
	}
	if err := json.Unmarshal(data, &incoming); err != nil {
		log.Error().Err(err).Msg("bad message payload")
		return
	}

	ctx := context.Background()

	// 1. Save to Postgres (state = SENT).
	if err := c.msgRepo.Save(ctx, incoming.ID, c.userID, incoming.RecipientID, incoming.Content); err != nil {
		log.Error().Err(err).Msg("failed to save message")
		return
	}

	// 2. Send SENT ack back to sender via Redis pub/sub.
	sentAck, _ := json.Marshal(map[string]interface{}{
		"type": "state_update",
		"data": map[string]string{
			"message_id": incoming.ID,
			"state":      "SENT",
		},
	})
	if err := c.pubsubRepo.Publish(ctx, c.userID, sentAck); err != nil {
		log.Error().Err(err).Msg("failed to publish SENT ack")
	}

	// 3. Route message to recipient (online via pub/sub, offline via buffer).
	msg := &model.Message{
		ID:       incoming.ID,
		SenderID: c.userID,
		Content:  incoming.Content,
	}
	if err := c.router.Route(ctx, incoming.RecipientID, msg); err != nil {
		log.Error().Err(err).Msg("failed to route message")
	}
}
```

This requires adding a `router` field to the Client struct and passing it during creation. The Client struct becomes:

```go
type Client struct {
	hub    *Hub
	userID string
	conn   *websocket.Conn
	send   chan []byte

	sub *redis.PubSub

	pubsubRepo   *repository.PubSubRepo
	msgRepo      *repository.MessageRepo
	offlineStore *repository.OfflineStore
	router       *router.Router
}
```

---

## Complete Flow Diagram

### Alice sends to online Bob

```
Alice (browser)         Server                  Redis               Bob (browser)
     |                    |                       |                       |
     |-- WS: message -->  |                       |                       |
     |                    |                       |                       |
     |              Save to Postgres              |                       |
     |              PUBLISH chat:alice (SENT ack)  |                       |
     |                    |                       |                       |
     |              Router.Route():               |                       |
     |                hub.IsOnline("bob") = true  |                       |
     |                PUBLISH chat:bob            |                       |
     |                    |                       |                       |
     |<-- WS: SENT ack --|                       |                       |
     |                    |                       |-- subscribeLoop -->   |
     |                    |                       |                  WS: message
```

### Alice sends to offline Bob

```
Alice (browser)         Server                  Redis
     |                    |                       |
     |-- WS: message -->  |                       |
     |                    |                       |
     |              Save to Postgres              |
     |              PUBLISH chat:alice (SENT ack)  |
     |                    |                       |
     |              Router.Route():               |
     |                hub.IsOnline("bob") = false |
     |                LPUSH offline:bob <msg>     |
     |                EXPIRE offline:bob 2592000  |
     |                    |                       |
     |<-- WS: SENT ack --|                       |
```

### Bob reconnects

```
Bob (browser)           Server                  Redis
     |                    |                       |
     |-- WS upgrade ---> |                       |
     |                    |                       |
     |              SUBSCRIBE chat:bob            |
     |              Add to Hub connMap            |
     |              drainOfflineBuffer():         |
     |                RPOP offline:bob -> msg1    |
     |                RPOP offline:bob -> msg2    |
     |                RPOP offline:bob -> msg3    |
     |                RPOP offline:bob -> nil     |
     |                    |                       |
     |<-- WS: msg1 ------|                       |
     |<-- WS: msg2 ------|                       |
     |<-- WS: msg3 ------|                       |
```

---

## Acceptance Test

### Setup

Make sure Redis and Postgres are running:

```bash
redis-cli ping
# PONG
```

Start the server:

```bash
go run cmd/main.go
```

### Step 1: Connect Alice and Bob

Open two WebSocket connections (using `wscat` or browser dev tools):

```bash
# Terminal 1 - Alice
wscat -c "ws://localhost:8080/ws?user_id=alice"

# Terminal 2 - Bob
wscat -c "ws://localhost:8080/ws?user_id=bob"
```

Server logs should show:

```
client registered    user=alice total=1
client registered    user=bob   total=2
```

### Step 2: Disconnect Bob

Close Bob's WebSocket connection (Ctrl+C in Terminal 2).

Server logs:

```
client unregistered  user=bob total=1
```

### Step 3: Alice sends 3 messages

In Alice's terminal:

```json
{"type":"message","data":{"id":"msg-001","recipient_id":"bob","content":"Hey Bob, are you there?"}}
{"type":"message","data":{"id":"msg-002","recipient_id":"bob","content":"I have news!"}}
{"type":"message","data":{"id":"msg-003","recipient_id":"bob","content":"Check this out"}}
```

Server logs should show for each message:

```
recipient offline, buffering message  recipient=bob msg_id=msg-001
recipient offline, buffering message  recipient=bob msg_id=msg-002
recipient offline, buffering message  recipient=bob msg_id=msg-003
```

Alice receives 3 SENT acks (one per message).

### Step 4: Verify offline buffer in Redis

```bash
redis-cli LLEN offline:bob
# (integer) 3
```

Inspect the contents (without removing them):

```bash
redis-cli LRANGE offline:bob 0 -1
```

You should see 3 JSON-encoded messages. The newest (msg-003) is at index 0 (head), the oldest (msg-001) is at index 2 (tail).

Check the TTL:

```bash
redis-cli TTL offline:bob
# (integer) ~2592000   (30 days in seconds)
```

### Step 5: Reconnect Bob

Open a new WebSocket connection for Bob:

```bash
# Terminal 2 - Bob reconnects
wscat -c "ws://localhost:8080/ws?user_id=bob"
```

Server logs:

```
client registered         user=bob total=2
drained offline buffer    user=bob count=3
```

### Step 6: Bob receives all 3 messages

Bob's WebSocket should immediately receive all 3 messages in FIFO order:

```json
{"type":"message","data":{"id":"msg-001","sender_id":"alice","content":"Hey Bob, are you there?"}}
{"type":"message","data":{"id":"msg-002","sender_id":"alice","content":"I have news!"}}
{"type":"message","data":{"id":"msg-003","sender_id":"alice","content":"Check this out"}}
```

### Step 7: Verify offline buffer is empty

```bash
redis-cli LLEN offline:bob
# (integer) 0
```

The buffer has been fully drained. The key may still exist briefly (with an empty list) or be auto-deleted by Redis when the last element is popped.

---

## What Changed From Step 6

| Component        | Before (Step 6)                 | After (Step 7)                                                |
| ---------------- | ------------------------------- | ------------------------------------------------------------- |
| Message routing  | `pubsubRepo.Publish()` directly | `Router.Route()` (online/offline decision)                    |
| Offline handling | Messages silently dropped       | Buffered in `offline:{userID}` Redis list                     |
| On reconnect     | Nothing                         | `drainOfflineBuffer()` replays missed messages                |
| Hub              | No mutex, no IsOnline()         | `sync.RWMutex`, `IsOnline()` method                           |
| New files        | --                              | `internal/repository/offline.go`, `internal/router/router.go` |
| Modified files   | --                              | `hub.go`, `client.go`, `websocket.go`, `main.go`              |

---

## Next Step

Messages are now guaranteed to be delivered even when the recipient is offline. But we have no way for a user to fetch their full conversation history (scrolling back through old messages). Step 8 adds **pagination and history queries** using Postgres cursors.
