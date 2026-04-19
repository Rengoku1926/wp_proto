package repository

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

//PubSubRepo wraps redis pub/sub operations for chat message routing
type PubSubRepo struct{
	rdb *redis.Client
}

//NewPubSubRepo creates a new PubSubRepo
func NewPubSubRepo(rdb *redis.Client) *PubSubRepo{
	return &PubSubRepo{rdb: rdb}
}

//channel returns the redis channel name for a given user
func channel(userID string) string{
	return fmt.Sprintf("chat:%s", userID)
}

//Publish sends data to the redis channel for the given user
//any server that has this user connected (and subscribed) will recieve it 
func (r *PubSubRepo) Publish(ctx context.Context, userID string, data []byte) error {
	return r.rdb.Publish(ctx, channel(userID), data).Err()
}

//Subscribe returns a *redis.PubSub handle subscribed to the channel for the given user
//the caller is responsible for calling Close() on it 
func (r *PubSubRepo) Subscribe(ctx context.Context, userID string) *redis.PubSub {
	return r.rdb.Subscribe(ctx, channel(userID))
}