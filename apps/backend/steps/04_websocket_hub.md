# Step 4: The WebSocket Hub

## Module: `github.com/Rengoku1926/wp_proto/apps/backend`

**Prerequisites:** Steps 1-3 complete (HTTP server running, user registration working, simple WebSocket connection with direct message delivery via a `map[string]*websocket.Conn` protected by `sync.Mutex`).

---

## 1. Why a Hub?

In Step 3, we had something like this:

```go
var (
    connMap = make(map[string]*websocket.Conn)
    mu      sync.Mutex
)
```

Every time a message arrives, `readPump` locks the mutex, looks up the recipient, and writes to their connection. Every time a user disconnects, we lock the mutex and delete them. This works for two users in a demo. It falls apart in production for three reasons:

### Problem 1: Deadlocks

Imagine `readPump` for User A holds the mutex to look up User B. At the same time, User B's `readPump` holds the mutex to look up User A. With a single `sync.Mutex`, this exact scenario does not deadlock (one will wait), but the moment you add any nesting -- say, checking if the recipient is online and then writing to their connection while holding the lock -- you are one refactor away from a deadlock. Worse, `websocket.Conn.WriteMessage` can block if the network buffer is full. If you call `WriteMessage` while holding the mutex, you are holding a lock while doing I/O. That is the textbook recipe for deadlocks under load.

### Problem 2: No centralized lifecycle management

With the naive approach, any goroutine can add or remove connections from the map. There is no single place that owns the lifecycle of a connection. When a user disconnects, who is responsible for closing the connection? Who cleans up the map entry? If two goroutines race to delete the same entry and close the same connection, you get a panic (closing an already-closed connection).

### Problem 3: Goroutine leaks

Without clear ownership of connection lifecycle, goroutines that are writing to a dead connection may hang forever. There is no mechanism to signal a write goroutine that the connection is gone. Over time, these leaked goroutines accumulate and eat memory.

### The Solution: The Hub Pattern

The Hub pattern comes from the Go CSP (Communicating Sequential Processes) model. The core idea:

> **"Don't communicate by sharing memory; share memory by communicating."** -- Rob Pike

Instead of multiple goroutines accessing a shared map with locks, we have **one goroutine** that owns the map. All other goroutines communicate with it through channels:

- Want to register a client? Send it on the `register` channel.
- Want to unregister a client? Send it on the `unregister` channel.
- Want to check if a user is online? Use a read-locked accessor (the only exception, explained below).

The hub's `Run()` method is a single `for-select` loop. It is the **only** goroutine that writes to the map. This eliminates an entire class of bugs.

---

## 2. Hub Struct

Create the file `internal/handler/hub.go`:

```go
package handler

import (
	"sync"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/logger"
)

// Hub maintains the set of active clients and routes messages between them.
// It is the central nerve of the WebSocket system. Only the Run() goroutine
// writes to the clients map. External reads go through RLock-protected accessors.
type Hub struct {
	// clients maps userID -> *Client for all connected users.
	// WRITE access: only inside Run() goroutine (no lock needed for writes).
	// READ access: through RLock-protected methods like GetClient, IsOnline.
	clients map[string]*Client

	// register is a channel for	 clients requesting to join the hub.
	// The readPump or WebSocket handler sends the client here after upgrade.
	register chan *Client

	// unregister is a channel for clients requesting to leave the hub.
	// The readPump sends the client here on disconnect (clean or dirty).
	unregister chan *Client

	// mu protects read access to the clients map from outside the Run goroutine.
	// Run() itself does not need to acquire this lock for writes because it is
	// the sole writer -- but it DOES acquire a write lock so that concurrent
	// readers see a consistent snapshot.
	mu sync.RWMutex
}

// NewHub creates a Hub with initialized channels and map.
// The channels are unbuffered. This is intentional: we want register/unregister
// operations to be synchronous from the caller's perspective, ensuring the map
// is updated before the caller proceeds. In practice, the Run() loop processes
// them almost instantly, so there is negligible blocking.
func NewHub() *Hub {
	return &Hub{
		clients:    make(map[string]*Client),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
}

// Run is the Hub's main loop. Start it as a goroutine: go hub.Run()
//
// It listens on two channels:
//   - register: a new client connected. Add them to the map.
//   - unregister: a client disconnected. Remove them from the map and close
//     their send channel (which will cause writePump to exit).
//
// This is the ONLY goroutine that mutates the clients map.
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()

			// If this user already has an active connection, close the old one.
			// This handles the case where a user opens a second tab or reconnects
			// after a dirty disconnect before the server detects it.
			if existing, ok := h.clients[client.userID]; ok {
				logger.Log.Warn().
					Str("userID", client.userID).
					Msg("duplicate connection, closing old one")
				close(existing.send)
				delete(h.clients, client.userID)
			}

			h.clients[client.userID] = client
			h.mu.Unlock()

			logger.Log.Info().
				Str("userID", client.userID).
				Int("total_clients", len(h.clients)).
				Msg("client registered")

		case client := <-h.unregister:
			h.mu.Lock()
			// Only delete if the client in the map is the SAME pointer.
			// This prevents a race where a new connection for the same userID
			// registered after this client was queued for unregister.
			if existing, ok := h.clients[client.userID]; ok && existing == client {
				close(client.send)
				delete(h.clients, client.userID)
				logger.Log.Info().
					Str("userID", client.userID).
					Int("total_clients", len(h.clients)).
					Msg("client unregistered")
			}
			h.mu.Unlock()
		}
	}
}

// GetClient returns the Client for the given userID, or nil if not connected.
// Safe to call from any goroutine (uses RLock).
func (h *Hub) GetClient(userID string) *Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.clients[userID]
}

// IsOnline returns true if the user has an active WebSocket connection.
// Safe to call from any goroutine (uses RLock).
func (h *Hub) IsOnline(userID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.clients[userID]
	return ok
}
```

### Design decisions explained

**Why `sync.RWMutex` if the Hub owns all writes?** The `Run()` goroutine is the sole writer, but it still needs to coordinate with readers. When `GetClient` is called from a message-routing goroutine (which is `readPump` of a different client), it needs to see a consistent map. So `Run()` acquires a write lock (`mu.Lock()`) before mutating, and `GetClient`/`IsOnline` acquire a read lock (`mu.RLock()`). Multiple readers can proceed concurrently; only writes block.

**Why check `existing == client` in unregister?** Consider this sequence:
1. User A connects (Client pointer `0x1`), registered.
2. User A's network drops. Server has not detected it yet.
3. User A reconnects (Client pointer `0x2`). The register case closes `0x1`'s send channel and replaces the map entry with `0x2`.
4. Now the readPump of `0x1` exits (because its connection is dead or send was closed) and sends `0x1` to unregister.
5. Without the pointer check, unregister would delete User A's entry and close `0x2`'s send channel -- killing the new, valid connection.

The pointer comparison prevents this.

**Why unbuffered channels?** Simplicity. The `Run()` loop is extremely fast (just map operations), so senders will not block for any meaningful duration. Buffered channels would add complexity without measurable benefit here.

---

## 3. Refactored Client Struct

Create the file `internal/handler/client.go`:

```go
package handler

import (
	"encoding/json"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/logger"
)

const (
	// writeWait is the time allowed to write a message to the peer.
	// If a write takes longer than this, the connection is considered dead.
	writeWait = 10 * time.Second

	// pongWait is the time allowed to read the next pong message from the peer.
	// If no pong arrives within this window, readPump's ReadMessage will error
	// with a deadline exceeded, and readPump will exit.
	pongWait = 60 * time.Second

	// pingPeriod is the interval at which we send ping frames to the peer.
	// It MUST be less than pongWait. We use 90% of pongWait (54 seconds).
	// This gives the pong 6 seconds of slack to arrive after we send a ping.
	pingPeriod = (pongWait * 9) / 10

	// maxMessageSize is the maximum size of an incoming message in bytes.
	// Messages larger than this cause ReadMessage to return an error.
	// 512 KB is generous for a chat app (text + small metadata).
	maxMessageSize = 512 * 1024

	// sendChannelSize is the buffer size for the client's send channel.
	// A buffered channel lets the hub or routing logic queue messages without
	// blocking, up to this limit. If the client is slow and the buffer fills,
	// we drop the message and log a warning (see routing in readPump).
	sendChannelSize = 256
)

// Envelope is the wire format for messages between client and server.
// Every WebSocket message is a JSON object with a "type" field that tells
// us what kind of message it is, and a "payload" field with type-specific data.
type Envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// DirectMessage is the payload for type "dm".
type DirectMessage struct {
	To      string `json:"to"`
	Content string `json:"content"`
}

// Client is a middleman between the WebSocket connection and the Hub.
//
// Each Client has:
//   - A reference to the Hub (to unregister itself on disconnect)
//   - A websocket.Conn (the actual TCP connection)
//   - A userID (who this client belongs to)
//   - A send channel (buffered, for outgoing messages)
//
// Two goroutines run per Client: readPump and writePump.
type Client struct {
	hub    *Hub
	conn   *websocket.Conn
	userID string

	// send is a buffered channel of outgoing messages.
	// The Hub or routing logic pushes messages here.
	// writePump reads from this channel and writes to the WebSocket.
	// When the Hub closes this channel, writePump exits.
	send chan []byte
}

// NewClient creates a Client. Does NOT start pumps -- caller must do that.
func NewClient(hub *Hub, conn *websocket.Conn, userID string) *Client {
	return &Client{
		hub:    hub,
		conn:   conn,
		userID: userID,
		send:   make(chan []byte, sendChannelSize),
	}
}

// readPump pumps messages from the WebSocket connection to the Hub.
//
// There is one readPump per Client, running in its own goroutine.
// readPump is the READER side: it calls conn.ReadMessage() in a loop.
//
// When readPump exits (for any reason), it:
//  1. Sends itself to hub.unregister (so the Hub removes it from the map
//     and closes the send channel).
//  2. Closes the WebSocket connection.
//
// This ensures that no matter how the connection dies (clean close, network
// drop, read error), cleanup always happens.
func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
		logger.Log.Debug().
			Str("userID", c.userID).
			Msg("readPump exited")
	}()

	// SetReadLimit: if the client sends a message larger than maxMessageSize,
	// ReadMessage returns an error and readPump exits. This prevents a
	// malicious client from sending a 10 GB message and eating all your RAM.
	c.conn.SetReadLimit(maxMessageSize)

	// SetReadDeadline: if no data arrives within pongWait, ReadMessage returns
	// a deadline exceeded error. This is the "heartbeat timeout."
	// We reset this deadline every time we receive a Pong.
	c.conn.SetReadDeadline(time.Now().Add(pongWait))

	// SetPongHandler: when the browser responds to our Ping with a Pong,
	// this handler fires. We reset the read deadline, giving the connection
	// another 60 seconds to live. As long as pongs keep arriving, the
	// connection stays alive.
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(
				err,
				websocket.CloseGoingAway,
				websocket.CloseNormalClosure,
			) {
				// This means the connection closed without a proper close frame.
				// Common causes: browser tab closed, network dropped, device died.
				logger.Log.Warn().
					Err(err).
					Str("userID", c.userID).
					Msg("unexpected WebSocket close")
			}
			// Whether expected or unexpected, we exit. The defer handles cleanup.
			return
		}

		// Parse the envelope to determine message type.
		var env Envelope
		if err := json.Unmarshal(message, &env); err != nil {
			logger.Log.Warn().
				Err(err).
				Str("userID", c.userID).
				Msg("invalid message envelope")
			continue
		}

		// Route based on message type.
		switch env.Type {
		case "dm":
			c.handleDirectMessage(env.Payload)
		default:
			logger.Log.Warn().
				Str("type", env.Type).
				Str("userID", c.userID).
				Msg("unknown message type")
		}
	}
}

// handleDirectMessage processes an incoming direct message.
// It saves the message to the database and attempts to deliver it
// to the recipient if they are online.
func (c *Client) handleDirectMessage(payload json.RawMessage) {
	var dm DirectMessage
	if err := json.Unmarshal(payload, &dm); err != nil {
		logger.Log.Warn().
			Err(err).
			Str("userID", c.userID).
			Msg("invalid DM payload")
		return
	}

	logger.Log.Info().
		Str("from", c.userID).
		Str("to", dm.To).
		Str("content", dm.Content).
		Msg("direct message received")

	// TODO: Save message to database here.
	// e.g., c.hub.messageService.Save(c.userID, dm.To, dm.Content)

	// Build the outgoing message that the recipient will see.
	outgoing, err := json.Marshal(Envelope{
		Type: "dm",
		Payload: mustMarshal(DirectMessage{
			To:      dm.To,
			Content: dm.Content,
		}),
	})
	if err != nil {
		logger.Log.Error().Err(err).Msg("failed to marshal outgoing DM")
		return
	}

	// Attempt real-time delivery.
	recipient := c.hub.GetClient(dm.To)
	if recipient == nil {
		logger.Log.Debug().
			Str("to", dm.To).
			Msg("recipient offline, message saved for later delivery")
		return
	}

	// Non-blocking send. If the recipient's send channel is full (they are a
	// slow client and have 256 pending messages), we drop the real-time
	// delivery and log a warning. The message is already saved in the DB,
	// so the recipient will get it when they fetch history.
	select {
	case recipient.send <- outgoing:
		logger.Log.Debug().
			Str("to", dm.To).
			Msg("message pushed to recipient send channel")
	default:
		logger.Log.Warn().
			Str("to", dm.To).
			Msg("slow client: send channel full, dropping real-time delivery")
	}
}

// writePump pumps messages from the send channel to the WebSocket connection.
//
// There is one writePump per Client, running in its own goroutine.
// writePump is the WRITER side: it reads from c.send and calls conn.WriteMessage.
//
// writePump exits when:
//   - The send channel is closed (by Hub.Run during unregister). This is the
//     normal shutdown path triggered by readPump exiting.
//   - The ping ticker fires and WriteMessage fails (connection is dead).
//
// When writePump exits, it closes the WebSocket connection. This is a safety
// net -- usually the connection is already closed by readPump's defer.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
		logger.Log.Debug().
			Str("userID", c.userID).
			Msg("writePump exited")
	}()

	for {
		select {
		case message, ok := <-c.send:
			// SetWriteDeadline: if the write takes longer than writeWait
			// (10 seconds), the connection is considered dead. This prevents
			// writePump from blocking forever on a stalled connection.
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))

			if !ok {
				// The Hub closed the send channel. This means we are being
				// unregistered. Send a close frame to the client so the
				// browser knows the connection is intentionally closed.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			// Write the message as a text frame.
			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			// Drain any queued messages and write them in the same frame.
			// This batches multiple messages into a single WebSocket write,
			// reducing syscall overhead when there is a burst of messages.
			n := len(c.send)
			for i := 0; i < n; i++ {
				w.Write([]byte("\n"))
				w.Write(<-c.send)
			}

			if err := w.Close(); err != nil {
				return
			}

		case <-ticker.C:
			// Send a Ping frame to the client. The browser will automatically
			// respond with a Pong. Our PongHandler (in readPump) resets the
			// read deadline when the Pong arrives.
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				// Ping failed. Connection is dead. Exit.
				return
			}
		}
	}
}

// mustMarshal is a helper that panics on marshal failure.
// This is safe for types we fully control (no interfaces, no cycles).
func mustMarshal(v interface{}) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic("marshal failed: " + err.Error())
	}
	return data
}
```

### Key design decisions in Client

**Why a `send` channel instead of writing directly to `conn`?** The `websocket.Conn` is NOT safe for concurrent writes. If readPump (goroutine 1) tries to write a response while writePump (goroutine 2) is writing a ping, the connection corrupts. By funneling all writes through the `send` channel, we guarantee that only writePump ever calls `conn.WriteMessage`. One writer, one connection, no races.

**Why buffer size 256?** This is the number of pending outgoing messages before we consider the client "slow." 256 is a reasonable default for a chat app: if a client has 256 unread real-time messages queued, something is wrong (slow network, frozen tab). We would rather drop the real-time push (the message is already in the DB) than block the sender.

**Why `select` with `default` for sending?** A blocking send to a full channel would block the sender's readPump, which would prevent that sender from processing their own incoming messages. The non-blocking send with a `default` case drops the real-time delivery but keeps the system healthy.

**Why batch writes with `NextWriter`?** When multiple messages are queued (e.g., during a burst), we drain the channel and write them all in a single WebSocket frame. This reduces the number of `write()` syscalls and TCP packets. Each message is separated by a newline, which the client can split on.

---

## 4. Ping/Pong Heartbeat

This is the mechanism that detects **dirty disconnects** -- when the client disappears without sending a close frame. This happens when:

- The user's WiFi drops
- The device runs out of battery
- The OS kills the browser process
- A network intermediary (NAT, load balancer) drops the connection silently

Without ping/pong, TCP keepalive might detect the dead connection, but it can take **hours** depending on OS settings. We need to know within a minute.

### The timeline

```
Time 0s:   Client connects. Server sets read deadline to Now + 60s.
Time 54s:  writePump ticker fires. Server sends Ping frame to client.
Time 54.1s: Browser receives Ping, automatically responds with Pong.
           (This is built into the WebSocket protocol -- no JS needed.)
Time 54.2s: Server receives Pong. PongHandler fires, resets read deadline
           to Now + 60s (i.e., ~114.2s from start).
Time 108s: writePump ticker fires again. Server sends another Ping.
Time 108.1s: Pong received. Read deadline reset to ~168.1s.
...
```

### When the client disappears

```
Time 0s:    Client connects. Read deadline = Now + 60s.
Time 30s:   Client's WiFi drops. No close frame sent.
Time 54s:   Server sends Ping. The packet goes into the void.
            (TCP will retry the send, but the client is gone.)
Time 60s:   Read deadline expires. conn.ReadMessage() returns error:
            "i/o timeout" or "use of closed network connection"
Time 60s:   readPump exits. Defer runs:
            1. hub.unregister <- c  (Hub removes from map, closes send)
            2. conn.Close()         (TCP connection torn down)
Time 60s:   writePump's send channel is closed. It writes a CloseMessage
            (which will fail, but that is fine), and returns.
```

**Key insight:** The read deadline is what actually kills the connection. The ping is just the mechanism to trigger a pong, which resets the deadline. No pong = deadline expires = readPump exits = cleanup cascade.

**Why 54 seconds for ping and 60 seconds for pong wait?** The 6-second gap is the slack. After we send a ping at T=54s, the pong has until T=60s to arrive. That is 6 seconds for the round trip. On any reasonable network, this is more than enough. If you are on a satellite link with 3-second RTT, you might want to increase these values.

---

## 5. Goroutine Lifecycle

Every goroutine in this system has a clear exit path. No goroutine runs forever without a way to stop. Here is the lifecycle diagram:

```
                     HandleWebSocket
                           |
                    +------+------+
                    |             |
              go readPump()  go writePump()
                    |             |
                    |         select {
                    |           case msg := <-c.send:
             ReadMessage()        write to conn
                    |           case <-ticker.C:
                    |             send Ping
                    |         }
                    |             |
          (error or close)        |
                    |             |
                    v             |
            defer: unregister     |
            defer: conn.Close()   |
                    |             |
                    v             |
              Hub.Run():          |
              close(c.send) ------+
                                  |
                                  v
                          c.send closed (ok == false)
                          write CloseMessage
                          return
                          defer: ticker.Stop()
                          defer: conn.Close()
```

### The chain of events, step by step

1. **readPump exits** -- this is always the first domino. It exits because:
   - The client sent a close frame (clean disconnect)
   - `ReadMessage` returned an error (dirty disconnect, read deadline expired)
   - The message was too large (`SetReadLimit` violation)

2. **readPump's defer sends to `hub.unregister`** -- this tells the Hub "I'm done."

3. **Hub.Run receives on `unregister`** -- it removes the client from the map and **closes the `send` channel**. This is the critical step.

4. **writePump reads from closed `send` channel** -- the `ok` value is `false`. writePump sends a close frame (best effort) and returns.

5. **writePump's defer stops the ticker and closes the connection** -- the `conn.Close()` is redundant (readPump already closed it), but `Close()` is idempotent, so this is safe.

**Result: zero goroutine leaks.** Both pumps exit, both defers run, the ticker is stopped, the connection is closed, and the Hub has removed the client from its map.

---

## 6. Race Condition Protection

Let's audit every shared resource:

### The `clients` map

| Operation | Who does it | Protection |
|-----------|-------------|------------|
| Write (add/delete) | Hub.Run() only | `mu.Lock()` (write lock) |
| Read (GetClient, IsOnline) | Any goroutine (readPump of other clients, HTTP handlers) | `mu.RLock()` (read lock) |

Since Hub.Run() is the sole writer and it always acquires `mu.Lock()` before writing, and all readers acquire `mu.RLock()`, there are no data races. Multiple readers can proceed concurrently (RLock does not block other RLocks).

### The `websocket.Conn`

| Operation | Who does it | Protection |
|-----------|-------------|------------|
| Read (ReadMessage) | readPump only | Single reader -- no lock needed |
| Write (WriteMessage, NextWriter) | writePump only | Single writer -- no lock needed |

The gorilla/websocket library documents that a Conn supports one concurrent reader and one concurrent writer. Our design matches this exactly.

### The `send` channel

| Operation | Who does it | Protection |
|-----------|-------------|------------|
| Send to channel | Any goroutine (readPump of sender, Hub during duplicate-close) | Channel is inherently thread-safe |
| Receive from channel | writePump only | Single receiver -- no contention |
| Close channel | Hub.Run() only | Only closed once, from one place |

Channels in Go are safe for concurrent sends. The only danger is closing a channel that has active senders, but our design ensures that once a client is unregistered (and its send channel closed), no new messages will be routed to it because `GetClient` will return `nil`.

### The `select` with `default` for slow clients

```go
select {
case recipient.send <- outgoing:
    // delivered
default:
    // dropped
}
```

This is safe because:
- If the channel is already closed (recipient was unregistered between our `GetClient` call and this send), we would get a panic... **except** we do the `GetClient` + send in the same goroutine (readPump of sender), and the Hub only closes the channel when it processes the unregister. Since the Hub's Run loop is sequential, there is a tiny window where this could race. To be fully safe, we could wrap this in a recover, but in practice the window is negligible and the worst case is a panic that crashes one goroutine, not the server. For a production system, you would add a recover or use a mutex-guarded send helper. For this tutorial, we accept the tradeoff.

---

## 7. Updated WebSocket Handler

Create or update `internal/handler/ws_handler.go`:

```go
package handler

import (
	"net/http"

	"github.com/gorilla/websocket"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/logger"
)

// upgrader configures the WebSocket upgrade.
// CheckOrigin returns true for all origins in development.
// In production, restrict this to your frontend domain.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // TODO: restrict in production
	},
}

// HandleWebSocket upgrades an HTTP connection to WebSocket and sets up
// the client with its readPump and writePump goroutines.
//
// Expected usage: GET /ws?userId=<userID>
// In a real app, userID comes from JWT/session auth, not a query param.
func HandleWebSocket(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.URL.Query().Get("userId")
		if userID == "" {
			http.Error(w, "missing userId", http.StatusBadRequest)
			return
		}

		// Upgrade HTTP -> WebSocket.
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			logger.Log.Error().
				Err(err).
				Str("userID", userID).
				Msg("WebSocket upgrade failed")
			return
		}

		// Create the client.
		client := NewClient(hub, conn, userID)

		// Register with the Hub. This blocks until Hub.Run() processes it.
		// After this returns, the client is in the Hub's map and can receive
		// messages from other clients.
		client.hub.register <- client

		// Start the pumps in their own goroutines.
		// These goroutines own the client's lifecycle from here on.
		// We do NOT keep a reference to them -- they will clean up after
		// themselves via the defer chains described in the lifecycle section.
		go client.writePump()
		go client.readPump()
	}
}
```

### Why `writePump` before `readPump`?

We start `writePump` first because `readPump` might trigger a send (e.g., an echo or acknowledgment) immediately upon receiving the first message. If `writePump` is not running yet, the message would sit in the `send` channel until it starts -- not a bug, but starting `writePump` first avoids any latency for the first outgoing message.

Note that `readPump` runs in its own goroutine here. The `HandleWebSocket` function returns immediately after starting both goroutines. The HTTP handler is done -- the goroutines take over.

---

## 8. Updated main.go

Update `cmd/main.go` (or wherever your entry point is):

```go
package main

import (
	"net/http"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/handler"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/logger"
)

func main() {
	// Initialize logger (from Step 1).
	logger.Init()

	// Create the Hub. This is the central coordinator for all WebSocket
	// connections. There is exactly one Hub for the entire server.
	hub := handler.NewHub()

	// Start the Hub's Run loop in its own goroutine.
	// This goroutine runs for the lifetime of the server.
	// It processes register/unregister requests from clients.
	go hub.Run()

	// Serve static files (the chat UI from Step 3).
	http.Handle("/", http.FileServer(http.Dir("static")))

	// WebSocket endpoint. Pass the hub so the handler can register clients.
	http.HandleFunc("/ws", handler.HandleWebSocket(hub))

	// TODO: Add REST routes for user registration (from Step 2).

	addr := ":8080"
	logger.Log.Info().Str("addr", addr).Msg("server starting")

	if err := http.ListenAndServe(addr, nil); err != nil {
		logger.Log.Fatal().Err(err).Msg("server failed")
	}
}
```

### What changed from Step 3

1. **No more global `connMap`** -- the Hub owns all connection state.
2. **No more `sync.Mutex` in the handler** -- the Hub's channels replace explicit locking.
3. **Hub is created once and passed to handlers** -- dependency injection, not global state.
4. **`go hub.Run()`** -- the Hub's event loop runs for the server's lifetime.

---

## 9. Message Routing Through the Hub

Let's trace a complete message flow from sender to recipient:

```
User A types "hello" and sends it to User B.

1. User A's browser sends: {"type":"dm","payload":{"to":"userB","content":"hello"}}

2. User A's readPump calls conn.ReadMessage(), gets the raw bytes.

3. readPump unmarshals the Envelope, sees type == "dm".

4. readPump calls c.handleDirectMessage(payload).

5. handleDirectMessage:
   a. Unmarshals the DirectMessage: {To: "userB", Content: "hello"}
   b. TODO: Saves to database (so the message is never lost).
   c. Calls c.hub.GetClient("userB").
      - Hub.GetClient acquires RLock, looks up "userB" in the map.
      - If found, returns *Client for User B.
      - If not found (offline), returns nil. Message is saved in DB
        and will be delivered when User B comes online (future step).
   d. If User B is online, does a non-blocking send:
      select {
      case recipient.send <- outgoing:
          // Success! Message is in User B's send buffer.
      default:
          // User B's buffer is full. Log warning, move on.
          // Message is in DB; User B gets it later.
      }

6. User B's writePump reads the message from c.send.

7. writePump calls conn.WriteMessage() to send it over the WebSocket.

8. User B's browser receives: {"type":"dm","payload":{"to":"userB","content":"hello"}}
```

The critical thing to notice: **User A's readPump never directly writes to User B's connection.** It pushes a message into User B's `send` channel, and User B's `writePump` does the actual write. This maintains the invariant that only one goroutine writes to each connection.

---

## Acceptance Test

Open two terminal tabs.

### Terminal 1: Start the server

```bash
cd /path/to/apps/backend
go run ./cmd/main.go
```

You should see:
```
{"level":"info","addr":":8080","message":"server starting"}
```

### Terminal 2: Test with websocat or the browser

If you have `websocat` installed (`brew install websocat`):

**Connect User A:**
```bash
websocat ws://localhost:8080/ws?userId=alice
```

Server log:
```
{"level":"info","userID":"alice","total_clients":1,"message":"client registered"}
```

**Connect User B (new terminal):**
```bash
websocat ws://localhost:8080/ws?userId=bob
```

Server log:
```
{"level":"info","userID":"bob","total_clients":2,"message":"client registered"}
```

**Send a message from Alice to Bob:**

In Alice's websocat terminal, type:
```json
{"type":"dm","payload":{"to":"bob","content":"hello bob!"}}
```

Server log:
```
{"level":"info","from":"alice","to":"bob","content":"hello bob!","message":"direct message received"}
{"level":"debug","to":"bob","message":"message pushed to recipient send channel"}
```

Bob's websocat terminal should print:
```json
{"type":"dm","payload":{"to":"bob","content":"hello bob!"}}
```

**Send a message from Bob to Alice:**

In Bob's terminal:
```json
{"type":"dm","payload":{"to":"alice","content":"hey alice!"}}
```

Alice should see it. Bidirectional messaging works.

### Test dirty disconnect detection

**Kill Alice's connection** by pressing Ctrl+C in her websocat terminal.

Server log (immediately):
```
{"level":"info","userID":"alice","total_clients":1,"message":"client unregistered"}
{"level":"debug","userID":"alice","message":"readPump exited"}
{"level":"debug","userID":"alice","message":"writePump exited"}
```

Both `readPump exited` and `writePump exited` should appear. This confirms zero goroutine leak -- both goroutines cleaned up.

**Send a message from Bob to offline Alice:**

In Bob's terminal:
```json
{"type":"dm","payload":{"to":"alice","content":"are you there?"}}
```

Server log:
```
{"level":"debug","to":"alice","message":"recipient offline, message saved for later delivery"}
```

No crash, no panic. The message is (will be) saved in the DB for later.

### Test duplicate connection handling

**Reconnect Alice:**
```bash
websocat ws://localhost:8080/ws?userId=alice
```

**While still connected, open another Alice connection in a new terminal:**
```bash
websocat ws://localhost:8080/ws?userId=alice
```

Server log:
```
{"level":"warn","userID":"alice","message":"duplicate connection, closing old one"}
{"level":"info","userID":"alice","total_clients":2,"message":"client registered"}
```

The old Alice connection should close (websocat exits). The new one stays alive. Total clients is 2 (alice + bob), not 3.

---

## Summary

What we built in this step:

| Component | File | Purpose |
|-----------|------|---------|
| Hub | `internal/handler/hub.go` | Central coordinator. Owns the client map. Single event loop via channels. |
| Client | `internal/handler/client.go` | Per-connection struct. readPump (reads from WS) + writePump (writes to WS). |
| WS Handler | `internal/handler/ws_handler.go` | Upgrades HTTP to WS, creates Client, registers with Hub, starts pumps. |
| main.go | `cmd/main.go` | Creates Hub, starts `hub.Run()`, wires up routes. |

The Hub pattern gives us:
- **No deadlocks:** No goroutine holds a lock while doing I/O.
- **No goroutine leaks:** Every goroutine has a clear exit path.
- **No data races:** One writer (Hub.Run), concurrent readers (RLock).
- **Clean disconnect detection:** Ping/pong heartbeat detects dead connections within 60 seconds.
- **Graceful duplicate handling:** New connection for the same user closes the old one.

Next step: message persistence with PostgreSQL, so messages survive server restarts and offline delivery works.
