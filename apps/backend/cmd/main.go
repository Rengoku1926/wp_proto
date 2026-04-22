package main

import (
	"context"
	"os"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/config"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/database"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/logger"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/server"
)

func main() {
	cfg, err := config.Load(".env.local")
	if err != nil {
		logger.Log.Fatal().Err(err).Msg("failed to load config")
	}

	log := logger.Init(cfg.Primary.Env)

	ctx := context.Background()

	pool, err := database.Connect(ctx, cfg.Database)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to database")
	}
	defer pool.Close()

	if err := database.Migrate(ctx, pool); err != nil {
		log.Fatal().Err(err).Msg("failed to run migrations")
	}
	log.Info().Msg("migrations applied")

	redisClient, err := database.ConnectRedis(ctx, cfg.Redis)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to redis")
	}
	defer redisClient.Close()

	srv := server.NewServer(pool, redisClient, log, cfg)
	srv.RegisterRoutes()

	if err := srv.Start(); err != nil {
		log.Error().Err(err).Msg("server stopped")
		os.Exit(1)
	}
}