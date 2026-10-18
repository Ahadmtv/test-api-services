package platform

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"test-api/migrations"
)

var migrationFS embed.FS = migrations.FS

var migrationNameRE = regexp.MustCompile(`^(\d{4})_(.+)\.(up|down)\.sql$`)

type migrationFile struct {
	Version int
	Name    string
	Kind    string // "up" or "down"
	Content string
}

func loadMigrations() ([]migrationFile, error) {
	entries, err := migrationFS.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("read migrations directory: %w", err)
	}

	var migrations []migrationFile
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		matches := migrationNameRE.FindStringSubmatch(entry.Name())
		if matches == nil {
			return nil, fmt.Errorf("invalid migration file name: %s", entry.Name())
		}
		content, err := migrationFS.ReadFile(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		migrations = append(migrations, migrationFile{
			Version: mustAtoi(matches[1]),
			Name:    matches[2],
			Kind:    matches[3],
			Content: string(content),
		})
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})
	return migrations, nil
}

func mustAtoi(s string) int {
	n, err := strconvAtoi(s)
	if err != nil {
		panic(err)
	}
	return n
}

func strconvAtoi(s string) (int, error) {
	var n int
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid number %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

// MigrationStatus reports whether a migration has been applied.
type MigrationStatus struct {
	Version int
	Name    string
	Applied bool
}

// Migrator applies and reverts versioned SQL migrations inside transactions.
type Migrator struct {
	pool       *pgxpool.Pool
	logger     *slog.Logger
	migrations []migrationFile
}

func NewMigrator(pool *pgxpool.Pool, logger *slog.Logger) (*Migrator, error) {
	migrations, err := loadMigrations()
	if err != nil {
		return nil, err
	}
	return &Migrator{pool: pool, logger: logger, migrations: migrations}, nil
}

// EnsureSchema creates the schema_migrations tracking table if missing.
func (m *Migrator) EnsureSchema(ctx context.Context) error {
	_, err := m.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version BIGINT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`)
	return err
}

func (m *Migrator) appliedVersions(ctx context.Context) (map[int64]struct{}, error) {
	rows, err := m.pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	applied := make(map[int64]struct{})
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		applied[version] = struct{}{}
	}
	return applied, rows.Err()
}

// Up applies all pending up migrations in version order, each inside its own
// transaction. Already applied migrations are skipped (idempotent).
func (m *Migrator) Up(ctx context.Context) error {
	if err := m.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("ensure schema: %w", err)
	}

	applied, err := m.appliedVersions(ctx)
	if err != nil {
		return fmt.Errorf("read applied migrations: %w", err)
	}

	for _, mv := range m.migrations {
		if mv.Kind != "up" {
			continue
		}
		if _, ok := applied[int64(mv.Version)]; ok {
			continue
		}

		tx, err := m.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin transaction for migration %d: %w", mv.Version, err)
		}
		err = func() error {
			if _, err := tx.Exec(ctx, mv.Content); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, mv.Version); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}()
		if err != nil {
			if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				return fmt.Errorf("migration %d failed and rollback failed: %w (rollback: %v)", mv.Version, err, rbErr)
			}
			return fmt.Errorf("apply migration %d (%s): %w", mv.Version, mv.Name, err)
		}
		m.logger.Info("migration applied", "version", mv.Version, "name", mv.Name)
	}
	return nil
}

// Down reverts the latest applied migration using its paired down SQL.
func (m *Migrator) Down(ctx context.Context) error {
	if err := m.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("ensure schema: %w", err)
	}

	var version int64
	err := m.pool.QueryRow(ctx, `SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		m.logger.Info("no migrations to revert")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read latest migration: %w", err)
	}

	var down migrationFile
	for _, mv := range m.migrations {
		if mv.Kind == "down" && mv.Version == int(version) {
			down = mv
			break
		}
	}
	if down.Content == "" {
		return fmt.Errorf("no down migration found for version %d", version)
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction for revert %d: %w", version, err)
	}
	err = func() error {
		if _, err := tx.Exec(ctx, down.Content); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, version); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}()
	if err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return fmt.Errorf("revert %d failed and rollback failed: %w (rollback: %v)", version, err, rbErr)
		}
		return fmt.Errorf("revert migration %d (%s): %w", version, down.Name, err)
	}
	m.logger.Info("migration reverted", "version", version, "name", down.Name)
	return nil
}

// Status returns every known migration with its applied state.
func (m *Migrator) Status(ctx context.Context) ([]MigrationStatus, error) {
	if err := m.EnsureSchema(ctx); err != nil {
		return nil, fmt.Errorf("ensure schema: %w", err)
	}

	applied, err := m.appliedVersions(ctx)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}

	statuses := make([]MigrationStatus, 0, len(m.migrations))
	for _, mv := range m.migrations {
		if mv.Kind != "up" {
			continue
		}
		statuses = append(statuses, MigrationStatus{
			Version: mv.Version,
			Name:    mv.Name,
			Applied: appliedContains(applied, mv.Version),
		})
	}
	return statuses, nil
}

func appliedContains(applied map[int64]struct{}, version int) bool {
	_, ok := applied[int64(version)]
	return ok
}
