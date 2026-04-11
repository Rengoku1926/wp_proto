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
