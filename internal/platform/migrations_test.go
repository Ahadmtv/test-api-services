package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"test-api/migrations"
)

func TestMigrationsEmbeddedListed(t *testing.T) {
	if _, err := migrations.FS.Open("0001_users.up.sql"); err != nil {
		t.Fatalf("open embedded up migration: %v", err)
	}
	if _, err := migrations.FS.Open("0001_users.down.sql"); err != nil {
		t.Fatalf("open embedded down migration: %v", err)
	}

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}

	var ups, downs []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".up.sql") {
			ups = append(ups, name)
		} else if strings.HasSuffix(name, ".down.sql") {
			downs = append(downs, name)
		}
	}

	if len(ups) == 0 {
		t.Error("no up migrations found")
	}
	if len(ups) != len(downs) {
		t.Errorf("up=%d down=%d: every migration should have a paired down", len(ups), len(downs))
	}
}

func TestNewMigratorLoadsMigrations(t *testing.T) {
	migrator, err := NewMigrator(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(migrator.migrations) == 0 {
		t.Error("expected migrations to be loaded")
	}
}

func TestMigrationsIntegration(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set; skipping database migration integration test")
	}

	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to database: %v", err)
	}
	defer adminPool.Close()

	schemaBytes := make([]byte, 8)
	if _, err := rand.Read(schemaBytes); err != nil {
		t.Fatalf("generate test schema: %v", err)
	}
	schema := "migration_test_" + hex.EncodeToString(schemaBytes)
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	defer adminPool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")

	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse database URL: %v", err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatalf("connect to test schema: %v", err)
	}
	defer pool.Close()

	migrator, err := NewMigrator(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("create migrator: %v", err)
	}

	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	statuses, err := migrator.Status(ctx)
	if err != nil {
		t.Fatalf("migrate status: %v", err)
	}
	applied := 0
	for _, s := range statuses {
		if s.Applied {
			applied++
		}
	}
	if applied != len(statuses) {
		t.Fatalf("applied migrations = %d, want %d", applied, len(statuses))
	}

	if err := migrator.Down(ctx); err != nil {
		t.Fatalf("migrate down: %v", err)
	}

	statuses, err = migrator.Status(ctx)
	if err != nil {
		t.Fatalf("migrate status after down: %v", err)
	}
	for _, s := range statuses {
		if s.Version == statuses[len(statuses)-1].Version && s.Applied {
			t.Errorf("latest migration %d should be reverted", s.Version)
		}
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("reapply latest migration: %v", err)
	}
}

func TestProviderProfileBackfillIntegration(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set; skipping provider profile backfill integration test")
	}

	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to database: %v", err)
	}
	defer adminPool.Close()

	schemaBytes := make([]byte, 8)
	if _, err := rand.Read(schemaBytes); err != nil {
		t.Fatalf("generate test schema: %v", err)
	}
	schema := "backfill_test_" + hex.EncodeToString(schemaBytes)
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	defer adminPool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")

	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse database URL: %v", err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatalf("connect to test schema: %v", err)
	}
	defer pool.Close()

	migrator, err := NewMigrator(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("create migrator: %v", err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	var userID int64
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ('ahahaja@example.test', 'x') RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("create provider user: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'provider')`, userID); err != nil {
		t.Fatalf("grant provider role: %v", err)
	}

	backfill, err := fs.ReadFile(migrations.FS, "0015_provider_profile_backfill.up.sql")
	if err != nil {
		t.Fatalf("read backfill migration: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := pool.Exec(ctx, string(backfill)); err != nil {
			t.Fatalf("run backfill pass %d: %v", i+1, err)
		}
	}

	var (
		count       int
		displayName string
		timeZone    string
		isActive    bool
		status      string
	)
	if err := pool.QueryRow(ctx, `
		SELECT count(*), min(display_name), min(time_zone), bool_or(is_active), min(verification_status)
		FROM provider_profiles WHERE provider_id = $1
	`, userID).Scan(&count, &displayName, &timeZone, &isActive, &status); err != nil {
		t.Fatalf("load backfilled profile: %v", err)
	}
	if count != 1 {
		t.Fatalf("backfilled profiles = %d, want exactly 1 after three passes", count)
	}
	if displayName != "" || timeZone != "Asia/Tehran" || isActive || status != "pending" {
		t.Fatalf("backfilled profile=(%q,%q,%v,%q), want ('','Asia/Tehran',false,pending)", displayName, timeZone, isActive, status)
	}
}
