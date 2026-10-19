package platform

import (
	"os"
	"strings"
	"testing"
	"time"
)

var _ = time.Second

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
		"DB_USER":                  "tester",
		"DB_NAME":                  "testdb",
		"DB_PASSWORD":              "secret",
		"HTTP_PORT":                "18080",
		"LOG_LEVEL":                "debug",
		"LOG_FORMAT":               "text",
		"AUTH_ACCESS_TOKEN_SECRET": "01234567890123456789012345678901",
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
	if cfg.HTTP.BodyLimit != defaultHTTPBodyLimit {
		t.Errorf("http body limit = %d, want %d", cfg.HTTP.BodyLimit, defaultHTTPBodyLimit)
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
	if cfg.Availability.SlotIncrement != 15*time.Minute || cfg.Availability.MinimumNotice != 2*time.Hour || cfg.Availability.MaximumHorizon != 90*24*time.Hour {
		t.Errorf("availability config = %+v", cfg.Availability)
	}
	if cfg.Booking.CancellationCutoff != 0 {
		t.Errorf("booking config = %+v", cfg.Booking)
	}

	dsn := cfg.DB.DSN()
	if !strings.Contains(dsn, "postgres://tester:secret@localhost:5432/testdb?sslmode=disable") {
		t.Errorf("unexpected DSN: %s", dsn)
	}
}

func TestLoadConfigAvailabilityOverrides(t *testing.T) {
	envWith(t, map[string]string{
		"DB_USER":                     "tester",
		"DB_NAME":                     "testdb",
		"AUTH_ACCESS_TOKEN_SECRET":    "01234567890123456789012345678901",
		"AVAILABILITY_SLOT_INCREMENT": "20m",
		"AVAILABILITY_MIN_NOTICE":     "0s",
		"AVAILABILITY_MAX_HORIZON":    "48h",
	})
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("load custom availability config: %v", err)
	}
	if cfg.Availability.SlotIncrement != 20*time.Minute || cfg.Availability.MinimumNotice != 0 || cfg.Availability.MaximumHorizon != 48*time.Hour {
		t.Fatalf("availability config = %+v", cfg.Availability)
	}
}

func TestLoadConfigRejectsInvalidAvailabilitySettings(t *testing.T) {
	for _, tc := range []struct {
		name, value string
	}{
		{"AVAILABILITY_SLOT_INCREMENT", "0m"},
		{"AVAILABILITY_SLOT_INCREMENT", "90s"},
		{"AVAILABILITY_MIN_NOTICE", "-1m"},
		{"AVAILABILITY_MAX_HORIZON", "1h"},
		{"AVAILABILITY_MAX_HORIZON", "invalid"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			t.Setenv("DB_USER", "tester")
			t.Setenv("DB_NAME", "testdb")
			t.Setenv("AUTH_ACCESS_TOKEN_SECRET", "01234567890123456789012345678901")
			t.Setenv("AVAILABILITY_SLOT_INCREMENT", "")
			t.Setenv("AVAILABILITY_MIN_NOTICE", "")
			t.Setenv("AVAILABILITY_MAX_HORIZON", "")
			t.Setenv(tc.name, tc.value)
			if _, err := LoadConfig(); err == nil {
				t.Fatalf("expected invalid availability setting %s=%s", tc.name, tc.value)
			}
		})
	}
}

func TestLoadConfigBookingPolicyOverrides(t *testing.T) {
	envWith(t, map[string]string{
		"DB_USER":                     "tester",
		"DB_NAME":                     "testdb",
		"AUTH_ACCESS_TOKEN_SECRET":    "01234567890123456789012345678901",
		"BOOKING_CANCELLATION_CUTOFF": "1h",
	})
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("load booking policy config: %v", err)
	}
	if cfg.Booking.CancellationCutoff != time.Hour {
		t.Fatalf("booking config = %+v", cfg.Booking)
	}
}

func TestLoadConfigRejectsInvalidBookingPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, value string
	}{
		{"BOOKING_CANCELLATION_CUTOFF", "-1m"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			t.Setenv("DB_USER", "tester")
			t.Setenv("DB_NAME", "testdb")
			t.Setenv("AUTH_ACCESS_TOKEN_SECRET", "01234567890123456789012345678901")
			t.Setenv("BOOKING_CANCELLATION_CUTOFF", "")
			t.Setenv(tc.name, tc.value)
			if _, err := LoadConfig(); err == nil {
				t.Fatalf("expected invalid booking setting %s=%s", tc.name, tc.value)
			}
		})
	}
}

func TestLoadConfigPaymentEnvironmentDefaultsToSandboxAndAllowsExplicitProduction(t *testing.T) {
	t.Setenv("DB_USER", "tester")
	t.Setenv("DB_NAME", "testdb")
	t.Setenv("AUTH_ACCESS_TOKEN_SECRET", "01234567890123456789012345678901")
	t.Setenv("PAYMENT_ENVIRONMENT", "")
	cfg, err := LoadConfig()
	if err != nil || cfg.Payments.Environment != "sandbox" {
		t.Fatalf("default payment environment=%+v error=%v", cfg, err)
	}
	t.Setenv("PAYMENT_ENVIRONMENT", "production")
	cfg, err = LoadConfig()
	if err != nil || cfg.Payments.Environment != "production" {
		t.Fatalf("production payment environment=%+v error=%v", cfg, err)
	}
	t.Setenv("PAYMENT_ENVIRONMENT", "unknown")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("unsupported payment environment accepted")
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

func TestLoadConfigAuthSecretRequired(t *testing.T) {
	envWith(t, map[string]string{
		"DB_USER":                  "tester",
		"DB_NAME":                  "testdb",
		"AUTH_ACCESS_TOKEN_SECRET": "",
	})

	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "AUTH_ACCESS_TOKEN_SECRET") {
		t.Errorf("expected AUTH_ACCESS_TOKEN_SECRET error, got: %v", err)
	}
}

func TestLoadConfigAuthSecretTooShort(t *testing.T) {
	envWith(t, map[string]string{
		"DB_USER":                  "tester",
		"DB_NAME":                  "testdb",
		"AUTH_ACCESS_TOKEN_SECRET": "short",
	})

	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "at least") {
		t.Errorf("expected too-short secret error, got: %v", err)
	}
}

var _ = os.Environ
