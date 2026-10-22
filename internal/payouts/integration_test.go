package payouts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgxpool"

	"test-api/internal/booking"
	"test-api/internal/catalog"
	"test-api/internal/identity"
	"test-api/internal/platform"
	"test-api/internal/providers"
	"test-api/internal/scheduling"
	"test-api/internal/wallet"
)

type payoutFixture struct {
	pool       *pgxpool.Pool
	service    *Service
	wallet     *wallet.Service
	providerID int64
	clientID   int64
	bookingSvc *booking.Service
	earnings   int64
}

func newPayoutFixture(t *testing.T) *payoutFixture {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set; skipping PostgreSQL payout integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to PostgreSQL: %v", err)
	}
	t.Cleanup(adminPool.Close)
	schemaBytes := make([]byte, 8)
	if _, err := rand.Read(schemaBytes); err != nil {
		t.Fatalf("generate schema name: %v", err)
	}
	schema := "payout_test_" + hex.EncodeToString(schemaBytes)
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() { _, _ = adminPool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatalf("connect to test schema: %v", err)
	}
	t.Cleanup(pool.Close)
	migrator, err := platform.NewMigrator(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("create migrator: %v", err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	f := &payoutFixture{pool: pool, earnings: 21_875}
	for _, user := range []*int64{&f.providerID, &f.clientID} {
		if err := pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ($1, 'test') RETURNING id`, payoutEmail(t)).Scan(user); err != nil {
			t.Fatalf("create fixture user: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO provider_profiles (provider_id, display_name, is_active, verification_status)
		VALUES ($1, 'Payout Provider', TRUE, 'verified')
	`, f.providerID); err != nil {
		t.Fatalf("create provider profile: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'provider')`, f.providerID); err != nil {
		t.Fatalf("grant provider role: %v", err)
	}
	for weekday := 1; weekday <= 7; weekday++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO weekly_working_hours (provider_id, weekday, opens_at, closes_at)
			VALUES ($1, $2, '00:00', '23:59')
		`, f.providerID, weekday); err != nil {
			t.Fatalf("create provider hours: %v", err)
		}
	}
	var serviceID int64
	if err := pool.QueryRow(ctx, `INSERT INTO services (provider_id, name, duration_minutes, price_irr) VALUES ($1, 'Service', 30, 25000) RETURNING id`, f.providerID).Scan(&serviceID); err != nil {
		t.Fatalf("create service: %v", err)
	}

	f.wallet = wallet.NewService(pool)
	if err := f.wallet.FundForDevelopment(ctx, f.clientID, 1_000_000, "fixture-"+time.Now().UTC().Format("150405.000000000")); err != nil {
		t.Fatalf("fund client wallet: %v", err)
	}

	policy := scheduling.AvailabilityPolicy{SlotIncrement: 15 * time.Minute, MinimumNotice: 0, MaximumHorizon: 90 * 24 * time.Hour}
	config := booking.Config{Availability: policy, CompletionDisputeWindow: 24 * time.Hour}
	if _, err := pool.Exec(ctx, `INSERT INTO commission_policies (version, rate_bps, effective_from) VALUES (2, 1250, now())`); err != nil {
		t.Fatalf("seed commission policy: %v", err)
	}
	f.bookingSvc = booking.NewService(pool, providers.NewService(pool), catalog.NewStore(pool), scheduling.NewStore(pool), config, f.wallet)
	if err := completeEarningsBooking(t, ctx, pool, f, serviceID, config, "first"); err != nil {
		t.Fatal(err)
	}
	if err := completeEarningsBooking(t, ctx, pool, f, serviceID, config, "second"); err != nil {
		t.Fatal(err)
	}
	f.service = NewService(pool, providers.NewService(pool), 0)
	return f
}

func completeEarningsBooking(t *testing.T, ctx context.Context, pool *pgxpool.Pool, f *payoutFixture, serviceID int64, config booking.Config, label string) error {
	t.Helper()
	start := payoutBookingStart(t)
	created, err := f.bookingSvc.Create(ctx, f.clientID, booking.CreateInput{ServiceID: serviceID, StartsAt: start}, config)
	if err != nil {
		return fmt.Errorf("create earnings booking: %w", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE bookings SET starts_at = now() - interval '1 minute', ends_at = now() + interval '29 minutes' WHERE id = $1`, created.ID); err != nil {
		return fmt.Errorf("move booking into service window: %w", err)
	}
	if _, err := f.bookingSvc.Start(ctx, f.providerID, created.ID, "start-request"); err != nil {
		return fmt.Errorf("start earnings booking: %w", err)
	}
	if _, err := f.bookingSvc.RequestCompletion(ctx, f.providerID, created.ID, "completion-request"); err != nil {
		return fmt.Errorf("request earnings completion: %w", err)
	}
	if _, err := f.bookingSvc.ConfirmCompletion(ctx, f.clientID, created.ID, "confirm-request"); err != nil {
		return fmt.Errorf("confirm earnings completion: %w", err)
	}
	return nil
}

func payoutBookingStart(t *testing.T) time.Time {
	t.Helper()
	location, err := time.LoadLocation("Asia/Tehran")
	if err != nil {
		t.Fatal(err)
	}
	day := time.Now().In(location).AddDate(0, 0, 3)
	return time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, location).UTC()
}

func payoutEmail(t *testing.T) string {
	t.Helper()
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("generate email suffix: %v", err)
	}
	return hex.EncodeToString(suffix) + "@payout.test"
}

func providerBalances(t *testing.T, pool *pgxpool.Pool, providerID int64) (earnings, reserved int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
		SELECT balance_irr FROM wallet_accounts WHERE owner_user_id = $1 AND kind = 'provider_earnings'
	`, providerID).Scan(&earnings); err != nil {
		t.Fatalf("read provider earnings: %v", err)
	}
	if err := pool.QueryRow(context.Background(), `
		SELECT balance_irr FROM wallet_accounts WHERE owner_user_id = $1 AND kind = 'provider_payout_reserved'
	`, providerID).Scan(&reserved); err != nil {
		t.Fatalf("read provider payout reservation: %v", err)
	}
	return earnings, reserved
}

func TestCreatePayoutReservesEarnings(t *testing.T) {
	f := newPayoutFixture(t)
	ctx := context.Background()

	item, replay, err := f.service.Create(ctx, CreateInput{
		ProviderID: f.providerID, AmountIRR: f.earnings, IdempotencyKey: "payout-key-1",
		RequestID: "payout-request-1", Nonce: "payout-request-1",
	})
	if err != nil {
		t.Fatalf("create payout: %v", err)
	}
	if replay {
		t.Fatal("first payout creation must not be a replay")
	}
	if item.Status != StatusRequested || item.AmountIRR != f.earnings || item.ProviderID != f.providerID {
		t.Fatalf("created payout=%+v", item)
	}

	earnings, reserved := providerBalances(t, f.pool, f.providerID)
	if earnings != f.earnings || reserved != f.earnings {
		t.Fatalf("balances after create earnings=%d reserved=%d; want %d/%d", earnings, reserved, f.earnings, f.earnings)
	}

	var reserveCount int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE kind = 'payout_reserve'`).Scan(&reserveCount); err != nil {
		t.Fatal(err)
	}
	if reserveCount != 1 {
		t.Fatalf("payout_reserve documents=%d; want 1", reserveCount)
	}

	replayed, replay, err := f.service.Create(ctx, CreateInput{
		ProviderID: f.providerID, AmountIRR: f.earnings, IdempotencyKey: "payout-key-1",
		RequestID: "payout-request-2", Nonce: "payout-request-2",
	})
	if err != nil {
		t.Fatalf("replay payout creation: %v", err)
	}
	if !replay || replayed.ID != item.ID {
		t.Fatalf("replay payout=%+v replay=%v; want original with replay=true", replayed, replay)
	}

	if err := f.service.Verify(ctx); err != nil {
		t.Fatalf("verify payouts after create: %v", err)
	}
}

func TestCreatePayoutValidation(t *testing.T) {
	f := newPayoutFixture(t)
	ctx := context.Background()

	if _, _, err := f.service.Create(ctx, CreateInput{ProviderID: f.providerID, AmountIRR: -1}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("negative amount error=%v; want ErrInvalidInput", err)
	}
	if _, _, err := f.service.Create(ctx, CreateInput{ProviderID: 999_999, AmountIRR: 100}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unknown provider error=%v; want ErrForbidden", err)
	}
	if _, _, err := f.service.Create(ctx, CreateInput{
		ProviderID: f.providerID, AmountIRR: f.earnings * 10, IdempotencyKey: "too-much", RequestID: "req", Nonce: "req",
	}); !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("insufficient funds error=%v; want wallet.ErrInsufficientFunds", err)
	}

	small := NewService(f.pool, providers.NewService(f.pool), f.earnings+1)
	if _, _, err := small.Create(ctx, CreateInput{
		ProviderID: f.providerID, AmountIRR: f.earnings, IdempotencyKey: "small", RequestID: "req", Nonce: "req",
	}); !errors.Is(err, ErrAmountTooSmall) {
		t.Fatalf("below minimum error=%v; want ErrAmountTooSmall", err)
	}

	if _, _, err := f.service.Create(ctx, CreateInput{
		ProviderID: f.providerID, AmountIRR: f.earnings, IdempotencyKey: "key",
		RequestID: "req", Nonce: "req",
	}); err != nil {
		t.Fatalf("seed payout: %v", err)
	}
	if _, _, err := f.service.Create(ctx, CreateInput{
		ProviderID: f.providerID, AmountIRR: f.earnings - 1, IdempotencyKey: "key",
		RequestID: "req", Nonce: "req",
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("idempotency conflict error=%v; want ErrIdempotencyConflict", err)
	}
	earnings, reserved := providerBalances(t, f.pool, f.providerID)
	if earnings != f.earnings || reserved != f.earnings {
		t.Fatalf("balances after conflict earnings=%d reserved=%d; want %d/%d", earnings, reserved, f.earnings, f.earnings)
	}
}

func TestCreatePayoutRejectsSecondActiveRequest(t *testing.T) {
	f := newPayoutFixture(t)
	ctx := context.Background()

	if _, _, err := f.service.Create(ctx, CreateInput{
		ProviderID: f.providerID, AmountIRR: f.earnings, IdempotencyKey: "first", RequestID: "req-1", Nonce: "req-1",
	}); err != nil {
		t.Fatalf("first payout: %v", err)
	}
	_, _, err := f.service.Create(ctx, CreateInput{
		ProviderID: f.providerID, AmountIRR: 5, IdempotencyKey: "second", RequestID: "req-2", Nonce: "req-2",
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second active payout error=%v; want ErrConflict", err)
	}

	earnings, reserved := providerBalances(t, f.pool, f.providerID)
	if earnings != f.earnings || reserved != f.earnings {
		t.Fatalf("balances after conflict earnings=%d reserved=%d; want %d/%d", earnings, reserved, f.earnings, f.earnings)
	}
	assertBalancedPayoutLedger(t, f.pool)
}

func TestCreatePayoutConcurrentRequestsReserveOnce(t *testing.T) {
	f := newPayoutFixture(t)
	ctx := context.Background()

	const workers = 6
	results := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := f.service.Create(ctx, CreateInput{
				ProviderID: f.providerID, AmountIRR: f.earnings,
				IdempotencyKey: fmt.Sprintf("worker-%d", i),
				RequestID:      fmt.Sprintf("worker-%d", i),
				Nonce:          fmt.Sprintf("worker-%d", i),
			})
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)

	created, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			created++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent payout error: %v", err)
		}
	}
	if created != 1 || conflicts != workers-1 {
		t.Fatalf("created=%d conflicts=%d; want 1 and %d", created, conflicts, workers-1)
	}
	earnings, reserved := providerBalances(t, f.pool, f.providerID)
	if earnings != f.earnings || reserved != f.earnings {
		t.Fatalf("balances after concurrency earnings=%d reserved=%d; want %d/%d", earnings, reserved, f.earnings, f.earnings)
	}
	assertBalancedPayoutLedger(t, f.pool)
}

func TestPayoutTransitionsAndReconciliation(t *testing.T) {
	f := newPayoutFixture(t)
	ctx := context.Background()

	var adminID int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ($1, 'test') RETURNING id`, payoutEmail(t)).Scan(&adminID); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'admin')`, adminID); err != nil {
		t.Fatal(err)
	}

	request, _, err := f.service.Create(ctx, CreateInput{
		ProviderID: f.providerID, AmountIRR: f.earnings, IdempotencyKey: "lifecycle",
		RequestID: "req", Nonce: "req",
	})
	if err != nil {
		t.Fatalf("create payout: %v", err)
	}

	items, more, err := f.service.List(ctx, f.providerID, 0, 10)
	if err != nil {
		t.Fatalf("list payouts: %v", err)
	}
	if len(items) < 1 || more {
		t.Fatalf("payout page items=%d more=%v; want one page covering the request", len(items), more)
	}
	if items[0].ID != request.ID {
		t.Fatalf("listed payout=%+v; want the created request", items[0])
	}

	approved, err := f.service.Approve(ctx, adminID, request.ID, "looks fine", "approve")
	if err != nil {
		t.Fatalf("approve payout: %v", err)
	}
	if approved.Status != StatusApproved {
		t.Fatalf("approved status=%s", approved.Status)
	}
	if _, err := f.service.Reject(ctx, adminID, request.ID, "", "reject-approved"); !errors.Is(err, ErrConflict) {
		t.Fatalf("reject approved error=%v; want ErrConflict", err)
	}

	processing, err := f.service.StartProcessing(ctx, adminID, request.ID, "", "processing")
	if err != nil {
		t.Fatalf("start processing: %v", err)
	}
	if processing.Status != StatusProcessing {
		t.Fatalf("processing status=%s", processing.Status)
	}

	paid, err := f.service.MarkPaid(ctx, adminID, request.ID, "TRX-77341", "wire sent", "paid-req")
	if err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	if paid.Status != StatusPaid || paid.ExternalTransferReference == nil || *paid.ExternalTransferReference != "TRX-77341" {
		t.Fatalf("paid payout=%+v", paid)
	}
	if _, err := f.service.Fail(ctx, adminID, request.ID, "", "fail-paid"); !errors.Is(err, ErrConflict) {
		t.Fatalf("fail paid payout error=%v; want ErrConflict", err)
	}

	earnings, reserved := providerBalances(t, f.pool, f.providerID)
	if earnings != f.earnings || reserved != 0 {
		t.Fatalf("balances after paid earnings=%d reserved=%d; want %d/0 (reserve moved to payout clearing)", earnings, reserved, f.earnings)
	}
	if err := f.service.Verify(ctx); err != nil {
		t.Fatalf("verify payout lifecycle: %v", err)
	}
	assertBalancedPayoutLedger(t, f.pool)
}

func TestPayoutRejectReleasesFunds(t *testing.T) {
	f := newPayoutFixture(t)
	ctx := context.Background()

	var adminID int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ($1, 'test') RETURNING id`, payoutEmail(t)).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'admin')`, adminID); err != nil {
		t.Fatal(err)
	}

	item, _, err := f.service.Create(ctx, CreateInput{
		ProviderID: f.providerID, AmountIRR: f.earnings, IdempotencyKey: "reject-key",
		RequestID: "req", Nonce: "req",
	})
	if err != nil {
		t.Fatalf("create payout: %v", err)
	}
	rejected, err := f.service.Reject(ctx, adminID, item.ID, "policy violation", "reject")
	if err != nil {
		t.Fatalf("reject payout: %v", err)
	}
	if rejected.Status != StatusRejected || rejected.AdminNote != "policy violation" {
		t.Fatalf("rejected payout=%+v", rejected)
	}
	if _, err := f.service.Reject(ctx, adminID, item.ID, "", "reject-retry"); err != nil {
		t.Fatalf("repeat reject: %v", err)
	}

	earnings, reserved := providerBalances(t, f.pool, f.providerID)
	if earnings != 2*f.earnings || reserved != 0 {
		t.Fatalf("balances after reject earnings=%d reserved=%d; want %d/0", earnings, reserved, 2*f.earnings)
	}

	second, _, err := f.service.Create(ctx, CreateInput{
		ProviderID: f.providerID, AmountIRR: f.earnings, IdempotencyKey: "reject-key-2",
		RequestID: "req-2", Nonce: "req-2",
	})
	if err != nil {
		t.Fatalf("create payout after reject: %v", err)
	}
	if second.ID == item.ID {
		t.Fatal("payout after reject must be a new request id")
	}
	if err := f.service.Verify(ctx); err != nil {
		t.Fatalf("verify payouts after reject cycle: %v", err)
	}
	assertBalancedPayoutLedger(t, f.pool)
}

func TestPayoutFailReleasesFunds(t *testing.T) {
	f := newPayoutFixture(t)
	ctx := context.Background()

	var adminID int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ($1, 'test') RETURNING id`, payoutEmail(t)).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'admin')`, adminID); err != nil {
		t.Fatal(err)
	}

	item, _, err := f.service.Create(ctx, CreateInput{
		ProviderID: f.providerID, AmountIRR: f.earnings, IdempotencyKey: "fail-key",
		RequestID: "req", Nonce: "req",
	})
	if err != nil {
		t.Fatalf("create payout: %v", err)
	}
	if _, err := f.service.Approve(ctx, adminID, item.ID, "", "approve"); err != nil {
		t.Fatalf("approve payout: %v", err)
	}
	if _, err := f.service.StartProcessing(ctx, adminID, item.ID, "", "processing"); err != nil {
		t.Fatalf("start processing: %v", err)
	}
	failed, err := f.service.Fail(ctx, adminID, item.ID, "bank rejected transfer", "fail")
	if err != nil {
		t.Fatalf("fail payout: %v", err)
	}
	if failed.Status != StatusFailed || failed.AdminNote != "bank rejected transfer" {
		t.Fatalf("failed payout=%+v", failed)
	}

	earnings, reserved := providerBalances(t, f.pool, f.providerID)
	if earnings != 2*f.earnings || reserved != 0 {
		t.Fatalf("balances after fail earnings=%d reserved=%d; want %d/0", earnings, reserved, 2*f.earnings)
	}
	if err := f.service.Verify(ctx); err != nil {
		t.Fatalf("verify payout after fail: %v", err)
	}
	assertBalancedPayoutLedger(t, f.pool)
}

func TestMarkPaidRequiresExternalReference(t *testing.T) {
	f := newPayoutFixture(t)
	ctx := context.Background()

	var adminID int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ($1, 'test') RETURNING id`, payoutEmail(t)).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'admin')`, adminID); err != nil {
		t.Fatal(err)
	}

	item, _, err := f.service.Create(ctx, CreateInput{
		ProviderID: f.providerID, AmountIRR: f.earnings, IdempotencyKey: "ref-key",
		RequestID: "req", Nonce: "req",
	})
	if err != nil {
		t.Fatalf("create payout: %v", err)
	}
	if _, err := f.service.MarkPaid(ctx, adminID, item.ID, "", "", "paid-empty"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("paid without reference error=%v; want ErrInvalidInput", err)
	}
	if _, err := f.service.MarkPaid(ctx, adminID, item.ID, "TRX-1", "", "paid-requested"); !errors.Is(err, ErrConflict) {
		t.Fatalf("paid from requested error=%v; want ErrConflict", err)
	}
	if _, err := f.service.Reject(ctx, adminID, item.ID, "", "reject"); err != nil {
		t.Fatalf("reject payout: %v", err)
	}
	if _, err := f.service.MarkPaid(ctx, adminID, item.ID, "TRX-2", "", "paid-rejected"); !errors.Is(err, ErrConflict) {
		t.Fatalf("paid rejected payout error=%v; want ErrConflict", err)
	}
	assertBalancedPayoutLedger(t, f.pool)
}

func TestAdminPayoutListAPI(t *testing.T) {
	f := newPayoutFixture(t)
	ctx := context.Background()

	item, _, err := f.service.Create(ctx, CreateInput{
		ProviderID: f.providerID, AmountIRR: f.earnings, IdempotencyKey: "admin-list-key",
		RequestID: "admin-list-request", Nonce: "admin-list-request",
	})
	if err != nil {
		t.Fatalf("create payout: %v", err)
	}

	app := fiber.New(fiber.Config{ErrorHandler: platform.ErrorHandler(slog.New(slog.NewTextHandler(io.Discard, nil)))})
	app.Use(platform.RequestID())
	NewHandler(f.service).Register(app, "01234567890123456789012345678901")

	type AdminPayoutItem struct {
		ID                  int64  `json:"id"`
		AmountIRR           int64  `json:"amount_irr"`
		Status              string `json:"status"`
		ProviderDisplayName string `json:"provider_display_name"`
	}
	var payoutPage struct {
		Items      []AdminPayoutItem `json:"items"`
		NextCursor *int64            `json:"next_cursor"`
	}

	adminToken := payoutToken(t, f.providerID, []string{"admin"})
	clientToken := payoutToken(t, f.clientID, []string{"client"})

	resp := payoutRequest(t, app, http.MethodGet, "/api/v1/admin/payouts", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous admin payout list status=%d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
	resp = payoutRequest(t, app, http.MethodGet, "/api/v1/admin/payouts", clientToken, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("client admin payout list status=%d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
	resp = payoutRequest(t, app, http.MethodGet, "/api/v1/admin/payouts?status=bogus", adminToken, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid status filter status=%d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	resp = payoutRequest(t, app, http.MethodGet, "/api/v1/admin/payouts", adminToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin payout list status=%d, want 200", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&payoutPage); err != nil {
		t.Fatalf("decode admin payout page: %v", err)
	}
	resp.Body.Close()
	if payoutPage.NextCursor != nil {
		t.Fatalf("single payout must not have next cursor, got %d", *payoutPage.NextCursor)
	}
	if len(payoutPage.Items) != 1 || payoutPage.Items[0].ID != item.ID || payoutPage.Items[0].Status != "requested" {
		t.Fatalf("admin payout page=%+v, want requested payout %d", payoutPage.Items, item.ID)
	}
	if payoutPage.Items[0].ProviderDisplayName != "Payout Provider" {
		t.Fatalf("provider display name=%q, want resolved", payoutPage.Items[0].ProviderDisplayName)
	}

	resp = payoutRequest(t, app, http.MethodGet, "/api/v1/admin/payouts?status=processing", adminToken, "")
	var processingPage struct {
		Items []AdminPayoutItem `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&processingPage); err != nil {
		t.Fatalf("decode processing filter page: %v", err)
	}
	resp.Body.Close()
	if len(processingPage.Items) != 0 {
		t.Fatalf("processing filter returned %+v, want empty", processingPage.Items)
	}

	if _, err := f.service.Approve(ctx, f.providerID, item.ID, "", "approve"); err != nil {
		t.Fatalf("approve payout: %v", err)
	}
	resp = payoutRequest(t, app, http.MethodGet, "/api/v1/admin/payouts?status=approved", adminToken, "")
	var approvedPage struct {
		Items []AdminPayoutItem `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&approvedPage); err != nil {
		t.Fatalf("decode approved filter page: %v", err)
	}
	resp.Body.Close()
	if len(approvedPage.Items) != 1 || approvedPage.Items[0].ID != item.ID || approvedPage.Items[0].Status != "approved" {
		t.Fatalf("approved filter returned %+v, want payout %d approved", approvedPage.Items, item.ID)
	}
}

func payoutToken(t *testing.T, userID int64, roles []string) string {
	t.Helper()
	token, _, err := identity.IssueAccessToken(userID, "payout-admin@example.test", roles, identity.AccessTokenConfig{Secret: "01234567890123456789012345678901", TTL: time.Hour})
	if err != nil {
		t.Fatalf("issue payout test token: %v", err)
	}
	return token
}

func payoutRequest(t *testing.T, app *fiber.App, method, path, token, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, "http://test.local"+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("create payout request: %v", err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := app.Test(request)
	if err != nil {
		t.Fatalf("send payout request: %v", err)
	}
	return response
}

func assertBalancedPayoutLedger(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM (
			SELECT t.id FROM ledger_transactions t JOIN ledger_entries e ON e.transaction_id = t.id
			GROUP BY t.id HAVING sum(e.amount_irr) FILTER (WHERE e.side = 'debit')
				<> sum(e.amount_irr) FILTER (WHERE e.side = 'credit')
		) unbalanced
	`).Scan(&count); err != nil {
		t.Fatalf("check ledger balance: %v", err)
	}
	if count != 0 {
		t.Fatalf("unbalanced ledger documents=%d", count)
	}
}
