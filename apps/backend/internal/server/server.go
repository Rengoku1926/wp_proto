package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/config"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/handler"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/service"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
)

type Server struct {
	pool *pgxpool.Pool
	redis *redis.Client
	logger zerolog.Logger
	mux *http.ServeMux
	cfg *config.Config
}

func NewServer(pool *pgxpool.Pool, redisClient *redis.Client, logger zerolog.Logger, cfg *config.Config) *Server {
    return &Server{
        pool:   pool,
        redis:  redisClient,
        logger: logger,
        mux:    http.NewServeMux(),
        cfg:    cfg,
    }
}

func (s *Server) RegisterRoutes() {
    // Health check
    s.mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusOK)
        w.Write([]byte(`{"status":"ok"}`))
    })

    // Build the dependency chain: repository -> service -> handler
    userRepo := repository.NewUserRepository(s.pool)
    userService := service.NewUserService(userRepo, s.logger)
    userHandler := handler.NewUserHandler(userService)

    // User routes (Go 1.22+ enhanced mux patterns)
    s.mux.HandleFunc("POST /api/users", userHandler.HandleRegister)
    s.mux.HandleFunc("GET /api/users/{id}", userHandler.HandleGetUserById)

    // WebSocket
    msgRepo := repository.NewMessageRepo(s.pool)
    registry := handler.NewConnRegistry()
    wsHandler := handler.NewWSHandler(registry, msgRepo)
    s.mux.HandleFunc("GET /ws", wsHandler.HandleWebSocket)
}

func (s *Server) Start() error {
	httpServer := &http.Server{
		 Addr:         ":" + s.cfg.Server.Port,
        Handler:      s.mux,
        ReadTimeout:  time.Duration(s.cfg.Server.ReadTimeout) * time.Second,
        WriteTimeout: time.Duration(s.cfg.Server.WriteTimeout) * time.Second,
        IdleTimeout:  time.Duration(s.cfg.Server.IdleTimeout) * time.Second,
	}

    //channel that listens for os signals
	shutdown := make(chan os.Signal, 1)
    signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

    //channel to chats server startup errors 
    serverErr := make(chan error, 1)

    go func(){
        s.logger.Info().Str("port", s.cfg.Server.Port).Msg("server starting")
        serverErr <- httpServer.ListenAndServe()
    }()

    select {
    case err := <-serverErr:
        return fmt.Errorf("server error : %w", err)
    case sig := <-shutdown:
        s.logger.Info().Str("signal", sig.String()).Msg("shutdown signal received")
        //give active requests 15 sec to complete
        ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
        defer cancel()

        if err := httpServer.Shutdown(ctx); err != nil{
            s.logger.Error().Err(err).Msg("graceful shutdown failed, forcing close")
            httpServer.Close()
            return fmt.Errorf("graceful shutdown failed: %w", err)
        }
        s.logger.Info().Msg("server shutdown gracefully")
    }
    return nil
}