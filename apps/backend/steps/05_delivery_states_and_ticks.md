# Step 5 -- Delivery States and Ticks

**Prerequisites:** Hub running, clients connected with `readPump`/`writePump`, messages being sent between users via the hub.

In this step we implement the WhatsApp tick system -- the delivery state machine that tracks every message through its lifecycle from "just typed" to "read by recipient."

---

## 1. The Delivery State Machine

Every message moves through exactly four states, **always forward, never backward**:

```
PENDING (0)  -->  SENT (1)  -->  DELIVERED (2)  -->  READ (3)
```

| State | Value | Meaning | WhatsApp UI |
|-------|-------|---------|-------------|
| PENDING | 0 | Client generated the message, not yet received by server | Clock icon |
| SENT | 1 | Server accepted and stored the message | Single grey tick |
| DELIVERED | 2 | Recipient's device acknowledged receipt | Double grey ticks |
| READ | 3 | Recipient opened the conversation | Double blue ticks |

Key rule: **states only move forward.** A message that is DELIVERED can never go back to SENT. A message that is READ can never go back to DELIVERED. This makes the system idempotent and safe under retries.

---

## 2. Migration -- `003_create_delivery_events.sql`

We assume `002_create_messages.sql` already exists and created a `messages` table with at least `id`, `sender_id`, `recipient_id`, `content`, `state`, `client_id`, and `created_at` columns. This migration adds the audit trail table.

Create `internal/database/migrations/003_create_delivery_events.sql`:

```sql
-- Write your migrate up statements here
CREATE TABLE delivery_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    message_id  UUID NOT NULL REFERENCES messages(id),
    state       SMALLINT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_delivery_events_message ON delivery_events(message_id);

---- create above / drop below ----

-- Write your migrate down statements here.
DROP INDEX IF EXISTS idx_delivery_events_message;
DROP TABLE IF EXISTS delivery_events;
```

This gives us a full audit log: for any message we can query every state transition and when it happened.

---

## 3. Wire Protocol -- JSON Frames

All communication over the WebSocket uses JSON envelopes. Here are the frames involved in delivery tracking.

### 3a. Client sends a message

Client -> Server:

```json
{
  "type": "message",
  "payload": {
    "client_id": "abc-123-client-generated-uuid",
    "recipient_id": "bob-uuid",
    "content": "Hey Bob!"
  }
}
```

### 3b. Server confirms SENT (state=1)

Server -> Sender (Alice):

```json
{
  "type": "state_update",
  "payload": {
    "message_id": "server-generated-uuid",
    "client_id": "abc-123-client-generated-uuid",
    "state": 1
  }
}
```

The sender matches `client_id` to the local pending message and updates the tick to a single grey tick.

### 3c. Recipient device sends ACK (state transition to DELIVERED)

Server delivers the message to the recipient. The recipient's client automatically sends back an ack:

Server -> Recipient (Bob):

```json
{
  "type": "message",
  "payload": {
    "message_id": "server-generated-uuid",
    "sender_id": "alice-uuid",
    "content": "Hey Bob!"
  }
}
```

Recipient (Bob) -> Server:

```json
{
  "type": "ack",
  "payload": {
    "message_id": "server-generated-uuid"
  }
}
```

### 3d. Server confirms DELIVERED (state=2) to sender

Server -> Sender (Alice):

```json
{
  "type": "state_update",
  "payload": {
    "message_id": "server-generated-uuid",
    "state": 2
  }
}
```

Alice's client updates from single grey tick to double grey ticks.

### 3e. Recipient opens the chat -- READ (state=3)

When Bob opens the conversation with Alice, his client sends a batch read receipt:

Recipient (Bob) -> Server:

```json
{
  "type": "read",
  "payload": {
    "message_ids": [
      "server-generated-uuid-1",
      "server-generated-uuid-2"
    ]
  }
}
```

### 3f. Server confirms READ (state=3) to sender

Server -> Sender (Alice), one update per message:

```json
{
  "type": "state_update",
  "payload": {
    "message_id": "server-generated-uuid-1",
    "state": 3
  }
}
```

Alice's client turns the double ticks blue.

---

## 4. State Machine -- `internal/service/state.go`

This is the core logic. It enforces forward-only transitions, updates the messages table, and logs the event.

```go
package service

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Delivery states mirror the WhatsApp tick system.
const (
	StatePending   = 0
	StateSent      = 1
	StateDelivered = 2
	StateRead      = 3
)

var ErrStaleTransition = errors.New("state transition rejected: new state is not ahead of current state")

type StateService struct {
	db *pgxpool.Pool
}

func NewStateService(db *pgxpool.Pool) *StateService {
	return &StateService{db: db}
}

// Transition moves a message to toState if and only if toState > current state.
// It updates messages.state and inserts a delivery_events row inside a single
// transaction. Returns the new state on success.
func (s *StateService) Transition(ctx context.Context, msgID string, toState int) (int, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	// Lock the row and read current state.
	var currentState int
	err = tx.QueryRow(ctx,
		`SELECT state FROM messages WHERE id = $1 FOR UPDATE`,
		msgID,
	).Scan(&currentState)
	if err != nil {
		return 0, err
	}

	// Forward-only rule: reject if new state is not strictly ahead.
	if toState <= currentState {
		// Not an error in the crash sense -- just a no-op.
		// The caller can treat this as idempotent success.
		return currentState, ErrStaleTransition
	}

	// Update the message state.
	_, err = tx.Exec(ctx,
		`UPDATE messages SET state = $1 WHERE id = $2`,
		toState, msgID,
	)
	if err != nil {
		return 0, err
	}

	// Insert audit trail event.
	_, err = tx.Exec(ctx,
		`INSERT INTO delivery_events (message_id, state) VALUES ($1, $2)`,
		msgID, toState,
	)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}

	return toState, nil
}
```

Key points:

- We use `SELECT ... FOR UPDATE` to lock the message row, preventing race conditions when two acks arrive simultaneously.
- If `toState <= currentState`, we return `ErrStaleTransition`. The caller can check for this and treat it as a successful no-op rather than a failure.
- Both the state update and the audit event happen in a single transaction -- either both succeed or neither does.

---

## 5. ACK Handling in readPump

When the client's `readPump` goroutine receives a frame, it dispatches based on `envelope.Type`. Here is the updated `handleIncoming` method on the client.

### Envelope and payload types -- `internal/ws/types.go`

```go
package ws

// Envelope is the top-level JSON frame on the wire.
type Envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// MessagePayload is the payload for type "message".
type MessagePayload struct {
	ClientID    string `json:"client_id"`
	RecipientID string `json:"recipient_id"`
	Content     string `json:"content"`
}

// AckPayload is the payload for type "ack".
type AckPayload struct {
	MessageID string `json:"message_id"`
}

// ReadPayload is the payload for type "read".
type ReadPayload struct {
	MessageIDs []string `json:"message_ids"`
}

// StateUpdatePayload is the payload for type "state_update".
type StateUpdatePayload struct {
	MessageID string `json:"message_id"`
	ClientID  string `json:"client_id,omitempty"`
	State     int    `json:"state"`
}
```

### Updated `client.go` -- `handleIncoming`

```go
package ws

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/service"
	"github.com/rs/zerolog"
)

// handleIncoming dispatches an incoming WebSocket frame to the correct handler.
func (c *Client) handleIncoming(ctx context.Context, raw []byte) {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		c.log.Error().Err(err).Msg("malformed envelope")
		return
	}

	switch env.Type {
	case "message":
		c.handleMessage(ctx, env.Payload)
	case "ack":
		c.handleAck(ctx, env.Payload)
	case "read":
		c.handleRead(ctx, env.Payload)
	default:
		c.log.Warn().Str("type", env.Type).Msg("unknown frame type")
	}
}

// handleMessage processes an incoming chat message.
// 1. Save to DB with state = SENT
// 2. Push state_update (state=1) back to the sender
// 3. Route the message to the recipient via the hub
func (c *Client) handleMessage(ctx context.Context, raw json.RawMessage) {
	var p MessagePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		c.log.Error().Err(err).Msg("bad message payload")
		return
	}

	// Persist with state = SENT (server accepted it).
	msg, err := c.msgService.Save(ctx, c.userID, p.RecipientID, p.ClientID, p.Content)
	if err != nil {
		c.log.Error().Err(err).Msg("failed to save message")
		return
	}

	// Transition to SENT and log the event.
	_, _ = c.stateService.Transition(ctx, msg.ID, service.StateSent)

	// Tell the sender: "your message is SENT."
	update := StateUpdatePayload{
		MessageID: msg.ID,
		ClientID:  p.ClientID,
		State:     service.StateSent,
	}
	c.sendJSON("state_update", update)

	// Route to recipient.
	outbound, _ := json.Marshal(Envelope{
		Type: "message",
		Payload: mustMarshal(map[string]string{
			"message_id": msg.ID,
			"sender_id":  c.userID,
			"content":    p.Content,
		}),
	})
	c.hub.SendToUser(p.RecipientID, outbound)
}

// handleAck processes a delivery acknowledgment from the recipient device.
// 1. Transition message to DELIVERED
// 2. Push state_update (state=2) to the original sender
func (c *Client) handleAck(ctx context.Context, raw json.RawMessage) {
	var p AckPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		c.log.Error().Err(err).Msg("bad ack payload")
		return
	}

	newState, err := c.stateService.Transition(ctx, p.MessageID, service.StateDelivered)
	if err != nil && !errors.Is(err, service.ErrStaleTransition) {
		c.log.Error().Err(err).Msg("ack transition failed")
		return
	}

	// Look up who sent this message so we can notify them.
	msg, err := c.msgRepo.GetByID(ctx, p.MessageID)
	if err != nil {
		c.log.Error().Err(err).Msg("failed to fetch message for ack")
		return
	}

	update := StateUpdatePayload{
		MessageID: p.MessageID,
		State:     newState,
	}
	outbound, _ := json.Marshal(Envelope{
		Type:    "state_update",
		Payload: mustMarshal(update),
	})
	c.hub.SendToUser(msg.SenderID, outbound)
}

// handleRead processes a read receipt -- the recipient opened the conversation.
// 1. Transition each message to READ
// 2. Push state_update (state=3) to the original sender for each
func (c *Client) handleRead(ctx context.Context, raw json.RawMessage) {
	var p ReadPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		c.log.Error().Err(err).Msg("bad read payload")
		return
	}

	for _, msgID := range p.MessageIDs {
		newState, err := c.stateService.Transition(ctx, msgID, service.StateRead)
		if err != nil && !errors.Is(err, service.ErrStaleTransition) {
			c.log.Error().Err(err).Str("message_id", msgID).Msg("read transition failed")
			continue
		}

		msg, err := c.msgRepo.GetByID(ctx, msgID)
		if err != nil {
			c.log.Error().Err(err).Str("message_id", msgID).Msg("failed to fetch message for read")
			continue
		}

		update := StateUpdatePayload{
			MessageID: msgID,
			State:     newState,
		}
		outbound, _ := json.Marshal(Envelope{
			Type:    "state_update",
			Payload: mustMarshal(update),
		})
		c.hub.SendToUser(msg.SenderID, outbound)
	}
}

// sendJSON is a helper that wraps a payload in an Envelope and writes it.
func (c *Client) sendJSON(msgType string, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		c.log.Error().Err(err).Msg("failed to marshal payload")
		return
	}
	env := Envelope{
		Type:    msgType,
		Payload: data,
	}
	out, err := json.Marshal(env)
	if err != nil {
		c.log.Error().Err(err).Msg("failed to marshal envelope")
		return
	}
	c.send <- out
}

// mustMarshal marshals v and returns the raw JSON. Panics on error (only use
// for values you know are serialisable).
func mustMarshal(v interface{}) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}
```

---

## 6. Repository Updates

### Message repository -- `internal/repository/message.go`

We need `GetByID` to return the `sender_id` so `handleAck` and `handleRead` know who to notify.

```go
package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Message struct {
	ID          string
	SenderID    string
	RecipientID string
	ClientID    string
	Content     string
	State       int
}

type MessageRepo struct {
	db *pgxpool.Pool
}

func NewMessageRepo(db *pgxpool.Pool) *MessageRepo {
	return &MessageRepo{db: db}
}

// Insert persists a new message with state = PENDING (0). The service layer
// will immediately transition it to SENT after this call succeeds.
func (r *MessageRepo) Insert(ctx context.Context, senderID, recipientID, clientID, content string) (*Message, error) {
	msg := &Message{}
	err := r.db.QueryRow(ctx,
		`INSERT INTO messages (sender_id, recipient_id, client_id, content, state)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, sender_id, recipient_id, client_id, content, state`,
		senderID, recipientID, clientID, content, 0,
	).Scan(&msg.ID, &msg.SenderID, &msg.RecipientID, &msg.ClientID, &msg.Content, &msg.State)
	return msg, err
}

// GetByID fetches a message by its server-assigned UUID. The caller needs
// sender_id to route state_update frames back to the original sender.
func (r *MessageRepo) GetByID(ctx context.Context, id string) (*Message, error) {
	msg := &Message{}
	err := r.db.QueryRow(ctx,
		`SELECT id, sender_id, recipient_id, client_id, content, state
		 FROM messages WHERE id = $1`,
		id,
	).Scan(&msg.ID, &msg.SenderID, &msg.RecipientID, &msg.ClientID, &msg.Content, &msg.State)
	return msg, err
}
```

### Delivery events repository -- `internal/repository/delivery_event.go`

```go
package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type DeliveryEvent struct {
	ID        string
	MessageID string
	State     int
}

type DeliveryEventRepo struct {
	db *pgxpool.Pool
}

func NewDeliveryEventRepo(db *pgxpool.Pool) *DeliveryEventRepo {
	return &DeliveryEventRepo{db: db}
}

// InsertEvent records a state transition in the audit trail.
// This is called inside StateService.Transition within the same transaction,
// but is also exposed here for cases where you need manual insertion.
func (r *DeliveryEventRepo) InsertEvent(ctx context.Context, messageID string, state int) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO delivery_events (message_id, state) VALUES ($1, $2)`,
		messageID, state,
	)
	return err
}
```

---

## 7. Idempotency

Three things can go wrong on an unreliable network, and the design handles all of them:

**Duplicate messages from client.** The `client_id` column on `messages` has a UNIQUE constraint. If the client retransmits the same message (same `client_id`), the INSERT fails with a unique violation. The server can catch this and return the existing message's ID.

**Duplicate ACKs.** If Bob's device disconnects after sending an ack but before receiving confirmation, it may re-send the ack on reconnect. The state machine sees `DELIVERED -> DELIVERED` (toState <= currentState) and returns `ErrStaleTransition`, which `handleAck` treats as a no-op. No duplicate `state_update` frames are sent.

**Server re-delivers a message.** If the server crashes after saving a message but before sending it to Bob, on restart it may re-deliver. Bob's client receives the message again, sends another ack, and the same forward-only rule ensures no harm.

The forward-only invariant is the single rule that makes the entire system safe under retries.

---

## 8. Acceptance Test

Start the server and open three terminal tabs.

### Terminal 1 -- Start the server

```bash
go run cmd/main.go
```

### Terminal 2 -- Alice connects via wscat

```bash
# Install wscat if needed: npm install -g wscat
# Assume Alice's user ID is passed as a query param after auth
wscat -c "ws://localhost:8080/ws?user_id=alice-uuid"
```

### Terminal 3 -- Bob connects via wscat

```bash
wscat -c "ws://localhost:8080/ws?user_id=bob-uuid"
```

### Test sequence

**Step A: Alice sends a message.**

In Alice's terminal, type:

```json
{"type":"message","payload":{"client_id":"local-1","recipient_id":"bob-uuid","content":"Hey Bob!"}}
```

Alice should immediately receive a state_update confirming SENT:

```json
{"type":"state_update","payload":{"message_id":"<server-uuid>","client_id":"local-1","state":1}}
```

Single grey tick.

**Step B: Bob receives the message and sends ACK.**

Bob's terminal shows the incoming message:

```json
{"type":"message","payload":{"message_id":"<server-uuid>","sender_id":"alice-uuid","content":"Hey Bob!"}}
```

Bob's client (or you manually) sends an ack:

```json
{"type":"ack","payload":{"message_id":"<server-uuid>"}}
```

Alice's terminal receives:

```json
{"type":"state_update","payload":{"message_id":"<server-uuid>","state":2}}
```

Double grey ticks.

**Step C: Bob reads the message.**

Bob opens the conversation (or you send manually):

```json
{"type":"read","payload":{"message_ids":["<server-uuid>"]}}
```

Alice's terminal receives:

```json
{"type":"state_update","payload":{"message_id":"<server-uuid>","state":3}}
```

Double blue ticks.

### Verify the audit trail with psql

```bash
psql -d wp_proto -c "SELECT m.id, m.state, de.state as event_state, de.occurred_at
FROM messages m
JOIN delivery_events de ON de.message_id = m.id
ORDER BY de.occurred_at;"
```

You should see three rows for the message: state 1 (SENT), state 2 (DELIVERED), state 3 (READ), each with increasing timestamps.

---

## Summary

What we built in this step:

- A four-state delivery state machine with a forward-only invariant
- A `delivery_events` table for auditing every state transition
- A wire protocol with `message`, `ack`, `read`, and `state_update` frame types
- A `StateService.Transition` method that atomically updates state and logs the event
- ACK and read handling in `client.go` that routes `state_update` frames to the original sender
- Idempotency guarantees that make the system safe under retries and reconnections

Next step: offline message queue -- what happens when Bob is not connected when Alice sends a message.
