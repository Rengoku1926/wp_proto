package database

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/config"
)

func ConnectRedis(ctx context.Context, cfg config.RedisConfig) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Address,
		Password: cfg.Password,
	})

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to ping redis: %w", err)
	}

	return client, nil
}
