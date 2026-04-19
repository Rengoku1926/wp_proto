package main

import (
	"context"
	"net/http"
	"os"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/config"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/database"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/handler"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/logger"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/server"
)

func main() {
	cfg, err := config.Load(".env.local")
	if err != nil {
		panic(err)
	}

	log := logger.New(cfg.Primary.Env)

	ctx := context.Background()

	pool, err := database.Connect(ctx, cfg.Database)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to database")
		os.Exit(1)
	}
	defer pool.Close()
	log.Info().Msg("successfully connected to postgres")

	if err := database.Migrate(ctx, pool); err != nil {
		log.Fatal().Err(err).Msg("failed to run migrations")
		os.Exit(1)
	}
	log.Info().Msg("migrations completed successfully")

	redisClient, err := database.ConnectRedis(ctx, cfg.Redis)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to redis")
		os.Exit(1)
	}
	defer redisClient.Close()
	log.Info().Msg("successfully connected to redis")

	msgRepo := repository.NewMessageRepo(pool)
	registry := handler.NewConnRegistry()
	wsHandler := handler.NewWSHandler(registry, msgRepo)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", wsHandler.HandleWebSocket)

	srv := server.NewServer(pool, redisClient, log, cfg)
	srv.RegisterRoutes()

	if err := srv.Start(); err != nil {
        log.Fatal().Err(err).Msg("server failed")
        os.Exit(1)
    }
}
