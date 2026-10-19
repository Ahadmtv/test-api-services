package booking

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgxpool"

	"test-api/internal/platform"
	"test-api/internal/wallet"
)

func TestWalletHoldSettlementRefundAndReconciliation(t *testing.T) {
	f := newBookingFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `INSERT INTO commission_policies (version, rate_bps, effective_from) VALUES (2, 1250, now())`); err != nil {
		t.Fatalf("insert commission policy: %v", err)
	}
	start := nextBookingStart()
	created, err := f.service.Create(ctx, f.clientID, CreateInput{ServiceID: f.serviceID, StartsAt: start}, f.config)
	if err != nil {
		t.Fatalf("create booking with hold: %v", err)
	}
	balance, err := f.service.wallet.Balance(ctx, f.clientID)
	if err != nil {
		t.Fatal(err)
	}
	if balance.AvailableIRR != 975_000 || balance.HeldIRR != 25_000 {
		t.Fatalf("balance after hold=%+v", balance)
	}
	assertBalancedLedger(t, f.pool)

	if _, err := f.pool.Exec(ctx, `UPDATE bookings SET starts_at = now() - interval '1 minute', ends_at = now() + interval '29 minutes' WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("move booking into service window: %v", err)
	}
	if _, err := f.service.Start(ctx, f.providerID, created.ID, "start-request"); err != nil {
		t.Fatalf("start service: %v", err)
	}
	if _, err := f.service.Complete(ctx, f.providerID, created.ID, "settle-request"); err != nil {
		t.Fatalf("settle service: %v", err)
	}
	if _, err := f.service.Complete(ctx, f.providerID, created.ID, "settle-retry"); err != nil {
		t.Fatalf("repeat settlement: %v", err)
	}
	var providerBalance, commissionBalance int64
	if err := f.pool.QueryRow(ctx, `SELECT balance_irr FROM wallet_accounts WHERE owner_user_id = $1 AND kind = 'provider_earnings'`, f.providerID).Scan(&providerBalance); err != nil {
		t.Fatalf("read provider earnings: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT balance_irr FROM wallet_accounts WHERE owner_user_id IS NULL AND kind = 'platform_commission'`).Scan(&commissionBalance); err != nil {
		t.Fatalf("read platform commission: %v", err)
	}
	if providerBalance != 21_875 || commissionBalance != 3_125 {
		t.Fatalf("settlement provider=%d commission=%d; want 21875/3125", providerBalance, commissionBalance)
	}
	var settlementCount int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE reference = $1`, "booking:"+jsonNumber(created.ID)+":settle").Scan(&settlementCount); err != nil || settlementCount != 1 {
		t.Fatalf("settlement count=%d error=%v; want exactly one", settlementCount, err)
	}

	var adminID int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ($1, 'test') RETURNING id`, randomEmail(t)).Scan(&adminID); err != nil {
		t.Fatalf("create admin fixture: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'admin')`, adminID); err != nil {
		t.Fatalf("grant admin fixture role: %v", err)
	}
	if err := f.service.Refund(ctx, adminID, created.ID, "refund-01", "refund-request"); err != nil {
		t.Fatalf("refund booking: %v", err)
	}
	if err := f.service.Refund(ctx, adminID, created.ID, "refund-01", "refund-retry"); err != nil {
		t.Fatalf("repeat refund: %v", err)
	}
	if err := f.service.Refund(ctx, adminID, created.ID, "refund-02", "refund-repeat"); !errors.Is(err, wallet.ErrNotReversible) {
		t.Fatalf("refund with a new key error=%v; want ErrNotReversible", err)
	}
	balance, err = f.service.wallet.Balance(ctx, f.clientID)
	if err != nil {
		t.Fatal(err)
	}
	if balance.AvailableIRR != 1_000_000 || balance.HeldIRR != 0 {
		t.Fatalf("balance after refund=%+v", balance)
	}
	if err := f.pool.QueryRow(ctx, `SELECT balance_irr FROM wallet_accounts WHERE owner_user_id = $1 AND kind = 'provider_earnings'`, f.providerID).Scan(&providerBalance); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT balance_irr FROM wallet_accounts WHERE owner_user_id IS NULL AND kind = 'platform_commission'`).Scan(&commissionBalance); err != nil {
		t.Fatal(err)
	}
	if providerBalance != 0 || commissionBalance != 0 {
		t.Fatalf("balances after reversal provider=%d commission=%d", providerBalance, commissionBalance)
	}
	assertBalancedLedger(t, f.pool)
	if err := f.service.wallet.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile ledger: %v", err)
	}
	transactions, _, err := f.service.wallet.Transactions(ctx, f.clientID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	foundHold := false
	for _, item := range transactions {
		if item.Reference == "booking:"+jsonNumber(created.ID)+":hold" {
			foundHold = true
			if item.AmountIRR != 25_000 {
				t.Fatalf("hold history amount=%d; want 25000", item.AmountIRR)
			}
		}
	}
	if !foundHold {
		t.Fatal("wallet history omitted the booking hold")
	}
}

func TestFreeBookingDoesNotCreateWalletHold(t *testing.T) {
	f := newBookingFixture(t)
	ctx := context.Background()
	var freeServiceID int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO services (provider_id, name, duration_minutes, price_irr) VALUES ($1, 'Free service', 30, 0) RETURNING id`, f.providerID).Scan(&freeServiceID); err != nil {
		t.Fatal(err)
	}
	created, err := f.service.Create(ctx, f.clientID, CreateInput{ServiceID: freeServiceID, StartsAt: nextBookingStart()}, f.config)
	if err != nil {
		t.Fatalf("create free booking: %v", err)
	}
	var paymentMethod string
	if err := f.pool.QueryRow(ctx, `SELECT payment_method FROM bookings WHERE id = $1`, created.ID).Scan(&paymentMethod); err != nil {
		t.Fatal(err)
	}
	var holds int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM wallet_holds WHERE booking_id = $1`, created.ID).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	if paymentMethod != "none" || holds != 0 {
		t.Fatalf("free booking payment_method=%s holds=%d", paymentMethod, holds)
	}
}

func TestWalletHoldReleaseAndProjectionProtection(t *testing.T) {
	f := newBookingFixture(t)
	ctx := context.Background()
	created, err := f.service.Create(ctx, f.clientID, CreateInput{ServiceID: f.serviceID, StartsAt: nextBookingStart()}, f.config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Cancel(ctx, f.clientID, created.ID, 0, "cancel-request"); err != nil {
		t.Fatalf("cancel booking: %v", err)
	}
	balance, err := f.service.wallet.Balance(ctx, f.clientID)
	if err != nil || balance.AvailableIRR != 1_000_000 || balance.HeldIRR != 0 {
		t.Fatalf("balance after release=%+v error=%v", balance, err)
	}
	var releaseCount int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE reference = $1`, "booking:"+jsonNumber(created.ID)+":release").Scan(&releaseCount); err != nil || releaseCount != 1 {
		t.Fatalf("release count=%d error=%v; want one", releaseCount, err)
	}
	_, err = f.pool.Exec(ctx, `UPDATE wallet_accounts SET balance_irr = 1 WHERE owner_user_id = $1 AND kind = 'customer_available'`, f.clientID)
	if err == nil {
		t.Fatal("direct balance update unexpectedly succeeded")
	}
	if err := f.service.wallet.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile released hold: %v", err)
	}

	var clientID int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ($1, 'test') RETURNING id`, randomEmail(t)).Scan(&clientID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.wallet.FundForDevelopment(ctx, clientID, 5_000, "funding-once"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.wallet.FundForDevelopment(ctx, clientID, 5_000, "funding-once"); err != nil {
		t.Fatalf("idempotent funding retry: %v", err)
	}
	if err := f.service.wallet.FundForDevelopment(ctx, clientID, 6_000, "funding-once"); !errors.Is(err, wallet.ErrIdempotencyConflict) {
		t.Fatalf("funding key payload conflict=%v; want conflict", err)
	}
	fundedBalance, err := f.service.wallet.Balance(ctx, clientID)
	if err != nil || fundedBalance.AvailableIRR != 5_000 {
		t.Fatalf("development funding balance=%+v error=%v", fundedBalance, err)
	}
}

func TestConcurrentWalletSpendingCannotOverspend(t *testing.T) {
	f := newBookingFixture(t)
	ctx := context.Background()
	var clientID int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ($1, 'test') RETURNING id`, randomEmail(t)).Scan(&clientID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.wallet.FundForDevelopment(ctx, clientID, 54_999, "limited-funds"); err != nil {
		t.Fatal(err)
	}
	start := nextBookingStart()
	services := []int64{f.serviceID, f.otherService}
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded, insufficient := 0, 0
	var unexpected []error
	for _, serviceID := range services {
		wg.Add(1)
		go func(serviceID int64) {
			defer wg.Done()
			_, err := f.service.Create(ctx, clientID, CreateInput{ServiceID: serviceID, StartsAt: start}, f.config)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				succeeded++
			} else if errors.Is(err, wallet.ErrInsufficientFunds) {
				insufficient++
			} else {
				unexpected = append(unexpected, err)
			}
		}(serviceID)
	}
	wg.Wait()
	if len(unexpected) != 0 || succeeded != 1 || insufficient != 1 {
		t.Fatalf("success=%d insufficient=%d unexpected=%v", succeeded, insufficient, unexpected)
	}
	balance, err := f.service.wallet.Balance(ctx, clientID)
	if err != nil {
		t.Fatal(err)
	}
	if balance.AvailableIRR+balance.HeldIRR != 54_999 || (balance.HeldIRR != 25_000 && balance.HeldIRR != 30_000) {
		t.Fatalf("concurrent spend balance=%+v", balance)
	}
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM bookings WHERE client_id = $1`, clientID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("bookings=%d error=%v; want one committed booking", count, err)
	}
	if err := f.service.wallet.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestInsufficientFundsRollsBackBookingAndHold(t *testing.T) {
	f := newBookingFixture(t)
	ctx := context.Background()
	var clientID int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ($1, 'test') RETURNING id`, randomEmail(t)).Scan(&clientID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Create(ctx, clientID, CreateInput{ServiceID: f.serviceID, StartsAt: nextBookingStart()}, f.config); !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("create booking without funds error=%v; want insufficient funds", err)
	}
	var bookings, holds, entries int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM bookings WHERE client_id = $1`, clientID).Scan(&bookings); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM wallet_holds WHERE customer_id = $1`, clientID).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries e JOIN wallet_accounts a ON a.id = e.account_id WHERE a.owner_user_id = $1`, clientID).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if bookings != 0 || holds != 0 || entries != 0 {
		t.Fatalf("rolled back state bookings=%d holds=%d entries=%d", bookings, holds, entries)
	}
}

func TestWalletAPIRequiresAuthenticationAndReturnsPrincipalBalance(t *testing.T) {
	f := newBookingFixture(t)
	app := fiber.New(fiber.Config{ErrorHandler: platform.ErrorHandler(slog.New(slog.NewTextHandler(io.Discard, nil)))})
	wallet.NewHandler(f.service.wallet).Register(app, integrationSecret)
	resp := bookingRequest(t, app, http.MethodGet, "/api/v1/wallet", "", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous wallet status=%d; want 401", resp.StatusCode)
	}
	resp.Body.Close()
	clientToken := bookingToken(t, f.clientID, []string{"client"})
	resp = bookingRequest(t, app, http.MethodGet, "/api/v1/wallet", clientToken, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated wallet status=%d; want 200", resp.StatusCode)
	}
	var balance wallet.Balance
	if err := json.NewDecoder(resp.Body).Decode(&balance); err != nil {
		t.Fatalf("decode wallet balance: %v", err)
	}
	resp.Body.Close()
	if balance.AvailableIRR != 1_000_000 || balance.HeldIRR != 0 || balance.Currency != wallet.CurrencyIRR {
		t.Fatalf("wallet returned unexpected principal balance: %+v", balance)
	}
}

func TestBookingRefundAuthorization(t *testing.T) {
	f := newBookingFixture(t)
	app := fiber.New(fiber.Config{ErrorHandler: platform.ErrorHandler(slog.New(slog.NewTextHandler(io.Discard, nil)))})
	NewHandler(f.service, f.config).Register(app, integrationSecret)
	clientToken := bookingToken(t, f.clientID, []string{"client"})
	adminToken := bookingToken(t, f.otherClientID, []string{"admin"})
	forgedAdminToken := bookingToken(t, f.clientID, []string{"admin"})
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO user_roles (user_id, role) VALUES ($1, 'admin')`, f.otherClientID); err != nil {
		t.Fatal(err)
	}
	created, err := f.service.Create(context.Background(), f.clientID, CreateInput{ServiceID: f.serviceID, StartsAt: nextBookingStart()}, f.config)
	if err != nil {
		t.Fatal(err)
	}
	resp := bookingRequest(t, app, http.MethodPost, "/api/v1/admin/bookings/"+jsonNumber(created.ID)+"/refund", clientToken, "", "refund-auth")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("client refund status=%d; want 403", resp.StatusCode)
	}
	resp.Body.Close()
	resp = bookingRequest(t, app, http.MethodPost, "/api/v1/admin/bookings/"+jsonNumber(created.ID)+"/refund", forgedAdminToken, "", "forged-refund")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unassigned administrator claim status=%d; want 403", resp.StatusCode)
	}
	resp.Body.Close()
	resp = bookingRequest(t, app, http.MethodPost, "/api/v1/admin/bookings/"+jsonNumber(created.ID)+"/refund", adminToken, "", "refund-auth")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("admin refund of unsettled booking status=%d; want 409", resp.StatusCode)
	}
	resp.Body.Close()
}

func assertBalancedLedger(t *testing.T, pool *pgxpool.Pool) {
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
		t.Fatalf("unbalanced transactions=%d", count)
	}
}
