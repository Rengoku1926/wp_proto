# Step 8 -- Group Messaging and Fanout

Module: `github.com/Rengoku1926/wp_proto/apps/backend`

**Prerequisites:** 1:1 messaging with Hub, pub/sub, offline buffer, delivery states (SENT/DELIVERED/READ) all working from previous steps.

---

## 1. Groups Theory

### Fanout on write

In 1:1 messaging, one message produces one PUBLISH to `chat:{recipientID}`. Group messaging uses **fanout on write**: one incoming message produces N publishes, one per group member (excluding the sender).

```
User A sends to Group G (members: A, B, C, D)

                         +-- PUBLISH chat:B  (message copy)
                         |
Message from A  ---------+-- PUBLISH chat:C  (message copy)
                         |
                         +-- PUBLISH chat:D  (message copy)
```

The alternative is fanout on read (store once, each client fetches on demand), but fanout on write is what WhatsApp uses. It is simpler for real-time delivery and lets each member's offline buffer work identically to 1:1 messages.

### Group tick system

In 1:1 messaging, state transitions are straightforward: one recipient ACKs and the sender sees the tick change immediately. Groups are different:

| Tick State                    | 1:1 Rule                    | Group Rule                  |
| ----------------------------- | --------------------------- | --------------------------- |
| Single grey tick (SENT)       | Server accepted the message | Server accepted the message |
| Double grey ticks (DELIVERED) | Recipient received it       | **ALL** members received it |
| Double blue ticks (READ)      | Recipient read it           | **ALL** members read it     |

This means if a group has 10 members and 9 have received the message but 1 is offline, the sender still sees a single tick. The aggregate state is the **minimum** across all members.

### Per-member state tracking

To compute the aggregate, we need to track each member's individual state. This is what the `group_message_delivery` table does: one row per (message, member) pair, each with its own state value.

```
group_message_delivery:
+-----------+-----------+-------+
| message_id| member_id | state |
+-----------+-----------+-------+
| msg-001   | user-B    | 2     |  (DELIVERED)
| msg-001   | user-C    | 1     |  (SENT)
| msg-001   | user-D    | 3     |  (READ)
+-----------+-----------+-------+

Aggregate state = MIN(2, 1, 3) = 1 (SENT)
Sender sees: single grey tick
```

When user-C finally receives the message (state goes to 2), the minimum becomes 2, and the sender gets double grey ticks.

---

## 2. Migrations

### `internal/database/migrations/004_create_groups.sql`

```sql
-- Write your migrate up statements here
CREATE TABLE groups (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE group_members (
    group_id UUID NOT NULL REFERENCES groups(id),
    user_id UUID NOT NULL REFERENCES users(id),
    joined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, user_id)
);

CREATE INDEX idx_group_members_group ON group_members(group_id);
CREATE INDEX idx_group_members_user ON group_members(user_id);

---- create above / drop below ----

-- Write your migrate down statements here.
DROP INDEX IF EXISTS idx_group_members_user;
DROP INDEX IF EXISTS idx_group_members_group;
DROP TABLE IF EXISTS group_members;
DROP TABLE IF EXISTS groups;
```

### `internal/database/migrations/005_create_group_message_delivery.sql`

```sql
-- Write your migrate up statements here
CREATE TABLE group_message_delivery (
    message_id UUID NOT NULL REFERENCES messages(id),
    member_id UUID NOT NULL REFERENCES users(id),
    state SMALLINT NOT NULL DEFAULT 1,
    PRIMARY KEY (message_id, member_id)
);

---- create above / drop below ----

-- Write your migrate down statements here.
DROP TABLE IF EXISTS group_message_delivery;
```

The `state` column uses the same SMALLINT values as the messages table: 0=PENDING, 1=SENT, 2=DELIVERED, 3=READ. The default is 1 (SENT) because by the time we insert delivery rows, the server has already accepted the message.

---

## 3. Group Model -- `internal/model/group.go`

```go
// internal/model/group.go
package model

import (
	"time"

	"github.com/google/uuid"
)

// Group represents a chat group.
type Group struct {
	ID        uuid.UUID `json:"id" db:"id"`
	Name      string    `json:"name" db:"name"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
}

// Member represents a user's membership in a group.
type Member struct {
	GroupID  uuid.UUID `json:"group_id" db:"group_id"`
	UserID   uuid.UUID `json:"user_id" db:"user_id"`
	JoinedAt time.Time `json:"joined_at" db:"joined_at"`
}

// DeliveryState represents one member's delivery state for a group message.
type DeliveryState struct {
	MessageID uuid.UUID `json:"message_id" db:"message_id"`
	MemberID  uuid.UUID `json:"member_id" db:"member_id"`
	State     int16     `json:"state" db:"state"`
}
```

`DeliveryState` maps directly to a row in `group_message_delivery`. The `State` field uses the same numeric values as the message state machine (1=SENT, 2=DELIVERED, 3=READ).

---

## 4. Group Repository -- `internal/repository/group.go`

```go
// internal/repository/group.go
package repository

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
)

// GroupRepo handles group and membership operations in Postgres.
type GroupRepo struct {
	pool *pgxpool.Pool
}

// NewGroupRepo creates a new GroupRepo.
func NewGroupRepo(pool *pgxpool.Pool) *GroupRepo {
	return &GroupRepo{pool: pool}
}

// Create creates a new group and returns it.
func (r *GroupRepo) Create(ctx context.Context, name string) (*model.Group, error) {
	g := &model.Group{}
	err := r.pool.QueryRow(ctx,
		`INSERT INTO groups (name) VALUES ($1)
		 RETURNING id, name, created_at`,
		name,
	).Scan(&g.ID, &g.Name, &g.CreatedAt)
	if err != nil {
		return nil, err
	}
	return g, nil
}

// AddMember adds a user to a group.
func (r *GroupRepo) AddMember(ctx context.Context, groupID, userID uuid.UUID) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO group_members (group_id, user_id)
		 VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`,
		groupID, userID,
	)
	return err
}

// RemoveMember removes a user from a group.
func (r *GroupRepo) RemoveMember(ctx context.Context, groupID, userID uuid.UUID) error {
	_, err := r.pool.Exec(ctx,
		`DELETE FROM group_members
		 WHERE group_id = $1 AND user_id = $2`,
		groupID, userID,
	)
	return err
}

// GetMembers returns all members of a group.
func (r *GroupRepo) GetMembers(ctx context.Context, groupID uuid.UUID) ([]model.Member, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT group_id, user_id, joined_at
		 FROM group_members
		 WHERE group_id = $1`,
		groupID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var members []model.Member
	for rows.Next() {
		var m model.Member
		if err := rows.Scan(&m.GroupID, &m.UserID, &m.JoinedAt); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// GetGroupsForUser returns all groups a user belongs to.
func (r *GroupRepo) GetGroupsForUser(ctx context.Context, userID uuid.UUID) ([]model.Group, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT g.id, g.name, g.created_at
		 FROM groups g
		 JOIN group_members gm ON g.id = gm.group_id
		 WHERE gm.user_id = $1
		 ORDER BY g.created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []model.Group
	for rows.Next() {
		var g model.Group
		if err := rows.Scan(&g.ID, &g.Name, &g.CreatedAt); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}
```

`ON CONFLICT DO NOTHING` on `AddMember` makes the operation idempotent. Adding the same user twice is a no-op.

---

## 5. Group Message Delivery Repository -- `internal/repository/group_delivery.go`

```go
// internal/repository/group_delivery.go
package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
)

// GroupDeliveryRepo tracks per-member delivery state for group messages.
type GroupDeliveryRepo struct {
	pool *pgxpool.Pool
}

// NewGroupDeliveryRepo creates a new GroupDeliveryRepo.
func NewGroupDeliveryRepo(pool *pgxpool.Pool) *GroupDeliveryRepo {
	return &GroupDeliveryRepo{pool: pool}
}

// InsertDeliveryRows creates one delivery row per member in a single bulk INSERT.
// All rows start with state=1 (SENT).
func (r *GroupDeliveryRepo) InsertDeliveryRows(ctx context.Context, messageID uuid.UUID, memberIDs []uuid.UUID) error {
	if len(memberIDs) == 0 {
		return nil
	}

	// Build a bulk INSERT: INSERT INTO ... VALUES ($1,$2), ($1,$3), ($1,$4), ...
	// $1 is always the messageID, $2..$N+1 are the memberIDs.
	valueStrings := make([]string, len(memberIDs))
	args := make([]interface{}, 0, 1+len(memberIDs))
	args = append(args, messageID) // $1

	for i, memberID := range memberIDs {
		paramIdx := i + 2 // $2, $3, $4, ...
		valueStrings[i] = fmt.Sprintf("($1, $%d, 1)", paramIdx)
		args = append(args, memberID)
	}

	query := fmt.Sprintf(
		`INSERT INTO group_message_delivery (message_id, member_id, state)
		 VALUES %s
		 ON CONFLICT DO NOTHING`,
		strings.Join(valueStrings, ", "),
	)

	_, err := r.pool.Exec(ctx, query, args...)
	return err
}

// UpdateMemberState updates the delivery state for a specific member.
// State can only move forward (higher value), never backward.
func (r *GroupDeliveryRepo) UpdateMemberState(ctx context.Context, messageID, memberID uuid.UUID, state int16) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE group_message_delivery
		 SET state = $3
		 WHERE message_id = $1 AND member_id = $2 AND state < $3`,
		messageID, memberID, state,
	)
	return err
}

// GetAllMemberStates returns the delivery state for every member of a message.
func (r *GroupDeliveryRepo) GetAllMemberStates(ctx context.Context, messageID uuid.UUID) ([]model.DeliveryState, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT message_id, member_id, state
		 FROM group_message_delivery
		 WHERE message_id = $1`,
		messageID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var states []model.DeliveryState
	for rows.Next() {
		var ds model.DeliveryState
		if err := rows.Scan(&ds.MessageID, &ds.MemberID, &ds.State); err != nil {
			return nil, err
		}
		states = append(states, ds)
	}
	return states, rows.Err()
}

// ComputeAggregateState returns the minimum state across all members for a message.
// This is the state the sender should see.
func (r *GroupDeliveryRepo) ComputeAggregateState(ctx context.Context, messageID uuid.UUID) (int16, error) {
	var minState int16
	err := r.pool.QueryRow(ctx,
		`SELECT COALESCE(MIN(state), 1)
		 FROM group_message_delivery
		 WHERE message_id = $1`,
		messageID,
	).Scan(&minState)
	return minState, err
}
```

Key design choices:

- **Bulk insert** uses a dynamically built `VALUES` clause. For a group of 50 members, this is one INSERT with 50 value tuples instead of 50 separate INSERT statements.
- **Forward-only state updates** via `WHERE state < $3`. If a member's state is already DELIVERED (2) and we receive a duplicate DELIVERED ACK, the UPDATE is a no-op. This matches the same idempotency rule from 1:1 messaging.
- **Aggregate is MIN**, not MAX or AVG. The sender only sees the double tick when the slowest member has reached that state.

---

## 6. Group Handler -- `internal/handler/group.go`

```go
// internal/handler/group.go
package handler

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/service"
	"github.com/rs/zerolog/log"
)

// GroupHandler handles HTTP endpoints for group management.
type GroupHandler struct {
	svc *service.GroupService
}

// NewGroupHandler creates a new GroupHandler.
func NewGroupHandler(svc *service.GroupService) *GroupHandler {
	return &GroupHandler{svc: svc}
}

// CreateGroup handles POST /api/groups.
//
// Request body: {"name": "My Group"}
// Response: the created group object.
func (h *GroupHandler) CreateGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	group, err := h.svc.CreateGroup(r.Context(), req.Name)
	if err != nil {
		log.Error().Err(err).Msg("failed to create group")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(group)
}

// AddMember handles POST /api/groups/{id}/members.
//
// Request body: {"user_id": "uuid-here"}
func (h *GroupHandler) AddMember(w http.ResponseWriter, r *http.Request) {
	groupIDStr := r.PathValue("id")
	groupID, err := uuid.Parse(groupIDStr)
	if err != nil {
		http.Error(w, "invalid group id", http.StatusBadRequest)
		return
	}

	var req struct {
		UserID uuid.UUID `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := h.svc.AddMember(r.Context(), groupID, req.UserID); err != nil {
		log.Error().Err(err).Msg("failed to add member")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ListMembers handles GET /api/groups/{id}/members.
func (h *GroupHandler) ListMembers(w http.ResponseWriter, r *http.Request) {
	groupIDStr := r.PathValue("id")
	groupID, err := uuid.Parse(groupIDStr)
	if err != nil {
		http.Error(w, "invalid group id", http.StatusBadRequest)
		return
	}

	members, err := h.svc.ListMembers(r.Context(), groupID)
	if err != nil {
		log.Error().Err(err).Msg("failed to list members")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(members)
}
```

We use `r.PathValue("id")` from Go 1.22's enhanced `http.ServeMux` routing. The routes are registered in the server setup:

```go
mux.HandleFunc("POST /api/groups", groupHandler.CreateGroup)
mux.HandleFunc("POST /api/groups/{id}/members", groupHandler.AddMember)
mux.HandleFunc("GET /api/groups/{id}/members", groupHandler.ListMembers)
```

---

## 7. Group Service -- `internal/service/group.go`

```go
// internal/service/group.go
package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
)

// GroupService contains business logic for group operations.
type GroupService struct {
	groupRepo *repository.GroupRepo
}

// NewGroupService creates a new GroupService.
func NewGroupService(groupRepo *repository.GroupRepo) *GroupService {
	return &GroupService{groupRepo: groupRepo}
}

// CreateGroup creates a new group.
func (s *GroupService) CreateGroup(ctx context.Context, name string) (*model.Group, error) {
	return s.groupRepo.Create(ctx, name)
}

// AddMember adds a user to a group.
func (s *GroupService) AddMember(ctx context.Context, groupID, userID uuid.UUID) error {
	return s.groupRepo.AddMember(ctx, groupID, userID)
}

// ListMembers returns all members of a group.
func (s *GroupService) ListMembers(ctx context.Context, groupID uuid.UUID) ([]model.Member, error) {
	return s.groupRepo.GetMembers(ctx, groupID)
}
```

The service layer is thin for now. As group logic grows (permissions, admin roles, member limits), this is where those rules live -- not in the handler, not in the repository.

---

## 8. Fanout Engine -- `internal/router/fanout.go`

This is the core of group messaging. The fanout engine takes one incoming group message and distributes it to every member.

```go
// internal/router/fanout.go
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
)

const (
	// pendingKeyTTL is how long the fanout pending counter lives in Redis.
	// 7 days gives plenty of time for offline members to come back.
	pendingKeyTTL = 7 * 24 * time.Hour
)

// pendingKey returns the Redis key for tracking remaining ACKs.
func pendingKey(messageID string) string {
	return fmt.Sprintf("fanout:pending:%s", messageID)
}

// FanoutEngine handles group message distribution.
type FanoutEngine struct {
	Hub              *handler.Hub
	PubSubRepo       *repository.PubSubRepo
	MsgRepo          *repository.MessageRepo
	GroupRepo        *repository.GroupRepo
	GroupDeliveryRepo *repository.GroupDeliveryRepo
	RDB              *redis.Client
}

// NewFanoutEngine creates a new FanoutEngine.
func NewFanoutEngine(
	hub *handler.Hub,
	pubsubRepo *repository.PubSubRepo,
	msgRepo *repository.MessageRepo,
	groupRepo *repository.GroupRepo,
	groupDeliveryRepo *repository.GroupDeliveryRepo,
	rdb *redis.Client,
) *FanoutEngine {
	return &FanoutEngine{
		Hub:              hub,
		PubSubRepo:       pubsubRepo,
		MsgRepo:          msgRepo,
		GroupRepo:        groupRepo,
		GroupDeliveryRepo: groupDeliveryRepo,
		RDB:              rdb,
	}
}

// Fanout distributes a group message to all members except the sender.
//
// Flow:
//  1. Fetch group members from Postgres.
//  2. Filter out the sender.
//  3. Insert per-member delivery rows (state=SENT).
//  4. Store pending counter in Redis for ACK tracking.
//  5. PUBLISH to each member's Redis channel concurrently.
func (f *FanoutEngine) Fanout(ctx context.Context, senderID string, messageID string, groupID string, content string) error {
	gid, err := uuid.Parse(groupID)
	if err != nil {
		return fmt.Errorf("invalid group ID: %w", err)
	}

	// 1. Fetch group members.
	members, err := f.GroupRepo.GetMembers(ctx, gid)
	if err != nil {
		return fmt.Errorf("failed to get group members: %w", err)
	}

	// 2. Filter out the sender and collect recipient IDs.
	var recipientIDs []uuid.UUID
	for _, m := range members {
		if m.UserID.String() != senderID {
			recipientIDs = append(recipientIDs, m.UserID)
		}
	}

	if len(recipientIDs) == 0 {
		log.Warn().Str("group", groupID).Msg("no recipients in group")
		return nil
	}

	msgID, err := uuid.Parse(messageID)
	if err != nil {
		return fmt.Errorf("invalid message ID: %w", err)
	}

	// 3. Insert per-member delivery rows.
	if err := f.GroupDeliveryRepo.InsertDeliveryRows(ctx, msgID, recipientIDs); err != nil {
		return fmt.Errorf("failed to insert delivery rows: %w", err)
	}

	// 4. Store pending counter in Redis.
	// The counter starts at len(recipientIDs). Each ACK decrements it.
	// When it hits 0, all members have reached that state.
	pk := pendingKey(messageID)
	if err := f.RDB.Set(ctx, pk, len(recipientIDs), pendingKeyTTL).Err(); err != nil {
		return fmt.Errorf("failed to set pending counter: %w", err)
	}

	// 5. PUBLISH to each member concurrently.
	outgoing, _ := json.Marshal(map[string]interface{}{
		"type": "message",
		"data": map[string]string{
			"id":        messageID,
			"sender_id": senderID,
			"group_id":  groupID,
			"content":   content,
		},
	})

	var wg sync.WaitGroup
	for _, recipientID := range recipientIDs {
		wg.Add(1)
		go func(rid uuid.UUID) {
			defer wg.Done()
			if err := f.PubSubRepo.Publish(ctx, rid.String(), outgoing); err != nil {
				log.Error().Err(err).
					Str("message", messageID).
					Str("recipient", rid.String()).
					Msg("failed to publish group message to member")
			}
		}(recipientID)
	}
	wg.Wait()

	log.Info().
		Str("message", messageID).
		Str("group", groupID).
		Int("recipients", len(recipientIDs)).
		Msg("fanout complete")

	return nil
}

// HandleMemberACK processes a delivery/read ACK from one group member.
//
// Flow:
//  1. Update this member's state in group_message_delivery.
//  2. Decrement the pending counter in Redis.
//  3. If counter hits 0, compute the aggregate state and push a state_update to the sender.
func (f *FanoutEngine) HandleMemberACK(ctx context.Context, messageID string, memberID string, senderID string, state int16) error {
	msgID, err := uuid.Parse(messageID)
	if err != nil {
		return fmt.Errorf("invalid message ID: %w", err)
	}
	memID, err := uuid.Parse(memberID)
	if err != nil {
		return fmt.Errorf("invalid member ID: %w", err)
	}

	// 1. Update this member's delivery state.
	if err := f.GroupDeliveryRepo.UpdateMemberState(ctx, msgID, memID, state); err != nil {
		return fmt.Errorf("failed to update member state: %w", err)
	}

	// 2. Decrement the pending counter.
	pk := pendingKey(messageID)
	remaining, err := f.RDB.Decr(ctx, pk).Result()
	if err != nil {
		return fmt.Errorf("failed to decrement pending counter: %w", err)
	}

	log.Debug().
		Str("message", messageID).
		Str("member", memberID).
		Int64("remaining", remaining).
		Msg("member ACK processed")

	// 3. If all members have reached this state, notify the sender.
	if remaining <= 0 {
		// All members have ACK'd at this state level.
		// Compute the actual aggregate from Postgres (source of truth).
		aggState, err := f.GroupDeliveryRepo.ComputeAggregateState(ctx, msgID)
		if err != nil {
			return fmt.Errorf("failed to compute aggregate state: %w", err)
		}

		// Map numeric state to string.
		stateStr := "SENT"
		switch aggState {
		case 2:
			stateStr = "DELIVERED"
		case 3:
			stateStr = "READ"
		}

		// Update the message's own state in the messages table.
		if err := f.MsgRepo.UpdateState(ctx, messageID, stateStr); err != nil {
			log.Error().Err(err).Msg("failed to update message aggregate state")
		}

		// Push state_update to the sender.
		payload, _ := json.Marshal(map[string]interface{}{
			"type": "state_update",
			"data": map[string]string{
				"message_id": messageID,
				"state":      stateStr,
			},
		})
		if err := f.PubSubRepo.Publish(ctx, senderID, payload); err != nil {
			log.Error().Err(err).Msg("failed to publish aggregate state to sender")
		}

		// Reset the counter for the next state level.
		// When members start sending the next state (e.g., READ after DELIVERED),
		// we need a fresh counter. Fetch current member count.
		members, err := f.GroupDeliveryRepo.GetAllMemberStates(ctx, msgID)
		if err == nil {
			// Count members who have NOT yet reached the next state.
			nextState := aggState + 1
			pending := 0
			for _, m := range members {
				if m.State < nextState {
					pending++
				}
			}
			if pending > 0 {
				f.RDB.Set(ctx, pk, pending, pendingKeyTTL)
			} else {
				// All members already at or past the next state. Clean up.
				f.RDB.Del(ctx, pk)
			}
		}

		log.Info().
			Str("message", messageID).
			Str("aggregate", stateStr).
			Msg("group aggregate state updated")
	}

	return nil
}
```

### How the pending counter works

The pending counter is an optimization to avoid querying Postgres on every single ACK. Here is the timeline for a 3-member group (A sends, B and C receive):

```
Fanout:
  Redis SET fanout:pending:msg-001 = 2    (B and C need to ACK)

B sends DELIVERED ACK:
  Postgres: update B's row to state=2
  Redis DECR fanout:pending:msg-001 -> 1
  remaining=1, do nothing

C sends DELIVERED ACK:
  Postgres: update C's row to state=2
  Redis DECR fanout:pending:msg-001 -> 0
  remaining=0, check Postgres: MIN(state) = 2 (DELIVERED)
  Update messages table: state = DELIVERED
  PUBLISH state_update to A: "DELIVERED"
  Reset counter for next state (READ): SET pending = 2

B sends READ ACK:
  Postgres: update B's row to state=3
  Redis DECR -> 1
  remaining=1, do nothing

C sends READ ACK:
  Postgres: update C's row to state=3
  Redis DECR -> 0
  remaining=0, check Postgres: MIN(state) = 3 (READ)
  PUBLISH state_update to A: "READ"
  Delete counter (no more states)
```

Without this counter, we would need to run `SELECT MIN(state) FROM group_message_delivery WHERE message_id = ?` on every single ACK from every member. For a group of 100 members, that is 100 queries per state transition. The counter reduces it to 1 query per state transition (when the counter hits 0).

---

## 9. Updated Router -- `internal/router/router.go`

The router now checks if the incoming message has a `group_id`. If it does, the message goes through the fanout engine instead of direct routing.

```go
// internal/router/router.go
package router

import (
	"context"
	"encoding/json"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
	"github.com/rs/zerolog/log"
)

// Router decides how to route an incoming message: 1:1 or group fanout.
type Router struct {
	PubSubRepo *repository.PubSubRepo
	MsgRepo    *repository.MessageRepo
	Fanout     *FanoutEngine
}

// NewRouter creates a new Router.
func NewRouter(pubsubRepo *repository.PubSubRepo, msgRepo *repository.MessageRepo, fanout *FanoutEngine) *Router {
	return &Router{
		PubSubRepo: pubsubRepo,
		MsgRepo:    msgRepo,
		Fanout:     fanout,
	}
}

// Route processes an incoming message and routes it to the correct destination.
func (r *Router) Route(ctx context.Context, senderID string, data json.RawMessage) {
	// Peek at the payload to determine routing.
	var peek struct {
		ID          string  `json:"id"`
		RecipientID *string `json:"recipient_id,omitempty"`
		GroupID     *string `json:"group_id,omitempty"`
		Content     string  `json:"content"`
	}
	if err := json.Unmarshal(data, &peek); err != nil {
		log.Error().Err(err).Msg("bad message payload in router")
		return
	}

	// Group message: fanout to all members.
	if peek.GroupID != nil && *peek.GroupID != "" {
		// Save message to Postgres with group_id.
		if err := r.MsgRepo.SaveGroupMessage(ctx, peek.ID, senderID, *peek.GroupID, peek.Content); err != nil {
			log.Error().Err(err).Msg("failed to save group message")
			return
		}

		// Send SENT ack to the sender.
		sentAck, _ := json.Marshal(map[string]interface{}{
			"type": "state_update",
			"data": map[string]string{
				"message_id": peek.ID,
				"state":      "SENT",
			},
		})
		if err := r.PubSubRepo.Publish(ctx, senderID, sentAck); err != nil {
			log.Error().Err(err).Msg("failed to publish SENT ack for group message")
		}

		// Fanout to all group members.
		if err := r.Fanout.Fanout(ctx, senderID, peek.ID, *peek.GroupID, peek.Content); err != nil {
			log.Error().Err(err).Msg("fanout failed")
		}
		return
	}

	// 1:1 message: direct route (existing logic).
	if peek.RecipientID == nil || *peek.RecipientID == "" {
		log.Error().Msg("message has neither recipient_id nor group_id")
		return
	}

	// Save to Postgres (state = SENT).
	if err := r.MsgRepo.Save(ctx, peek.ID, senderID, *peek.RecipientID, peek.Content); err != nil {
		log.Error().Err(err).Msg("failed to save message")
		return
	}

	// Send SENT ack to sender.
	sentAck, _ := json.Marshal(map[string]interface{}{
		"type": "state_update",
		"data": map[string]string{
			"message_id": peek.ID,
			"state":      "SENT",
		},
	})
	if err := r.PubSubRepo.Publish(ctx, senderID, sentAck); err != nil {
		log.Error().Err(err).Msg("failed to publish SENT ack")
	}

	// Publish to recipient.
	outgoing, _ := json.Marshal(map[string]interface{}{
		"type": "message",
		"data": map[string]string{
			"id":        peek.ID,
			"sender_id": senderID,
			"content":   peek.Content,
		},
	})
	if err := r.PubSubRepo.Publish(ctx, *peek.RecipientID, outgoing); err != nil {
		log.Error().Err(err).Msg("failed to publish message to recipient")
	}
}
```

The key change is the `if peek.GroupID != nil` branch at the top of `Route()`. Everything else in the 1:1 path is unchanged.

---

## 10. Updated `readPump` -- Group ACK Handling

The `handleStateUpdate` method in `client.go` now needs to distinguish between 1:1 ACKs and group ACKs.

```go
// handleStateUpdate processes DELIVERED / READ acknowledgements.
//
// For 1:1 messages: update state and forward to sender (existing behavior).
// For group messages: delegate to FanoutEngine.HandleMemberACK.
func (c *Client) handleStateUpdate(data json.RawMessage) {
	var su struct {
		MessageID string  `json:"message_id"`
		State     string  `json:"state"`
		SenderID  string  `json:"sender_id"`
		GroupID   *string `json:"group_id,omitempty"`
	}
	if err := json.Unmarshal(data, &su); err != nil {
		log.Error().Err(err).Msg("bad state_update payload")
		return
	}

	ctx := context.Background()

	// Map string state to numeric.
	var stateNum int16
	switch su.State {
	case "DELIVERED":
		stateNum = 2
	case "READ":
		stateNum = 3
	default:
		log.Error().Str("state", su.State).Msg("unknown state in ACK")
		return
	}

	// Group message ACK: delegate to fanout engine.
	if su.GroupID != nil && *su.GroupID != "" {
		if err := c.fanoutEngine.HandleMemberACK(ctx, su.MessageID, c.userID, su.SenderID, stateNum); err != nil {
			log.Error().Err(err).Msg("failed to handle group member ACK")
		}
		return
	}

	// 1:1 message ACK: existing logic.
	if err := c.msgRepo.UpdateState(ctx, su.MessageID, su.State); err != nil {
		log.Error().Err(err).Msg("failed to update message state")
		return
	}

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
```

The `Client` struct gains a new field:

```go
type Client struct {
	hub          *Hub
	userID       string
	conn         *websocket.Conn
	send         chan []byte
	sub          *redis.PubSub
	pubsubRepo   *repository.PubSubRepo
	msgRepo      *repository.MessageRepo
	fanoutEngine *FanoutEngine  // NEW: for group message ACK handling
}
```

---

## 11. Wire Protocol for Group Messages

### Client sends a group message

```json
{
  "type": "message",
  "data": {
    "id": "msg-g-001",
    "group_id": "550e8400-e29b-41d4-a716-446655440000",
    "content": "Hello group!"
  }
}
```

Note: no `recipient_id`. The presence of `group_id` tells the router this is a group message.

### Sender receives SENT ack

```json
{
  "type": "state_update",
  "data": {
    "message_id": "msg-g-001",
    "state": "SENT"
  }
}
```

This comes immediately after the server saves the message, before fanout begins.

### Each member receives the message

```json
{
  "type": "message",
  "data": {
    "id": "msg-g-001",
    "sender_id": "alice-uuid",
    "group_id": "550e8400-e29b-41d4-a716-446655440000",
    "content": "Hello group!"
  }
}
```

The `group_id` field tells the recipient this is a group message. The client UI uses this to render it in the correct group conversation.

### Members send individual ACKs

```json
{
  "type": "state_update",
  "data": {
    "message_id": "msg-g-001",
    "state": "DELIVERED",
    "sender_id": "alice-uuid",
    "group_id": "550e8400-e29b-41d4-a716-446655440000"
  }
}
```

The `group_id` in the ACK tells the server to route through `HandleMemberACK` instead of the 1:1 path.

### Sender receives aggregate state update

Only when ALL members have reached a state:

```json
{
  "type": "state_update",
  "data": {
    "message_id": "msg-g-001",
    "state": "DELIVERED"
  }
}
```

The sender does not get individual member ACKs -- only the aggregate. This matches WhatsApp behavior.

### Complete flow diagram

```
User A (sender)              Server                    Redis              User B          User C
     |                          |                        |                  |                |
     |-- WS: {message,          |                        |                  |                |
     |    group_id, content} -->|                        |                  |                |
     |                          |                        |                  |                |
     |                   Save to Postgres                |                  |                |
     |                   Insert delivery rows            |                  |                |
     |                   SET pending:msg = 2             |                  |                |
     |                          |                        |                  |                |
     |                          |-- PUBLISH chat:A ----->|                  |                |
     |                          |   (SENT ack)           |                  |                |
     |<-- WS: SENT ------------|                        |                  |                |
     |                          |                        |                  |                |
     |                          |-- PUBLISH chat:B ----->|--- WS: msg ---->|                |
     |                          |-- PUBLISH chat:C ----->|                  |-- WS: msg --->|
     |                          |                        |                  |                |
     |                          |                        |                  |                |
     |                          |<-- WS: DELIVERED ------|  (B ACKs)        |                |
     |                   Update B's row = 2              |                  |                |
     |                   DECR pending -> 1               |                  |                |
     |                   (1 > 0, do nothing)             |                  |                |
     |                          |                        |                  |                |
     |                          |<-- WS: DELIVERED ------|------------------|  (C ACKs)     |
     |                   Update C's row = 2              |                  |                |
     |                   DECR pending -> 0               |                  |                |
     |                   MIN(state) = 2 -> DELIVERED     |                  |                |
     |                          |-- PUBLISH chat:A ----->|                  |                |
     |<-- WS: DELIVERED --------|                        |                  |                |
     |                          |                        |                  |                |
```

---

## Acceptance Test

### Prerequisites

Make sure Postgres, Redis, and the server are running. Run migrations:

```bash
# Run the new migrations
tern migrate -m internal/database/migrations
```

### Step 1: Create three users

```bash
# Create users (adjust to your registration endpoint)
curl -s -X POST http://localhost:8080/api/users \
  -H "Content-Type: application/json" \
  -d '{"name": "Alice", "phone": "+1111111111"}' | jq .

curl -s -X POST http://localhost:8080/api/users \
  -H "Content-Type: application/json" \
  -d '{"name": "Bob", "phone": "+2222222222"}' | jq .

curl -s -X POST http://localhost:8080/api/users \
  -H "Content-Type: application/json" \
  -d '{"name": "Charlie", "phone": "+3333333333"}' | jq .
```

Save the returned UUIDs:

```bash
ALICE_ID="<alice-uuid>"
BOB_ID="<bob-uuid>"
CHARLIE_ID="<charlie-uuid>"
```

### Step 2: Create a group and add members

```bash
# Create the group
GROUP=$(curl -s -X POST http://localhost:8080/api/groups \
  -H "Content-Type: application/json" \
  -d '{"name": "Test Group"}')
echo $GROUP | jq .
GROUP_ID=$(echo $GROUP | jq -r '.id')

# Add all three users
curl -s -X POST "http://localhost:8080/api/groups/${GROUP_ID}/members" \
  -H "Content-Type: application/json" \
  -d "{\"user_id\": \"${ALICE_ID}\"}"

curl -s -X POST "http://localhost:8080/api/groups/${GROUP_ID}/members" \
  -H "Content-Type: application/json" \
  -d "{\"user_id\": \"${BOB_ID}\"}"

curl -s -X POST "http://localhost:8080/api/groups/${GROUP_ID}/members" \
  -H "Content-Type: application/json" \
  -d "{\"user_id\": \"${CHARLIE_ID}\"}"

# Verify members
curl -s "http://localhost:8080/api/groups/${GROUP_ID}/members" | jq .
```

### Step 3: Connect all three users via WebSocket

Open three terminals:

```bash
# Terminal 1: Alice
wscat -c "ws://localhost:8080/ws?user_id=${ALICE_ID}"

# Terminal 2: Bob
wscat -c "ws://localhost:8080/ws?user_id=${BOB_ID}"

# Terminal 3: Charlie
wscat -c "ws://localhost:8080/ws?user_id=${CHARLIE_ID}"
```

### Step 4: Alice sends a group message

In Alice's terminal:

```json
{
  "type": "message",
  "data": {
    "id": "msg-g-001",
    "group_id": "<GROUP_ID>",
    "content": "Hello group!"
  }
}
```

**Expected: Alice receives SENT ack:**

```json
{
  "type": "state_update",
  "data": { "message_id": "msg-g-001", "state": "SENT" }
}
```

**Expected: Bob receives the message:**

```json
{
  "type": "message",
  "data": {
    "id": "msg-g-001",
    "sender_id": "<ALICE_ID>",
    "group_id": "<GROUP_ID>",
    "content": "Hello group!"
  }
}
```

**Expected: Charlie receives the same message:**

```json
{
  "type": "message",
  "data": {
    "id": "msg-g-001",
    "sender_id": "<ALICE_ID>",
    "group_id": "<GROUP_ID>",
    "content": "Hello group!"
  }
}
```

### Step 5: Bob ACKs -- sender still sees single tick

In Bob's terminal:

```json
{
  "type": "state_update",
  "data": {
    "message_id": "msg-g-001",
    "state": "DELIVERED",
    "sender_id": "<ALICE_ID>",
    "group_id": "<GROUP_ID>"
  }
}
```

**Expected: Alice receives NOTHING.** Charlie has not ACK'd yet, so the aggregate state is still SENT (MIN of DELIVERED, SENT) = SENT. No state_update is sent to Alice.

Verify in Redis:

```bash
redis-cli GET fanout:pending:msg-g-001
# "1"  (Charlie still pending)
```

### Step 6: Charlie ACKs -- sender gets double tick

In Charlie's terminal:

```json
{
  "type": "state_update",
  "data": {
    "message_id": "msg-g-001",
    "state": "DELIVERED",
    "sender_id": "<ALICE_ID>",
    "group_id": "<GROUP_ID>"
  }
}
```

**Expected: Alice receives DELIVERED state update:**

```json
{
  "type": "state_update",
  "data": { "message_id": "msg-g-001", "state": "DELIVERED" }
}
```

Now all members have received the message. Alice sees double grey ticks.

### Step 7: Both read -- sender gets blue tick

In Bob's terminal:

```json
{
  "type": "state_update",
  "data": {
    "message_id": "msg-g-001",
    "state": "READ",
    "sender_id": "<ALICE_ID>",
    "group_id": "<GROUP_ID>"
  }
}
```

**Expected: Alice receives NOTHING.** Charlie has not read yet.

In Charlie's terminal:

```json
{
  "type": "state_update",
  "data": {
    "message_id": "msg-g-001",
    "state": "READ",
    "sender_id": "<ALICE_ID>",
    "group_id": "<GROUP_ID>"
  }
}
```

**Expected: Alice receives READ state update:**

```json
{
  "type": "state_update",
  "data": { "message_id": "msg-g-001", "state": "READ" }
}
```

Alice sees double blue ticks. All members have read the message.

### Step 8: Test with one member offline

Disconnect Charlie (close Terminal 3). Then have Alice send a new message:

In Alice's terminal:

```json
{
  "type": "message",
  "data": {
    "id": "msg-g-002",
    "group_id": "<GROUP_ID>",
    "content": "Charlie is offline"
  }
}
```

**Expected:**

- Alice receives SENT ack.
- Bob receives the message (via Redis pub/sub).
- Charlie does NOT receive the message (offline, Redis PUBLISH is fire-and-forget).

Bob ACKs:

```json
{
  "type": "state_update",
  "data": {
    "message_id": "msg-g-002",
    "state": "DELIVERED",
    "sender_id": "<ALICE_ID>",
    "group_id": "<GROUP_ID>"
  }
}
```

**Expected: Alice receives NOTHING.** The pending counter decrements to 0, but when the engine computes the aggregate from Postgres, MIN(state) = 1 (Charlie's row is still SENT). So the aggregate is SENT and Alice does not see any tick change.

The message sits in this state until Charlie comes back online. When the offline buffer replays `msg-g-002` to Charlie and Charlie's client sends a DELIVERED ACK, the aggregate finally reaches DELIVERED and Alice gets the double tick.

Verify the delivery rows in Postgres:

```bash
psql -d wp_proto -c "SELECT member_id, state FROM group_message_delivery WHERE message_id = 'msg-g-002';"
```

```
 member_id  | state
------------+-------
 <BOB_ID>   |     2
 <CHARLIE_ID>|     1
```

Bob is at DELIVERED (2), Charlie is still at SENT (1). Aggregate = MIN(2, 1) = 1 (SENT).

---

## What Changed From Step 7

| Component       | Before (Step 7)                   | After (Step 8)                                                                                                                      |
| --------------- | --------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| Message routing | 1:1 only via `pubsubRepo.Publish` | 1:1 or group via Router.Route()                                                                                                     |
| State tracking  | One state per message             | Per-member state via `group_message_delivery`                                                                                       |
| ACK handling    | Forward to sender immediately     | Group: aggregate via FanoutEngine                                                                                                   |
| New tables      | --                                | `groups`, `group_members`, `group_message_delivery`                                                                                 |
| New files       | --                                | `model/group.go`, `repository/group.go`, `repository/group_delivery.go`, `handler/group.go`, `service/group.go`, `router/fanout.go` |
| Modified files  | --                                | `router/router.go`, `handler/client.go`                                                                                             |

---

## Next Step

Groups now work with fanout and aggregate tick tracking. Step 9 adds **media messages** -- file uploads to object storage with message attachments.
