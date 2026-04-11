# Step 10 -- Message History and Pagination

**Prerequisites:** Full messaging system working -- users can register, connect via WebSocket, send DMs and group messages, delivery states are tracked, offline buffering works.

In this step we add REST APIs for fetching message history. WebSocket handles real-time delivery, but when a user opens a conversation they need to load past messages. The offline buffer only stores undelivered messages, not the full conversation log. Postgres is the durable message store -- we query it for history.

---

## 1. Why Message History?

Three distinct message delivery paths exist in the system:

1. **Real-time (WebSocket):** Messages arrive while you have the chat open. The hub routes them instantly.
2. **Offline buffer (Redis):** Messages that arrived while you were disconnected. Flushed on reconnect.
3. **History (Postgres):** The complete conversation log. Neither WebSocket nor Redis gives you this.

When a user taps on a conversation, the client needs to:
- Load the most recent N messages from history (REST API)
- Subscribe to real-time messages (WebSocket -- already working)
- Flush any offline buffer (already working from previous steps)

This step builds that first bullet point.

---

## 2. Cursor-Based Pagination

### Why not offset pagination?

Offset pagination (`LIMIT 20 OFFSET 100`) seems simple but has a critical flaw: the database still scans and discards 100 rows before returning 20. At `OFFSET 10000`, it scans 10,000 rows. Performance degrades linearly with depth.

It also breaks when new messages arrive. If someone sends a message while you are paginating, every offset shifts by one -- you either skip a message or see a duplicate.

### Cursor pagination

Instead, we use a cursor: "give me 20 messages older than this timestamp."

```
GET /api/messages/dm/{user_id}?cursor=2024-06-15T10:30:00Z&limit=20
```

The query becomes:

```sql
WHERE created_at < '2024-06-15T10:30:00Z'
ORDER BY created_at DESC
LIMIT 20
```

This uses an index seek -- no matter how deep you paginate, it is O(log n) not O(n).

### Handling timestamp collisions

Two messages can share the same `created_at` value (sub-millisecond inserts, clock resolution). If you cursor only on timestamp, you might skip messages or return duplicates.

Solution: use `(created_at, id)` as a composite cursor. Since `id` is a UUID (which is unique), it breaks ties:

```sql
WHERE (created_at, id) < ($cursor_time, $cursor_id)
ORDER BY created_at DESC, id DESC
LIMIT $limit
```

For simplicity in this step, we use `created_at` alone as the cursor. The odds of collision are low for a prototype, and switching to a composite cursor later is a mechanical change. The response includes the `created_at` of the last message as `next_cursor`.

---

## 3. Message History Repository

> **File:** `internal/repository/message.go`

This file may already exist if you built message persistence in earlier steps. Add these methods to the existing repository (or create the file if it does not exist yet).

```go
package repository

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
)

type MessageRepository struct {
	pool *pgxpool.Pool
}

func NewMessageRepository(pool *pgxpool.Pool) *MessageRepository {
	return &MessageRepository{pool: pool}
}

// SaveMessage persists a message to Postgres.
// (This likely exists already from the messaging step.)
func (r *MessageRepository) SaveMessage(ctx context.Context, msg *model.Message) error {
	query := `
		INSERT INTO messages (id, sender_id, recipient_id, group_id, content, state, client_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`
	_, err := r.pool.Exec(ctx, query,
		msg.ID, msg.SenderID, msg.RecipientID, msg.GroupID,
		msg.Content, msg.State, msg.ClientID, msg.CreatedAt,
	)
	return err
}

// GetConversationHistory returns DM messages between two users,
// ordered newest-first, paginated by cursor.
func (r *MessageRepository) GetConversationHistory(
	ctx context.Context,
	userID string,
	otherUserID string,
	cursor time.Time,
	limit int,
) ([]model.Message, error) {
	query := `
		SELECT id, sender_id, recipient_id, group_id, content, state, client_id, created_at
		FROM messages
		WHERE (
			(sender_id = $1 AND recipient_id = $2)
			OR
			(sender_id = $2 AND recipient_id = $1)
		)
		AND created_at < $3
		ORDER BY created_at DESC
		LIMIT $4
	`

	rows, err := r.pool.Query(ctx, query, userID, otherUserID, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []model.Message
	for rows.Next() {
		var msg model.Message
		if err := rows.Scan(
			&msg.ID, &msg.SenderID, &msg.RecipientID, &msg.GroupID,
			&msg.Content, &msg.State, &msg.ClientID, &msg.CreatedAt,
		); err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}

	return messages, rows.Err()
}

// GetGroupHistory returns messages in a group chat,
// ordered newest-first, paginated by cursor.
func (r *MessageRepository) GetGroupHistory(
	ctx context.Context,
	groupID string,
	cursor time.Time,
	limit int,
) ([]model.Message, error) {
	query := `
		SELECT id, sender_id, recipient_id, group_id, content, state, client_id, created_at
		FROM messages
		WHERE group_id = $1
		AND created_at < $2
		ORDER BY created_at DESC
		LIMIT $3
	`

	rows, err := r.pool.Query(ctx, query, groupID, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []model.Message
	for rows.Next() {
		var msg model.Message
		if err := rows.Scan(
			&msg.ID, &msg.SenderID, &msg.RecipientID, &msg.GroupID,
			&msg.Content, &msg.State, &msg.ClientID, &msg.CreatedAt,
		); err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}

	return messages, rows.Err()
}
```

### What each query does

**GetConversationHistory:** Fetches messages where user A sent to user B or user B sent to user A. The `OR` clause covers both directions. The `created_at < $3` filter is the cursor -- it only returns messages older than the cursor. `ORDER BY created_at DESC` gives newest first. `LIMIT $4` caps the page size.

**GetGroupHistory:** Simpler -- just filter by `group_id`. Same cursor and ordering logic.

---

## 4. Message History Service

> **File:** `internal/service/message.go`

The service layer validates input, applies defaults, and orchestrates the repository call. It also builds the pagination response metadata.

```go
package service

import (
	"context"
	"time"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
)

const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// HistoryResponse wraps a page of messages with pagination metadata.
type HistoryResponse struct {
	Messages   []model.Message `json:"messages"`
	NextCursor string          `json:"next_cursor,omitempty"`
	HasMore    bool            `json:"has_more"`
}

type MessageService struct {
	repo *repository.MessageRepository
}

func NewMessageService(repo *repository.MessageRepository) *MessageService {
	return &MessageService{repo: repo}
}

// clampLimit enforces the default and max page size.
func clampLimit(limit int) int {
	if limit <= 0 {
		return DefaultPageSize
	}
	if limit > MaxPageSize {
		return MaxPageSize
	}
	return limit
}

// GetConversationHistory returns a page of DM messages between two users.
func (s *MessageService) GetConversationHistory(
	ctx context.Context,
	userID string,
	otherUserID string,
	cursor time.Time,
	limit int,
) (*HistoryResponse, error) {
	limit = clampLimit(limit)

	// Fetch one extra to determine if there are more pages.
	messages, err := s.repo.GetConversationHistory(ctx, userID, otherUserID, cursor, limit+1)
	if err != nil {
		return nil, err
	}

	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit] // trim the extra
	}

	resp := &HistoryResponse{
		Messages: messages,
		HasMore:  hasMore,
	}

	if hasMore && len(messages) > 0 {
		last := messages[len(messages)-1]
		resp.NextCursor = last.CreatedAt.Format(time.RFC3339Nano)
	}

	return resp, nil
}

// GetGroupHistory returns a page of group messages.
func (s *MessageService) GetGroupHistory(
	ctx context.Context,
	groupID string,
	cursor time.Time,
	limit int,
) (*HistoryResponse, error) {
	limit = clampLimit(limit)

	messages, err := s.repo.GetGroupHistory(ctx, groupID, cursor, limit+1)
	if err != nil {
		return nil, err
	}

	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}

	resp := &HistoryResponse{
		Messages: messages,
		HasMore:  hasMore,
	}

	if hasMore && len(messages) > 0 {
		last := messages[len(messages)-1]
		resp.NextCursor = last.CreatedAt.Format(time.RFC3339Nano)
	}

	return resp, nil
}
```

### The limit+1 trick

We request `limit + 1` rows from the database. If we get 21 rows back when the client asked for 20, we know there is at least one more page. We trim the extra row before returning. This avoids a separate `COUNT(*)` query, which would be expensive on large tables.

---

## 5. Message History Handler

> **File:** `internal/handler/message.go`

```go
package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/service"
)

type MessageHandler struct {
	messageService *service.MessageService
}

func NewMessageHandler(messageService *service.MessageService) *MessageHandler {
	return &MessageHandler{messageService: messageService}
}

// parsePaginationParams extracts cursor and limit from query parameters.
func parsePaginationParams(r *http.Request) (time.Time, int) {
	cursor := time.Now()
	if cursorStr := r.URL.Query().Get("cursor"); cursorStr != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, cursorStr); err == nil {
			cursor = parsed
		}
	}

	limit := service.DefaultPageSize
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if parsed, err := strconv.Atoi(limitStr); err == nil {
			limit = parsed
		}
	}

	return cursor, limit
}

// GetConversationHistory handles GET /api/messages/dm/{user_id}
func (h *MessageHandler) GetConversationHistory(w http.ResponseWriter, r *http.Request) {
	// The authenticated user's ID comes from the auth middleware context.
	userID, ok := r.Context().Value("user_id").(string)
	if !ok || userID == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	// The other user's ID comes from the URL path.
	otherUserID := r.PathValue("user_id")
	if otherUserID == "" {
		http.Error(w, `{"error":"user_id is required"}`, http.StatusBadRequest)
		return
	}

	cursor, limit := parsePaginationParams(r)

	resp, err := h.messageService.GetConversationHistory(r.Context(), userID, otherUserID, cursor, limit)
	if err != nil {
		http.Error(w, `{"error":"failed to fetch messages"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// GetGroupHistory handles GET /api/messages/group/{group_id}
func (h *MessageHandler) GetGroupHistory(w http.ResponseWriter, r *http.Request) {
	// Auth check -- user must be authenticated (group membership check is optional for now).
	_, ok := r.Context().Value("user_id").(string)
	if !ok {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	groupID := r.PathValue("group_id")
	if groupID == "" {
		http.Error(w, `{"error":"group_id is required"}`, http.StatusBadRequest)
		return
	}

	cursor, limit := parsePaginationParams(r)

	resp, err := h.messageService.GetGroupHistory(r.Context(), groupID, cursor, limit)
	if err != nil {
		http.Error(w, `{"error":"failed to fetch messages"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
```

### Notes on the handler

- `r.PathValue("user_id")` uses Go 1.22's built-in router path parameters. If you are using `chi` or `gorilla/mux`, use their respective path extraction instead.
- The authenticated user ID comes from context, set by auth middleware (built in an earlier step).
- `parsePaginationParams` defaults cursor to `time.Now()` so the first request (with no cursor) returns the newest messages.

---

## 6. Database Indexes

The queries in section 3 need indexes to perform well. Your `002_create_messages.sql` migration likely already created some of these. Here is what each query needs and why.

### DM history query

```sql
WHERE (sender_id = $1 AND recipient_id = $2) OR (sender_id = $2 AND recipient_id = $1)
AND created_at < $3
ORDER BY created_at DESC
```

Indexes that serve this:

```sql
-- Basic indexes (likely already exist)
CREATE INDEX idx_messages_sender    ON messages(sender_id);
CREATE INDEX idx_messages_recipient ON messages(recipient_id);
```

These work but force the database to do two index scans (one per OR branch) and merge results. For a prototype this is fine.

For production, a composite index is significantly faster:

```sql
-- Optimal composite index for DM history
CREATE INDEX idx_messages_dm_history
    ON messages(sender_id, recipient_id, created_at DESC);
```

This index covers one branch of the OR in a single seek. Postgres will use it for both branches (swapping sender/recipient) and merge. The `DESC` on `created_at` matches the `ORDER BY`, so no sort step is needed.

### Group history query

```sql
WHERE group_id = $1 AND created_at < $2
ORDER BY created_at DESC
```

Index:

```sql
-- Basic (likely already exists)
CREATE INDEX idx_messages_group ON messages(group_id);

-- Optimal composite
CREATE INDEX idx_messages_group_history
    ON messages(group_id, created_at DESC);
```

The composite index lets Postgres seek to the group, then walk backwards from the cursor in index order -- no sort, no filter.

### Migration for the composite indexes

Create `internal/database/migrations/004_message_history_indexes.sql` (adjust number to your sequence):

```sql
-- Write your migrate up statements here
CREATE INDEX IF NOT EXISTS idx_messages_dm_history
    ON messages(sender_id, recipient_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_messages_group_history
    ON messages(group_id, created_at DESC);

---- create above / drop below ----

-- Write your migrate down statements here.
DROP INDEX IF EXISTS idx_messages_group_history;
DROP INDEX IF EXISTS idx_messages_dm_history;
```

---

## 7. Wire Up Routes

> **File:** `cmd/server/main.go` (or wherever your router is configured)

Add the message history routes alongside your existing routes:

```go
// --- Existing setup ---
messageRepo := repository.NewMessageRepository(pool)
messageService := service.NewMessageService(messageRepo)
messageHandler := handler.NewMessageHandler(messageService)

// --- Routes (inside your authenticated route group) ---
mux.HandleFunc("GET /api/messages/dm/{user_id}", messageHandler.GetConversationHistory)
mux.HandleFunc("GET /api/messages/group/{group_id}", messageHandler.GetGroupHistory)
```

If you use middleware chaining (e.g., `authMiddleware(messageHandler.GetConversationHistory)`), apply it here. Both endpoints require authentication so the handler can read the caller's user ID from context.

---

## 8. Acceptance Test

> **File:** `internal/handler/message_history_test.go`

This integration test creates users, inserts messages directly into the database, then hits the REST API to verify pagination works end to end.

```go
package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/handler"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/service"
)

// setupTestDB returns a connection pool to a test database.
// Set DATABASE_URL in your environment or adjust the connection string.
func setupTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://postgres:postgres@localhost:5432/wp_proto_test?sslmode=disable")
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

// insertTestUser creates a user directly in the database.
func insertTestUser(t *testing.T, pool *pgxpool.Pool, id, username string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, username, created_at) VALUES ($1, $2, now())
		 ON CONFLICT (id) DO NOTHING`,
		id, username,
	)
	if err != nil {
		t.Fatalf("failed to insert test user: %v", err)
	}
}

// insertTestMessage creates a message directly in the database with a controlled timestamp.
func insertTestMessage(t *testing.T, pool *pgxpool.Pool, senderID, recipientID, groupID, content string, createdAt time.Time) {
	t.Helper()
	query := `
		INSERT INTO messages (id, sender_id, recipient_id, group_id, content, state, client_id, created_at)
		VALUES ($1, $2, $3, $4, $5, 1, $6, $7)
	`
	var groupIDPtr *string
	if groupID != "" {
		groupIDPtr = &groupID
	}
	_, err := pool.Exec(context.Background(), query,
		uuid.New().String(), senderID, recipientID, groupIDPtr,
		content, uuid.New().String(), createdAt,
	)
	if err != nil {
		t.Fatalf("failed to insert test message: %v", err)
	}
}

// withAuthContext returns middleware that injects a user_id into the request context.
func withAuthContext(userID string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), "user_id", userID)
		next(w, r.WithContext(ctx))
	}
}

func TestConversationHistory_Pagination(t *testing.T) {
	pool := setupTestDB(t)

	// Clean up before test.
	pool.Exec(context.Background(), "DELETE FROM messages")

	aliceID := uuid.New().String()
	bobID := uuid.New().String()
	insertTestUser(t, pool, aliceID, "alice")
	insertTestUser(t, pool, bobID, "bob")

	// Insert 25 messages between Alice and Bob, alternating sender.
	baseTime := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		sender, recipient := aliceID, bobID
		if i%2 == 1 {
			sender, recipient = bobID, aliceID
		}
		createdAt := baseTime.Add(time.Duration(i) * time.Minute)
		insertTestMessage(t, pool, sender, recipient, "", fmt.Sprintf("message-%d", i), createdAt)
	}

	// Build the handler stack.
	repo := repository.NewMessageRepository(pool)
	svc := service.NewMessageService(repo)
	msgHandler := handler.NewMessageHandler(svc)

	// --- Page 1: no cursor, limit=10 ---
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/messages/dm/{user_id}", withAuthContext(aliceID, msgHandler.GetConversationHistory))

	req := httptest.NewRequest("GET", fmt.Sprintf("/api/messages/dm/%s?limit=10", bobID), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("page 1: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var page1 service.HistoryResponse
	if err := json.NewDecoder(rec.Body).Decode(&page1); err != nil {
		t.Fatalf("page 1: failed to decode response: %v", err)
	}

	if len(page1.Messages) != 10 {
		t.Fatalf("page 1: expected 10 messages, got %d", len(page1.Messages))
	}
	if !page1.HasMore {
		t.Fatal("page 1: expected has_more=true")
	}
	if page1.NextCursor == "" {
		t.Fatal("page 1: expected next_cursor to be set")
	}

	// Verify ordering: newest first.
	for i := 1; i < len(page1.Messages); i++ {
		if page1.Messages[i].CreatedAt.After(page1.Messages[i-1].CreatedAt) {
			t.Fatalf("page 1: messages not in descending order at index %d", i)
		}
	}

	t.Logf("page 1: got %d messages, has_more=%v, next_cursor=%s",
		len(page1.Messages), page1.HasMore, page1.NextCursor)

	// --- Page 2: use next_cursor, limit=10 ---
	req = httptest.NewRequest("GET",
		fmt.Sprintf("/api/messages/dm/%s?cursor=%s&limit=10", bobID, page1.NextCursor), nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("page 2: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var page2 service.HistoryResponse
	if err := json.NewDecoder(rec.Body).Decode(&page2); err != nil {
		t.Fatalf("page 2: failed to decode response: %v", err)
	}

	if len(page2.Messages) != 10 {
		t.Fatalf("page 2: expected 10 messages, got %d", len(page2.Messages))
	}
	if !page2.HasMore {
		t.Fatal("page 2: expected has_more=true")
	}

	// Verify no overlap with page 1.
	page1Last := page1.Messages[len(page1.Messages)-1].CreatedAt
	page2First := page2.Messages[0].CreatedAt
	if !page2First.Before(page1Last) {
		t.Fatalf("page 2 first message (%v) should be before page 1 last message (%v)",
			page2First, page1Last)
	}

	t.Logf("page 2: got %d messages, has_more=%v, next_cursor=%s",
		len(page2.Messages), page2.HasMore, page2.NextCursor)

	// --- Page 3: use next_cursor, limit=10 -> should return 5, has_more=false ---
	req = httptest.NewRequest("GET",
		fmt.Sprintf("/api/messages/dm/%s?cursor=%s&limit=10", bobID, page2.NextCursor), nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("page 3: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var page3 service.HistoryResponse
	if err := json.NewDecoder(rec.Body).Decode(&page3); err != nil {
		t.Fatalf("page 3: failed to decode response: %v", err)
	}

	if len(page3.Messages) != 5 {
		t.Fatalf("page 3: expected 5 messages, got %d", len(page3.Messages))
	}
	if page3.HasMore {
		t.Fatal("page 3: expected has_more=false")
	}

	t.Logf("page 3: got %d messages, has_more=%v", len(page3.Messages), page3.HasMore)

	// Total messages across all pages.
	total := len(page1.Messages) + len(page2.Messages) + len(page3.Messages)
	if total != 25 {
		t.Fatalf("expected 25 total messages across all pages, got %d", total)
	}
}

func TestGroupHistory_Pagination(t *testing.T) {
	pool := setupTestDB(t)

	// Clean up before test.
	pool.Exec(context.Background(), "DELETE FROM messages")

	aliceID := uuid.New().String()
	bobID := uuid.New().String()
	groupID := uuid.New().String()
	insertTestUser(t, pool, aliceID, "alice")
	insertTestUser(t, pool, bobID, "bob")

	// Insert 25 group messages.
	baseTime := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		sender := aliceID
		if i%2 == 1 {
			sender = bobID
		}
		createdAt := baseTime.Add(time.Duration(i) * time.Minute)
		insertTestMessage(t, pool, sender, "", groupID, fmt.Sprintf("group-msg-%d", i), createdAt)
	}

	repo := repository.NewMessageRepository(pool)
	svc := service.NewMessageService(repo)
	msgHandler := handler.NewMessageHandler(svc)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/messages/group/{group_id}", withAuthContext(aliceID, msgHandler.GetGroupHistory))

	// --- Page 1 ---
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/messages/group/%s?limit=10", groupID), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("page 1: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var page1 service.HistoryResponse
	if err := json.NewDecoder(rec.Body).Decode(&page1); err != nil {
		t.Fatalf("page 1: failed to decode: %v", err)
	}

	if len(page1.Messages) != 10 {
		t.Fatalf("page 1: expected 10 messages, got %d", len(page1.Messages))
	}
	if !page1.HasMore {
		t.Fatal("page 1: expected has_more=true")
	}

	t.Logf("group page 1: got %d messages, has_more=%v", len(page1.Messages), page1.HasMore)

	// --- Page 2 ---
	req = httptest.NewRequest("GET",
		fmt.Sprintf("/api/messages/group/%s?cursor=%s&limit=10", groupID, page1.NextCursor), nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var page2 service.HistoryResponse
	json.NewDecoder(rec.Body).Decode(&page2)

	if len(page2.Messages) != 10 {
		t.Fatalf("page 2: expected 10 messages, got %d", len(page2.Messages))
	}
	if !page2.HasMore {
		t.Fatal("page 2: expected has_more=true")
	}

	// --- Page 3 ---
	req = httptest.NewRequest("GET",
		fmt.Sprintf("/api/messages/group/%s?cursor=%s&limit=10", groupID, page2.NextCursor), nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var page3 service.HistoryResponse
	json.NewDecoder(rec.Body).Decode(&page3)

	if len(page3.Messages) != 5 {
		t.Fatalf("page 3: expected 5 messages, got %d", len(page3.Messages))
	}
	if page3.HasMore {
		t.Fatal("page 3: expected has_more=false")
	}

	total := len(page1.Messages) + len(page2.Messages) + len(page3.Messages)
	if total != 25 {
		t.Fatalf("expected 25 total group messages, got %d", total)
	}

	t.Logf("group history: all 25 messages retrieved across 3 pages")
}
```

### Running the test

```bash
# Make sure Postgres is running with a wp_proto_test database and migrations applied.
createdb wp_proto_test 2>/dev/null || true
DATABASE_URL="postgres://postgres:postgres@localhost:5432/wp_proto_test?sslmode=disable" \
  go test ./internal/handler/ -run "TestConversationHistory_Pagination|TestGroupHistory_Pagination" -v
```

### Expected output

```
=== RUN   TestConversationHistory_Pagination
    page 1: got 10 messages, has_more=true, next_cursor=2024-06-01T12:15:00Z
    page 2: got 10 messages, has_more=true, next_cursor=2024-06-01T12:05:00Z
    page 3: got 5 messages, has_more=false
--- PASS: TestConversationHistory_Pagination (0.05s)
=== RUN   TestGroupHistory_Pagination
    group page 1: got 10 messages, has_more=true
    group history: all 25 messages retrieved across 3 pages
--- PASS: TestGroupHistory_Pagination (0.04s)
```

---

## Summary

| Component | File | What it does |
|-----------|------|-------------|
| Repository | `internal/repository/message.go` | SQL queries with cursor-based WHERE clause |
| Service | `internal/service/message.go` | Input validation, limit+1 trick, pagination metadata |
| Handler | `internal/handler/message.go` | HTTP endpoints, query param parsing, JSON response |
| Migration | `internal/database/migrations/004_message_history_indexes.sql` | Composite indexes for efficient history queries |
| Routes | `cmd/server/main.go` | `GET /api/messages/dm/{user_id}` and `GET /api/messages/group/{group_id}` |

The client flow is now: open conversation -> `GET /api/messages/dm/{user_id}?limit=20` -> render messages -> scroll up -> `GET /api/messages/dm/{user_id}?cursor=<next_cursor>&limit=20` -> append older messages. Real-time messages continue arriving over WebSocket as before.
