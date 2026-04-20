package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
	"github.com/redis/go-redis/v9"
)

const offlineTTL = 30 * 24 * time.Hour

type OfflineStore struct {
	rdb *redis.Client
}

func NewOfflineStore(rdb *redis.Client) *OfflineStore {
	return &OfflineStore{rdb: rdb}
}

//offline key returns the Redis key for a user's offline buffer
// offline:123e4567-e89b-12d3-a456-426614174000
func offlineKey(userID string) string {
	return fmt.Sprintf("offline:%s", userID)
}
// offline:123... → [
//   "{json message 3}",
//   "{json message 2}",
//   "{json message 1}"
// ]
//
// JSON
// {
//   "id": "msg-id",
//   "sender_id": "alice-id",
//   "recipient_id": "bob-id",
//   "content": "hello",
//   "created_at": "..."
// }

//push adds a message to the user's offline buffer.
func(s *OfflineStore) Push(ctx context.Context, userID string, msg *model.Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	key := offlineKey(userID)

	// Pipeline: send both commands in one round-trip
	pipe := s.rdb.Pipeline()
	pipe.LPush(ctx, key, data)
	pipe.Expire(ctx, key, offlineTTL)
	_, err = pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("offline push pipeline: %w", err)
	}

	return nil
}

//drain removes and returns all messages from the user's offline buffer. 
func(s *OfflineStore) Drain(ctx context.Context, userID string) ([]*model.Message, error) {
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