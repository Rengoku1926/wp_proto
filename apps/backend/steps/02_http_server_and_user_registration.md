# Step 2: HTTP Server and User Registration

In Step 1, we set up config loading, logging, Postgres (pgxpool), Redis (go-redis), tern migrations, and created the `users` table. Everything boots and connects, but nothing listens for traffic.

This step builds the HTTP server and user registration endpoint -- the foundation we need before adding WebSockets in a later step.

---

## The Layered Architecture

We follow a clean, three-layer pattern that keeps responsibilities separated:

```
HTTP Request
    |
    v
  Handler    -- parses HTTP, validates input shape, writes HTTP responses
    |
    v
  Service    -- business logic, orchestration, input validation rules
    |
    v
  Repository -- database queries, maps DB errors to domain errors
    |
    v
  Postgres
```

**Why bother with layers?**

- **Handlers** know about HTTP but nothing about SQL. If you switch from JSON to gRPC, only handlers change.
- **Services** contain your business rules. They are testable without spinning up an HTTP server.
- **Repositories** isolate SQL. If you move from Postgres to something else, only repositories change.
- Each layer depends only on the one below it. This makes unit testing straightforward -- you mock one layer at a time.

---

## File-by-File Walkthrough

### 1. Custom Errors -- `internal/errs/errors.go`

Before building layers, we define domain-level sentinel errors. The repository layer translates database-specific errors (like Postgres unique-violation code `23505`) into these, so the service and handler layers never import `pgx` or know anything about Postgres error codes.

```go
// internal/errs/errors.go
package errs

import "errors"

var (
    // ErrNotFound is returned when a queried resource does not exist.
    ErrNotFound = errors.New("resource not found")

    // ErrDuplicate is returned when a unique constraint is violated.
    ErrDuplicate = errors.New("resource already exists")

    // ErrValidation is returned when input fails validation rules.
    ErrValidation = errors.New("validation error")
)
```

Three errors cover the vast majority of cases in a CRUD API:

| Error | HTTP Status | When |
|-------|-------------|------|
| `ErrNotFound` | 404 | `SELECT` returns no rows |
| `ErrDuplicate` | 409 | `INSERT` violates a `UNIQUE` constraint |
| `ErrValidation` | 400 | Empty username, too long, etc. |

---

### 2. User Model -- `internal/model/user.go`

The model is a plain struct that mirrors the `users` table. It carries JSON tags so it serializes cleanly in HTTP responses.

```go
// internal/model/user.go
package model

import (
    "time"

    "github.com/google/uuid"
)

type User struct {
    ID        uuid.UUID `json:"id"`
    Username  string    `json:"username"`
    CreatedAt time.Time `json:"created_at"`
}
```

We use `github.com/google/uuid` which is already an indirect dependency in our `go.mod`. After this step, it becomes a direct one.

---

### 3. Repository Layer -- `internal/repository/user.go`

The repository owns all SQL for the `users` table. It takes a `*pgxpool.Pool` (not a single connection) so queries are automatically distributed across the pool.

```go
// internal/repository/user.go
package repository

import (
    "context"
    "errors"

    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgconn"
    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/google/uuid"

    "github.com/Rengoku1926/wp_proto/apps/backend/internal/errs"
    "github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
)

type UserRepository struct {
    pool *pgxpool.Pool
}

func NewUserRepository(pool *pgxpool.Pool) *UserRepository {
    return &UserRepository{pool: pool}
}

// Create inserts a new user and returns the created record.
// If the username already exists, it returns errs.ErrDuplicate.
func (r *UserRepository) Create(ctx context.Context, username string) (*model.User, error) {
    user := &model.User{}
    err := r.pool.QueryRow(
        ctx,
        `INSERT INTO users (username) VALUES ($1)
         RETURNING id, username, created_at`,
        username,
    ).Scan(&user.ID, &user.Username, &user.CreatedAt)

    if err != nil {
        // Postgres error code 23505 = unique_violation
        var pgErr *pgconn.PgError
        if errors.As(err, &pgErr) && pgErr.Code == "23505" {
            return nil, errs.ErrDuplicate
        }
        return nil, err
    }

    return user, nil
}

// GetByID fetches a user by their UUID primary key.
func (r *UserRepository) GetByID(ctx context.Context, id uuid.UUID) (*model.User, error) {
    user := &model.User{}
    err := r.pool.QueryRow(
        ctx,
        `SELECT id, username, created_at FROM users WHERE id = $1`,
        id,
    ).Scan(&user.ID, &user.Username, &user.CreatedAt)

    if err != nil {
        if errors.Is(err, pgx.ErrNoRows) {
            return nil, errs.ErrNotFound
        }
        return nil, err
    }

    return user, nil
}

// GetByUsername fetches a user by their unique username.
func (r *UserRepository) GetByUsername(ctx context.Context, username string) (*model.User, error) {
    user := &model.User{}
    err := r.pool.QueryRow(
        ctx,
        `SELECT id, username, created_at FROM users WHERE username = $1`,
        username,
    ).Scan(&user.ID, &user.Username, &user.CreatedAt)

    if err != nil {
        if errors.Is(err, pgx.ErrNoRows) {
            return nil, errs.ErrNotFound
        }
        return nil, err
    }

    return user, nil
}
```

Key points:

- **`INSERT ... RETURNING`** gives us the generated `id` and `created_at` in a single round trip instead of requiring a follow-up `SELECT`.
- **`errors.As(err, &pgErr)`** unwraps the pgx error chain to find the underlying Postgres error. We check `.Code == "23505"` (the Postgres error code for unique-constraint violation) and translate it to our domain error `errs.ErrDuplicate`. This is the critical boundary -- no Postgres-specific knowledge leaks past the repository.
- **`pgx.ErrNoRows`** is translated to `errs.ErrNotFound` so the service layer never imports `pgx`.

---

### 4. Service Layer -- `internal/service/user.go`

The service layer holds business logic. Right now that is just input validation and calling the repository, but this is where you would later add things like rate limiting, event publishing, or multi-step workflows.

```go
// internal/service/user.go
package service

import (
    "context"
    "fmt"
    "strings"

    "github.com/google/uuid"
    "github.com/rs/zerolog"

    "github.com/Rengoku1926/wp_proto/apps/backend/internal/errs"
    "github.com/Rengoku1926/wp_proto/apps/backend/internal/model"
    "github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
)

type UserService struct {
    repo   *repository.UserRepository
    logger zerolog.Logger
}

func NewUserService(repo *repository.UserRepository, logger zerolog.Logger) *UserService {
    return &UserService{
        repo:   repo,
        logger: logger,
    }
}

// RegisterUser validates the username and creates a new user.
func (s *UserService) RegisterUser(ctx context.Context, username string) (*model.User, error) {
    // Trim whitespace
    username = strings.TrimSpace(username)

    // Validation rules
    if username == "" {
        return nil, fmt.Errorf("%w: username is required", errs.ErrValidation)
    }
    if len(username) < 3 {
        return nil, fmt.Errorf("%w: username must be at least 3 characters", errs.ErrValidation)
    }
    if len(username) > 30 {
        return nil, fmt.Errorf("%w: username must be at most 30 characters", errs.ErrValidation)
    }

    user, err := s.repo.Create(ctx, username)
    if err != nil {
        s.logger.Error().Err(err).Str("username", username).Msg("failed to create user")
        return nil, err
    }

    s.logger.Info().Str("user_id", user.ID.String()).Str("username", username).Msg("user registered")
    return user, nil
}

// GetUser retrieves a user by ID.
func (s *UserService) GetUser(ctx context.Context, id uuid.UUID) (*model.User, error) {
    user, err := s.repo.GetByID(ctx, id)
    if err != nil {
        return nil, err
    }
    return user, nil
}
```

Notice how validation errors wrap `errs.ErrValidation` using `fmt.Errorf("%w: ...")`. This lets the handler check `errors.Is(err, errs.ErrValidation)` while still carrying the specific message ("username is required") for the response body.

---

### 5. Handler Layer -- `internal/handler/user.go`

Handlers are the HTTP boundary. They parse requests, call services, and write responses. They never contain SQL or business rules.

```go
// internal/handler/user.go
package handler

import (
    "encoding/json"
    "errors"
    "net/http"

    "github.com/google/uuid"

    "github.com/Rengoku1926/wp_proto/apps/backend/internal/errs"
    "github.com/Rengoku1926/wp_proto/apps/backend/internal/service"
)

type UserHandler struct {
    userService *service.UserService
}

func NewUserHandler(userService *service.UserService) *UserHandler {
    return &UserHandler{userService: userService}
}

// registerRequest is the expected JSON body for POST /api/users.
type registerRequest struct {
    Username string `json:"username"`
}

// errorResponse is a standard error JSON envelope.
type errorResponse struct {
    Error string `json:"error"`
}

// HandleRegister handles POST /api/users
func (h *UserHandler) HandleRegister(w http.ResponseWriter, r *http.Request) {
    var req registerRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON body"})
        return
    }

    user, err := h.userService.RegisterUser(r.Context(), req.Username)
    if err != nil {
        switch {
        case errors.Is(err, errs.ErrValidation):
            writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
        case errors.Is(err, errs.ErrDuplicate):
            writeJSON(w, http.StatusConflict, errorResponse{Error: "username already taken"})
        default:
            writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
        }
        return
    }

    writeJSON(w, http.StatusCreated, user)
}

// HandleGetUser handles GET /api/users/{id}
func (h *UserHandler) HandleGetUser(w http.ResponseWriter, r *http.Request) {
    // Go 1.22+ mux: r.PathValue extracts named segments from the pattern
    idStr := r.PathValue("id")

    id, err := uuid.Parse(idStr)
    if err != nil {
        writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid user id"})
        return
    }

    user, err := h.userService.GetUser(r.Context(), id)
    if err != nil {
        if errors.Is(err, errs.ErrNotFound) {
            writeJSON(w, http.StatusNotFound, errorResponse{Error: "user not found"})
            return
        }
        writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
        return
    }

    writeJSON(w, http.StatusOK, user)
}

// writeJSON is a helper that sets Content-Type and encodes the response.
func writeJSON(w http.ResponseWriter, status int, data any) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    json.NewEncoder(w).Encode(data)
}
```

Key details:

- **`r.PathValue("id")`** -- this is the Go 1.22+ standard library mux feature. The pattern `GET /api/users/{id}` is registered on the mux and Go automatically extracts path parameters. No third-party router needed.
- **Error mapping** uses `errors.Is` to check our sentinel errors. The `switch` maps domain errors to HTTP status codes: `ErrValidation` to 400, `ErrDuplicate` to 409, `ErrNotFound` to 404, everything else to 500.
- **`writeJSON`** is a tiny helper to avoid repeating the Content-Type/encode boilerplate. It lives in the handler package since it is an HTTP concern.
- The handler never logs -- that is the service layer's job. The handler only translates between HTTP and domain types.

---

### 6. HTTP Server -- `internal/server/server.go`

The server ties everything together: it holds dependencies, registers routes, and manages the HTTP lifecycle including graceful shutdown.

```go
// internal/server/server.go
package server

import (
    "context"
    "fmt"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/redis/go-redis/v9"
    "github.com/rs/zerolog"

    "github.com/Rengoku1926/wp_proto/apps/backend/internal/config"
    "github.com/Rengoku1926/wp_proto/apps/backend/internal/handler"
    "github.com/Rengoku1926/wp_proto/apps/backend/internal/repository"
    "github.com/Rengoku1926/wp_proto/apps/backend/internal/service"
)

// Server holds all dependencies and the HTTP mux.
type Server struct {
    pool   *pgxpool.Pool
    redis  *redis.Client
    logger zerolog.Logger
    mux    *http.ServeMux
    cfg    *config.Config
}

// NewServer creates a new Server with all dependencies injected.
func NewServer(pool *pgxpool.Pool, redisClient *redis.Client, logger zerolog.Logger, cfg *config.Config) *Server {
    return &Server{
        pool:   pool,
        redis:  redisClient,
        logger: logger,
        mux:    http.NewServeMux(),
        cfg:    cfg,
    }
}

// RegisterRoutes wires up all HTTP routes.
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
    s.mux.HandleFunc("GET /api/users/{id}", userHandler.HandleGetUser)
}

// Start launches the HTTP server and blocks until shutdown completes.
func (s *Server) Start() error {
    httpServer := &http.Server{
        Addr:         ":" + s.cfg.Server.Port,
        Handler:      s.mux,
        ReadTimeout:  time.Duration(s.cfg.Server.ReadTimeout) * time.Second,
        WriteTimeout: time.Duration(s.cfg.Server.WriteTimeout) * time.Second,
        IdleTimeout:  time.Duration(s.cfg.Server.IdleTimeout) * time.Second,
    }

    // ---------------------------------------------------------------
    // Graceful Shutdown
    // ---------------------------------------------------------------
    //
    // Why this matters (especially for a chat app with WebSockets):
    //
    // Without graceful shutdown, calling os.Exit() or returning from
    // main() immediately closes all TCP connections. Every connected
    // WebSocket client gets a broken pipe with no warning. In-flight
    // HTTP requests get dropped. Database transactions may be left
    // in an inconsistent state.
    //
    // server.Shutdown(ctx) does the following:
    //   1. Stops accepting NEW connections immediately.
    //   2. Waits for all EXISTING requests/connections to finish.
    //   3. If they don't finish before the context deadline, it
    //      forcefully closes remaining connections.
    //
    // For WebSockets specifically, this gives connected clients a
    // window to receive a close frame and reconnect to another
    // instance during a rolling deploy.
    // ---------------------------------------------------------------

    // Channel to catch OS signals
    shutdown := make(chan os.Signal, 1)
    signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

    // Channel to catch server startup errors
    serverErr := make(chan error, 1)

    // Start the server in a goroutine
    go func() {
        s.logger.Info().Str("port", s.cfg.Server.Port).Msg("server starting")
        serverErr <- httpServer.ListenAndServe()
    }()

    // Block until we receive a signal or a startup error
    select {
    case err := <-serverErr:
        // ListenAndServe returned immediately -- likely a port conflict
        return fmt.Errorf("server error: %w", err)

    case sig := <-shutdown:
        s.logger.Info().Str("signal", sig.String()).Msg("shutdown signal received")

        // Give active requests 15 seconds to complete
        ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
        defer cancel()

        if err := httpServer.Shutdown(ctx); err != nil {
            // Force close if graceful shutdown times out
            s.logger.Error().Err(err).Msg("graceful shutdown failed, forcing close")
            httpServer.Close()
            return fmt.Errorf("graceful shutdown failed: %w", err)
        }

        s.logger.Info().Msg("server stopped gracefully")
    }

    return nil
}
```

Walk through the `Start()` method:

1. We create an `http.Server` with timeouts pulled from config. **ReadTimeout** limits how long the server waits to read the full request (protects against slowloris attacks). **WriteTimeout** limits response writing. **IdleTimeout** controls how long keep-alive connections stay open between requests.

2. We set up a channel listening for `SIGINT` (Ctrl+C) and `SIGTERM` (what Docker/Kubernetes sends during shutdown).

3. The server starts in a goroutine. The main goroutine blocks in a `select`.

4. When a signal arrives, `httpServer.Shutdown(ctx)` begins the graceful process: stop accepting new connections, drain existing ones, respect the 15-second deadline.

---

### 7. Updated `cmd/main.go`

The main function now creates the server, registers routes, and starts it. The graceful shutdown logic lives inside `server.Start()`, keeping main clean.

```go
// cmd/main.go
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

    // Create server, register routes, and start
    srv := server.NewServer(pool, redisClient, log, cfg)
    srv.RegisterRoutes()

    if err := srv.Start(); err != nil {
        log.Fatal().Err(err).Msg("server failed")
        os.Exit(1)
    }
}
```

The only changes from Step 1 are the last four lines: we removed the `_ = pool` and `_ = redisClient` placeholders and replaced them with actual server creation and startup.

---

### 8. Migration Reference

The `users` table already exists from Step 1. For reference, this is `internal/database/migrations/001_create_users.sql`:

```sql
CREATE TABLE users (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username   TEXT UNIQUE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

The `UNIQUE` constraint on `username` is what triggers the Postgres `23505` error code that the repository maps to `errs.ErrDuplicate`.

---

## Project Structure After This Step

```
apps/backend/
├── cmd/
│   └── main.go                          # entry point (updated)
├── internal/
│   ├── config/
│   │   ├── config.go                    # Load() function
│   │   └── interface.go                 # Config structs
│   ├── database/
│   │   ├── database.go                  # Connect()
│   │   ├── redis.go                     # ConnectRedis()
│   │   ├── migration.go                 # Migrate()
│   │   └── migrations/
│   │       └── 001_create_users.sql
│   ├── errs/
│   │   └── errors.go                    # NEW - sentinel errors
│   ├── handler/
│   │   └── user.go                      # NEW - HTTP handlers
│   ├── logger/
│   │   └── logger.go
│   ├── model/
│   │   └── user.go                      # NEW - User struct
│   ├── repository/
│   │   └── user.go                      # NEW - database queries
│   ├── server/
│   │   └── server.go                    # NEW - HTTP server
│   └── service/
│       └── user.go                      # NEW - business logic
├── go.mod
├── go.sum
└── .env.local
```

---

## Testing with curl

Start the server:

```bash
cd apps/backend
go run cmd/main.go
```

You should see logs like:

```
INF successfully connected to postgres
INF migrations completed successfully
INF successfully connected to redis
INF server starting port=8080
```

### Health Check

```bash
curl -s http://localhost:8080/health | jq
```

```json
{
  "status": "ok"
}
```

### Register a User

```bash
curl -s -X POST http://localhost:8080/api/users \
  -H "Content-Type: application/json" \
  -d '{"username": "prateek"}' | jq
```

```json
{
  "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "username": "prateek",
  "created_at": "2026-04-11T10:30:00.123456Z"
}
```

The response status is **201 Created**.

### Get User by ID

Use the `id` from the registration response:

```bash
curl -s http://localhost:8080/api/users/a1b2c3d4-e5f6-7890-abcd-ef1234567890 | jq
```

```json
{
  "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "username": "prateek",
  "created_at": "2026-04-11T10:30:00.123456Z"
}
```

### Duplicate Username (409 Conflict)

```bash
curl -s -X POST http://localhost:8080/api/users \
  -H "Content-Type: application/json" \
  -d '{"username": "prateek"}' | jq
```

```json
{
  "error": "username already taken"
}
```

Status: **409 Conflict**.

### Validation Error (400)

```bash
curl -s -X POST http://localhost:8080/api/users \
  -H "Content-Type: application/json" \
  -d '{"username": "ab"}' | jq
```

```json
{
  "error": "validation error: username must be at least 3 characters"
}
```

### User Not Found (404)

```bash
curl -s http://localhost:8080/api/users/00000000-0000-0000-0000-000000000000 | jq
```

```json
{
  "error": "user not found"
}
```

### Graceful Shutdown

Press `Ctrl+C` while the server is running:

```
INF shutdown signal received signal=interrupt
INF server stopped gracefully
```

The server waits for any in-flight requests to complete (up to 15 seconds) before exiting.

---

## Acceptance Checklist

Run through these in order to verify everything works:

- [ ] `go run cmd/main.go` starts the server without errors
- [ ] `GET /health` returns `{"status":"ok"}` with 200
- [ ] `POST /api/users` with `{"username":"alice"}` returns 201 with user JSON including a UUID and timestamp
- [ ] `GET /api/users/{id}` with the returned UUID returns 200 with the same user
- [ ] `POST /api/users` with `{"username":"alice"}` again returns 409 with `"username already taken"`
- [ ] `POST /api/users` with `{"username":"ab"}` returns 400 with validation error
- [ ] `POST /api/users` with `{}` returns 400 with "username is required"
- [ ] `GET /api/users/not-a-uuid` returns 400 with "invalid user id"
- [ ] `GET /api/users/00000000-0000-0000-0000-000000000000` returns 404
- [ ] `Ctrl+C` logs graceful shutdown messages and exits cleanly

---

## What is Next

With the HTTP server running and user registration working, we have the foundation in place. In Step 3, we will add WebSocket support -- upgrading HTTP connections to persistent, bidirectional channels for real-time messaging. The graceful shutdown we built here will ensure those WebSocket connections are drained cleanly during deploys.
