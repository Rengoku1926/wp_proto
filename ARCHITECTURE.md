# WhatsApp Prototype — Architecture & Engineering Guide

> A complete reference for building a store-and-forward messaging engine with delivery states, offline buffering, and group fanout using Go, Redis, Postgres, and WebSockets.

---

## Table of Contents

### Backend
1. [Project Overview](#1-project-overview)
2. [Core Concepts & Theory](#2-core-concepts--theory)
3. [System Architecture](#3-system-architecture)
4. [Monorepo Structure](#4-monorepo-structure)
5. [Tooling & Libraries](#5-tooling--libraries)
6. [Data Models](#6-data-models)
7. [Component Deep-Dives](#7-component-deep-dives)
   - 7.1 [WebSocket Hub](#71-websocket-hub)
   - 7.2 [Redis Pub/Sub Layer](#72-redis-pubsub-layer)
   - 7.3 [Message Router](#73-message-router)
   - 7.4 [Offline Buffer](#74-offline-buffer)
   - 7.5 [Delivery State Machine](#75-delivery-state-machine)
   - 7.6 [Group Fanout Engine](#76-group-fanout-engine)
8. [Database Schema & Migrations](#8-database-schema--migrations)
9. [Configuration](#9-configuration)
10. [Logging](#10-logging)
11. [API Documentation](#11-api-documentation)
12. [Message Lifecycle (End-to-End)](#12-message-lifecycle-end-to-end)
13. [Connection State Management](#13-connection-state-management)
14. [The Tick System](#14-the-tick-system)
15. [Group Messaging Deep-Dive](#15-group-messaging-deep-dive)
16. [Build Order & Implementation Roadmap](#16-build-order--implementation-roadmap)
17. [Key Go Patterns Used](#17-key-go-patterns-used)
18. [Redis Command Reference](#18-redis-command-reference)
19. [Common Pitfalls & How to Avoid Them](#19-common-pitfalls--how-to-avoid-them)

### Frontend
20. [Client Application](#20-client-application)

---

## 1. Project Overview

This project is a from-scratch implementation of the core engine that powers WhatsApp's reliability guarantees. The goal is not to build a production-ready clone but to deeply understand the engineering decisions behind:

- How a message reaches a device that was offline when the message was sent
- How the single tick, double tick, and blue tick states are tracked and propagated
- How a single group message is efficiently delivered to tens or hundreds of members
- How concurrent connections are managed safely in Go

**Stack:**

| Layer          | Technology                    | Why                                             |
| -------------- | ----------------------------- | ----------------------------------------------- |
| Transport      | WebSocket (gorilla/websocket) | Full-duplex, persistent, low-overhead           |
| Concurrency    | Goroutines + channels         | Go's native CSP model                           |
| Message broker | Redis pub/sub                 | Decouples hub from delivery, enables multi-node |
| Offline buffer | Redis lists (LPUSH/BRPOP)     | Ordered, blocking-pop, TTL-able                 |
| Hot state      | Redis hashes                  | Connection presence, delivery states            |
| Persistence    | Postgres                      | Durable message log, group membership           |
| Migrations     | Tern                          | SQL-based, sequential, no ORM dependency        |
| Config         | Koanf                         | Composable config from env, files, flags        |
| Logging        | Zerolog                       | Structured, zero-allocation JSON logging        |
| API Docs       | OpenAPI + Scalar              | Interactive API reference from spec             |

---

## 2. Core Concepts & Theory

### 2.1 Store-and-Forward

Store-and-forward is the fundamental pattern behind every reliable messaging system — email, SMS, WhatsApp, and Telegram all use it.

**The problem it solves:** If you send a message directly to a device (peer-to-peer), the message is lost if the device is offline.

**The solution:** Route every message through a server that stores it until the recipient is ready.

```
Sender -> Server (stores message) -> Recipient (when online)
```

The server acts as a trusted intermediary. The message is never truly "sent" until the server has accepted it. From that point, delivery to the recipient becomes the server's responsibility, not the sender's.

In this project, "storing" happens in two places:

- **Redis list** (`offline:{userID}`) — fast, temporary, bounded TTL (30 days)
- **Postgres** (`messages` table) — durable, permanent record

### 2.2 The Delivery State Machine

A state machine is a system that can be in exactly one of a finite set of states at any time, and transitions between states based on events.

For a message in WhatsApp, the states are:

```
[PENDING] -> [SENT] -> [DELIVERED] -> [READ]
```

| State     | Meaning                                           | Trigger                   | Tick UI    |
| --------- | ------------------------------------------------- | ------------------------- | ---------- |
| PENDING   | Client has the message, hasn't reached server yet | —                         | clock icon |
| SENT      | Server accepted the message                       | Server wrote to DB        | single grey tick   |
| DELIVERED | Recipient's device received and ACK'd the message | Device sends `ack` frame  | double grey tick  |
| READ      | Recipient opened the conversation                 | Device sends `read` frame | double blue tick |

Key rule: **state only moves forward**. A message can never go from DELIVERED back to SENT. Transitions are append-only events in the database.

### 2.3 Pub/Sub vs Message Queue

These two patterns are often confused. Understanding the difference is critical for this project.

**Message Queue (point-to-point):**

- One producer, one consumer
- Each message is consumed exactly once and removed
- Used here: `offline:{userID}` Redis list — one buffer per user, drained on reconnect

**Pub/Sub (broadcast):**

- One publisher, many subscribers
- All subscribers receive every message on the channel
- Used here: `chat:{userID}` Redis channel — the router publishes, the hub's subscriber goroutine for that user receives it
- Messages are NOT stored — if no subscriber is listening, the message is dropped

This is why both patterns are needed. Pub/sub handles the live delivery path. The queue handles the offline path.

### 2.4 Fanout

Fanout is what happens when one event must be delivered to N recipients independently.

In group messaging, Alice sends one message to a group with 50 members. The server must deliver it to all 50 independently. This is a **write fanout** — one write by Alice triggers 50 writes (one publish per member).

Two strategies exist:

**Fanout on write (used here):** When a message arrives, immediately publish to every member's channel. Each member's router handles their own online/offline logic. Pros: low read latency. Cons: expensive for huge groups.

**Fanout on read:** Store the message once. Each device fetches it when they come online. Pros: cheaper for huge groups. Cons: read-time join complexity.

For a prototype with groups up to ~500 members, fanout on write is simpler and correct.

### 2.5 Idempotency

Idempotency means performing the same operation twice produces the same result as performing it once.

In messaging, if a device disconnects right after receiving a message but before ACK'ing it, the server may re-deliver on reconnect. Without idempotency checks, the user sees the message twice.

The fix: every message has a `client_id` (UUID generated by the sender's device). Before inserting to the DB or delivering to the client, check if this `client_id` was already processed.

### 2.6 Connection State Management

This is the hub's core responsibility. The hub maintains a map of `userID -> *websocket.Conn` that represents who is online right now.

This map is the source of truth for the router's online/offline decision:

```
Is userID in connMap? -> YES: push via WebSocket
                      -> NO:  push to offline buffer
```

The map must be:

- **Concurrent-safe**: multiple goroutines read and write it simultaneously
- **Fast**: the router checks it on every message
- **Accurate**: stale entries (dead connections) must be cleaned up

This is why connection state management is the hardest correctness problem in the hub. A stale entry (a goroutine that died but wasn't removed from the map) causes messages to be written to a dead connection, not to the offline buffer — so the user never receives those messages.

---

## 3. System Architecture

```
+-----------------------------------------------------------+
|                    CLIENT LAYER                            |
|  Device A (online)   Device B (offline)   Device C        |
|        |                                       |          |
|        +------------- WebSocket ---------------+          |
+----------------------------+------------------------------+
                             | HTTP Upgrade -> WS
+----------------------------v------------------------------+
|               WEBSOCKET HUB  (apps/backend)               |
|                                                           |
|  connMap: map[userID]*Conn  (RWMutex protected)           |
|  register chan, unregister chan, broadcast chan             |
|  One goroutine per connection (readPump + writePump)      |
+------------+----------------------------------------------+
             | PUBLISH to chat:{userID}
+------------v----------------------------------------------+
|              REDIS PUB/SUB  (message broker)              |
|                                                           |
|  Channels: chat:{userID}                                  |
|  Hub subscribes on connect, unsubscribes on disconnect    |
+------------+----------------------------------------------+
             | message arrives on channel
+------------v----------------------------------------------+
|            MESSAGE ROUTER  (internal/router)              |
|                                                           |
|  Lookup connMap -> Online?                                |
|    YES -> push to writePump channel                       |
|    NO  -> LPUSH offline:{userID}  (Redis list)            |
+------------+----------------------+-----------------------+
             |                      |
+------------v----------+  +--------v-----------------------+
|  DELIVERY STATE       |  |  OFFLINE BUFFER  (Redis list)  |
|  MACHINE              |  |                                |
|                       |  |  LPUSH offline:{userID} msg    |
|  SENT -> DELIVERED    |  |  TTL: 30 days                  |
|  DELIVERED -> READ    |  |  On reconnect: BRPOP drain     |
|                       |  |  -> bulk deliver + fire ACKs   |
|  Stored: Redis +      |  +--------------------------------+
|  Postgres             |
+------------+----------+
             |
+------------v----------------------------------------------+
|              PERSISTENCE LAYER                            |
|                                                           |
|  Postgres: messages, delivery_events, groups, members     |
|  Redis:    connMap presence, offline:{uid}, state cache   |
+-----------------------------------------------------------+

GROUP FANOUT (runs inside router):

  1 message -> fetch group member IDs from Postgres
            -> spawn goroutine per member
            -> each goroutine: PUBLISH to chat:{memberID}
            -> each member's router handles online/offline
            -> aggregate ACKs in Redis counter
            -> group message DELIVERED when counter == member count
```

### Traffic Flow Summary

**Happy path (both online):**

```
Alice sends msg -> Hub receives -> PUBLISH chat:{bob} ->
Router sees Bob online -> push to Bob's writePump ->
Bob's device ACKs -> Router updates state to DELIVERED ->
Router pushes state update to Alice's writePump -> Alice sees double tick
```

**Offline path:**

```
Alice sends msg -> Hub receives -> PUBLISH chat:{bob} ->
Router sees Bob offline -> LPUSH offline:{bob} msg ->
Bob reconnects -> Hub drains BRPOP -> delivers messages ->
Bob's device ACKs each -> DELIVERED states update ->
Alice sees double tick (possibly hours later)
```

---

## 4. Monorepo Structure

```
wp_proto/
|-- ARCHITECTURE.md
|
|-- apps/
|   |-- backend/                    <-- Go backend service
|   |   |-- cmd/
|   |   |   +-- main.go             <-- Entrypoint
|   |   |
|   |   |-- internal/
|   |   |   |-- config/             <-- Koanf-based configuration loading
|   |   |   |-- database/           <-- Postgres + Redis connection setup
|   |   |   |-- errs/               <-- Custom error types
|   |   |   |-- handler/            <-- HTTP/WS request handlers
|   |   |   |-- lib/                <-- Shared utilities
|   |   |   |-- logger/             <-- Zerolog setup and helpers
|   |   |   |-- middleware/         <-- HTTP middleware (auth, logging, recovery)
|   |   |   |-- model/              <-- Domain structs (Message, Group, User, Envelope)
|   |   |   |-- repository/         <-- Data access layer (Postgres queries, Redis ops)
|   |   |   |-- router/             <-- Message routing + fanout engine
|   |   |   |-- server/             <-- HTTP server bootstrap, graceful shutdown
|   |   |   |-- service/            <-- Business logic layer
|   |   |   |-- sqlerr/             <-- Postgres error classification helpers
|   |   |   |-- testing/            <-- Test helpers and fixtures
|   |   |   +-- validation/         <-- Request validation
|   |   |
|   |   |-- static/
|   |   |   |-- openapi.html        <-- Scalar UI entrypoint
|   |   |   +-- openapi.json        <-- OpenAPI 3.1 spec
|   |   |
|   |   |-- go.mod
|   |   |-- go.sum
|   |   +-- Taskfile.yml            <-- Task runner (build, test, migrate, generate)
|   |
|   +-- client/                     <-- Frontend application
|       +-- index.html
|
+-- infra/
    +-- docker-compose.yml          <-- Postgres + Redis (no migrations)
```

The `internal/` package boundary enforces that all packages inside it are private to `apps/backend`. Nothing outside the module can import them. This is Go's built-in encapsulation — no linter or convention needed.

---

## 5. Tooling & Libraries

### 5.1 Tern — Database Migrations

[Tern](https://github.com/jackc/tern) is a standalone SQL migration tool by the author of pgx (the Postgres driver). It runs plain `.sql` files in sequential order.

**Why Tern over golang-migrate or goose:**
- No Go code in migrations — pure SQL, reviewable by DBAs
- Built on pgx — same driver the app uses, no dependency mismatch
- Simple CLI: `tern migrate`, `tern new`, `tern status`
- No "down" migrations by default — encourages forward-only, append-only schema changes

**Migration directory:** `apps/backend/` (configured via `tern.conf`)

**Typical migration file:**

```sql
-- 001_create_users.sql
CREATE TABLE users (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username   TEXT UNIQUE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**Workflow:**

```bash
# Create a new migration
tern new create_messages

# Run all pending migrations
tern migrate

# Check migration status
tern status
```

Migrations are NOT part of docker-compose. They run as a separate step via `tern migrate` (or via Taskfile: `task migrate`). This keeps infrastructure (docker-compose) and schema management (tern) cleanly separated.

### 5.2 Zerolog — Structured Logging

[Zerolog](https://github.com/rs/zerolog) is a zero-allocation JSON logger for Go. Every log line is a valid JSON object, making it easy to parse with tools like `jq`, Loki, or Datadog.

**Why Zerolog:**
- Zero allocations on common log paths — no GC pressure in hot loops (message routing, WebSocket frames)
- Chained API: `log.Info().Str("user", id).Msg("connected")` — no `fmt.Sprintf`
- Leveled logging with runtime level switching
- Context-aware: attach fields to a `context.Context` and they appear on every log line

**Setup (internal/logger/):**

```go
package logger

import (
    "os"
    "time"
    "github.com/rs/zerolog"
)

func New(level string) zerolog.Logger {
    lvl, _ := zerolog.ParseLevel(level)
    return zerolog.New(os.Stdout).
        Level(lvl).
        With().
        Timestamp().
        Caller().
        Logger()
}
```

**Usage throughout the codebase:**

```go
// In handler
log.Info().Str("user_id", uid).Msg("websocket connected")

// In router
log.Debug().Str("recipient", rid).Bool("online", true).Msg("routing message")

// Errors with context
log.Error().Err(err).Str("message_id", mid).Msg("failed to persist message")
```

For local development, zerolog supports a `ConsoleWriter` that pretty-prints with colors. Toggle via config.

### 5.3 Koanf — Configuration Management

[Koanf](https://github.com/knadh/koanf) is a composable configuration library. It loads config from multiple sources (files, env vars, flags) and merges them with a clear priority order.

**Why Koanf over Viper:**
- Lighter, no global state — each `koanf.Koanf` instance is independent
- Cleaner API for nested config
- First-class support for env var prefixes and delimiters
- No dependency on `fsnotify` or `remote` by default

**Config loading order (last wins):**

1. `config.yaml` — defaults
2. `config.local.yaml` — local overrides (gitignored)
3. Environment variables (`WP_` prefix)

**Setup (internal/config/):**

```go
package config

import (
    "github.com/knadh/koanf/v2"
    "github.com/knadh/koanf/parsers/yaml"
    "github.com/knadh/koanf/providers/env"
    "github.com/knadh/koanf/providers/file"
)

type Config struct {
    Server   ServerConfig   `koanf:"server"`
    Postgres PostgresConfig `koanf:"postgres"`
    Redis    RedisConfig    `koanf:"redis"`
    Log      LogConfig      `koanf:"log"`
}

type ServerConfig struct {
    Port int    `koanf:"port"`
    Host string `koanf:"host"`
}

type PostgresConfig struct {
    DSN string `koanf:"dsn"`
}

type RedisConfig struct {
    Addr string `koanf:"addr"`
}

type LogConfig struct {
    Level string `koanf:"level"`
}

func Load() (*Config, error) {
    k := koanf.New(".")

    // 1. Load defaults from config.yaml
    k.Load(file.Provider("config.yaml"), yaml.Parser())

    // 2. Load local overrides (gitignored)
    k.Load(file.Provider("config.local.yaml"), yaml.Parser())

    // 3. Env vars override everything: WP_SERVER_PORT -> server.port
    k.Load(env.Provider("WP_", ".", func(s string) string {
        return strings.Replace(strings.ToLower(strings.TrimPrefix(s, "WP_")), "_", ".", -1)
    }), nil)

    var cfg Config
    if err := k.Unmarshal("", &cfg); err != nil {
        return nil, err
    }
    return &cfg, nil
}
```

**Example config.yaml:**

```yaml
server:
  host: "0.0.0.0"
  port: 8080

postgres:
  dsn: "postgres://postgres:secret@localhost:5432/whatsapp_proto?sslmode=disable"

redis:
  addr: "localhost:6379"

log:
  level: "debug"
```

### 5.4 OpenAPI + Scalar — API Documentation

The API is documented with an [OpenAPI 3.1](https://spec.openapis.org/oas/v3.1.0) spec file (`static/openapi.json`) and rendered with [Scalar](https://github.com/scalar/scalar) for an interactive UI.

**Why OpenAPI + Scalar:**
- OpenAPI is the industry standard — tools for codegen, testing, mocking all read it
- Scalar provides a modern, interactive API explorer (replaces Swagger UI)
- The spec is a static JSON file — no runtime generation, no reflection magic
- Scalar is a single HTML file with a CDN script tag — zero build step

**Files:**
- `apps/backend/static/openapi.json` — the OpenAPI 3.1 spec
- `apps/backend/static/openapi.html` — Scalar UI that loads the spec

**Serving the docs:**

```go
// In server setup
mux.Handle("/docs", http.FileServer(http.Dir("static")))
mux.HandleFunc("/docs/openapi.json", func(w http.ResponseWriter, r *http.Request) {
    http.ServeFile(w, r, "static/openapi.json")
})
```

Visit `http://localhost:8080/docs/openapi.html` for the interactive API reference.

The OpenAPI spec covers REST endpoints (user registration, group management, message history). WebSocket frames are documented in [Section 14 — The Tick System](#14-the-tick-system).

---

## 6. Data Models

### Message (`internal/model/message.go`)

```go
package model

import (
    "time"
    "github.com/google/uuid"
)

type DeliveryState int

const (
    StatePending   DeliveryState = 0  // client has it, not yet at server
    StateSent      DeliveryState = 1  // server accepted (single grey tick)
    StateDelivered DeliveryState = 2  // device ACK received (double grey tick)
    StateRead      DeliveryState = 3  // read receipt received (blue double tick)
)

type Message struct {
    ID          uuid.UUID     `json:"id"`
    ClientID    uuid.UUID     `json:"client_id"`    // dedup key, set by sender
    SenderID    uuid.UUID     `json:"sender_id"`
    RecipientID uuid.UUID     `json:"recipient_id"` // nil for group messages
    GroupID     *uuid.UUID    `json:"group_id"`     // nil for 1:1 messages
    Body        string        `json:"body"`
    State       DeliveryState `json:"state"`
    CreatedAt   time.Time     `json:"created_at"`
    UpdatedAt   time.Time     `json:"updated_at"`
}

// Envelope wraps a message for WebSocket transport.
type Envelope struct {
    Type    string      `json:"type"`    // "message", "ack", "state_update", "read"
    Payload interface{} `json:"payload"`
}

// ACK is sent by the recipient's device back to the server.
type ACK struct {
    MessageID uuid.UUID `json:"message_id"`
    UserID    uuid.UUID `json:"user_id"`
}

// StateUpdate is pushed to the sender when delivery state changes.
type StateUpdate struct {
    MessageID uuid.UUID     `json:"message_id"`
    State     DeliveryState `json:"state"`
}
```

### Group (`internal/model/group.go`)

```go
package model

import (
    "time"
    "github.com/google/uuid"
)

type Group struct {
    ID        uuid.UUID `json:"id"`
    Name      string    `json:"name"`
    CreatedAt time.Time `json:"created_at"`
}

type Member struct {
    GroupID  uuid.UUID `json:"group_id"`
    UserID   uuid.UUID `json:"user_id"`
    JoinedAt time.Time `json:"joined_at"`
}
```

---

## 7. Component Deep-Dives

### 7.1 WebSocket Hub

The hub is the central nerve of the backend service. It has one job: maintain a safe, accurate map of connected users.

**The Hub struct:**

```go
// internal/handler/hub.go
type Hub struct {
    connMap map[string]*Client
    mu      sync.RWMutex

    register   chan *Client
    unregister chan *Client
}
```

**Why use channels for register/unregister instead of locking directly?**

The common beginner approach is to lock the map every time you read or write it. This works but leads to subtle deadlocks when, for example, the writePump goroutine tries to acquire the lock while the hub's Run loop holds it.

The channel approach is safer: all mutations to `connMap` happen in a single goroutine (the Run loop), eliminating the need for a lock on writes. The map is only read (not written) from other goroutines, so a `sync.RWMutex` for reads is correct.

**The Run loop:**

```go
func (h *Hub) Run() {
    for {
        select {
        case client := <-h.register:
            h.mu.Lock()
            h.connMap[client.userID] = client
            h.mu.Unlock()
            go client.drainOfflineBuffer()

        case client := <-h.unregister:
            h.mu.Lock()
            if _, ok := h.connMap[client.userID]; ok {
                delete(h.connMap, client.userID)
                close(client.send)
            }
            h.mu.Unlock()
        }
    }
}
```

**The Client struct:**

Each connected WebSocket connection gets a `Client`. Two goroutines run per client: `readPump` (reads frames from the network) and `writePump` (writes frames to the network).

```go
// internal/handler/client.go
type Client struct {
    hub    *Hub
    conn   *websocket.Conn
    userID string
    send   chan []byte  // buffered channel; writePump drains this
}
```

**readPump** — runs in its own goroutine, owned by the Client:

```go
func (c *Client) readPump() {
    defer func() {
        c.hub.unregister <- c
        c.conn.Close()
    }()

    c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
    c.conn.SetPongHandler(func(string) error {
        c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
        return nil
    })

    for {
        _, msg, err := c.conn.ReadMessage()
        if err != nil {
            break
        }
        c.handleIncoming(msg)
    }
}
```

**writePump** — runs in its own goroutine, only goroutine that writes to this connection:

```go
func (c *Client) writePump() {
    ticker := time.NewTicker(54 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case msg, ok := <-c.send:
            if !ok {
                c.conn.WriteMessage(websocket.CloseMessage, []byte{})
                return
            }
            c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
            c.conn.WriteMessage(websocket.TextMessage, msg)

        case <-ticker.C:
            c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
            if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
                return
            }
        }
    }
}
```

**Critical rule:** `writePump` is the ONLY goroutine that calls `conn.Write*`. `gorilla/websocket` connections are not safe for concurrent writes. The `send` channel enforces this.

### 7.2 Redis Pub/Sub Layer

Redis pub/sub is the decoupling layer between the hub and the router.

**Why not push directly from hub to the recipient?**

If Alice and Bob are connected to the same server, direct push works. But in production, they might be on different servers. Redis pub/sub solves this: any server can publish to `chat:{bob}`, and whichever server Bob is connected to will receive it.

**Channel naming:** `chat:{userID}` — one channel per user.

```go
// internal/repository/pubsub.go

func Publish(ctx context.Context, rdb *redis.Client, userID string, msg []byte) error {
    return rdb.Publish(ctx, "chat:"+userID, msg).Err()
}

func Subscribe(ctx context.Context, rdb *redis.Client, userID string) *redis.PubSub {
    return rdb.Subscribe(ctx, "chat:"+userID)
}
```

**Subscriber goroutine (per client):**

```go
func (c *Client) subscribeLoop(rdb *redis.Client) {
    sub := repository.Subscribe(context.Background(), rdb, c.userID)
    defer sub.Close()

    ch := sub.Channel()
    for msg := range ch {
        c.send <- []byte(msg.Payload)
    }
}
```

### 7.3 Message Router

The router decides if the recipient is online and acts accordingly.

```go
// internal/router/router.go

func (r *Router) Route(msg *model.Message) error {
    recipientID := msg.RecipientID.String()

    r.hub.mu.RLock()
    client, online := r.hub.connMap[recipientID]
    r.hub.mu.RUnlock()

    if online {
        data, _ := json.Marshal(model.Envelope{Type: "message", Payload: msg})
        select {
        case client.send <- data:
        default:
            online = false
        }
    }

    if !online {
        return r.offlineStore.Push(context.Background(), recipientID, msg)
    }

    return nil
}
```

The `select` with `default` prevents one slow client from stalling the entire message pipeline.

### 7.4 Offline Buffer

The offline buffer is a Redis list per user. Redis lists support O(1) push and blocking pop.

```
Key: offline:{userID}
Type: Redis List
Direction: LPUSH (push to head), BRPOP (pop from tail, blocking)
TTL: 30 days per message
```

**Push (when recipient is offline):**

```go
// internal/repository/offline.go

func (s *OfflineStore) Push(ctx context.Context, userID string, msg *model.Message) error {
    data, err := json.Marshal(msg)
    if err != nil {
        return err
    }
    key := "offline:" + userID
    pipe := s.rdb.Pipeline()
    pipe.LPush(ctx, key, data)
    pipe.Expire(ctx, key, 30*24*time.Hour)
    _, err = pipe.Exec(ctx)
    return err
}
```

**Drain (called when user connects):**

```go
func (s *OfflineStore) Drain(ctx context.Context, userID string) ([]*model.Message, error) {
    key := "offline:" + userID
    var messages []*model.Message

    for {
        result, err := s.rdb.RPop(ctx, key).Result()
        if err == redis.Nil {
            break
        }
        if err != nil {
            return messages, err
        }
        var msg model.Message
        if err := json.Unmarshal([]byte(result), &msg); err != nil {
            continue
        }
        messages = append(messages, &msg)
    }

    return messages, nil
}
```

### 7.5 Delivery State Machine

State transitions update both Redis (for speed) and Postgres (for durability).

```go
func (sm *StateMachine) Transition(msgID uuid.UUID, to model.DeliveryState) error {
    current, err := sm.GetState(msgID)
    if err != nil {
        return err
    }
    if to <= current {
        return nil // idempotent
    }

    sm.rdb.HSet(context.Background(), "state:"+msgID.String(), "state", int(to))
    return sm.pg.UpdateDeliveryState(context.Background(), msgID, to)
}
```

**Read receipt propagation:**

When a client opens a chat and sees messages, it sends a `read` frame. The server transitions those messages to `StateRead`, then pushes a `state_update` envelope back to the sender.

### 7.6 Group Fanout Engine

Group messages require publishing to every member independently.

```go
// internal/router/fanout.go

func (f *FanoutEngine) Fanout(ctx context.Context, msg *model.Message) error {
    members, err := f.pg.GetGroupMembers(ctx, *msg.GroupID)
    if err != nil {
        return err
    }

    var wg sync.WaitGroup
    errCh := make(chan error, len(members))

    for _, member := range members {
        if member.UserID == msg.SenderID {
            continue
        }

        wg.Add(1)
        go func(recipientID uuid.UUID) {
            defer wg.Done()
            recipientMsg := *msg
            recipientMsg.RecipientID = recipientID
            data, _ := json.Marshal(model.Envelope{Type: "message", Payload: &recipientMsg})
            if err := f.rdb.Publish(ctx, "chat:"+recipientID.String(), data).Err(); err != nil {
                errCh <- err
            }
        }(member.UserID)
    }

    wg.Wait()
    close(errCh)

    f.rdb.Set(ctx, "fanout:pending:"+msg.ID.String(), len(members)-1, 7*24*time.Hour)
    return nil
}
```

**Aggregate delivery for groups:**

```go
func (f *FanoutEngine) HandleMemberACK(ctx context.Context, msgID uuid.UUID) error {
    key := "fanout:pending:" + msgID.String()
    remaining, err := f.rdb.Decr(ctx, key).Result()
    if err != nil {
        return err
    }
    if remaining == 0 {
        return f.stateMachine.Transition(msgID, model.StateDelivered)
    }
    return nil
}
```

---

## 8. Database Schema & Migrations

Migrations are managed by **tern** and live alongside the backend code. They are plain SQL files executed in order.

**Running migrations:**

```bash
cd apps/backend
tern migrate
```

**Schema:**

```sql
-- 001_create_users.sql
CREATE TABLE users (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username   TEXT UNIQUE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 002_create_messages.sql
CREATE TABLE messages (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id     UUID UNIQUE NOT NULL,
    sender_id     UUID NOT NULL REFERENCES users(id),
    recipient_id  UUID REFERENCES users(id),
    group_id      UUID REFERENCES groups(id),
    body          TEXT NOT NULL,
    state         SMALLINT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_messages_sender    ON messages(sender_id);
CREATE INDEX idx_messages_recipient ON messages(recipient_id);
CREATE INDEX idx_messages_group     ON messages(group_id);
CREATE INDEX idx_messages_client_id ON messages(client_id);

-- 003_create_groups.sql
CREATE TABLE groups (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE group_members (
    group_id  UUID NOT NULL REFERENCES groups(id),
    user_id   UUID NOT NULL REFERENCES users(id),
    joined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, user_id)
);

CREATE INDEX idx_group_members_group ON group_members(group_id);
CREATE INDEX idx_group_members_user  ON group_members(user_id);

-- 004_create_delivery_events.sql
CREATE TABLE delivery_events (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id UUID NOT NULL REFERENCES messages(id),
    state      SMALLINT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_delivery_events_message ON delivery_events(message_id);

-- 005_create_group_message_delivery.sql
CREATE TABLE group_message_delivery (
    message_id   UUID NOT NULL REFERENCES messages(id),
    member_id    UUID NOT NULL REFERENCES users(id),
    state        SMALLINT NOT NULL DEFAULT 1,
    PRIMARY KEY (message_id, member_id)
);
```

---

## 9. Configuration

Configuration is loaded via **koanf** in `internal/config/`. See [Section 5.3](#53-koanf--configuration-management) for the full setup.

**Priority order (last wins):**

1. `config.yaml` — checked-in defaults
2. `config.local.yaml` — local overrides (gitignored)
3. `WP_*` environment variables — production/CI overrides

**All config keys:**

| Key               | Env Var            | Default                          | Description               |
| ----------------- | ------------------ | -------------------------------- | ------------------------- |
| `server.host`     | `WP_SERVER_HOST`   | `0.0.0.0`                       | Bind address              |
| `server.port`     | `WP_SERVER_PORT`   | `8080`                          | HTTP port                 |
| `postgres.dsn`    | `WP_POSTGRES_DSN`  | `postgres://...localhost:5432/…` | Postgres connection       |
| `redis.addr`      | `WP_REDIS_ADDR`    | `localhost:6379`                 | Redis address             |
| `log.level`       | `WP_LOG_LEVEL`     | `debug`                         | Zerolog level             |

---

## 10. Logging

Logging is handled by **zerolog** via `internal/logger/`. See [Section 5.2](#52-zerolog--structured-logging) for setup details.

**Conventions:**

- Always use structured fields: `.Str("user_id", id)` not `fmt.Sprintf`
- Every log line must have a `.Msg()` — zerolog drops the line without it
- Use `log.Error().Err(err)` for errors — this adds an `"error"` field automatically
- Use `log.Debug()` for per-message routing decisions (high volume)
- Use `log.Info()` for connection lifecycle events (connect, disconnect)
- Use `log.Warn()` for recoverable issues (channel full, slow client)
- Use `log.Error()` for failures that need attention (DB errors, Redis failures)

---

## 11. API Documentation

The API is documented with **OpenAPI 3.1** and rendered with **Scalar**. See [Section 5.4](#54-openapi--scalar--api-documentation) for details.

**Endpoints:**

| Method | Path                         | Description                      |
| ------ | ---------------------------- | -------------------------------- |
| GET    | `/ws`                        | WebSocket upgrade                |
| GET    | `/docs/openapi.html`         | Scalar interactive API docs      |
| GET    | `/docs/openapi.json`         | Raw OpenAPI spec                 |
| POST   | `/api/users`                 | Register user                    |
| GET    | `/api/users/:id`             | Get user                         |
| POST   | `/api/groups`                | Create group                     |
| POST   | `/api/groups/:id/members`    | Add member to group              |
| GET    | `/api/messages/:conversation`| Fetch message history            |
| GET    | `/health`                    | Health check                     |

WebSocket frame types are documented in [Section 14](#14-the-tick-system).

---

## 12. Message Lifecycle (End-to-End)

### 12.1 Sending a 1:1 Message (Recipient Online)

```
1. Alice's device generates client_id (UUID), constructs Envelope{type:"message"}
2. Alice's device sends frame over WebSocket to backend
3. Hub's readPump receives frame, parses Envelope
4. Hub inserts message to Postgres with state=SENT
5. Hub publishes Envelope to Redis channel: PUBLISH chat:{bob-id} <json>
6. Bob's subscriber goroutine receives from Redis channel
7. Router: hub.connMap[bob-id] exists -> push to bob.send channel
8. Bob's writePump writes frame to Bob's WebSocket connection
9. Bob's device receives message, immediately sends ACK frame back
10. Hub's readPump on Bob's connection receives ACK
11. Router transitions message state: DELIVERED
12. Router pushes StateUpdate{id, DELIVERED} to Alice's send channel
13. Alice's device receives StateUpdate -> renders double tick
14. Bob opens chat -> device sends read frame
15. Router transitions state: READ
16. Router pushes StateUpdate{id, READ} to Alice's send channel
17. Alice's device renders blue double tick
```

### 12.2 Sending a 1:1 Message (Recipient Offline)

```
1-5. Same as above
6. Bob's subscriber goroutine: no active subscriber (Bob is offline)
   -> message is dropped by Redis pub/sub
7. Router: hub.connMap[bob-id] does NOT exist
8. Router: LPUSH offline:{bob-id} <json>
9. EXPIRE offline:{bob-id} 30 days (refreshed on each push)

--- Hours later ---

10. Bob's device reconnects, HTTP upgrade succeeds
11. Hub registers new Client for Bob, pushes to register channel
12. Hub.Run() adds Bob to connMap
13. drainOfflineBuffer() goroutine starts
14. RPOP offline:{bob-id} -> get message (loop until empty)
15. Each message pushed to bob.send -> writePump delivers to device
16. Bob's device ACKs each message
17. State transitions to DELIVERED for each message
18. Alice receives StateUpdate -> double tick for each message
```

---

## 13. Connection State Management

Connection state management is the art of keeping the `connMap` correct at all times.

### The Problem

Every network connection can die in two ways:

1. **Clean close:** Device sends a WebSocket close frame. `conn.ReadMessage()` returns a `*websocket.CloseError`. The `readPump`'s defer runs, unregistering the client cleanly.

2. **Dirty close:** Network drops, device battery dies, process killed. No close frame is sent. `conn.ReadMessage()` will block forever — unless you use read deadlines.

### The Solution: Ping/Pong Heartbeat

```
Server sends Ping every 54 seconds
Client must respond with Pong within 6 seconds (read deadline = 60s)
If no Pong received within 60s -> ReadMessage returns timeout error -> unregister
```

### Race Condition: Concurrent Unregister

**Protection strategy:**

- `connMap` mutations happen only in the Hub's Run goroutine (via channels)
- `connMap` reads use `sync.RWMutex`
- After removing a client from `connMap`, `close(client.send)` signals the `writePump` to stop

### Goroutine Leak Prevention

Every goroutine spawned must have a clear exit condition:

| Goroutine            | Exit condition                                              |
| -------------------- | ----------------------------------------------------------- |
| `readPump`           | `conn.ReadMessage()` returns error -> defer unregisters     |
| `writePump`          | `c.send` channel closed -> returns                          |
| `subscribeLoop`      | `sub.Channel()` closed (on `sub.Close()` in readPump defer) |
| `drainOfflineBuffer` | List is empty (RPOP returns redis.Nil)                      |
| Fanout goroutine     | Finishes publish, wg.Done() called                          |

Run `go test -race ./...` regularly.

---

## 14. The Tick System

The tick system is a visual representation of the delivery state machine.

### State -> UI Mapping

| State         | Sender sees       | How triggered                                |
| ------------- | ----------------- | -------------------------------------------- |
| PENDING (0)   | Clock icon        | Before server ACK                            |
| SENT (1)      | Single grey tick  | Server inserts message to DB                 |
| DELIVERED (2) | Double grey tick  | Recipient device sends `ack` frame           |
| READ (3)      | Double blue tick  | Recipient sends `read` frame on opening chat |

### Wire Protocol

**Outgoing message (client -> server):**

```json
{
  "type": "message",
  "payload": {
    "client_id": "550e8400-e29b-41d4-a716-446655440000",
    "recipient_id": "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
    "body": "Hey, how are you?"
  }
}
```

**Server ACK back to sender (confirms SENT state):**

```json
{
  "type": "state_update",
  "payload": {
    "client_id": "550e8400-e29b-41d4-a716-446655440000",
    "message_id": "7f000001-7b0a-4000-a000-000000000001",
    "state": 1
  }
}
```

**Recipient ACK (device -> server, confirms DELIVERED):**

```json
{
  "type": "ack",
  "payload": {
    "message_id": "7f000001-7b0a-4000-a000-000000000001"
  }
}
```

**Read receipt (device -> server, confirms READ):**

```json
{
  "type": "read",
  "payload": {
    "message_ids": ["7f000001-...", "7f000002-..."]
  }
}
```

**State update pushed to original sender:**

```json
{
  "type": "state_update",
  "payload": {
    "message_id": "7f000001-7b0a-4000-a000-000000000001",
    "state": 3
  }
}
```

---

## 15. Group Messaging Deep-Dive

### How Group Double Tick Works Differently

For 1:1 messages, double tick means one person received it. For groups, WhatsApp shows double tick only when **all** group members have received it. The blue double tick appears only when **all** members have read it.

**Per-member state tracking:**

```sql
-- Each member has their own delivery state for each group message
CREATE TABLE group_message_delivery (
    message_id   UUID NOT NULL REFERENCES messages(id),
    member_id    UUID NOT NULL REFERENCES users(id),
    state        SMALLINT NOT NULL DEFAULT 1,
    PRIMARY KEY (message_id, member_id)
);
```

**Aggregate state computation:**

```go
func (r *Router) ComputeGroupState(ctx context.Context, msgID uuid.UUID) (model.DeliveryState, error) {
    states, err := r.pg.GetAllMemberStates(ctx, msgID)
    if err != nil {
        return model.StateSent, err
    }
    min := model.StateRead
    for _, s := range states {
        if s < min {
            min = s
        }
    }
    return min, nil
}
```

### Group Fanout Flow

```
Alice sends to Group G (members: Bob, Carol, Dave)

1. Server inserts message to Postgres (state=SENT, recipient=nil, group_id=G)
2. Insert per-member delivery rows: (msg, bob, SENT), (msg, carol, SENT), (msg, dave, SENT)
3. Fanout: spawn 3 goroutines
   |-- goroutine 1: PUBLISH chat:{bob}   -> Bob is online  -> push to Bob's writePump
   |-- goroutine 2: PUBLISH chat:{carol} -> Carol offline  -> LPUSH offline:{carol}
   +-- goroutine 3: PUBLISH chat:{dave}  -> Dave is online -> push to Dave's writePump

4. Bob's device ACKs  -> update (msg, bob, DELIVERED) -> aggregate: min(D,S,D)=S -> still SENT
5. Dave's device ACKs -> update (msg, dave, DELIVERED) -> aggregate: min(D,S,D)=S -> still SENT

--- Carol comes online ---

6. Carol reconnects -> drain offline buffer -> deliver msg -> Carol ACKs
7. Update (msg, carol, DELIVERED) -> aggregate: min(D,D,D)=D -> transition to DELIVERED
8. Push StateUpdate{DELIVERED} to Alice -> Alice sees double tick

--- All members open group chat ---

9. All members -> READ -> aggregate = READ -> push StateUpdate{READ} to Alice -> blue double tick
```

---

## 16. Build Order & Implementation Roadmap

Follow these steps in order. Each step produces something runnable.

### Step 1 — Tooling & Config

Set up `go.mod`, Taskfile, koanf config loading, zerolog logger. Verify `task build` compiles.

### Step 2 — Infrastructure

Write `docker-compose.yml` with Postgres and Redis. Write tern migrations. Run `task migrate`.

**Acceptance test:** `docker compose up`, `redis-cli ping` -> PONG, `psql` connects, `tern status` shows all migrations applied.

### Step 3 — Data Models (`internal/model`)

Write `message.go`, `group.go`, `user.go`. Define all structs, enums, and JSON tags.

**Acceptance test:** `go build ./...` passes.

### Step 4 — Repository Layer (`internal/repository`)

Implement Postgres queries and Redis operations. Pubsub, offline buffer, presence.

### Step 5 — WebSocket Hub (`internal/handler`)

Build the Hub struct, Run loop, Client struct, readPump, writePump, and the HTTP upgrade handler.

**Acceptance test:** Connect two browser tabs. Messages typed in one tab appear in server logs.

### Step 6 — Redis Pub/Sub Integration

Have each client's readPump publish to `chat:{recipientID}` via Redis. Have each client's subscribeLoop receive and push to the `send` channel.

**Acceptance test:** A message from User A appears in User B's tab in real time.

### Step 7 — Offline Buffer

Modify the router: check connMap -> if offline, LPUSH. Add drainOfflineBuffer on connect.

**Acceptance test:** Disconnect B. Send from A. Reconnect B. Message appears.

### Step 8 — Delivery State Machine

Add state tracking, ACK frame handling, StateUpdate push to sender.

**Acceptance test:** A sends -> sees single tick. B connects -> A sees double tick. B reads -> A sees blue.

### Step 9 — Group Fanout

Add fanout engine, per-member delivery state, aggregate computation.

**Acceptance test:** Group with 3 members. One offline. Verify ticks appear only after all members receive.

### Step 10 — API Docs

Write OpenAPI spec, add Scalar HTML, serve at `/docs`.

---

## 17. Key Go Patterns Used

### Pattern 1: Hub with central Run goroutine

Use channels for all hub mutations. Never lock the connMap for writes from outside the Run goroutine.

### Pattern 2: One goroutine per connection, communicate via channels

```go
go client.readPump()
go client.writePump()
go client.subscribeLoop()
```

### Pattern 3: context.Context for cancellation

Every long-running goroutine should accept a `ctx context.Context`. Use `context.WithCancel` to stop goroutines cleanly on shutdown.

### Pattern 4: select with default for non-blocking channel operations

```go
select {
case client.send <- data:
default:
    // channel full
}
```

### Pattern 5: sync.WaitGroup for goroutine fan-out

```go
var wg sync.WaitGroup
for _, member := range members {
    wg.Add(1)
    go func(id uuid.UUID) {
        defer wg.Done()
        // ... do work
    }(member.UserID)
}
wg.Wait()
```

---

## 18. Redis Command Reference

| Command                    | Usage in this project                              |
| -------------------------- | -------------------------------------------------- |
| `PUBLISH channel message`  | Route message to a user's channel                  |
| `SUBSCRIBE channel`        | Hub subscribes for each connected user             |
| `LPUSH key value`          | Push message to offline buffer (head of list)      |
| `RPOP key`                 | Pop oldest message from offline buffer (tail)      |
| `EXPIRE key seconds`       | Set TTL on offline buffer key                      |
| `HSET key field value`     | Store delivery state: `HSET state:{msgID} state 2` |
| `HGET key field`           | Read delivery state                                |
| `SET key value EX seconds` | Store fanout pending counter with TTL              |
| `DECR key`                 | Decrement fanout counter on each member ACK        |
| `DEL key`                  | Remove offline buffer after draining               |

---

## 19. Common Pitfalls & How to Avoid Them

### Pitfall 1: Concurrent writes to a WebSocket connection

**Symptom:** Garbled frames, `concurrent write to websocket connection` panic.
**Fix:** `writePump` is the ONLY writer. Always go through the `c.send` channel.

### Pitfall 2: Goroutine leak on client disconnect

**Symptom:** Memory grows over time. `runtime.NumGoroutine()` keeps increasing.
**Fix:** Ensure every goroutine started for a client is stopped when the client unregisters.

### Pitfall 3: Stale connMap entries

**Symptom:** Messages silently dropped.
**Fix:** Ping/pong heartbeat with 60s read deadline.

### Pitfall 4: Lost messages during fanout goroutine panic

**Symptom:** Some group members never receive a message.
**Fix:** Recover from panics in fanout goroutines.

### Pitfall 5: Duplicate message delivery on reconnect

**Symptom:** User sees the same message twice.
**Fix:** Idempotency check using `client_id`.

### Pitfall 6: Group message shows double tick prematurely

**Symptom:** Sender sees double tick but some members haven't received yet.
**Fix:** Use Redis counter pattern. Only transition to DELIVERED when counter reaches zero.

### Pitfall 7: Redis pub/sub message loss

**Symptom:** Message published but never received even though recipient is online.
**Cause:** Subscriber started after PUBLISH was sent.
**Fix:** Start subscribe goroutine before adding client to connMap.

---

## 20. Client Application

The frontend lives in `apps/client/`. Technology choice is flexible (React, plain HTML+JS, etc.). The client must implement:

- WebSocket connection management with automatic reconnect
- Envelope serialization/deserialization (JSON)
- ACK frame sending on message receipt
- Read receipt sending when a conversation is opened
- Tick UI rendering based on `state_update` frames
- Local dedup using `client_id` to prevent duplicate display on re-delivery

---

_This document covers the full architecture of the WhatsApp prototype. Follow the build order in Section 16 to implement it step by step._

|─────────────────────────────────────────┼────────────────────────────────────────────────────┤
  │ 01_project_setup.md                     │ Config, DB, Redis, Logger, Migrations, Air         │
  ├─────────────────────────────────────────┼────────────────────────────────────────────────────┤
  │ 02_http_server_and_user_registration.md │ HTTP server, graceful shutdown, user CRUD          │
  ├─────────────────────────────────────────┼────────────────────────────────────────────────────┤
  │ 03_websocket_first_message.md           │ First WS connection, naive 1:1 messaging           │
  ├─────────────────────────────────────────┼────────────────────────────────────────────────────┤
  │ 04_websocket_hub.md                     │ Hub pattern, CSP, ping/pong, goroutine lifecycle   │
  ├─────────────────────────────────────────┼────────────────────────────────────────────────────┤
  │ 05_delivery_states_and_ticks.md         │ State machine, ACK/read frames, tick system        │
  ├─────────────────────────────────────────┼────────────────────────────────────────────────────┤
  │ 06_redis_pubsub.md                      │ Pub/sub layer, decoupled routing                   │
  ├─────────────────────────────────────────┼────────────────────────────────────────────────────┤
  │ 07_offline_buffer.md                    │ Redis lists, store-and-forward, drain on reconnect │
  ├─────────────────────────────────────────┼────────────────────────────────────────────────────┤
  │ 08_groups_and_fanout.md                 │ Group creation, fanout engine, aggregate delivery  │
  ├─────────────────────────────────────────┼────────────────────────────────────────────────────┤                            
  │ 09_backpressure_and_resilience.md       │ Slow clients, channel overflow, recovery           │
  ├─────────────────────────────────────────┼────────────────────────────────────────────────────┤                            
  │ 10_message_history_api.md               │ REST endpoint for conversation history, pagination │
  ├─────────────────────────────────────────┼────────────────────────────────────────────────────┤                            
  │ 11_horizontal_scaling.md                │ Multi-node considerations, Redis as shared state   │
  ├─────────────────────────────────────────┼────────────────────────────────────────────────────┤                            
  │ 12_api_docs.md                          │ OpenAPI spec + Scalar UI                           │
  └─────────────────────────────────────────┴────────────────────────────────────────────────────┘