package platform

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pinger abstracts a database connection availability check.
type Pinger interface {
	Ping(context.Context) error
}

// DB wraps a pgx connection pool.
type DB struct {
	pool *pgxpool.Pool
}

// ConnectDB establishes a pgx connection pool with explicit pool settings and
// verifies connectivity by pinging the database.
func ConnectDB(ctx context.Context, cfg DBConfig, logger *slog.Logger) (*DB, error) {
	poolConfig, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("parse connection string: %w", err)
	}

	poolConfig.MinConns = int32(cfg.MaxIdleConns)
	poolConfig.MaxConns = int32(cfg.MaxOpenConns)
	poolConfig.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolConfig.MaxConnLifetime = cfg.MaxConnLifetime
	poolConfig.MaxConnLifetimeJitter = cfg.MaxConnLifetime / 10
	poolConfig.HealthCheckPeriod = time.Minute

	poolConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_ = conn
		logger.LogAttrs(ctx, slog.LevelDebug, "database connection established")
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return &DB{pool: pool}, nil
}

// Pool returns the underlying pgx connection pool.
func (d *DB) Pool() *pgxpool.Pool {
	return d.pool
}

// Ping verifies database availability.
func (d *DB) Ping(ctx context.Context) error {
	if d == nil || d.pool == nil {
		return fmt.Errorf("database not connected")
	}
	return d.pool.Ping(ctx)
}

// Close releases the connection pool resources.
func (d *DB) Close() {
	if d == nil || d.pool == nil {
		return
	}
	d.pool.Close()
}
