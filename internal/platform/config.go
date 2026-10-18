package platform

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrMissingConfig is returned when required environment configuration is absent.
var ErrMissingConfig = errors.New("required configuration is missing")

type Config struct {
	Env     string
	HTTP    HTTPConfig
	DB      DBConfig
	Log     LogConfig
	Version string
}

type HTTPConfig struct {
	Host         string
	Port         int
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
}

type DBConfig struct {
	Host            string
	Port            int
	User            string
	Password        string
	Name            string
	SSLMode         string
	MaxOpenConns    int
	MaxIdleConns    int
	MaxConnIdleTime time.Duration
	MaxConnLifetime time.Duration
}

type LogConfig struct {
	Level  string
	Format string
}

const (
	defaultEnv            = "development"
	defaultVersion        = "0.1.0"
	defaultHTTPHost       = "0.0.0.0"
	defaultHTTPPort       = 8080
	defaultReadTimeout    = 10 * time.Second
	defaultWriteTimeout   = 10 * time.Second
	defaultIdleTimeout    = 30 * time.Second
	defaultDBHost         = "localhost"
	defaultDBPort         = 5432
	defaultDBSSLMode      = "disable"
	defaultDBMaxOpenConns = 25
	defaultDBMaxIdleConns = 5
	defaultDBMaxConnIdle  = 10 * time.Minute
	defaultDBMaxConnLife  = 1 * time.Hour
	defaultLogLevel       = "info"
	defaultLogFormat      = "json"
)

// LoadConfig reads configuration from environment variables and validates it.
// Required variables that are missing are reported explicitly; no secret is
// ever logged.
func LoadConfig() (*Config, error) {
	cfg := &Config{
		Env:     firstNonEmpty(os.Getenv("APP_ENV"), defaultEnv),
		Version: firstNonEmpty(os.Getenv("APP_VERSION"), defaultVersion),
		HTTP: HTTPConfig{
			Host:         firstNonEmpty(os.Getenv("HTTP_HOST"), defaultHTTPHost),
			Port:         parseInt(os.Getenv("HTTP_PORT"), defaultHTTPPort, "HTTP_PORT"),
			ReadTimeout:  parseDuration(os.Getenv("HTTP_READ_TIMEOUT"), defaultReadTimeout, "HTTP_READ_TIMEOUT"),
			WriteTimeout: parseDuration(os.Getenv("HTTP_WRITE_TIMEOUT"), defaultWriteTimeout, "HTTP_WRITE_TIMEOUT"),
			IdleTimeout:  parseDuration(os.Getenv("HTTP_IDLE_TIMEOUT"), defaultIdleTimeout, "HTTP_IDLE_TIMEOUT"),
		},
		DB: DBConfig{
			Host:            firstNonEmpty(os.Getenv("DB_HOST"), defaultDBHost),
			Port:            parseInt(os.Getenv("DB_PORT"), defaultDBPort, "DB_PORT"),
			User:            os.Getenv("DB_USER"),
			Password:        os.Getenv("DB_PASSWORD"),
			Name:            os.Getenv("DB_NAME"),
			SSLMode:         firstNonEmpty(os.Getenv("DB_SSL_MODE"), defaultDBSSLMode),
			MaxOpenConns:    parseInt(os.Getenv("DB_MAX_OPEN_CONNS"), defaultDBMaxOpenConns, "DB_MAX_OPEN_CONNS"),
			MaxIdleConns:    parseInt(os.Getenv("DB_MAX_IDLE_CONNS"), defaultDBMaxIdleConns, "DB_MAX_IDLE_CONNS"),
			MaxConnIdleTime: parseDuration(os.Getenv("DB_MAX_CONN_IDLE_TIME"), defaultDBMaxConnIdle, "DB_MAX_CONN_IDLE_TIME"),
			MaxConnLifetime: parseDuration(os.Getenv("DB_MAX_CONN_LIFETIME"), defaultDBMaxConnLife, "DB_MAX_CONN_LIFETIME"),
		},
		Log: LogConfig{
			Level:  firstNonEmpty(os.Getenv("LOG_LEVEL"), defaultLogLevel),
			Format: firstNonEmpty(os.Getenv("LOG_FORMAT"), defaultLogFormat),
		},
	}

	if missing := requiredMissing(
		"DB_USER", cfg.DB.User,
		"DB_NAME", cfg.DB.Name,
	); len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrMissingConfig, strings.Join(missing, ", "))
	}

	if cfg.Log.Format != "json" && cfg.Log.Format != "text" {
		return nil, fmt.Errorf("invalid LOG_FORMAT %q (expected json or text)", cfg.Log.Format)
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Log.Level)); err != nil {
		return nil, fmt.Errorf("invalid LOG_LEVEL %q: %w", cfg.Log.Level, err)
	}
	_ = level

	return cfg, nil
}

// DSN builds a libpq-style connection string for pgx from configuration parts.
func (c DBConfig) DSN() string {
	var b strings.Builder
	b.WriteString("postgres://")
	if c.User != "" {
		b.WriteString(c.User)
	}
	if c.Password != "" {
		b.WriteString(":")
		b.WriteString(c.Password)
	}
	b.WriteString("@")
	b.WriteString(c.Host)
	b.WriteString(":")
	b.WriteString(strconv.Itoa(c.Port))
	b.WriteString("/")
	b.WriteString(c.Name)
	b.WriteString("?sslmode=")
	b.WriteString(c.SSLMode)
	return b.String()
}

func requiredMissing(namesAndValues ...string) []string {
	var missing []string
	for i := 0; i+1 < len(namesAndValues); i += 2 {
		if strings.TrimSpace(namesAndValues[i+1]) == "" {
			missing = append(missing, namesAndValues[i])
		}
	}
	return missing
}

func firstNonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func parseInt(value string, fallback int, name string) int {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}

func parseDuration(value string, fallback time.Duration, name string) time.Duration {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return d
}
