package platform

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/gofiber/fiber/v3"
)

// App bundles the HTTP server, database pool and shared dependencies.
type App struct {
	fiber  *fiber.App
	config *Config
	logger *slog.Logger
	db     *DB
	mu     sync.Mutex
}

// NewApp builds the Fiber application, registers middleware and routes.
func NewApp(config *Config, logger *slog.Logger, db *DB) *App {
	fiberApp := fiber.New(fiber.Config{
		ErrorHandler: ErrorHandler(logger),
		ReadTimeout:  config.HTTP.ReadTimeout,
		WriteTimeout: config.HTTP.WriteTimeout,
		IdleTimeout:  config.HTTP.IdleTimeout,
		BodyLimit:    config.HTTPBodyLimitForUploads(),
		ServerHeader: "test-api",
	})

	fiberApp.Use(RequestID())
	fiberApp.Use(AccessLogger(logger))
	fiberApp.Use(Recovery(logger))

	health := Health{DB: db}
	fiberApp.Get("/health/live", health.Live)
	fiberApp.Get("/health/ready", health.Ready)

	return &App{
		fiber:  fiberApp,
		config: config,
		logger: logger,
		db:     db,
	}
}

// Start listens on the configured address. It blocks until the listener is
// closed (for example by Shutdown).
func (a *App) Start() error {
	addr := net.JoinHostPort(a.config.HTTP.Host, fmt.Sprintf("%d", a.config.HTTP.Port))
	return a.fiber.Listen(addr)
}

// Shutdown gracefully stops the HTTP server. It honors the context deadline.
func (a *App) Shutdown(ctx context.Context) error {
	return a.fiber.ShutdownWithContext(ctx)
}

// Fiber returns the underlying Fiber application for route registration.
func (a *App) Fiber() *fiber.App {
	return a.fiber
}

// DB returns the database connection pool.
func (a *App) DB() *DB {
	return a.db
}

// Config returns the active configuration.
func (a *App) Config() *Config {
	return a.config
}

// Logger returns the structured logger.
func (a *App) Logger() *slog.Logger {
	return a.logger
}
