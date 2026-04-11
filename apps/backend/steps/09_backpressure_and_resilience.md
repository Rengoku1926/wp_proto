# Step 9 -- Backpressure and Resilience

Module: `github.com/Rengoku1926/wp_proto/apps/backend`

**Prerequisites:** Full messaging system working -- 1:1, groups, pub/sub, offline buffer, delivery states, fanout.

Up until now, the happy path works: messages flow, clients connect, Redis routes, Postgres persists. But production systems fail in interesting ways. A slow client can block an entire goroutine. A crashed Redis loses all pub/sub subscriptions silently. A SIGTERM kills active WebSocket connections mid-message. This step hardens the system against all of that.

---

## 1. Backpressure -- Protecting the Pipeline from Slow Clients

### The problem

Every connected client has a `send` channel:

```go
type Client struct {
    // ...
    send chan []byte
}
```

This channel is buffered (256 messages). The `writePump` goroutine reads from it and writes to the WebSocket. But what if the client's network is slow, or they stopped reading entirely? The channel fills up, and the next write blocks. If that write happens inside the hub's main loop, the entire hub stalls -- no messages route to anyone.

### The solution: non-blocking sends with drop semantics

Never do a blocking send to `client.send`. Always use `select` with a `default` case.

### Updated Hub.Run() -- `internal/ws/hub.go`

```go
// internal/ws/hub.go
package ws

import (
	"context"
	"runtime"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

const (
	sendBufferSize       = 256
	maxDroppedBeforeKick = 20
)

// Hub maintains the set of active clients and routes messages.
type Hub struct {
	clients    map[string]*Client
	register   chan *Client
	unregister chan *Client
	broadcast  chan []byte

	mu     sync.RWMutex
	logger zerolog.Logger
}

func NewHub(logger zerolog.Logger) *Hub {
	return &Hub{
		clients:    make(map[string]*Client),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		broadcast:  make(chan []byte, 256),
		logger:     logger,
	}
}

func (h *Hub) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			h.logger.Info().Msg("hub shutting down")
			h.mu.Lock()
			for _, client := range h.clients {
				close(client.send)
			}
			h.clients = make(map[string]*Client)
			h.mu.Unlock()
			return

		case client := <-h.register:
			h.mu.Lock()
			// --- Reconnect race fix (Section 4) ---
			if existing, ok := h.clients[client.userID]; ok {
				// Old connection still registered. Close it.
				h.logger.Warn().
					Str("user_id", client.userID).
					Msg("replacing stale connection")
				close(existing.send)
			}
			h.clients[client.userID] = client
			h.mu.Unlock()
			h.logger.Info().
				Str("user_id", client.userID).
				Int("active_connections", len(h.clients)).
				Msg("client registered")

		case client := <-h.unregister:
			h.mu.Lock()
			// --- Reconnect race fix (Section 4) ---
			// Only remove if the connection pointer matches.
			// If a new connection already replaced this one, do nothing.
			if existing, ok := h.clients[client.userID]; ok && existing == client {
				delete(h.clients, client.userID)
				close(client.send)
				h.logger.Info().
					Str("user_id", client.userID).
					Msg("client unregistered")
			} else {
				h.logger.Warn().
					Str("user_id", client.userID).
					Msg("ignoring stale unregister -- connection already replaced")
			}
			h.mu.Unlock()

		case message := <-h.broadcast:
			h.mu.RLock()
			for userID, client := range h.clients {
				h.sendToClient(client, userID, message)
			}
			h.mu.RUnlock()
		}
	}
}

// sendToClient performs a non-blocking send. If the client's buffer is full,
// the message is dropped and the client's drop counter increments.
func (h *Hub) sendToClient(client *Client, userID string, message []byte) {
	select {
	case client.send <- message:
		// delivered to buffer
	default:
		// channel full -- client is slow
		client.dropped++
		h.logger.Warn().
			Str("user_id", userID).
			Int("dropped", client.dropped).
			Int("buffer_size", sendBufferSize).
			Msg("backpressure: message dropped for slow client")

		if client.dropped >= maxDroppedBeforeKick {
			h.logger.Error().
				Str("user_id", userID).
				Int("total_dropped", client.dropped).
				Msg("kicking slow client -- too many dropped messages")
			// Close the send channel. writePump will detect this and
			// send a WebSocket close frame.
			delete(h.clients, userID)
			close(client.send)
		}
	}
}

// RouteToUser sends a message to a specific user with backpressure.
func (h *Hub) RouteToUser(userID string, message []byte) bool {
	h.mu.RLock()
	client, ok := h.clients[userID]
	h.mu.RUnlock()
	if !ok {
		return false
	}

	select {
	case client.send <- message:
		return true
	default:
		client.dropped++
		h.logger.Warn().
			Str("user_id", userID).
			Int("dropped", client.dropped).
			Msg("backpressure: message dropped in RouteToUser")
		return false
	}
}

// ActiveConnections returns the number of connected clients.
func (h *Hub) ActiveConnections() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}
```

### Updated Client struct -- `internal/ws/client.go`

```go
// internal/ws/client.go
package ws

import (
	"context"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 4096
)

type Client struct {
	hub     *Hub
	conn    *websocket.Conn
	send    chan []byte
	userID  string
	dropped int // backpressure: count of messages dropped due to slow reads
	logger  zerolog.Logger
}

func NewClient(hub *Hub, conn *websocket.Conn, userID string, logger zerolog.Logger) *Client {
	return &Client{
		hub:    hub,
		conn:   conn,
		send:   make(chan []byte, sendBufferSize),
		userID: userID,
		logger: logger,
	}
}
```

### Updated writePump with panic recovery

```go
// writePump pumps messages from the hub to the WebSocket connection.
func (c *Client) writePump(ctx context.Context) {
	// --- Panic recovery (Section 3) ---
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error().
				Interface("panic", r).
				Str("user_id", c.userID).
				Msg("recovered panic in writePump")
		}
		c.conn.Close()
	}()

	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Graceful shutdown: send close message before returning
			c.conn.WriteMessage(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutting down"),
			)
			return

		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// Hub closed the channel (kicked or shutting down)
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			// Drain any queued messages into the same write frame
			n := len(c.send)
			for i := 0; i < n; i++ {
				w.Write([]byte("\n"))
				w.Write(<-c.send)
			}

			if err := w.Close(); err != nil {
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

### Updated readPump with panic recovery

```go
// readPump pumps messages from the WebSocket connection to the hub.
func (c *Client) readPump(ctx context.Context) {
	// --- Panic recovery (Section 3) ---
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error().
				Interface("panic", r).
				Str("user_id", c.userID).
				Msg("recovered panic in readPump")
		}
		c.hub.unregister <- c
		c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(
				err,
				websocket.CloseGoalAway,
				websocket.CloseNormalClosure,
			) {
				c.logger.Error().Err(err).Str("user_id", c.userID).Msg("read error")
			}
			return
		}

		c.handleMessage(ctx, message)
	}
}
```

---

## 2. Graceful Shutdown

When the server receives SIGINT (Ctrl+C) or SIGTERM (from a process manager like systemd or Kubernetes), it should:

1. Stop accepting new connections
2. Tell every connected client "we are shutting down"
3. Wait for goroutines to drain
4. Close Redis and Postgres connections

### Updated `cmd/main.go`

```go
// cmd/main.go
package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/config"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/database"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/logger"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/ws"
)

func main() {
	cfg, err := config.Load(".env.local")
	if err != nil {
		panic(err)
	}

	log := logger.New(cfg.Primary.Env)

	// --- Root context: cancelling this stops everything ---
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- Database connections ---
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

	// --- Hub ---
	hub := ws.NewHub(log)
	go hub.Run(ctx)

	// --- HTTP server ---
	mux := http.NewServeMux()

	// Health check (Section 8)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		healthHandler(w, r, pool, redisClient, hub)
	})

	// WebSocket upgrade endpoint
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		ws.ServeWs(hub, w, r, log)
	})

	// ... other routes ...

	server := &http.Server{
		Addr:    ":" + cfg.Primary.Port,
		Handler: mux,
	}

	// --- Signal handling ---
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start server in a goroutine
	go func() {
		log.Info().Str("port", cfg.Primary.Port).Msg("server starting")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("server failed")
		}
	}()

	// --- Wait for shutdown signal ---
	sig := <-sigChan
	log.Info().Str("signal", sig.String()).Msg("shutdown signal received")

	// 1. Stop accepting new HTTP/WebSocket connections.
	//    Give active requests 10 seconds to finish.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Error().Err(err).Msg("http server shutdown error")
	}
	log.Info().Msg("http server stopped accepting new connections")

	// 2. Cancel the root context. This signals:
	//    - Hub.Run() to close all client channels and return
	//    - All readPump/writePump goroutines to exit (they watch ctx.Done())
	//    - All Redis subscription loops to exit
	cancel()

	// 3. Give goroutines a moment to drain.
	time.Sleep(2 * time.Second)

	// 4. Redis and Postgres are closed by deferred calls above.
	log.Info().Msg("shutdown complete")
}
```

### The shutdown sequence visualized

```
SIGTERM received
    |
    v
server.Shutdown()  -- stops accepting new connections,
    |                  waits for in-flight HTTP requests
    v
cancel()           -- cancels root context
    |
    +---> hub.Run() sees ctx.Done()
    |         |
    |         +---> closes all client.send channels
    |         +---> writePump detects closed channel, sends CloseMessage
    |         +---> readPump sees ctx.Done(), returns
    |
    +---> subscribeLoop sees ctx.Done(), unsubscribes from Redis
    |
    v
deferred pool.Close()    -- closes Postgres connection pool
deferred redisClient.Close() -- closes Redis connection
    |
    v
process exits cleanly
```

---

## 3. Recovery from Panics in Goroutines

If a goroutine panics without recovery, it crashes the entire process. Every goroutine we spawn must have `defer/recover` at the top.

### The pattern

```go
func safeGo(logger zerolog.Logger, name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error().
					Interface("panic", r).
					Str("goroutine", name).
					Msg("recovered panic in goroutine")
			}
		}()
		fn()
	}()
}
```

### Usage everywhere you spawn goroutines

```go
// In the WebSocket upgrade handler:
func ServeWs(hub *Hub, w http.ResponseWriter, r *http.Request, logger zerolog.Logger) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Error().Err(err).Msg("websocket upgrade failed")
		return
	}

	userID := r.URL.Query().Get("user_id")
	client := NewClient(hub, conn, userID, logger)
	hub.register <- client

	ctx := r.Context()
	safeGo(logger, "writePump:"+userID, func() { client.writePump(ctx) })
	safeGo(logger, "readPump:"+userID, func() { client.readPump(ctx) })
}
```

### For fanout goroutines

If you spawn per-group fanout goroutines, wrap them the same way:

```go
// In group message fanout
func (h *Hub) fanoutToGroup(ctx context.Context, groupID string, members []string, message []byte) {
	for _, memberID := range members {
		memberID := memberID // capture loop variable
		safeGo(h.logger, "fanout:"+groupID+":"+memberID, func() {
			h.RouteToUser(memberID, message)
		})
	}
}
```

---

## 4. Connection Registry and the Reconnect Race

### The problem

Consider this sequence:

```
t=0  User A connects      -> register(connA)     -> clients["A"] = connA
t=1  User A disconnects   -> readPump returns     -> unregister(connA) queued
t=2  User A reconnects    -> register(connB)      -> clients["A"] = connB
t=3  unregister(connA) processed                  -> clients["A"] deleted!
```

At t=3, the hub processes the stale unregister from connA. But `clients["A"]` now points to connB, the new connection. The hub deletes it, and connB is silently orphaned -- it never receives messages again.

### The fix: pointer comparison in unregister

This is already shown in the Hub.Run() code above, but here is the critical check isolated:

```go
case client := <-h.unregister:
    h.mu.Lock()
    if existing, ok := h.clients[client.userID]; ok && existing == client {
        // Same pointer -- safe to delete
        delete(h.clients, client.userID)
        close(client.send)
    }
    // If existing != client, a newer connection has already taken the slot.
    // Do nothing -- the old connection's channels are already dead.
    h.mu.Unlock()
```

The `existing == client` comparison checks if the pointer in the map is the exact same `*Client` struct that is asking to unregister. If a new connection has already replaced it, the pointers differ and the unregister is a no-op.

---

## 5. Goroutine Leak Prevention

Every goroutine you spawn must have a clear exit condition. If even one goroutine leaks per connection, you will eventually run out of memory.

### Goroutine inventory

| Goroutine | Spawned by | Exit condition |
|-----------|-----------|----------------|
| `hub.Run()` | `main()` | `ctx.Done()` fires |
| `readPump` | `ServeWs` | WebSocket read error OR `ctx.Done()` |
| `writePump` | `ServeWs` | `client.send` closed OR `ctx.Done()` |
| `subscribeLoop` per user | Hub register | `ctx.Done()` OR Redis subscription error |
| `fanout` per group message | `handleMessage` | Completes after routing (short-lived) |
| `offlineBuffer replay` | On reconnect | Completes after replay (short-lived) |

### Monitor goroutine count in health check

```go
import "runtime"

goroutineCount := runtime.NumGoroutine()
```

If goroutine count climbs steadily over time without a corresponding increase in connected clients, you have a leak.

### Detect data races

Always run tests with the race detector:

```bash
go test -race ./...
```

This instruments the binary to detect concurrent reads and writes to shared memory. Any violation is a hard failure. Run this in CI on every commit.

---

## 6. Redis Connection Resilience

### What happens when Redis goes down?

The `go-redis` library has automatic reconnection built in. If the TCP connection to Redis drops, subsequent commands will fail with an error, and `go-redis` will try to reconnect on the next command.

But there is a critical subtlety: **pub/sub subscriptions are not automatically restored.**

When Redis reconnects, the client re-establishes a TCP connection, but all SUBSCRIBE commands from the previous session are gone. Your subscription goroutine is sitting on a dead channel, receiving nothing.

### Updated subscribeLoop with reconnection logic

```go
// internal/repository/pubsub.go
package repository

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
)

type PubSubRepo struct {
	rdb    *redis.Client
	logger zerolog.Logger
}

func NewPubSubRepo(rdb *redis.Client, logger zerolog.Logger) *PubSubRepo {
	return &PubSubRepo{rdb: rdb, logger: logger}
}

func (p *PubSubRepo) Publish(ctx context.Context, channel string, message []byte) error {
	return p.rdb.Publish(ctx, channel, message).Err()
}

// SubscribeLoop subscribes to a Redis channel and calls handler for every
// received message. If the subscription fails or drops, it re-subscribes
// automatically until ctx is cancelled.
func (p *PubSubRepo) SubscribeLoop(
	ctx context.Context,
	channel string,
	handler func(ctx context.Context, payload []byte),
) {
	for {
		// Check if context is already cancelled before subscribing
		select {
		case <-ctx.Done():
			return
		default:
		}

		p.logger.Info().Str("channel", channel).Msg("subscribing to Redis channel")
		sub := p.rdb.Subscribe(ctx, channel)

		// Wait for confirmation that subscription is active
		_, err := sub.Receive(ctx)
		if err != nil {
			p.logger.Error().Err(err).
				Str("channel", channel).
				Msg("failed to subscribe -- retrying in 2s")
			sub.Close()
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
				continue
			}
		}

		p.logger.Info().Str("channel", channel).Msg("subscription active")

		// Read messages from the subscription
		ch := sub.Channel()
		subscriptionAlive := true
		for subscriptionAlive {
			select {
			case <-ctx.Done():
				sub.Close()
				return
			case msg, ok := <-ch:
				if !ok {
					// Channel closed -- subscription died
					p.logger.Warn().
						Str("channel", channel).
						Msg("subscription channel closed -- reconnecting")
					subscriptionAlive = false
					break
				}
				handler(ctx, []byte(msg.Payload))
			}
		}

		sub.Close()

		// Backoff before re-subscribing
		p.logger.Info().Str("channel", channel).Msg("re-subscribing in 1s")
		select {
		case <-ctx.Done():
			return
		case <-time.After(1 * time.Second):
		}
	}
}
```

### Key points

- The outer `for` loop is the reconnection loop. Every time the subscription dies, it goes back to the top and re-subscribes.
- `sub.Receive(ctx)` blocks until the subscription is confirmed or fails. If Redis is down, this returns an error and we retry after a 2-second backoff.
- `sub.Channel()` returns a Go channel that closes when the subscription drops. The inner `for` loop reads from it until it closes.
- Every `select` checks `ctx.Done()` so the loop exits cleanly during shutdown.

---

## 7. Postgres Connection Pool Health

`pgxpool` handles connection pooling and automatic reconnection internally. If a connection in the pool goes bad, pgxpool detects it on the next use and replaces it. You do not need to manage reconnection yourself.

The risk is different: **long-running queries can block shutdown.** If a goroutine is mid-query when the server starts shutting down, the deferred `pool.Close()` will block until that query finishes (or the underlying connection times out, which could be minutes).

### Solution: context timeouts on all database operations

Create a wrapper that injects a timeout into every context used for DB calls:

```go
// internal/database/timeout.go
package database

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const defaultQueryTimeout = 5 * time.Second

// DB wraps pgxpool.Pool and injects timeouts into every operation.
type DB struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

func NewDB(pool *pgxpool.Pool) *DB {
	return &DB{pool: pool, timeout: defaultQueryTimeout}
}

func (db *DB) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, db.timeout)
}

func (db *DB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	ctx, cancel := db.withTimeout(ctx)
	// Note: We cannot defer cancel() here because QueryRow is lazy.
	// The caller must scan before the context expires.
	// For safety, we rely on the timeout expiring.
	_ = cancel
	return db.pool.QueryRow(ctx, sql, args...)
}

func (db *DB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	ctx, cancel := db.withTimeout(ctx)
	defer cancel()
	return db.pool.Query(ctx, sql, args...)
}

func (db *DB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	ctx, cancel := db.withTimeout(ctx)
	defer cancel()
	return db.pool.Exec(ctx, sql, args...)
}

func (db *DB) Ping(ctx context.Context) error {
	ctx, cancel := db.withTimeout(ctx)
	defer cancel()
	return db.pool.Ping(ctx)
}

func (db *DB) Close() {
	db.pool.Close()
}

// Pool returns the underlying pool for cases where direct access is needed.
func (db *DB) Pool() *pgxpool.Pool {
	return db.pool
}
```

### Usage in main.go

```go
pool, err := database.Connect(ctx, cfg.Database)
if err != nil {
    log.Fatal().Err(err).Msg("failed to connect to database")
}
db := database.NewDB(pool)
defer db.Close()
```

Now every query automatically gets a 5-second timeout. During shutdown, even if `cancel()` fires on the root context, the individual query contexts will also expire within 5 seconds, ensuring `pool.Close()` does not hang.

---

## 8. Updated Health Check

The health check endpoint now reports the actual state of all dependencies.

### `cmd/health.go`

```go
// cmd/health.go
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/ws"
)

type healthResponse struct {
	Status      string `json:"status"`
	Postgres    string `json:"postgres"`
	Redis       string `json:"redis"`
	Connections int    `json:"active_connections"`
	Goroutines  int    `json:"active_goroutines"`
	Timestamp   string `json:"timestamp"`
}

func healthHandler(
	w http.ResponseWriter,
	r *http.Request,
	pool *pgxpool.Pool,
	rdb *redis.Client,
	hub *ws.Hub,
) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	resp := healthResponse{
		Status:      "ok",
		Connections: hub.ActiveConnections(),
		Goroutines:  runtime.NumGoroutine(),
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}

	// Postgres check
	if err := pool.Ping(ctx); err != nil {
		resp.Postgres = "error: " + err.Error()
		resp.Status = "degraded"
	} else {
		resp.Postgres = "ok"
	}

	// Redis check
	if err := rdb.Ping(ctx).Err(); err != nil {
		resp.Redis = "error: " + err.Error()
		resp.Status = "degraded"
	} else {
		resp.Redis = "ok"
	}

	w.Header().Set("Content-Type", "application/json")
	if resp.Status != "ok" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(resp)
}
```

### Example responses

Healthy:

```json
{
  "status": "ok",
  "postgres": "ok",
  "redis": "ok",
  "active_connections": 42,
  "active_goroutines": 91,
  "timestamp": "2026-04-11T14:30:00Z"
}
```

Redis down:

```json
{
  "status": "degraded",
  "postgres": "ok",
  "redis": "error: dial tcp 127.0.0.1:6379: connect: connection refused",
  "active_connections": 42,
  "active_goroutines": 91,
  "timestamp": "2026-04-11T14:30:00Z"
}
```

---

## Acceptance Test Scenarios

### Test 1: Redis failure -- graceful degradation

```bash
# Terminal 1: Start the server
go run ./cmd/...

# Terminal 2: Connect a client and verify messaging works
websocat ws://localhost:8080/ws?user_id=alice

# Terminal 3: Kill Redis
redis-cli SHUTDOWN

# Terminal 4: Check health
curl http://localhost:8080/health
# Expected: {"status":"degraded","redis":"error: ...","postgres":"ok",...}

# Terminal 2: Send a message from Alice
# Expected: server logs an error publishing to Redis, message is still saved
# to Postgres. No crash. When Redis comes back, subscriptions re-establish.

# Terminal 3: Restart Redis
redis-server

# Wait 2-3 seconds for re-subscription
curl http://localhost:8080/health
# Expected: {"status":"ok","redis":"ok",...}
```

### Test 2: Backpressure -- slow client

```bash
# Terminal 1: Start the server
go run ./cmd/...

# Terminal 2: Connect a "slow" client (we simulate by just not reading)
# Connect via websocat but pipe to `sleep infinity` so it never reads responses
websocat -n ws://localhost:8080/ws?user_id=slow_bob | sleep infinity &

# Terminal 3: Flood messages to slow_bob
for i in $(seq 1 500); do
  websocat ws://localhost:8080/ws?user_id=alice -1 <<< \
    "{\"type\":\"message\",\"to\":\"slow_bob\",\"content\":\"msg $i\"}"
done

# Expected in server logs:
# backpressure: message dropped for slow client  user_id=slow_bob  dropped=1
# backpressure: message dropped for slow client  user_id=slow_bob  dropped=2
# ...
# kicking slow client -- too many dropped messages  user_id=slow_bob  total_dropped=20
```

### Test 3: Graceful shutdown

```bash
# Terminal 1: Start the server
go run ./cmd/...
# Note the PID, or just use Ctrl+C

# Terminal 2: Connect a client
websocat ws://localhost:8080/ws?user_id=alice

# Terminal 1: Send SIGTERM
kill -TERM $(pgrep -f "go run")
# Or just press Ctrl+C

# Expected log output (in order):
# shutdown signal received  signal=interrupt
# http server stopped accepting new connections
# hub shutting down
# shutdown complete

# Terminal 2: The WebSocket client should receive a close frame with
# code 1001 (Going Away) and the message "server shutting down"
```

---

## Summary of changes

| File | What changed |
|------|-------------|
| `internal/ws/hub.go` | Non-blocking sends, backpressure drop counter, slow client kick, reconnect-race-safe unregister, context-aware Run loop |
| `internal/ws/client.go` | `dropped` field added, panic recovery in readPump/writePump, context-aware pumps |
| `cmd/main.go` | Signal handling, graceful shutdown sequence, `context.WithCancel` as root context |
| `cmd/health.go` | Health check with Postgres ping, Redis ping, connection count, goroutine count |
| `internal/repository/pubsub.go` | `SubscribeLoop` with automatic re-subscription on Redis reconnect |
| `internal/database/timeout.go` | DB wrapper that injects 5-second timeouts on all queries |
