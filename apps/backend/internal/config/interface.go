package config

type Config struct {
	Primary       Primary              `koanf:"primary" validate:"required"`
	Server        ServerConfig         `koanf:"server" validate:"required"`
	Database      DatabaseConfig       `koanf:"database" validate:"required"`
	Auth          AuthConfig           `koanf:"auth" validate:"required"`
	Redis         RedisConfig          `koanf:"redis" validate:"required"`
	// Integration   IntegrationConfig    `koanf:"integration" validate:"required"`
	// Observability *ObservabilityConfig `koanf:"observability"`
	// AWS           AWSConfig            `koanf:"aws" validate:"required"`
	// Cron          *CronConfig          `koanf:"cron"`
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

// type IntegrationConfig struct {
// 	ResendAPIKey string `koanf:"resend_api_key" validate:"required"`
// }

type AuthConfig struct {
	SecretKey string `koanf:"secret_key" validate:"required"`
}

// type AWSConfig struct {
// 	Region          string `koanf:"region" validate:"required"`
// 	AccessKeyID     string `koanf:"access_key_id" validate:"required"`
// 	SecretAccessKey string `koanf:"secret_access_key" validate:"required"`
// 	UploadBucket    string `koanf:"upload_bucket" validate:"required"`
// 	EndpointURL     string `koanf:"endpoint_url"`
// }

// type CronConfig struct {
// 	ArchiveDaysThreshold        int `koanf:"archive_days_threshold"`
// 	BatchSize                   int `koanf:"batch_size"`
// 	ReminderHours               int `koanf:"reminder_hours"`
// 	MaxTodosPerUserNotification int `koanf:"max_todos_per_user_notification"`
// }
