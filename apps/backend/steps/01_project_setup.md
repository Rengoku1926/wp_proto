# Step 1 -- Project Setup

This step takes you from an empty directory to a running Go backend wired to Postgres and Redis. By the end you will have: a Go module, Docker services, configuration loading, structured logging, database connections with pooling, schema migrations, and live-reload during development.

---

## 1. Initialize the Go Module

```bash
mkdir -p apps/backend && cd apps/backend
go mod init github.com/Rengoku1926/wp_proto/apps/backend
```

This creates `go.mod` and declares the import path every internal package will use.

---

## 2. Docker Compose -- Postgres 16 and Redis 7

> **File:** `docker-compose.yml`

```yaml
version: "3.9"

services:
  postgres:
    image: postgres:16
    container_name: wp_proto_postgres
    restart: unless-stopped
    environment:
      POSTGRES_USER: root
      POSTGRES_PASSWORD: root
      POSTGRES_DB: wp_proto
    ports:
      - "5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data

  redis:
    image: redis:7-alpine
    container_name: wp_proto_redis
    restart: unless-stopped
    ports:
      - "6379:6379"

volumes:
  pgdata:
```

**Why:** We need Postgres for durable message storage and Redis for pub/sub fan-out and ephemeral state (presence, typing indicators). Named volumes keep data across restarts.

Start them:

```bash
docker compose up -d
```

---

## 3. Install Dependencies

```bash
go get github.com/jackc/pgx/v5
go get github.com/jackc/tern/v2
go get github.com/redis/go-redis/v9
go get github.com/rs/zerolog
go get github.com/joho/godotenv
go get github.com/gorilla/websocket
go get github.com/google/uuid
```

| Package             | Role                                                     |
| ------------------- | -------------------------------------------------------- |
| `pgx/v5`            | Postgres driver with connection pooling (`pgxpool`)      |
| `tern/v2`           | Lightweight, SQL-file-based migration runner             |
| `go-redis/v9`       | Redis client                                             |
| `zerolog`           | Zero-allocation structured logger                        |
| `godotenv`          | Loads `.env.local` into `os.Getenv`                      |
| `gorilla/websocket` | Production-grade WebSocket upgrade (used in later steps) |
| `google/uuid`       | UUID generation (used in later steps)                    |

---

## 4. Environment Variables

> **File:** `.env.local`

```env
WP_SERVER.ENV = development

WP_SERVER.PORT = 8080
WP_SERVER.READ_TIMEOUT = 30
WP_SERVER.WRITE_TIMEOUT = 30
WP_SERVER.IDLE_TIMEOUT = 60
WP_SERVER.CORS_ALLOWED_ORIGINS = http://localhost:3000

WP_SERVER.DB_DSN=postgres://root:root@localhost:5432/wp_proto?sslmode=disable
WP_SERVER.HOST=localhost
WP_SERVER.DB_PORT=5432
WP_SERVER.USER=root
WP_SERVER.PASSWORD=root
WP_SERVER.NAME=wp_proto
WP_SERVER.SSL_MODE=disable
WP_SERVER.MAX_OPEN_CONNS=25
WP_SERVER.MAX_IDLE_CONNS=10
WP_SERVER.CONN_MAX_LIFETIME=300
WP_SERVER.CONN_MAX_IDLE_TIME=60

WP_SERVER.SECRET_KEY=

WP_SERVER.REDIS_ADDR=localhost:6379
WP_SERVER.REDIS_PASSWORD=
```

**Why:** Every tunable lives here. The `WP_SERVER.` prefix namespaces our vars so they never collide with system env. `.env.local` should be in `.gitignore` -- it holds secrets.

---

## 5. Config Loading

### 5a. Config struct

> **File:** `internal/config/interface.go`

```go
package config

type Config struct {
	Primary     Primary           `koanf:"primary" validate:"required"`
	Server      ServerConfig      `koanf:"server" validate:"required"`
	Database    DatabaseConfig    `koanf:"database" validate:"required"`
	Auth        AuthConfig        `koanf:"auth" validate:"required"`
	Redis       RedisConfig       `koanf:"redis" validate:"required"`
	Integration IntegrationConfig `koanf:"integration" validate:"required"`
	AWS         AWSConfig         `koanf:"aws" validate:"required"`
	Cron        *CronConfig       `koanf:"cron"`
}

type Primary struct {
	Env string `koanf:"env" validate:"required"`
}

type ServerConfig struct {
	Port               string   `koanf:"port" validate:"required"`
	ReadTimeout        int      `koanf:"read_timeout" validate:"required"`
	WriteTimeout       int      `koanf:"write_timeout" validate:"required"`
	IdleTimeout        int      `koanf:"idle_timeout" validate:"required"`
	CORSAllowedOrigins []string `koanf:"cors_allowed_origins" validate:"required"`
}

type DatabaseConfig struct {
	Host            string `koanf:"host" validate:"required"`
	Port            int    `koanf:"port" validate:"required"`
	User            string `koanf:"user" validate:"required"`
	Password        string `koanf:"password"`
	Name            string `koanf:"name" validate:"required"`
	SSLMode         string `koanf:"ssl_mode" validate:"required"`
	MaxOpenConns    int    `koanf:"max_open_conns" validate:"required"`
	MaxIdleConns    int    `koanf:"max_idle_conns" validate:"required"`
	ConnMaxLifetime int    `koanf:"conn_max_lifetime" validate:"required"`
	ConnMaxIdleTime int    `koanf:"conn_max_idle_time" validate:"required"`
}

type RedisConfig struct {
	Address  string `koanf:"address" validate:"required"`
	Password string `koanf:"password"`
}

type IntegrationConfig struct {
	ResendAPIKey string `koanf:"resend_api_key" validate:"required"`
}

type AuthConfig struct {
	SecretKey string `koanf:"secret_key" validate:"required"`
}

type AWSConfig struct {
	Region          string `koanf:"region" validate:"required"`
	AccessKeyID     string `koanf:"access_key_id" validate:"required"`
	SecretAccessKey string `koanf:"secret_access_key" validate:"required"`
	UploadBucket    string `koanf:"upload_bucket" validate:"required"`
	EndpointURL     string `koanf:"endpoint_url"`
}

type CronConfig struct {
	ArchiveDaysThreshold        int `koanf:"archive_days_threshold"`
	BatchSize                   int `koanf:"batch_size"`
	ReminderHours               int `koanf:"reminder_hours"`
	MaxTodosPerUserNotification int `koanf:"max_todos_per_user_notification"`
}
```

**Why:** A single, strongly-typed struct is the source of truth for every setting in the system. Struct tags are forward-compatible with koanf if we switch config backends later. Separate sub-structs keep concerns isolated -- database settings never leak into auth code.

### 5b. Config loader

> **File:** `internal/config/config.go`

```go
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

func Load(path string) (*Config, error) {
	if err := godotenv.Load(path); err != nil {
		return nil, fmt.Errorf("failed to load env file: %w", err)
	}

	cfg := &Config{
		Primary: Primary{
			Env: getEnv("WP_SERVER.ENV", "development"),
		},
		Server: ServerConfig{
			Port:               getEnv("WP_SERVER.PORT", "8080"),
			ReadTimeout:        getEnvInt("WP_SERVER.READ_TIMEOUT", 30),
			WriteTimeout:       getEnvInt("WP_SERVER.WRITE_TIMEOUT", 30),
			IdleTimeout:        getEnvInt("WP_SERVER.IDLE_TIMEOUT", 60),
			CORSAllowedOrigins: strings.Split(getEnv("WP_SERVER.CORS_ALLOWED_ORIGINS", "http://localhost:3000"), ","),
		},
		Database: DatabaseConfig{
			Host:            getEnv("WP_SERVER.HOST", "localhost"),
			Port:            getEnvInt("WP_SERVER.DB_PORT", 5432),
			User:            getEnv("WP_SERVER.USER", "root"),
			Password:        getEnv("WP_SERVER.PASSWORD", ""),
			Name:            getEnv("WP_SERVER.NAME", "wp_proto"),
			SSLMode:         getEnv("WP_SERVER.SSL_MODE", "disable"),
			MaxOpenConns:    getEnvInt("WP_SERVER.MAX_OPEN_CONNS", 25),
			MaxIdleConns:    getEnvInt("WP_SERVER.MAX_IDLE_CONNS", 10),
			ConnMaxLifetime: getEnvInt("WP_SERVER.CONN_MAX_LIFETIME", 300),
			ConnMaxIdleTime: getEnvInt("WP_SERVER.CONN_MAX_IDLE_TIME", 60),
		},
		Auth: AuthConfig{
			SecretKey: getEnv("WP_SERVER.SECRET_KEY", ""),
		},
		Redis: RedisConfig{
			Address:  getEnv("WP_SERVER.REDIS_ADDR", "localhost:6379"),
			Password: getEnv("WP_SERVER.REDIS_PASSWORD", ""),
		},
	}

	return cfg, nil
}

func (d DatabaseConfig) DSN() string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s",
		d.User, d.Password, d.Host, d.Port, d.Name, d.SSLMode,
	)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return i
		}
	}
	return fallback
}
```

**Why:** `godotenv.Load` injects `.env.local` values into the process environment, then we read them with `os.Getenv`. The helper functions (`getEnv`, `getEnvInt`) provide type-safe defaults so the app never panics on a missing optional var. The `DSN()` method on `DatabaseConfig` builds the Postgres connection string in one place -- every consumer calls `cfg.Database.DSN()` instead of formatting it themselves.

---

## 6. Logger

> **File:** `internal/logger/logger.go`

```go
package logger

import (
	"os"
	"time"

	"github.com/rs/zerolog"
)

func New(env string) zerolog.Logger {
	if env == "development" {
		return zerolog.New(zerolog.ConsoleWriter{
			Out:        os.Stdout,
			TimeFormat: time.RFC3339,
		}).With().Timestamp().Caller().Logger()
	}

	return zerolog.New(os.Stdout).With().Timestamp().Logger()
}
```

**Why:** In development we want coloured, human-readable output with caller info (file + line number) for fast debugging. In production we emit structured JSON that log aggregators (Datadog, Loki, CloudWatch) can index and query. Zerolog is zero-allocation, so it adds negligible overhead in hot paths like message delivery.

---

## 7. Database Connection

> **File:** `internal/database/database.go`

```go
package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/config"
)

func Connect(ctx context.Context, cfg config.DatabaseConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("failed to parse database config: %w", err)
	}

	poolCfg.MaxConns = int32(cfg.MaxOpenConns)
	poolCfg.MinConns = int32(cfg.MaxIdleConns)
	poolCfg.MaxConnLifetime = time.Duration(cfg.ConnMaxLifetime) * time.Second
	poolCfg.MaxConnIdleTime = time.Duration(cfg.ConnMaxIdleTime) * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return pool, nil
}
```

**Why:** `pgxpool` manages a pool of connections internally -- we never open/close individual connections ourselves. The pool settings are critical for a messaging backend:

- **MaxConns (25):** Caps total connections so we never exhaust Postgres `max_connections`.
- **MinConns (10):** Keeps warm connections ready so the first message after an idle period does not pay the TCP+TLS handshake cost.
- **MaxConnLifetime (300s):** Rotates connections to pick up DNS changes and avoid stale server-side state.
- **MaxConnIdleTime (60s):** Reclaims connections that have been sitting unused, freeing Postgres resources.

The `Ping` call at the end is a fail-fast check -- if the database is unreachable we find out at startup, not when the first user sends a message.

---

## 8. Redis Connection

> **File:** `internal/database/redis.go`

```go
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
```

**Why:** Redis serves two roles in our architecture: (1) pub/sub for broadcasting messages across multiple server instances, and (2) caching presence/typing state that does not need durability. The `Ping` at startup confirms Redis is reachable before we accept traffic.

---

## 9. Migrations

### 9a. Migration runner

> **File:** `internal/database/migration.go`

```go
package database

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/tern/v2/migrate"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("failed to acquire connection: %w", err)
	}
	defer conn.Release()

	migrator, err := migrate.NewMigrator(ctx, conn.Conn(), "schema_version")
	if err != nil {
		return fmt.Errorf("failed to create migrator: %w", err)
	}

	migrationsFS, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("failed to access migrations directory: %w", err)
	}

	if err := migrator.LoadMigrations(migrationsFS); err != nil {
		return fmt.Errorf("failed to load migrations: %w", err)
	}

	if err := migrator.Migrate(ctx); err != nil {
		return fmt.Errorf("failed to run migrations: %w", err)
	}

	return nil
}
```

**Why:** Migrations are embedded into the binary with `//go:embed` so the deployed artifact is fully self-contained -- no separate migration files to ship. Tern tracks applied migrations in a `schema_version` table and only runs new ones. We acquire a single connection from the pool (not the pool itself) because tern operates on a raw `*pgx.Conn`.

### 9b. First migration -- users table

> **File:** `internal/database/migrations/001_create_users.sql`

```sql
-- Write your migrate up statements here
CREATE TABLE users (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username   TEXT UNIQUE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
---- create above / drop below ----

-- Write your migrate down statements here. If this migration is irreversible
-- Then delete the separator line above.
```

**Why:** The `---- create above / drop below ----` separator is tern's convention for splitting up/down migrations in a single file. We use `UUID` primary keys (via Postgres `gen_random_uuid()`) because they are safe for distributed ID generation -- no coordination needed between app instances. `TIMESTAMPTZ` stores timestamps in UTC, avoiding timezone bugs.

---

## 10. Air -- Live Reload

> **File:** `.air.toml`

```toml
root = "."
tmp_dir = "tmp"

[build]
cmd = "go build -o ./tmp/main ./cmd/main.go"
bin = "./tmp/main"
delay = 1000
exclude_dir = ["tmp", "vendor", "node_modules"]
exclude_regex = ["_test.go"]
include_ext = ["go", "toml", "env"]
kill_delay = "0s"
stop_on_error = true

[log]
time = false

[misc]
clean_on_exit = true
```

**Why:** Air watches for file changes and rebuilds automatically. The `include_ext` list ensures we also restart when `.env.local` or `.air.toml` itself changes. `stop_on_error = true` prevents running a stale binary when the build fails. Install Air with:

```bash
go install github.com/air-verse/air@latest
```

---

## 11. main.go -- Wire Everything Together

> **File:** `cmd/main.go`

```go
package main

import (
	"context"
	"os"

	"github.com/Rengoku1926/wp_proto/apps/backend/internal/config"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/database"
	"github.com/Rengoku1926/wp_proto/apps/backend/internal/logger"
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

	_ = pool
	_ = redisClient
}
```

**Why:** This is the composition root. Every dependency is created here and passed down -- no globals, no init() magic. The startup order matters: config first (everything depends on it), then logger (so we can report errors), then database (migrations need a pool), then Redis. `defer pool.Close()` and `defer redisClient.Close()` ensure clean shutdown. The `_ = pool` and `_ = redisClient` lines suppress unused-variable warnings until we wire up the HTTP server in the next step.

---

## Project Structure After Step 1

```
apps/backend/
├── .air.toml
├── .env.local                          # git-ignored
├── docker-compose.yml
├── go.mod
├── go.sum
├── cmd/
│   └── main.go
└── internal/
    ├── config/
    │   ├── interface.go                # Config struct definitions
    │   └── config.go                   # Load() + DSN() + env helpers
    ├── database/
    │   ├── database.go                 # Postgres pool
    │   ├── redis.go                    # Redis client
    │   ├── migration.go                # Tern migration runner
    │   └── migrations/
    │       └── 001_create_users.sql
    └── logger/
        └── logger.go                   # Zerolog setup
```

---

## Acceptance Test

1. Start the infrastructure:

```bash
docker compose up -d
```

2. Run the app:

```bash
go run cmd/main.go
```

3. Expected output (coloured in your terminal):

```
<timestamp> INF successfully connected to postgres caller=cmd/main.go:28
<timestamp> INF migrations completed successfully caller=cmd/main.go:34
<timestamp> INF successfully connected to redis caller=cmd/main.go:41
```

4. Verify the migration created the table:

```bash
docker exec -it wp_proto_postgres psql -U root -d wp_proto -c '\dt'
```

Expected:

```
          List of relations
 Schema |      Name      | Type  | Owner
--------+----------------+-------+-------
 public | schema_version | table | root
 public | users          | table | root
(2 rows)
```

5. For live-reload during development:

```bash
air
```

If all three log lines appear and the `users` table exists, Step 1 is complete. You have a working Go backend connected to Postgres (with migrations) and Redis, ready for the HTTP server and WebSocket layer in Step 2.
