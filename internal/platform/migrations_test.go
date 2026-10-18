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
