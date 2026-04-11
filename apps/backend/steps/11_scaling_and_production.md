# Step 11 -- Scaling and Production Hardening

Module: `github.com/Rengoku1926/wp_proto/apps/backend`

**Prerequisites:** Full messaging system working -- WebSocket hub, Redis pub/sub, offline buffering, delivery states, group messaging, and typing indicators all functional.

---

## 1. Horizontal Scaling Architecture

Everything we have built so far runs on a single server. The `connMap` (or `hub.clients`) is an in-memory Go map. Only the server that holds the map entry knows who is connected. This is fine for development, but in production you run multiple servers behind a load balancer.

### Why it already (mostly) works

When we introduced Redis pub/sub in Step 6, we solved the hardest part. A message from User A on Server 1 gets published to `chat:B` in Redis. Server 2, which holds User B's WebSocket, is subscribed to that channel and delivers it. The message routing layer is already distributed.

What is **not** distributed yet:

| Concern | Single server | Multi-server gap |
|---|---|---|
| Message routing | in-memory map | Solved by pub/sub |
| Connection presence | in-memory map | Need shared state |
| Offline detection | check local map | Need shared presence |
| Rate limiting | per-process counter | Need shared counter or accept per-server limits |

### The missing piece: presence

When Server 1 wants to know if User B is online (to decide whether to buffer a message), it checks its local map. On a multi-server setup, User B might be on Server 2. We need a shared presence store. Redis hashes are perfect for this.

```
                Load Balancer
               /             \
         Server 1           Server 2
         connMap: {A}       connMap: {B}
              \                /
               \              /
                Redis
          +-----------------+
          | presence hash:  |
          |   A -> srv1     |
          |   B -> srv2     |
          +-----------------+
          | pub/sub channels|
          |   chat:A        |
          |   chat:B        |
          +-----------------+
```

On connect: `HSET presence {userID} {serverID}`
On disconnect: `HDEL presence {userID}`

To check if someone is online: `HGET presence {userID}` -- returns the server ID (truthy = online) or nil (offline).

---

## 2. Redis Hashes for Presence

Create a presence repository that wraps Redis hash operations.

### `internal/repository/presence.go`

```go
package repository

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

const presenceKey = "presence"

type PresenceRepo struct {
	rdb *redis.Client
}

func NewPresenceRepo(rdb *redis.Client) *PresenceRepo {
	return &PresenceRepo{rdb: rdb}
}

// SetOnline marks a user as connected on a specific server.
func (p *PresenceRepo) SetOnline(ctx context.Context, userID, serverID string) error {
	return p.rdb.HSet(ctx, presenceKey, userID, serverID).Err()
}

// SetOffline removes a user from the presence hash.
func (p *PresenceRepo) SetOffline(ctx context.Context, userID string) error {
	return p.rdb.HDel(ctx, presenceKey, userID).Err()
}

// IsOnline checks if a user is online and returns the server they are on.
func (p *PresenceRepo) IsOnline(ctx context.Context, userID string) (bool, string, error) {
	serverID, err := p.rdb.HGet(ctx, presenceKey, userID).Result()
	if err == redis.Nil {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("presence check failed: %w", err)
	}
	return true, serverID, nil
}

// GetAllOnline returns a map of userID -> serverID for all online users.
func (p *PresenceRepo) GetAllOnline(ctx context.Context) (map[string]string, error) {
	return p.rdb.HGetAll(ctx, presenceKey).Result()
}
```

### Integrating with the hub

When a client connects:

```go
func (h *Hub) Register(client *Client) {
	h.mu.Lock()
	h.clients[client.UserID] = client
	h.mu.Unlock()

	// Mark presence in Redis so other servers know this user is here
	if err := h.presenceRepo.SetOnline(context.Background(), client.UserID, h.serverID); err != nil {
		log.Error().Err(err).Str("user_id", client.UserID).Msg("failed to set presence")
	}
}
```

When a client disconnects:

```go
func (h *Hub) Unregister(client *Client) {
	h.mu.Lock()
	delete(h.clients, client.UserID)
	h.mu.Unlock()

	if err := h.presenceRepo.SetOffline(context.Background(), client.UserID); err != nil {
		log.Error().Err(err).Str("user_id", client.UserID).Msg("failed to clear presence")
	}
}
```

Now, before buffering an offline message, check shared presence instead of the local map:

```go
online, _, err := h.presenceRepo.IsOnline(ctx, recipientID)
if err != nil {
	log.Error().Err(err).Msg("presence check failed, falling back to pub/sub")
	// Publish anyway -- if they are online somewhere, they will get it
}
if !online {
	// Buffer to offline store
}
// Always publish to pub/sub regardless -- the subscribed server delivers it
```

The `serverID` can be the hostname, a UUID generated at startup, or an environment variable:

```go
serverID := os.Getenv("SERVER_ID")
if serverID == "" {
	serverID, _ = os.Hostname()
}
```

---

## 3. Redis Sorted Sets for Recent Messages (Optional Optimization)

For conversations with high message volume, you may want fast time-range lookups without hitting Postgres every time. Redis sorted sets let you store recent message IDs with timestamps as scores.

```go
// Store a message reference after saving to Postgres
func (r *MessageCacheRepo) AddRecentMessage(ctx context.Context, conversationID, messageID string, timestamp int64) error {
	return r.rdb.ZAdd(ctx, "recent:"+conversationID, redis.Z{
		Score:  float64(timestamp),
		Member: messageID,
	}).Err()
}

// Get message IDs in a time range
func (r *MessageCacheRepo) GetMessagesInRange(ctx context.Context, conversationID string, from, to int64) ([]string, error) {
	return r.rdb.ZRangeByScore(ctx, "recent:"+conversationID, &redis.ZRangeBy{
		Min: fmt.Sprintf("%d", from),
		Max: fmt.Sprintf("%d", to),
	}).Result()
}

// Cleanup: remove messages older than a cutoff
func (r *MessageCacheRepo) CleanupOlderThan(ctx context.Context, conversationID string, cutoff int64) error {
	return r.rdb.ZRemRangeByScore(ctx, "recent:"+conversationID,
		"-inf",
		fmt.Sprintf("%d", cutoff),
	).Err()
}
```

The flow: save to Postgres (source of truth), then `ZAdd` to Redis (fast cache). Periodically run `ZRemRangeByScore` to evict old entries. When fetching recent messages, check Redis first, fall back to Postgres.

---

## 4. Redis Transactions (MULTI/EXEC)

In Step 8 (offline buffering) we used `Pipeline` to batch `LPUSH` + `EXPIRE`. A pipeline sends commands in one round-trip but they are **not atomic** -- another client could sneak a command between them.

`MULTI/EXEC` wraps commands in a true transaction: either all execute or none do.

```go
// Atomic: push to offline buffer and set expiry together
func (r *OfflineRepo) BufferMessageAtomic(ctx context.Context, userID string, payload []byte, ttl time.Duration) error {
	key := "offline:" + userID

	// TxPipeline wraps everything in MULTI/EXEC
	_, err := r.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.LPush(ctx, key, payload)
		pipe.Expire(ctx, key, ttl)
		return nil
	})
	return err
}
```

**When to use which:**

| Approach | Atomicity | Use when |
|---|---|---|
| `Pipeline` | No -- just batched network I/O | Commands are independent, you just want fewer round-trips |
| `TxPipeline` (MULTI/EXEC) | Yes -- all-or-nothing | Commands depend on each other or must not be interleaved |
| `Watch` + `TxPipeline` | Optimistic locking | You need to read-then-write without races (like CAS) |

For most of our use cases, `Pipeline` is sufficient. Use `TxPipeline` when correctness depends on atomicity.

---

## 5. Redis Pipelining -- Deeper Dive

We touched on pipelining before. Here is where it really shines: **fanout**.

When a group message goes to 50 members, you publish to 50 channels. Without pipelining, that is 50 round-trips to Redis. With pipelining, it is 1.

```go
// PublishToMany sends a message to multiple pub/sub channels in a single round-trip.
func PublishToMany(ctx context.Context, rdb *redis.Client, channels []string, payload []byte) error {
	pipe := rdb.Pipeline()

	for _, ch := range channels {
		pipe.Publish(ctx, ch, payload)
	}

	cmds, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("pipeline exec failed: %w", err)
	}

	// Check individual command results if needed
	for i, cmd := range cmds {
		if cmd.Err() != nil {
			log.Warn().
				Err(cmd.Err()).
				Str("channel", channels[i]).
				Msg("publish failed for channel")
		}
	}

	return nil
}
```

### Pipelining for batch presence checks

When rendering a contact list, you need presence for N users. Pipeline all the `HGET` calls:

```go
func (p *PresenceRepo) AreManyOnline(ctx context.Context, userIDs []string) (map[string]bool, error) {
	pipe := p.rdb.Pipeline()

	cmds := make(map[string]*redis.StringCmd, len(userIDs))
	for _, uid := range userIDs {
		cmds[uid] = pipe.HGet(ctx, presenceKey, uid)
	}

	_, err := pipe.Exec(ctx)
	if err != nil && err != redis.Nil {
		return nil, fmt.Errorf("batch presence check failed: %w", err)
	}

	result := make(map[string]bool, len(userIDs))
	for uid, cmd := range cmds {
		_, err := cmd.Result()
		result[uid] = (err == nil) // nil error means key exists -> user is online
	}

	return result, nil
}
```

---

## 6. Connection-Level Middleware

### Rate limiting per user

Prevent a single client from flooding the server with messages.

```go
package ws

import (
	"sync"
	"time"
)

type RateLimiter struct {
	mu       sync.Mutex
	tokens   int
	max      int
	interval time.Duration
	lastFill time.Time
}

func NewRateLimiter(maxPerSecond int) *RateLimiter {
	return &RateLimiter{
		tokens:   maxPerSecond,
		max:      maxPerSecond,
		interval: time.Second,
		lastFill: time.Now(),
	}
}

// Allow returns true if the user has tokens remaining.
func (rl *RateLimiter) Allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(rl.lastFill)

	if elapsed >= rl.interval {
		rl.tokens = rl.max
		rl.lastFill = now
	}

	if rl.tokens <= 0 {
		return false
	}

	rl.tokens--
	return true
}
```

Use it in the client's read loop:

```go
func (c *Client) ReadPump() {
	limiter := NewRateLimiter(10) // 10 messages per second

	for {
		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			break
		}

		if !limiter.Allow() {
			log.Warn().Str("user_id", c.UserID).Msg("rate limited")
			// Optionally send an error frame back to the client
			continue
		}

		c.hub.Broadcast <- msg
	}
}
```

### Message size limits

Set this on the WebSocket connection immediately after upgrade:

```go
const maxMessageSize = 4096 // 4 KB

conn.SetReadLimit(maxMessageSize)
```

Any message exceeding this limit causes `ReadMessage` to return an error, and the connection closes. This prevents a client from sending a 100 MB payload.

### Authentication on WebSocket upgrade

Currently we take `user_id` from a query parameter. In production, use a JWT or session token.

```go
package ws

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		// In production, check against allowed origins
		origin := r.Header.Get("Origin")
		return origin == "https://yourapp.com"
	},
}

func HandleWebSocket(hub *Hub, w http.ResponseWriter, r *http.Request) {
	// Extract and validate token
	tokenString := r.URL.Query().Get("token")
	if tokenString == "" {
		// Also check Authorization header
		auth := r.Header.Get("Authorization")
		tokenString = strings.TrimPrefix(auth, "Bearer ")
	}

	if tokenString == "" {
		http.Error(w, "missing auth token", http.StatusUnauthorized)
		return
	}

	userID, err := validateToken(tokenString)
	if err != nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	conn.SetReadLimit(maxMessageSize)

	client := &Client{
		UserID: userID,
		conn:   conn,
		send:   make(chan []byte, 256),
		hub:    hub,
	}

	hub.Register(client)
	go client.WritePump()
	go client.ReadPump()
}

func validateToken(tokenString string) (string, error) {
	secret := []byte("your-secret-key") // Use env var in production

	token, err := jwt.Parse(tokenString, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return secret, nil
	})
	if err != nil {
		return "", err
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return "", fmt.Errorf("invalid token claims")
	}

	userID, ok := claims["user_id"].(string)
	if !ok {
		return "", fmt.Errorf("user_id not found in token")
	}

	return userID, nil
}
```

The key change: the client never tells the server who they are. The server derives `user_id` from a cryptographically signed token.

---

## 7. Structured Logging Best Practices

We set up zerolog earlier. Here is how to use it properly at scale.

### Add context to every log line

```go
// At connection setup, create a logger scoped to this user
func (c *Client) setupLogger() {
	c.logger = log.With().
		Str("user_id", c.UserID).
		Str("remote_addr", c.conn.RemoteAddr().String()).
		Logger()
}

// Then use c.logger everywhere in the client
func (c *Client) ReadPump() {
	c.setupLogger()

	for {
		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			c.logger.Info().Msg("client disconnected")
			break
		}

		c.logger.Debug().Int("size", len(msg)).Msg("message received")
		// ...
	}
}
```

### Request ID for traceability

For HTTP endpoints, add a request ID middleware:

```go
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = uuid.NewString()
		}

		ctx := context.WithValue(r.Context(), "request_id", requestID)
		w.Header().Set("X-Request-ID", requestID)

		logger := log.With().Str("request_id", requestID).Logger()
		ctx = logger.WithContext(ctx)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
```

### Log levels guide

| Level | When to use | Example |
|---|---|---|
| `Debug` | Per-message events, frequent operations | "message received", "publish to channel" |
| `Info` | Connection lifecycle, significant events | "client connected", "client disconnected" |
| `Warn` | Degraded behavior, slow clients | "rate limited", "slow consumer, dropping message" |
| `Error` | Failures that need attention | "redis publish failed", "database write failed" |
| `Fatal` | Cannot continue | "failed to connect to Redis on startup" |

In production, set the log level to `Info`. In development, use `Debug`. Never log at `Debug` level in production -- the volume will overwhelm your log aggregator.

```go
// Set via environment variable
level := os.Getenv("LOG_LEVEL")
switch level {
case "debug":
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
case "warn":
	zerolog.SetGlobalLevel(zerolog.WarnLevel)
default:
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
}
```

---

## 8. Monitoring and Observability

### Metrics to track

Expose these either via a `/metrics` endpoint or by logging them periodically:

```go
type Metrics struct {
	mu               sync.RWMutex
	activeConns      int64
	totalMsgSent     int64
	totalMsgReceived int64
	offlineBuffered  int64
}

var metrics = &Metrics{}

func (m *Metrics) IncrConns()      { atomic.AddInt64(&m.activeConns, 1) }
func (m *Metrics) DecrConns()      { atomic.AddInt64(&m.activeConns, -1) }
func (m *Metrics) IncrMsgSent()    { atomic.AddInt64(&m.totalMsgSent, 1) }
func (m *Metrics) IncrMsgRecv()    { atomic.AddInt64(&m.totalMsgReceived, 1) }
func (m *Metrics) IncrBuffered()   { atomic.AddInt64(&m.offlineBuffered, 1) }

func (m *Metrics) Snapshot() map[string]int64 {
	return map[string]int64{
		"active_connections":  atomic.LoadInt64(&m.activeConns),
		"total_msg_sent":     atomic.LoadInt64(&m.totalMsgSent),
		"total_msg_received": atomic.LoadInt64(&m.totalMsgReceived),
		"offline_buffered":   atomic.LoadInt64(&m.offlineBuffered),
	}
}
```

### Health endpoint

```go
func HealthHandler(rdb *redis.Client, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		health := map[string]string{
			"status": "ok",
		}

		// Check Redis
		if err := rdb.Ping(ctx).Err(); err != nil {
			health["status"] = "degraded"
			health["redis"] = "down"
		} else {
			health["redis"] = "ok"
		}

		// Check Postgres
		if err := db.PingContext(ctx); err != nil {
			health["status"] = "degraded"
			health["postgres"] = "down"
		} else {
			health["postgres"] = "ok"
		}

		// Include metrics
		for k, v := range metrics.Snapshot() {
			health[k] = fmt.Sprintf("%d", v)
		}

		w.Header().Set("Content-Type", "application/json")
		if health["status"] != "ok" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(health)
	}
}
```

### Prometheus (brief mention)

For production-grade monitoring, expose metrics in Prometheus format using `prometheus/client_golang`. Define gauges for active connections, counters for messages sent/received, and histograms for message processing latency. Grafana dashboards built on top of Prometheus give you real-time visibility into your system. This is beyond the scope of this tutorial, but the patterns above give you the data -- Prometheus just standardizes how you export it.

---

## 9. Security Considerations

### WebSocket origin checking

We showed this in the upgrader above. Never use `CheckOrigin: func(r *http.Request) bool { return true }` in production. Whitelist your frontend domain(s).

### Message body sanitization

Never trust client input. Before storing or forwarding a message:

```go
import "html"

func sanitizeMessage(body string) string {
	// Strip or escape HTML to prevent XSS if messages render in a browser
	return html.EscapeString(body)
}
```

For a WhatsApp-style app where messages are plain text displayed in a native client, this is less critical. But if you ever render messages in a web view, this matters.

### Rate limiting

Already covered above. The key numbers:
- 10 messages per second per user is a reasonable default
- Adjust based on your use case (group chats may need higher limits)
- Consider server-wide limits too (total messages per second across all users)

### TLS termination

Never run WebSocket connections over plain `ws://` in production. Use `wss://` (WebSocket Secure).

You typically do not terminate TLS in the Go server itself. Instead, put a reverse proxy in front:

```
Client --wss://--> Nginx/Caddy (TLS termination) --ws://--> Go server
```

Caddy makes this trivial:

```
yourapp.com {
    reverse_proxy localhost:8080
}
```

Caddy automatically provisions and renews Let's Encrypt certificates.

---

## Production Deployment Summary

Here is what a production-ready deployment of this system looks like:

```
                    Internet
                       |
                   Load Balancer
                   (Caddy / Nginx)
                   TLS termination
                  /       |       \
            Server 1  Server 2  Server 3
            (Go app)  (Go app)  (Go app)
                  \       |       /
                   \      |      /
                    Redis Cluster
                    - Pub/Sub (message routing)
                    - Hashes (presence)
                    - Lists (offline buffer)
                    - Sorted Sets (message cache)
                        |
                    PostgreSQL
                    - Users
                    - Messages (source of truth)
                    - Conversations
```

**Each Go server:**
- Accepts WebSocket connections with JWT authentication
- Registers presence in Redis on connect, clears on disconnect
- Publishes messages to Redis pub/sub
- Subscribes to channels for its connected users
- Buffers offline messages in Redis lists
- Persists messages to Postgres
- Enforces rate limits and message size limits
- Exports structured logs with user context
- Exposes health and metrics endpoints

**What we built across all 11 steps:**

1. Project setup and module structure
2. HTTP server and user registration (Postgres)
3. First WebSocket connection and echo
4. Hub pattern for managing connections
5. Delivery states (sent/delivered/read ticks)
6. Redis pub/sub for distributed message routing
7. Group messaging
8. Offline message buffering (Redis lists)
9. Typing indicators
10. Structured logging with zerolog
11. Production hardening and scaling (this step)

The system is horizontally scalable because no server holds exclusive state. Redis is the shared coordination layer, Postgres is the durable store, and each Go server is a stateless worker that manages WebSocket connections. You can add or remove servers behind the load balancer without losing messages.
