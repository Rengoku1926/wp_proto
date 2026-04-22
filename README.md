# WhatsApp-Protocol Messaging Backend

A production-grade real-time messaging backend in **Go**, built from scratch to implement the core mechanics behind a messaging system like WhatsApp — WebSocket connections, pub/sub routing, offline message buffering, delivery state tracking, and group fanout.

---

## Demo

https://github.com/user-attachments/assets/f0fc6220-8eb5-4176-a998-86562767db24

## Architecture

![System Architecture](diagram-export-22-04-2026-00_49_25.svg)



---

## What This Is

A backend-only implementation of the real-time messaging protocol, focusing on correctness and systems design over UI. The goal was to understand and implement the exact mechanics that make a chat system reliable at the infrastructure level — not just "send and receive messages", but:

- Guaranteed delivery when recipients are offline
- Multi-server routing via Redis pub/sub
- Correct delivery state progression (Pending → Sent → Delivered → Read)
- Group message fanout with per-member delivery tracking
- Aggregate state rollup for group messages (sender sees the minimum across all members)

---

## Stack

| Layer            | Technology                       |
| ---------------- | -------------------------------- |
| Language         | Go 1.25                          |
| HTTP / WebSocket | `net/http` + `gorilla/websocket` |
| Database         | PostgreSQL (via `pgx/v5`)        |
| Cache / Broker   | Redis (`go-redis/v9`)            |
| Migrations       | `jackc/tern`                     |
| Logging          | `zerolog`                        |

---

## Core Design Concepts

### 1. WebSocket Hub — Single-Writer Concurrency Model

The `Hub` is the in-memory registry of all connected clients (`userID → *Client`). It follows a strict single-writer pattern: only the `Hub.Run()` goroutine mutates the `clients` map, receiving register/unregister events over channels. External readers acquire an `sync.RWMutex` read lock. This eliminates map races without lock contention on the hot path.

Each `Client` has two goroutines — `readPump` (inbound frames) and `writePump` (outbound frames) — plus a `subscribeLoop` goroutine listening on its personal Redis channel.

```
Client connects
  └─ readPump goroutine    ← reads WebSocket frames, dispatches handleIncoming
  └─ writePump goroutine   ← drains send channel, writes WebSocket frames
  └─ subscribeLoop         ← listens on Redis channel "chat:<userID>", pushes to send channel
```

### 2. Redis Pub/Sub — Intra-Server and Cross-Server Routing

Every connected client subscribes to a personal Redis channel `chat:<userID>` at connection time. When a message needs to be delivered to a recipient, the sender's goroutine calls `PubSubRepo.Publish(recipientID, payload)` — Redis fans this out to whichever server instance holds that user's WebSocket connection.

This decouples message routing from which server the recipient is connected to, enabling horizontal scaling with zero coordination between instances.

```
Sender → handleMessage → msgRepo.Create → PubSubRepo.Publish(recipientID)
                                                    ↓
                                          Redis channel: chat:<recipientID>
                                                    ↓
                                     subscribeLoop on recipient's server
                                                    ↓
                                          client.send channel → writePump → WebSocket
```

### 3. Offline Buffer — Redis List as a Durable Queue

If a recipient is offline at the time of delivery, the message is pushed to a Redis list keyed `offline:<userID>` with a 30-day TTL. The list is treated as a FIFO queue: `LPUSH` on write, `RPOP` on drain (oldest messages delivered first).

When a client reconnects and is registered in the Hub, `drainOfflineBuffer()` fires in a goroutine — it atomically pops all buffered messages and enqueues them on the client's `send` channel before any new real-time traffic arrives.

```
Sender → IsOnline? No → OfflineStore.Push (LPUSH offline:<recipientID>)

Recipient reconnects
  └─ Hub.register
  └─ go client.drainOfflineBuffer()
       └─ OfflineStore.Drain (RPOP loop until nil)
       └─ push each message to client.send channel
```

The `Push` uses a Redis pipeline (`LPUSH` + `EXPIRE`) to keep the TTL refresh atomic.

### 4. Delivery State Machine

Every message has a state column in Postgres: `0=Pending → 1=Sent → 2=Delivered → 3=Read`. Transitions are enforced by `StateService.Transition` inside a serializable transaction with `SELECT ... FOR UPDATE` to prevent concurrent backward transitions.

Each transition also writes an immutable row to `delivery_events`, giving a full audit trail of when each state change occurred.

```sql
-- delivery_events: append-only event log
message_id | state | created_at
```

State updates are pushed back to the sender in real-time via the same pub/sub channel.

### 5. Group Messages — Fanout on Write with Per-Member Delivery Tracking

Group messaging uses a **fanout-on-write** pattern. When a sender sends a message to a group, the `FanoutEngine` immediately resolves all group members from Postgres, inserts per-member delivery rows into `group_message_delivery`, and spawns a goroutine per recipient to deliver concurrently.

```
Sender → handleMessage (group_id present)
  └─ msgRepo.Create
  └─ stateService.Transition(SENT)
  └─ FanoutEngine.Fanout(senderID, messageID, groupID, content)
       └─ GroupRepo.GetMembers(groupID)          ← Postgres
       └─ GroupDeliveryRepo.InsertDeliveryRows   ← one row per member
       └─ RDB.Set(fanout:pending:<msgID>, count) ← Redis counter
       └─ for each recipient (parallel goroutines):
            IsOnline? → PubSubRepo.Publish       ← real-time
                      → OfflineStore.Push        ← or offline buffer
```

**Aggregate State Rollup via Redis Counter**

Each per-member ACK or read receipt calls `FanoutEngine.HandleMemberACK`. A Redis counter (`fanout:pending:<messageID>`) is decremented atomically on each ACK. When it reaches zero, all members have responded at the current state level — the engine queries Postgres for the minimum member state, updates the message aggregate, and publishes a `state_update` to the sender.

```
Member ACKs → GroupDeliveryRepo.UpdateMemberState (Postgres)
            → DECR fanout:pending:<msgID> (Redis)
            → if remaining == 0:
                 ComputeAggregateState (MIN across members)
                 MsgRepo.UpdateState
                 PubSubRepo.Publish(senderID, state_update)
                 Reset counter for next state level
```

This means the sender sees a single aggregate tick (delivered/read) reflecting the worst-off member — matching the semantics of real messaging apps.

---

## Database Schema

```
users                    — accounts (UUID primary key)
messages                 — all messages, state column tracks delivery
delivery_events          — append-only log of state transitions
groups                   — group metadata
group_members            — membership join table
group_message_delivery   — per-member delivery state for group messages
```

Migrations are managed with `tern`, applied at startup via `database.RunMigrations`.

---

## Wire Protocol

All WebSocket frames are JSON envelopes:

```json
{ "type": "<frame_type>", "payload": { ... } }
```

| Frame Type     | Direction       | Purpose                                |
| -------------- | --------------- | -------------------------------------- |
| `message`      | client → server | Send a message (1:1 or group)          |
| `message`      | server → client | Deliver an incoming message            |
| `ack`          | client → server | Recipient acknowledges delivery        |
| `read`         | client → server | Recipient marks messages as read       |
| `state_update` | server → client | Notify sender of delivery state change |

---

## Key Engineering Decisions

**Why fanout-on-write for groups?** Fanout-on-read would require scanning all members at read time. Fanout-on-write does the work once at send time and each member gets a personal copy in their offline buffer if offline. For this use case (small-to-medium groups), write amplification is acceptable for read simplicity.

**Why a Redis counter instead of a Postgres COUNT query per ACK?** A counter decrement on each ACK is O(1) and avoids a Postgres round-trip per member acknowledgement. The expensive `MIN` aggregation only runs once when the counter hits zero.

**Why single-writer Hub?** Avoiding lock contention on the write path. The `clients` map is mutated exclusively by the `Run()` goroutine via channels; external goroutines only read via `RLock`. This is safer and faster than a full mutex on every send.

**Why Redis for offline buffers instead of Postgres?** Redis lists give O(1) push/pop with configurable TTL. For short-lived offline buffers (minutes to hours), Redis is the right primitive. Postgres would add unnecessary write amplification to the critical delivery path.
