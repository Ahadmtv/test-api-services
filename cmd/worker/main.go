// Command worker اجرای دوره‌ای کارهای عملیاتی سامانه است؛ همان کارهایی که
// به‌صورت فرمان یک‌باره در cmd/api هم در دسترس‌اند برای زمان‌بند بیرونی.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"test-api/internal/booking"
	"test-api/internal/catalog"
	"test-api/internal/payments"
	"test-api/internal/payouts"
	"test-api/internal/platform"
	"test-api/internal/providers"
	"test-api/internal/scheduling"
	"test-api/internal/wallet"
)

const defaultInterval = 300 * time.Second

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "worker: %v\n", err)
		os.Exit(1)
	}
}

type job struct {
	name             string
	run              func(context.Context, *platform.DB, *platform.Config, *slog.Logger) error
	enabledByDefault bool
}

func run(ctx context.Context) error {
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
	if !enabled("WORKER_ENABLED", true) {
		logger.Info("worker disabled by configuration")
		return nil
	}
	interval := defaultInterval
	if raw := os.Getenv("WORKER_INTERVAL_SECONDS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			return fmt.Errorf("WORKER_INTERVAL_SECONDS must be a positive integer")
		}
		interval = time.Duration(parsed) * time.Second
	}

	db, err := platform.ConnectDB(ctx, cfg.DB, logger)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer db.Close()

	jobs := []job{
		{name: "payments-reconcile", run: runPaymentsReconcile, enabledByDefault: true},
		{name: "bookings-expire-payments", run: runExpirePendingPayments, enabledByDefault: true},
		{name: "bookings-settle-expired", run: runSettleDisputeWindows, enabledByDefault: true},
		{name: "payouts-reconcile", run: runPayoutsReconcile, enabledByDefault: true},
	}
	active := make([]job, 0, len(jobs))
	for _, item := range jobs {
		if enabled("WORKER_"+envFlagName(item.name)+"_ENABLED", item.enabledByDefault) {
			active = append(active, item)
		} else {
			logger.Info("job disabled by configuration", "job", item.name)
		}
	}

	logger.Info("worker started", "interval", interval.String(), "jobs", len(active))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	executeAll(ctx, active, db, cfg, logger)
	for {
		select {
		case <-ctx.Done():
			logger.Info("worker stopped")
			return nil
		case <-ticker.C:
			executeAll(ctx, active, db, cfg, logger)
		}
	}
}

func executeAll(ctx context.Context, jobs []job, db *platform.DB, cfg *platform.Config, logger *slog.Logger) {
	for _, item := range jobs {
		if err := ctx.Err(); err != nil {
			return
		}
		started := time.Now()
		if err := item.run(ctx, db, cfg, logger); err != nil {
			logger.Error("job failed", "job", item.name, "duration", time.Since(started).String(), "error", err)
			continue
		}
		logger.Info("job completed", "job", item.name, "duration", time.Since(started).String())
	}
}

func runPaymentsReconcile(ctx context.Context, db *platform.DB, cfg *platform.Config, logger *slog.Logger) error {
	gateway, err := payments.NewZarinpalGateway(cfg.Payments.MerchantID, payments.ZarinpalEnvironment(cfg.Payments.Environment))
	if err != nil {
		return fmt.Errorf("payment gateway configuration: %w", err)
	}
	walletService := wallet.NewService(db.Pool())
	bookingService := booking.NewService(db.Pool(), providers.NewService(db.Pool()), catalog.NewStore(db.Pool()), scheduling.NewStore(db.Pool()), bookingConfigFrom(cfg), walletService)
	service := payments.NewService(db.Pool(), walletService, gateway, cfg.Payments.CallbackURL, bookingService)
	resolved, err := service.ReconcileUnknown(ctx, 100)
	if err != nil {
		return fmt.Errorf("reconcile uncertain payments: %w", err)
	}
	report, err := service.UnresolvedReport(ctx)
	if err != nil {
		return fmt.Errorf("summarize unresolved payments: %w", err)
	}
	logger.Info("payment reconciliation completed", "resolved", resolved, "unresolved", report)
	return nil
}

func runExpirePendingPayments(ctx context.Context, db *platform.DB, cfg *platform.Config, logger *slog.Logger) error {
	bookingSvc := booking.NewService(db.Pool(), providers.NewService(db.Pool()), catalog.NewStore(db.Pool()), scheduling.NewStore(db.Pool()), bookingConfigFrom(cfg))
	expired, err := bookingSvc.ExpirePendingPayments(ctx, 100)
	if err != nil {
		return fmt.Errorf("expire unpaid bookings: %w", err)
	}
	logger.Info("pending booking payment expiry completed", "expired", expired)
	return nil
}

func runSettleDisputeWindows(ctx context.Context, db *platform.DB, cfg *platform.Config, logger *slog.Logger) error {
	bookingSvc := booking.NewService(db.Pool(), providers.NewService(db.Pool()), catalog.NewStore(db.Pool()), scheduling.NewStore(db.Pool()), bookingConfigFrom(cfg))
	settled, err := bookingSvc.ExpireDisputeWindows(ctx, cfg.Booking.CompletionDisputeWindow, 100)
	if err != nil {
		return fmt.Errorf("settle expired dispute windows: %w", err)
	}
	logger.Info("dispute window settlement completed", "settled", settled)
	return nil
}

func runPayoutsReconcile(ctx context.Context, db *platform.DB, cfg *platform.Config, logger *slog.Logger) error {
	if err := payouts.NewService(db.Pool(), providers.NewService(db.Pool()), cfg.Payouts.MinAmountIRR).Verify(ctx); err != nil {
		return fmt.Errorf("payout reconciliation: %w", err)
	}
	if err := wallet.NewService(db.Pool()).Reconcile(ctx); err != nil {
		return fmt.Errorf("wallet reconciliation: %w", err)
	}
	logger.Info("payout reconciliation completed")
	return nil
}

func bookingConfigFrom(cfg *platform.Config) booking.Config {
	return booking.Config{
		CancellationCutoff:      cfg.Booking.CancellationCutoff,
		CompletionDisputeWindow: cfg.Booking.CompletionDisputeWindow,
		PendingPaymentTTL:       cfg.Booking.PendingPaymentTTL,
		Availability: scheduling.AvailabilityPolicy{
			SlotIncrement:  cfg.Availability.SlotIncrement,
			MinimumNotice:  cfg.Availability.MinimumNotice,
			MaximumHorizon: cfg.Availability.MaximumHorizon,
		},
	}
}

func enabled(name string, fallback bool) bool {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return value
}

// envFlagName turns "payments-reconcile" into "PAYMENTS_RECONCILE".
func envFlagName(jobName string) string {
	out := make([]rune, 0, len(jobName))
	for _, r := range jobName {
		if r == '-' {
			out = append(out, '_')
			continue
		}
		if r >= 'a' && r <= 'z' {
			out = append(out, r-'a'+'A')
			continue
		}
		out = append(out, r)
	}
	return string(out)
}
