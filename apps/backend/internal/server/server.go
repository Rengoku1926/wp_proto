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
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/router"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/service"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
)

type Server struct {
	pool   *pgxpool.Pool
	redis  *redis.Client
	logger zerolog.Logger
	mux    *http.ServeMux
	cfg    *config.Config
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
	s.mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	userRepo := repository.NewUserRepository(s.pool)
	userService := service.NewUserService(userRepo, s.logger)
	userHandler := handler.NewUserHandler(userService)

	s.mux.HandleFunc("POST /api/users", userHandler.HandleRegister)
	s.mux.HandleFunc("GET /api/users/{id}", userHandler.HandleGetUserById)

	msgRepo := repository.NewMessageRepo(s.pool)
	registry := handler.NewConnRegistry()
	go registry.Run()
	stateService := service.NewStateService(s.pool)
	pubsubRepo := repository.NewPubSubRepo(s.redis)
	offlineStore := repository.NewOfflineStore(s.redis)

	groupRepo := repository.NewGroupRepo(s.pool)
	groupDeliveryRepo := repository.NewGroupDeliveryRepo(s.pool)
	fanoutEngine := router.NewFanoutEngine(registry, pubsubRepo, msgRepo, groupRepo, groupDeliveryRepo, offlineStore, s.redis)

	wsHandler := handler.NewWSHandler(registry, pubsubRepo, msgRepo, stateService, offlineStore, fanoutEngine)
	s.mux.HandleFunc("GET /ws", wsHandler.HandleWebSocket)

	groupSvc := service.NewGroupService(groupRepo)
	groupHandler := handler.NewGroupHandler(groupSvc)
	s.mux.HandleFunc("POST /api/groups", groupHandler.CreateGroup)
	s.mux.HandleFunc("POST /api/groups/{id}/members", groupHandler.AddMember)
	s.mux.HandleFunc("GET /api/groups/{id}/members", groupHandler.ListMembers)
}

func (s *Server) Start() error {
	httpServer := &http.Server{
		Addr:         ":" + s.cfg.Server.Port,
		Handler:      s.mux,
		ReadTimeout:  time.Duration(s.cfg.Server.ReadTimeout) * time.Second,
		WriteTimeout: time.Duration(s.cfg.Server.WriteTimeout) * time.Second,
		IdleTimeout:  time.Duration(s.cfg.Server.IdleTimeout) * time.Second,
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	serverErr := make(chan error, 1)
	go func() {
		s.logger.Info().Str("port", s.cfg.Server.Port).Msg("server starting")
		serverErr <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		return fmt.Errorf("server error: %w", err)
	case sig := <-shutdown:
		s.logger.Info().Str("signal", sig.String()).Msg("shutdown signal received")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		if err := httpServer.Shutdown(ctx); err != nil {
			s.logger.Error().Err(err).Msg("graceful shutdown failed, forcing close")
			httpServer.Close()
			return fmt.Errorf("graceful shutdown failed: %w", err)
		}
		s.logger.Info().Msg("server shutdown gracefully")
	}
	return nil
}