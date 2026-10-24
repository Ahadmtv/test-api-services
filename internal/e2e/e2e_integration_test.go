package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgxpool"

	"test-api/internal/adminapi"
	"test-api/internal/auth"
	"test-api/internal/booking"
	"test-api/internal/catalog"
	"test-api/internal/identity"
	"test-api/internal/payments"
	"test-api/internal/payouts"
	"test-api/internal/platform"
	providerapi "test-api/internal/providerapi"
	"test-api/internal/providers"
	"test-api/internal/scheduling"
	"test-api/internal/wallet"
)

// This end-to-end test drives the real HTTP surface of the service (routing,
// authentication, JSON contracts and error mapping) against a real PostgreSQL
// schema created and migrated at test time. It complements the package-level
// integration tests, which exercise the service layer directly.
const (
	e2eSecret = "0123456789abcdef0123456789abcdef"

	e2ePassword    = "e2e-password-1"
	servicePrice   = int64(25_000)
	topUpAmountIRR = int64(200_000)
)

var e2eCounter atomic.Uint64

func e2eSuffix() string {
	return fmt.Sprintf("%d%06d", time.Now().UnixNano()%1_000_000_000, e2eCounter.Add(1))
}

type e2eEnv struct {
	t               *testing.T
	app             *fiber.App
	pool            *pgxpool.Pool
	clientEmail     string
	providerEmail   string
	adminEmail      string
	clientID        int64
	providerID      int64
	adminID         int64
	clientToken     string
	providerToken   string
	adminToken      string
	providerRefresh string
}

// nextSlot returns the next local 10:00 slot in the provider's business time
// zone, matching the seeded working hours.
func nextSlot(t *testing.T) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Tehran")
	if err != nil {
		t.Fatalf("load provider time zone: %v", err)
	}
	day := time.Now().In(loc).AddDate(0, 0, 1)
	return time.Date(day.Year(), day.Month(), day.Day(), 10, 0, 0, 0, loc)
}

func helperDo(t *testing.T, app *fiber.App, method, path, token, body, idempotencyKey string) (int, map[string]any, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, "https://test.local"+path, reader)
	if err != nil {
		t.Fatalf("build %s %s request: %v", method, path, err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := app.Test(request)
	if err != nil {
		t.Fatalf("send %s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s %s response: %v", method, path, err)
	}
	payload := map[string]any{}
	if len(raw) > 0 && strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode %s %s response %q: %v", method, path, raw, err)
		}
	}
	return response.StatusCode, payload, string(raw)
}

func expectStatus(t *testing.T, got, want int, label, raw string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s status=%d (%s); want %d", label, got, raw, want)
	}
}

func number(t *testing.T, payload map[string]any, key, label string) int64 {
	t.Helper()
	value, ok := payload[key].(float64)
	if !ok {
		t.Fatalf("%s: field %q missing or not numeric in %v", label, key, payload)
	}
	return int64(value)
}

func text(t *testing.T, payload map[string]any, key, label string) string {
	t.Helper()
	value, ok := payload[key].(string)
	if !ok {
		t.Fatalf("%s: field %q missing or not a string in %v", label, key, payload)
	}
	return value
}

func setupE2E(t *testing.T) *e2eEnv {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set; skipping PostgreSQL end-to-end test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to PostgreSQL: %v", err)
	}
	t.Cleanup(adminPool.Close)

	schema := "e2e_test_" + e2eSuffix()
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
	if _, err := pool.Exec(ctx, `INSERT INTO commission_policies (version, rate_bps, effective_from) VALUES (2, 1250, now())`); err != nil {
		t.Fatalf("seed commission policy: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	identityService := identity.NewService(identity.Config{
		Store:             identity.NewPgxStore(pool),
		PasswordParams:    identity.DefaultParams,
		AccessTokenSecret: e2eSecret,
		AccessTokenTTL:    15 * time.Minute,
		RefreshTokenTTL:   time.Hour,
		Logger:            logger,
	})
	authHandler := auth.NewHandler(identityService, logger, auth.NewRateLimiter(1000), 900)

	fiberApp := fiber.New(fiber.Config{ErrorHandler: platform.ErrorHandler(logger)})
	fiberApp.Use(platform.RequestID())

	authGroup := fiberApp.Group("/api/v1/auth")
	authGroup.Post("/register", authHandler.Register)
	authGroup.Post("/login", authHandler.Login)
	authGroup.Post("/refresh", authHandler.Refresh)
	authGroup.Post("/logout", authHandler.Logout)
	fiberApp.Get("/api/v1/me", auth.Authenticator(e2eSecret, identityService), authHandler.Me)

	policy := scheduling.AvailabilityPolicy{
		SlotIncrement:  15 * time.Minute,
		MinimumNotice:  0,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	providerHandler := providerapi.NewHandler(providers.NewService(pool), catalog.NewStore(pool), scheduling.NewStore(pool), scheduling.NewOccupancyStore(pool), policy)
	providerHandler.Register(fiberApp, e2eSecret, identityService)

	walletService := wallet.NewService(pool)
	wallet.NewHandler(walletService).Register(fiberApp, e2eSecret, identityService)
	gateway := &payments.FakeGateway{}
	payments.NewHandler(payments.NewService(pool, walletService, gateway, "https://test.local/api/v1/payments/zarinpal/callback"), auth.NewRateLimiter(1000), "").Register(fiberApp, e2eSecret, identityService)
	bookingConfig := booking.Config{Availability: policy, CompletionDisputeWindow: 24 * time.Hour}
	bookingService := booking.NewService(pool, providers.NewService(pool), catalog.NewStore(pool), scheduling.NewStore(pool), bookingConfig, walletService)
	booking.NewHandler(bookingService, bookingConfig).Register(fiberApp, e2eSecret, identityService)
	payouts.NewHandler(payouts.NewService(pool, providers.NewService(pool), 0)).Register(fiberApp, e2eSecret, identityService)
	adminapi.NewHandler(identityService).Register(fiberApp, e2eSecret, identityService)

	env := &e2eEnv{t: t, app: fiberApp, pool: pool}
	env.clientEmail = env.register("client")
	env.providerEmail = env.register("provider")
	env.adminEmail = env.register("admin")
	env.clientID = env.userID(ctx, env.clientEmail)
	env.providerID = env.userID(ctx, env.providerEmail)
	env.adminID = env.userID(ctx, env.adminEmail)
	for _, grant := range []struct {
		id   int64
		role string
	}{{env.providerID, "provider"}, {env.adminID, "admin"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, grant.id, grant.role); err != nil {
			t.Fatalf("grant role %s: %v", grant.role, err)
		}
	}
	env.clientToken, _ = env.login(env.clientEmail)
	env.providerToken, env.providerRefresh = env.login(env.providerEmail)
	env.adminToken, _ = env.login(env.adminEmail)
	return env
}

func (e *e2eEnv) register(label string) string {
	e.t.Helper()
	email := fmt.Sprintf("e2e-%s-%s@test.local", label, e2eSuffix())
	status, _, raw := helperDo(e.t, e.app, http.MethodPost, "/api/v1/auth/register", "",
		fmt.Sprintf(`{"email":%q,"password":%q}`, email, e2ePassword), "")
	expectStatus(e.t, status, http.StatusCreated, "register "+label, raw)
	return email
}

func (e *e2eEnv) userID(ctx context.Context, email string) int64 {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(ctx, `SELECT id FROM users WHERE email = $1`, email).Scan(&id); err != nil {
		e.t.Fatalf("lookup user %s: %v", email, err)
	}
	return id
}

func (e *e2eEnv) login(email string) (string, string) {
	e.t.Helper()
	status, payload, raw := helperDo(e.t, e.app, http.MethodPost, "/api/v1/auth/login", "",
		fmt.Sprintf(`{"email":%q,"password":%q}`, email, e2ePassword), "")
	expectStatus(e.t, status, http.StatusOK, "login "+email, raw)
	return text(e.t, payload, "access_token", "login"), text(e.t, payload, "refresh_token", "login")
}

func (e *e2eEnv) balance(token string) (available, held, earnings, reserved int64) {
	e.t.Helper()
	status, payload, raw := helperDo(e.t, e.app, http.MethodGet, "/api/v1/wallet", token, "", "")
	expectStatus(e.t, status, http.StatusOK, "wallet balance", raw)
	return number(e.t, payload, "available_irr", "balance"),
		number(e.t, payload, "held_irr", "balance"),
		number(e.t, payload, "provider_earnings_irr", "balance"),
		number(e.t, payload, "provider_payout_reserved_irr", "balance")
}

// topUp funds the client wallet through the payment boundary and verifies that
// a duplicate gateway callback is idempotent.
func (e *e2eEnv) topUp(token string, amount int64, key string) int64 {
	e.t.Helper()
	status, payload, raw := helperDo(e.t, e.app, http.MethodPost, "/api/v1/payments/zarinpal", token,
		fmt.Sprintf(`{"amount_irr":%d}`, amount), key)
	expectStatus(e.t, status, http.StatusCreated, "start payment", raw)
	payment, ok := payload["payment"].(map[string]any)
	if !ok {
		e.t.Fatalf("payment envelope missing in %s", raw)
	}
	authority := text(e.t, payment, "authority", "start payment")
	paymentID := number(e.t, payment, "id", "start payment")

	callbackPath := "/api/v1/payments/zarinpal/callback?authority=" + url.QueryEscape(authority) + "&status=OK"
	for attempt := 0; attempt < 2; attempt++ {
		status, _, raw = helperDo(e.t, e.app, http.MethodGet, callbackPath, "", "", "")
		expectStatus(e.t, status, http.StatusNoContent, "payment callback", raw)
	}
	return paymentID
}

// createBooking creates a booking for the next local 10:00 slot and asserts the
// initial confirmed state.
func (e *e2eEnv) createBooking(serviceID int64, key string) int64 {
	e.t.Helper()
	slot := nextSlot(e.t)
	status, payload, raw := helperDo(e.t, e.app, http.MethodPost, "/api/v1/bookings", e.clientToken,
		fmt.Sprintf(`{"service_id":%d,"starts_at":%q}`, serviceID, slot.Format(time.RFC3339)), key)
	expectStatus(e.t, status, http.StatusCreated, "create booking", raw)
	if state := text(e.t, payload, "status", "create booking"); state != "confirmed" {
		e.t.Fatalf("new booking status=%q; want confirmed (%s)", state, raw)
	}
	return number(e.t, payload, "id", "create booking")
}

// moveIntoServiceWindow rewrites the schedule so the provider is allowed to
// start the booking immediately, matching how a real clock would advance.
func (e *e2eEnv) moveIntoServiceWindow(bookingID int64) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE bookings SET starts_at = now() - interval '1 minute', ends_at = now() + interval '29 minutes' WHERE id = $1`,
		bookingID); err != nil {
		e.t.Fatalf("move booking %d into service window: %v", bookingID, err)
	}
}

func (e *e2eEnv) providerAction(bookingID int64, action, label string) map[string]any {
	e.t.Helper()
	status, payload, raw := helperDo(e.t, e.app, http.MethodPost, fmt.Sprintf("/api/v1/provider/bookings/%d/%s", bookingID, action), e.providerToken, "", "")
	expectStatus(e.t, status, http.StatusOK, label, raw)
	return payload
}

func (e *e2eEnv) clientAction(bookingID int64, action, body, label string) map[string]any {
	e.t.Helper()
	status, payload, raw := helperDo(e.t, e.app, http.MethodPost, fmt.Sprintf("/api/v1/bookings/%d/%s", bookingID, action), e.clientToken, body, "")
	expectStatus(e.t, status, http.StatusOK, label, raw)
	return payload
}

func (e *e2eEnv) adminAction(path, body, label string) map[string]any {
	e.t.Helper()
	status, payload, raw := helperDo(e.t, e.app, http.MethodPost, path, e.adminToken, body, "")
	expectStatus(e.t, status, http.StatusOK, label, raw)
	return payload
}

func (e *e2eEnv) assertLedgerBalanced() {
	e.t.Helper()
	ctx := context.Background()
	var unbalanced int64
	if err := e.pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT transaction_id FROM ledger_entries
			GROUP BY transaction_id
			HAVING COALESCE(SUM(amount_irr) FILTER (WHERE side = 'debit'), 0)
			     <> COALESCE(SUM(amount_irr) FILTER (WHERE side = 'credit'), 0)
		) AS unbalanced
	`).Scan(&unbalanced); err != nil {
		e.t.Fatalf("ledger balance query: %v", err)
	}
	if unbalanced != 0 {
		e.t.Fatalf("found %d unbalanced ledger transactions", unbalanced)
	}
	var thin int64
	if err := e.pool.QueryRow(ctx, `
		SELECT count(*) FROM ledger_transactions t
		WHERE (SELECT count(*) FROM ledger_entries e WHERE e.transaction_id = t.id) < 2
	`).Scan(&thin); err != nil {
		e.t.Fatalf("ledger completeness query: %v", err)
	}
	if thin != 0 {
		e.t.Fatalf("found %d ledger transactions with fewer than two entries", thin)
	}
}

func TestFullBusinessWorkflow(t *testing.T) {
	e := setupE2E(t)

	// Provider onboarding: profile, schedule, catalog, then admin verification.
	t.Run("provider_onboarding", func(t *testing.T) {
		status, _, raw := helperDo(t, e.app, http.MethodPut, "/api/v1/provider/profile", e.providerToken,
			`{"display_name":"E2E Provider","is_active":true,"time_zone":"Asia/Tehran"}`, "")
		expectStatus(t, status, http.StatusOK, "put profile", raw)

		var intervals []string
		for weekday := 1; weekday <= 7; weekday++ {
			intervals = append(intervals, fmt.Sprintf(`{"weekday":%d,"opens_at":"09:00","closes_at":"20:00"}`, weekday))
		}
		status, _, raw = helperDo(t, e.app, http.MethodPut, "/api/v1/provider/working-hours", e.providerToken,
			`{"intervals":[`+strings.Join(intervals, ",")+`]}`, "")
		expectStatus(t, status, http.StatusOK, "put working hours", raw)

		status, _, raw = helperDo(t, e.app, http.MethodPatch, fmt.Sprintf("/api/v1/admin/providers/%d/verification", e.providerID), e.adminToken,
			`{"verification_status":"verified"}`, "")
		expectStatus(t, status, http.StatusOK, "verify provider", raw)
	})

	status, payload, raw := helperDo(t, e.app, http.MethodPost, "/api/v1/provider/services", e.providerToken,
		`{"name":"E2E Beauty Service","category":"haircut","duration_minutes":30,"price_irr":25000,"is_active":true}`, "")
	expectStatus(t, status, http.StatusCreated, "create service", raw)
	serviceID := number(t, payload, "id", "create service")

	t.Run("availability_is_public_and_descriptive", func(t *testing.T) {
		loc, err := time.LoadLocation("Asia/Tehran")
		if err != nil {
			t.Fatalf("load location: %v", err)
		}
		day := time.Now().In(loc).AddDate(0, 0, 1).Format("2006-01-02")
		status, payload, raw := helperDo(t, e.app, http.MethodGet,
			fmt.Sprintf("/api/v1/providers/%d/availability?service_id=%d&date=%s", e.providerID, serviceID, day), "", "", "")
		expectStatus(t, status, http.StatusOK, "availability", raw)
		slots, ok := payload["slots"].([]any)
		if !ok || len(slots) == 0 {
			t.Fatalf("availability returned no slots (%s)", raw)
		}
	})

	t.Run("refresh_preserves_identity", func(t *testing.T) {
		status, payload, raw := helperDo(t, e.app, http.MethodPost, "/api/v1/auth/refresh", "",
			fmt.Sprintf(`{"refresh_token":%q}`, e.providerRefresh), "")
		expectStatus(t, status, http.StatusOK, "refresh provider", raw)
		rotated := text(t, payload, "access_token", "refresh provider")
		status, _, raw = helperDo(t, e.app, http.MethodGet, "/api/v1/provider/profile", rotated, "", "")
		expectStatus(t, status, http.StatusOK, "rotated token keeps provider role", raw)
	})

	t.Run("insufficient_funds_then_topup", func(t *testing.T) {
		status, _, raw = helperDo(t, e.app, http.MethodPost, "/api/v1/bookings", e.clientToken,
			fmt.Sprintf(`{"service_id":%d,"starts_at":%q}`, serviceID, nextSlot(t).Format(time.RFC3339)), "e2e-insufficient")
		expectStatus(t, status, http.StatusConflict, "booking without funds", raw)

		available, held, _, _ := e.balance(e.clientToken)
		if available != 0 || held != 0 {
			t.Fatalf("wallet should start empty: available=%d held=%d", available, held)
		}
		e.topUp(e.clientToken, topUpAmountIRR, "e2e-topup-1")
		available, held, _, _ = e.balance(e.clientToken)
		if available != topUpAmountIRR || held != 0 {
			t.Fatalf("after top-up: available=%d held=%d; want %d,0", available, held, topUpAmountIRR)
		}
	})

	t.Run("happy_path_booking_and_payout", func(t *testing.T) {
		bookingID := e.createBooking(serviceID, "e2e-booking-1")

		if _, held, _, _ := e.balance(e.clientToken); held != servicePrice {
			t.Fatalf("hold after booking=%d; want %d", held, servicePrice)
		}

		e.moveIntoServiceWindow(bookingID)
		e.providerAction(bookingID, "start", "start booking")
		e.providerAction(bookingID, "request-completion", "request completion")
		payload := e.clientAction(bookingID, "confirm-completion", "", "confirm completion")
		if state := text(t, payload, "status", "confirm completion"); state != "completed" {
			t.Fatalf("booking status=%q; want completed", state)
		}

		available, held, _, _ := e.balance(e.clientToken)
		if held != 0 || available != topUpAmountIRR-servicePrice {
			t.Fatalf("client after settle: available=%d held=%d", available, held)
		}
		if _, _, earnings, reserved := e.balance(e.providerToken); earnings != 21_875 || reserved != 0 {
			t.Fatalf("provider after settle: earnings=%d reserved=%d; want 21875,0", earnings, reserved)
		}

		status, first, raw := helperDo(t, e.app, http.MethodPost, "/api/v1/provider/payouts/", e.providerToken,
			`{"amount_irr":21875}`, "e2e-payout-1")
		expectStatus(t, status, http.StatusCreated, "create payout", raw)
		payoutID := number(t, first, "id", "create payout")

		status, replay, raw := helperDo(t, e.app, http.MethodPost, "/api/v1/provider/payouts/", e.providerToken,
			`{"amount_irr":21875}`, "e2e-payout-1")
		expectStatus(t, status, http.StatusOK, "replay payout", raw)
		if number(t, replay, "id", "replay payout") != payoutID {
			t.Fatalf("payout replay returned a new request (%s)", raw)
		}
		if _, _, earnings, reserved := e.balance(e.providerToken); earnings != 0 || reserved != 21_875 {
			t.Fatalf("provider reserve state: earnings=%d reserved=%d", earnings, reserved)
		}

		e.adminAction(fmt.Sprintf("/api/v1/admin/payouts/%d/approve", payoutID), "", "approve payout")
		e.adminAction(fmt.Sprintf("/api/v1/admin/payouts/%d/processing", payoutID), "", "processing payout")
		status, paid, raw := helperDo(t, e.app, http.MethodPost, fmt.Sprintf("/api/v1/admin/payouts/%d/paid", payoutID), e.adminToken,
			`{"external_transfer_reference":"BANK-REF-E2E-1"}`, "")
		expectStatus(t, status, http.StatusOK, "paid payout", raw)
		if state := text(t, paid, "status", "paid payout"); state != "paid" {
			t.Fatalf("payout status=%q; want paid", state)
		}
		if _, _, earnings, reserved := e.balance(e.providerToken); earnings != 0 || reserved != 0 {
			t.Fatalf("provider after payout: earnings=%d reserved=%d", earnings, reserved)
		}
	})

	t.Run("dispute_settlement", func(t *testing.T) {
		e.topUp(e.clientToken, topUpAmountIRR, "e2e-topup-2")
		bookingID := e.createBooking(serviceID, "e2e-booking-2")
		e.moveIntoServiceWindow(bookingID)
		e.providerAction(bookingID, "start", "start disputed booking")
		e.providerAction(bookingID, "request-completion", "request disputed completion")
		e.clientAction(bookingID, "dispute", `{"reason":"ÃšÂ©Ã›Å’Ã™ÂÃ›Å’Ã˜Âª Ã˜Â®Ã˜Â¯Ã™â€¦Ã˜Âª Ã™â€¦Ã˜Â·Ã˜Â§Ã˜Â¨Ã™â€š Ã˜ÂªÃ™Ë†Ã˜Â§Ã™ÂÃ™â€š Ã™â€ Ã˜Â¨Ã™Ë†Ã˜Â¯"}`, "dispute booking")
		payload := e.adminAction(fmt.Sprintf("/api/v1/admin/bookings/%d/dispute-resolution", bookingID), `{"resolution":"settle"}`, "resolve dispute")
		if state := text(t, payload, "status", "resolve dispute"); state != "completed" {
			t.Fatalf("resolved dispute status=%q; want completed", state)
		}
		if _, _, earnings, _ := e.balance(e.providerToken); earnings != 21_875 {
			t.Fatalf("provider earnings after dispute settle=%d; want 21875", earnings)
		}
	})

	t.Run("admin_refund_is_idempotent", func(t *testing.T) {
		e.topUp(e.clientToken, topUpAmountIRR, "e2e-topup-3")
		bookingID := e.createBooking(serviceID, "e2e-booking-3")
		e.moveIntoServiceWindow(bookingID)
		e.providerAction(bookingID, "start", "start refunded booking")
		e.providerAction(bookingID, "request-completion", "request refunded completion")
		e.clientAction(bookingID, "confirm-completion", "", "confirm refunded completion")

		before, _, _, _ := e.balance(e.clientToken)
		for attempt := 0; attempt < 2; attempt++ {
			status, _, raw := helperDo(t, e.app, http.MethodPost, fmt.Sprintf("/api/v1/admin/bookings/%d/refund", bookingID), e.adminToken, "", "e2e-refund-1")
			expectStatus(t, status, http.StatusNoContent, "refund booking", raw)
		}
		after, _, _, _ := e.balance(e.clientToken)
		if after != before+servicePrice {
			t.Fatalf("refund delta=%d; want %d", after-before, servicePrice)
		}
	})

	t.Run("financial_integrity", func(t *testing.T) {
		e.assertLedgerBalanced()
	})
}

func TestAdminRoleManagement(t *testing.T) {
	e := setupE2E(t)

	status, _, raw := helperDo(t, e.app, http.MethodGet, "/api/v1/admin/users", e.clientToken, "", "")
	expectStatus(t, status, http.StatusForbidden, "client cannot list users", raw)

	status, page, raw := helperDo(t, e.app, http.MethodGet, "/api/v1/admin/users?q="+url.QueryEscape(e.clientEmail), e.adminToken, "", "")
	expectStatus(t, status, http.StatusOK, "admin lists users", raw)
	items, ok := page["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("admin user search returned %v", page)
	}
	first, ok := items[0].(map[string]any)
	if !ok || number(t, first, "id", "searched user") != e.clientID {
		t.Fatalf("admin user search returned wrong user: %v", items[0])
	}

	status, updated, raw := helperDo(t, e.app, http.MethodPut,
		fmt.Sprintf("/api/v1/admin/users/%d/roles/provider", e.clientID), e.adminToken, `{"granted":true}`, "")
	expectStatus(t, status, http.StatusOK, "grant provider role", raw)
	if got := fmt.Sprint(updated["roles"]); !strings.Contains(got, "provider") {
		t.Fatalf("granted roles=%v", updated["roles"])
	}

	// The token minted before the grant must immediately observe the new role.
	status, _, raw = helperDo(t, e.app, http.MethodGet, "/api/v1/provider/services", e.clientToken, "", "")
	expectStatus(t, status, http.StatusOK, "existing token observes granted role", raw)

	var auditCount int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events
		WHERE actor_user_id = $1 AND target_id = $2 AND action = 'user.role.granted'
	`, e.adminID, e.clientID).Scan(&auditCount); err != nil {
		t.Fatalf("count role audit event: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("role audit events=%d; want 1", auditCount)
	}

	status, _, raw = helperDo(t, e.app, http.MethodPut,
		fmt.Sprintf("/api/v1/admin/users/%d/roles/provider", e.clientID), e.adminToken, `{"granted":false}`, "")
	expectStatus(t, status, http.StatusOK, "revoke provider role", raw)

	status, _, raw = helperDo(t, e.app, http.MethodGet, "/api/v1/provider/services", e.clientToken, "", "")
	expectStatus(t, status, http.StatusForbidden, "existing token observes revoked role", raw)

	status, _, raw = helperDo(t, e.app, http.MethodPut,
		fmt.Sprintf("/api/v1/admin/users/%d/roles/admin", e.clientID), e.adminToken, `{"granted":true}`, "")
	expectStatus(t, status, http.StatusOK, "grant admin role", raw)
	clientAdminToken, _ := e.login(e.clientEmail)

	status, _, raw = helperDo(t, e.app, http.MethodPut,
		fmt.Sprintf("/api/v1/admin/users/%d/roles/admin", e.adminID), clientAdminToken, `{"granted":false}`, "")
	expectStatus(t, status, http.StatusOK, "revoke admin role", raw)

	// Revocation is effective immediately for the old administrator token.
	status, _, raw = helperDo(t, e.app, http.MethodGet, "/api/v1/admin/users", e.adminToken, "", "")
	expectStatus(t, status, http.StatusForbidden, "revoked admin token is rejected", raw)

	status, _, raw = helperDo(t, e.app, http.MethodPut,
		fmt.Sprintf("/api/v1/admin/users/%d/roles/admin", e.clientID), clientAdminToken, `{"granted":false}`, "")
	expectStatus(t, status, http.StatusConflict, "cannot remove own admin role", raw)

}
func TestGatewayMinimumRejectedBeforeSlotReservation(t *testing.T) {
	e := setupE2E(t)

	status, _, raw := helperDo(t, e.app, http.MethodPut, "/api/v1/provider/profile", e.providerToken,
		`{"display_name":"Minimum Test Provider","is_active":true,"time_zone":"Asia/Tehran"}`, "")
	expectStatus(t, status, http.StatusOK, "put low-price provider profile", raw)

	var intervals []string
	for weekday := 1; weekday <= 7; weekday++ {
		intervals = append(intervals, fmt.Sprintf(`{"weekday":%d,"opens_at":"09:00","closes_at":"20:00"}`, weekday))
	}
	status, _, raw = helperDo(t, e.app, http.MethodPut, "/api/v1/provider/working-hours", e.providerToken,
		`{"intervals":[`+strings.Join(intervals, ",")+`]}`, "")
	expectStatus(t, status, http.StatusOK, "put low-price working hours", raw)
	status, _, raw = helperDo(t, e.app, http.MethodPatch,
		fmt.Sprintf("/api/v1/admin/providers/%d/verification", e.providerID), e.adminToken,
		`{"verification_status":"verified"}`, "")
	expectStatus(t, status, http.StatusOK, "verify low-price provider", raw)

	status, service, raw := helperDo(t, e.app, http.MethodPost, "/api/v1/provider/services", e.providerToken,
		`{"name":"Low Price Service","category":"haircut","duration_minutes":30,"price_irr":5000,"is_active":true}`, "")
	expectStatus(t, status, http.StatusCreated, "create low-price service", raw)
	serviceID := number(t, service, "id", "create low-price service")

	status, _, raw = helperDo(t, e.app, http.MethodPost, "/api/v1/bookings", e.clientToken,
		fmt.Sprintf(`{"service_id":%d,"starts_at":%q,"payment_method":"zarinpal"}`, serviceID, nextSlot(t).Format(time.RFC3339)), "e2e-payment-minimum")
	expectStatus(t, status, http.StatusBadRequest, "reject gateway payment below minimum", raw)
	if !strings.Contains(raw, `"code":"payment_minimum_not_met"`) {
		t.Fatalf("payment minimum response=%s", raw)
	}

	var bookings int
	if err := e.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM bookings WHERE service_id = $1 AND client_id = $2
	`, serviceID, e.clientID).Scan(&bookings); err != nil {
		t.Fatalf("count low-price bookings: %v", err)
	}
	if bookings != 0 {
		t.Fatalf("low-price payment attempt reserved %d bookings", bookings)
	}
}

func TestProviderOnboardingLifecycle(t *testing.T) {
	e := setupE2E(t)

	// 1) Admin grants the provider role: the auto-created profile appears in
	// the admin verification list as incomplete.
	status, _, raw := helperDo(t, e.app, http.MethodPut,
		fmt.Sprintf("/api/v1/admin/users/%d/roles/provider", e.clientID), e.adminToken, `{"granted":true}`, "")
	expectStatus(t, status, http.StatusOK, "grant provider role", raw)

	var createdProfiles int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM provider_profiles WHERE provider_id = $1`, e.clientID).Scan(&createdProfiles); err != nil {
		t.Fatalf("count auto-created profiles: %v", err)
	}
	if createdProfiles != 1 {
		t.Fatalf("role grant created %d provider profiles; want 1", createdProfiles)
	}

	status, listPage, raw := helperDo(t, e.app, http.MethodGet,
		"/api/v1/admin/providers?q="+url.QueryEscape(e.clientEmail), e.adminToken, "", "")
	expectStatus(t, status, http.StatusOK, "admin finds newly granted provider", raw)
	listItems, ok := listPage["items"].([]any)
	if !ok || len(listItems) != 1 {
		t.Fatalf("admin provider search returned %v", listPage)
	}
	incomplete, ok := listItems[0].(map[string]any)
	if !ok {
		t.Fatalf("admin provider search item is not an object: %v", listItems[0])
	}
	if number(t, incomplete, "provider_id", "searched provider") != e.clientID {
		t.Fatalf("admin provider search returned wrong provider: %v", listItems[0])
	}
	if got, _ := incomplete["profile_completed"].(bool); got {
		t.Fatalf("newly granted provider must be incomplete: %v", incomplete)
	}
	verificationStatus := text(t, incomplete, "verification_status", "searched provider verification")
	if verificationStatus != "pending" {
		t.Fatalf("newly granted provider verification status=%q; want pending", verificationStatus)
	}
	if displayName := fmt.Sprint(incomplete["display_name"]); displayName != "" {
		t.Fatalf("newly granted provider display_name=%q; want empty", displayName)
	}

	// 2) The service rejects verification of the incomplete profile.
	status, _, raw = helperDo(t, e.app, http.MethodPatch,
		fmt.Sprintf("/api/v1/admin/providers/%d/verification", e.clientID), e.adminToken,
		`{"verification_status":"verified"}`, "")
	expectStatus(t, status, http.StatusConflict, "verify incomplete profile", raw)
	if !strings.Contains(raw, `"code":"profile_incomplete"`) {
		t.Fatalf("verify incomplete response=%s; want profile_incomplete", raw)
	}

	// 3) The provider fills the profile, adds working hours and a service.
	status, _, raw = helperDo(t, e.app, http.MethodGet, "/api/v1/provider/profile", e.clientToken, "", "")
	expectStatus(t, status, http.StatusOK, "provider loads auto-created profile", raw)

	status, _, raw = helperDo(t, e.app, http.MethodPut, "/api/v1/provider/profile", e.clientToken,
		`{"display_name":"Onboarding Salon","is_active":true,"time_zone":"Asia/Tehran"}`, "")
	expectStatus(t, status, http.StatusOK, "complete provider profile", raw)

	var intervals []string
	for weekday := 1; weekday <= 7; weekday++ {
		intervals = append(intervals, fmt.Sprintf(`{"weekday":%d,"opens_at":"09:00","closes_at":"20:00"}`, weekday))
	}
	status, _, raw = helperDo(t, e.app, http.MethodPut, "/api/v1/provider/working-hours", e.clientToken,
		`{"intervals":[`+strings.Join(intervals, ",")+`]}`, "")
	expectStatus(t, status, http.StatusOK, "put onboarding working hours", raw)

	status, service, raw := helperDo(t, e.app, http.MethodPost, "/api/v1/provider/services", e.clientToken,
		`{"name":"Onboarding Service","category":"haircut","duration_minutes":30,"price_irr":25000,"is_active":true}`, "")
	expectStatus(t, status, http.StatusCreated, "create onboarding service", raw)
	serviceID := number(t, service, "id", "create onboarding service")

	// 4) Approval makes the service publicly visible.
	status, _, raw = helperDo(t, e.app, http.MethodPatch,
		fmt.Sprintf("/api/v1/admin/providers/%d/verification", e.clientID), e.adminToken,
		`{"verification_status":"verified"}`, "")
	expectStatus(t, status, http.StatusOK, "verify completed profile", raw)

	status, publicList, raw := helperDo(t, e.app, http.MethodGet, "/api/v1/services?category=haircut", "", "", "")
	expectStatus(t, status, http.StatusOK, "public services after verification", raw)
	item, found := findService(t, publicList, serviceID)
	if !found || item == nil {
		t.Fatalf("public services list=%v; want service %d", publicList, serviceID)
	}
	status, _, raw = helperDo(t, e.app, http.MethodGet, fmt.Sprintf("/api/v1/services/%d", serviceID), "", "", "")
	expectStatus(t, status, http.StatusOK, "public service detail", raw)

	// 5) Revoking the role keeps the profile but hides it publicly and
	// blocks new bookings.
	status, _, raw = helperDo(t, e.app, http.MethodPut,
		fmt.Sprintf("/api/v1/admin/users/%d/roles/provider", e.clientID), e.adminToken, `{"granted":false}`, "")
	expectStatus(t, status, http.StatusOK, "revoke provider role", raw)

	var keptProfiles int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM provider_profiles WHERE provider_id = $1`, e.clientID).Scan(&keptProfiles); err != nil {
		t.Fatalf("count profiles after revoke: %v", err)
	}
	if keptProfiles != 1 {
		t.Fatalf("revoke removed profile; want kept (count=%d)", keptProfiles)
	}

	status, publicList, raw = helperDo(t, e.app, http.MethodGet, "/api/v1/services?category=haircut", "", "", "")
	expectStatus(t, status, http.StatusOK, "public services after revoke", raw)
	if item, found := findService(t, publicList, serviceID); found || item != nil {
		t.Fatalf("revoked provider service=%v still publicly listed", item)
	}
	status, _, raw = helperDo(t, e.app, http.MethodGet, fmt.Sprintf("/api/v1/services/%d", serviceID), "", "", "")
	expectStatus(t, status, http.StatusNotFound, "public service detail after revoke", raw)
	status, _, raw = helperDo(t, e.app, http.MethodPost, "/api/v1/bookings", e.clientToken,
		fmt.Sprintf(`{"service_id":%d,"starts_at":%q}`, serviceID, nextSlot(t).Format(time.RFC3339)), "e2e-revoked-booking")
	expectStatus(t, status, http.StatusNotFound, "revoked provider cannot accept new bookings", raw)
	if !strings.Contains(raw, `"code":"service_unavailable"`) {
		t.Fatalf("revoked provider booking response=%s", raw)
	}

	// 6) Re-granting restores visibility without resetting the profile.
	status, _, raw = helperDo(t, e.app, http.MethodPut,
		fmt.Sprintf("/api/v1/admin/users/%d/roles/provider", e.clientID), e.adminToken, `{"granted":true}`, "")
	expectStatus(t, status, http.StatusOK, "re-grant provider role", raw)
	status, publicList, raw = helperDo(t, e.app, http.MethodGet, "/api/v1/services?category=haircut", "", "", "")
	expectStatus(t, status, http.StatusOK, "public services after re-grant", raw)
	if item, found := findService(t, publicList, serviceID); !found || item == nil {
		t.Fatalf("public services after re-grant=%v; want service %d visible again", publicList, serviceID)
	}
	var keptName string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT display_name FROM provider_profiles WHERE provider_id = $1`, e.clientID).Scan(&keptName); err != nil {
		t.Fatalf("load profile after re-grant: %v", err)
	}
	if keptName != "Onboarding Salon" {
		t.Fatalf("re-grant reset profile display_name=%q", keptName)
	}
}

func findService(t *testing.T, page map[string]any, serviceID int64) (map[string]any, bool) {
	t.Helper()
	items, ok := page["items"].([]any)
	if !ok {
		t.Fatalf("services page has no items: %v", page)
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("invalid service item: %v", raw)
		}
		if number(t, item, "id", "service item") == serviceID {
			return item, true
		}
	}
	return nil, false
}
