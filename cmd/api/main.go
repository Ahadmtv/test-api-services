package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"test-api/internal/platform"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "api: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := platform.LoadConfig()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	logger, err := platform.NewLogger(cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		return fmt.Errorf("logger: %w", err)
	}

	db, err := platform.ConnectDB(ctx, cfg.DB, logger)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer db.Close()

	if len(args) > 0 {
		return runMigration(ctx, args[0], db, logger)
	}

	app := platform.NewApp(cfg, logger, db)

	addr := fmt.Sprintf("%s:%d", cfg.HTTP.Host, cfg.HTTP.Port)
	go func() {
		if err := app.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server stopped unexpectedly", "address", addr, "error", err)
		}
	}()

	logger.Info("api started", "address", addr, "environment", cfg.Env, "version", cfg.Version)

	<-ctx.Done()
	logger.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown failed", "error", err)
		return err
	}
	logger.Info("server stopped")
	return nil
}

func runMigration(ctx context.Context, command string, db *platform.DB, logger *slog.Logger) error {
	migrator, err := platform.NewMigrator(db.Pool(), logger)
	if err != nil {
		return fmt.Errorf("migration loader: %w", err)
	}

	switch command {
	case "migrate":
		if err := migrator.Up(ctx); err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
	case "migrate-down":
		if err := migrator.Down(ctx); err != nil {
			return fmt.Errorf("migrate down: %w", err)
		}
	case "migrate-status":
		statuses, err := migrator.Status(ctx)
		if err != nil {
			return fmt.Errorf("migrate status: %w", err)
		}
		fmt.Println("version  name     status")
		fmt.Println("-------  ------   ------")
		for _, s := range statuses {
			state := "pending"
			if s.Applied {
				state = "applied"
			}
			fmt.Printf("%-7d %-10s %s\n", s.Version, s.Name, state)
		}
	default:
		return fmt.Errorf("unknown command %q (expected migrate, migrate-down or migrate-status)", command)
	}
	return nil
}
