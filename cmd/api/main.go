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

	"github.com/gofiber/fiber/v3"

	"test-api/internal/auth"
	"test-api/internal/booking"
	"test-api/internal/catalog"
	"test-api/internal/identity"
	"test-api/internal/payments"
	"test-api/internal/payouts"
	"test-api/internal/platform"
	"test-api/internal/providerapi"
	"test-api/internal/providers"
	"test-api/internal/scheduling"
	"test-api/internal/wallet"
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
		if args[0] == "payments-reconcile" {
			gateway, err := payments.NewZarinpalGateway(cfg.Payments.MerchantID, payments.ZarinpalEnvironment(cfg.Payments.Environment))
			if err != nil {
				return fmt.Errorf("payment gateway configuration: %w", err)
			}
			service := payments.NewService(db.Pool(), wallet.NewService(db.Pool()), gateway, cfg.Payments.CallbackURL)
			resolved, err := service.ReconcileUnknown(ctx, 100)
			if err != nil {
				return fmt.Errorf("reconcile uncertain payments: %w", err)
			}
			report, err := service.UnresolvedReport(ctx)
			if err != nil {
				return fmt.Errorf("summarize unresolved payments: %w", err)
			}
			logger.Info("payment reconciliation completed",
				"resolved", resolved,
				"unresolved", report,
			)
			return nil
		}
		if args[0] == "bookings-settle-expired" {
			bookingSvc := booking.NewService(db.Pool(), providers.NewService(db.Pool()), catalog.NewStore(db.Pool()), scheduling.NewStore(db.Pool()), bookingConfigFrom(cfg))
			settled, err := bookingSvc.ExpireDisputeWindows(ctx, cfg.Booking.CompletionDisputeWindow, 100)
			if err != nil {
				return fmt.Errorf("settle expired dispute windows: %w", err)
			}
			logger.Info("dispute window settlement completed", "settled", settled)
			return nil
		}
		if args[0] == "payouts-reconcile" {
			if err := payouts.NewService(db.Pool(), providers.NewService(db.Pool()), cfg.Payouts.MinAmountIRR).Verify(ctx); err != nil {
				return fmt.Errorf("payout reconciliation: %w", err)
			}
			if err := wallet.NewService(db.Pool()).Reconcile(ctx); err != nil {
				return fmt.Errorf("wallet reconciliation: %w", err)
			}
			logger.Info("payout reconciliation completed")
			return nil
		}
		return runMigration(ctx, args[0], db, logger)
	}

	store := identity.NewPgxStore(db.Pool())
	service := identity.NewService(identity.Config{
		Store:             store,
		PasswordParams:    identity.DefaultParams,
		AccessTokenSecret: cfg.Auth.AccessTokenSecret,
		AccessTokenTTL:    cfg.Auth.AccessTokenTTL,
		RefreshTokenTTL:   cfg.Auth.RefreshTokenTTL,
		Logger:            logger,
	})

	rateLimiter := auth.NewRateLimiter(cfg.Auth.RateLimitPerMinute)
	handler := auth.NewHandler(service, logger, rateLimiter, int64(cfg.Auth.AccessTokenTTL.Seconds()))

	app := platform.NewApp(cfg, logger, db)
	occupancy := scheduling.NewOccupancyStore(db.Pool())
	policy := scheduling.AvailabilityPolicy{
		SlotIncrement:  cfg.Availability.SlotIncrement,
		MinimumNotice:  cfg.Availability.MinimumNotice,
		MaximumHorizon: cfg.Availability.MaximumHorizon,
	}
	providerHandler := providerapi.NewHandler(
		providers.NewService(db.Pool()),
		catalog.NewStore(db.Pool()),
		scheduling.NewStore(db.Pool()),
		occupancy,
		policy,
	)
	providerHandler.Register(app.Fiber(), cfg.Auth.AccessTokenSecret)
	walletService := wallet.NewService(db.Pool())
	wallet.NewHandler(walletService).Register(app.Fiber(), cfg.Auth.AccessTokenSecret)
	paymentGateway, err := payments.NewZarinpalGateway(cfg.Payments.MerchantID, payments.ZarinpalEnvironment(cfg.Payments.Environment))
	if err != nil {
		return fmt.Errorf("payment gateway configuration: %w", err)
	}
	payments.NewHandler(payments.NewService(db.Pool(), walletService, paymentGateway, cfg.Payments.CallbackURL), rateLimiter).Register(app.Fiber(), cfg.Auth.AccessTokenSecret)
	bookingService := booking.NewService(db.Pool(), providers.NewService(db.Pool()), catalog.NewStore(db.Pool()), scheduling.NewStore(db.Pool()), bookingConfigFrom(cfg), walletService)
	bookingHandler := booking.NewHandler(bookingService, bookingConfigFrom(cfg))
	bookingHandler.Register(app.Fiber(), cfg.Auth.AccessTokenSecret)
	payouts.NewHandler(payouts.NewService(db.Pool(), providers.NewService(db.Pool()), cfg.Payouts.MinAmountIRR)).Register(app.Fiber(), cfg.Auth.AccessTokenSecret)

	authGroup := app.Fiber().Group("/api/v1/auth")
	authGroup.Post("/register", handler.Register)
	authGroup.Post("/login", handler.Login)
	authGroup.Post("/refresh", handler.Refresh)
	authGroup.Post("/logout", handler.Logout)

	app.Fiber().Get("/api/v1/me", auth.Authenticator(cfg.Auth.AccessTokenSecret), handler.Me)

	// Admin-only diagnostic route.
	adminGroup := app.Fiber().Group("/api/v1/admin")
	adminGroup.Use(auth.Authenticator(cfg.Auth.AccessTokenSecret))
	adminGroup.Use(auth.RequireRoles("admin"))
	adminGroup.Get("/whoami", func(c fiber.Ctx) error {
		principal := c.Locals(platform.PrincipalKey).(identity.Principal)
		return c.Status(200).Type("application/json").JSON(map[string]any{
			"user_id": principal.UserID,
			"roles":   principal.Roles,
		})
	})

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
	case "wallet-reconcile":
		if err := wallet.NewService(db.Pool()).Reconcile(ctx); err != nil {
			return fmt.Errorf("wallet reconciliation: %w", err)
		}
		logger.Info("wallet reconciliation completed")
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
		return fmt.Errorf("unknown command %q (expected migrate, migrate-down, migrate-status, wallet-reconcile)", command)
	}
	return nil
}

func bookingConfigFrom(cfg *platform.Config) booking.Config {
	return booking.Config{
		CancellationCutoff:      cfg.Booking.CancellationCutoff,
		CompletionDisputeWindow: cfg.Booking.CompletionDisputeWindow,
		Availability: scheduling.AvailabilityPolicy{
			SlotIncrement:  cfg.Availability.SlotIncrement,
			MinimumNotice:  cfg.Availability.MinimumNotice,
			MaximumHorizon: cfg.Availability.MaximumHorizon,
		},
	}
}
