package platform

import (
	"os"
	"strings"
	"testing"
	"time"
)

func envWith(t *testing.T, vars map[string]string) {
	t.Helper()
	for k, v := range vars {
		t.Setenv(k, v)
	}
}

func TestLoadConfigMissingRequired(t *testing.T) {
	envWith(t, map[string]string{
		"DB_USER": "",
		"DB_NAME": "",
	})

	cfg, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for missing required config, got nil")
	}
	if !strings.Contains(err.Error(), "DB_USER") {
		t.Errorf("error should mention DB_USER, got: %v", err)
	}
	if !strings.Contains(err.Error(), "DB_NAME") {
		t.Errorf("error should mention DB_NAME, got: %v", err)
	}
	if cfg != nil {
		t.Errorf("expected nil config on validation failure, got %v", cfg)
	}
}

func TestLoadConfigDefaultsAndDSN(t *testing.T) {
	envWith(t, map[string]string{
		"DB_USER":     "tester",
		"DB_NAME":     "testdb",
		"DB_PASSWORD": "secret",
		"HTTP_PORT":   "18080",
		"LOG_LEVEL":   "debug",
		"LOG_FORMAT":  "text",
	})

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Env != "development" {
		t.Errorf("default env = %q, want %q", cfg.Env, "development")
	}
	if cfg.Version == "" {
		t.Error("version should default")
	}
	if cfg.HTTP.Port != 18080 {
		t.Errorf("http port = %d, want 18080", cfg.HTTP.Port)
	}
	if cfg.HTTP.ReadTimeout != 10*time.Second {
		t.Errorf("read timeout = %v", cfg.HTTP.ReadTimeout)
	}
	if cfg.DB.User != "tester" || cfg.DB.Name != "testdb" || cfg.DB.Password != "secret" {
		t.Errorf("db config = %+v", cfg.DB)
	}
	if cfg.DB.MaxOpenConns != 25 {
		t.Errorf("max open conns = %d", cfg.DB.MaxOpenConns)
	}
	if cfg.Log.Level != "debug" || cfg.Log.Format != "text" {
		t.Errorf("log config = %+v", cfg.Log)
	}

	dsn := cfg.DB.DSN()
	if !strings.Contains(dsn, "postgres://tester:secret@localhost:5432/testdb?sslmode=disable") {
		t.Errorf("unexpected DSN: %s", dsn)
	}
}

func TestLoadConfigInvalidLogFormat(t *testing.T) {
	envWith(t, map[string]string{
		"DB_USER":    "tester",
		"DB_NAME":    "testdb",
		"LOG_FORMAT": "xml",
	})

	if _, err := LoadConfig(); err == nil {
		t.Error("expected error for invalid LOG_FORMAT")
	}
}

func TestLoadConfigMissingOnlyName(t *testing.T) {
	envWith(t, map[string]string{
		"DB_USER": "tester",
		"DB_NAME": "",
	})

	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "DB_NAME") {
		t.Errorf("expected DB_NAME missing error, got: %v", err)
	}
}

var _ = os.Environ
