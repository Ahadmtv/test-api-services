package payments

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgxpool"

	"test-api/internal/identity"
	"test-api/internal/platform"
	"test-api/internal/wallet"
)

const testAccessSecret = "01234567890123456789012345678901"

func TestPaymentTopUpSuccessAndDuplicateCallback(t *testing.T) {
	f := newPaymentFixture(t)
	ctx := context.Background()
	payment, redirect, err := f.service.Start(ctx, f.userID, 50_000, "top-up-1", "request-1")
	if err != nil {
		t.Fatal(err)
	}
	if payment.Status != "pending" || redirect == "" || f.gateway.CreateCalls != 1 {
		t.Fatalf("payment=%+v redirect=%q create calls=%d", payment, redirect, f.gateway.CreateCalls)
	}

	if _, err := f.service.HandleCallback(ctx, *payment.Authority, "OK", "callback-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.HandleCallback(ctx, *payment.Authority, "OK", "callback-2"); err != nil {
		t.Fatal(err)
	}
	balance, err := f.wallet.Balance(ctx, f.userID)
	if err != nil || balance.AvailableIRR != 50_000 {
		t.Fatalf("balance=%+v error=%v", balance, err)
	}
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE reference = $1`, "payment:"+strconvID(payment.ID)+":funding").Scan(&count); err != nil || count != 1 {
		t.Fatalf("funding postings=%d error=%v", count, err)
	}
	if err := f.wallet.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPaymentDuplicateCallbackRaceCreditsOnce(t *testing.T) {
	f := newPaymentFixture(t)
	ctx := context.Background()
	payment, _, err := f.service.Start(ctx, f.userID, 20_000, "top-up-race", "request-race")
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	var mu sync.Mutex
	var failures []error
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := f.service.HandleCallback(ctx, *payment.Authority, "OK", "callback-race"); err != nil {
				mu.Lock()
				failures = append(failures, err)
				mu.Unlock()
			}
		}()
	}
	wait.Wait()
	if len(failures) != 0 {
		t.Fatalf("callback errors=%v", failures)
	}
	balance, err := f.wallet.Balance(ctx, f.userID)
	if err != nil || balance.AvailableIRR != 20_000 {
		t.Fatalf("balance=%+v error=%v", balance, err)
	}
	if f.gateway.VerificationCalls < 1 {
		t.Fatal("gateway was not verified")
	}
}

func TestPaymentInvalidAuthorityAndAmountMismatchDoNotCredit(t *testing.T) {
	f := newPaymentFixture(t)
	ctx := context.Background()
	payment, _, err := f.service.Start(ctx, f.userID, 20_000, "top-up-mismatch", "request-mismatch")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.HandleCallback(ctx, "invalid", "OK", "bad-callback"); !errors.Is(err, ErrInvalidAuthority) {
		t.Fatalf("invalid authority error=%v", err)
	}
	f.gateway.NextVerify = Verification{Authority: *payment.Authority, AmountIRR: payment.AmountIRR + 1, ReferenceID: "mismatch-ref", GatewayCode: "100", Approved: true}
	result, err := f.service.HandleCallback(ctx, *payment.Authority, "OK", "mismatch-callback")
	if err != nil || result.Status != "verification_unknown" {
		t.Fatalf("payment=%+v error=%v", result, err)
	}
	balance, err := f.wallet.Balance(ctx, f.userID)
	if err != nil || balance.AvailableIRR != 0 {
		t.Fatalf("balance=%+v error=%v", balance, err)
	}
}

func TestPaymentDeclinedAndUnknownVerification(t *testing.T) {
	f := newPaymentFixture(t)
	ctx := context.Background()
	payment, _, err := f.service.Start(ctx, f.userID, 15_000, "top-up-declined", "request-declined")
	if err != nil {
		t.Fatal(err)
	}
	f.gateway.NextVerify = Verification{Authority: *payment.Authority, AmountIRR: payment.AmountIRR, GatewayCode: "failed", Approved: false}
	result, err := f.service.HandleCallback(ctx, *payment.Authority, "NOK", "callback-declined")
	if err != nil || result.Status != "failed" {
		t.Fatalf("payment=%+v error=%v", result, err)
	}
	balance, err := f.wallet.Balance(ctx, f.userID)
	if err != nil || balance.AvailableIRR != 0 {
		t.Fatalf("balance=%+v error=%v", balance, err)
	}

	other, _, err := f.service.Start(ctx, f.userID, 15_000, "top-up-unknown", "request-unknown")
	if err != nil {
		t.Fatal(err)
	}
	f.gateway.NextVerifyErr = ErrGatewayUnknownOutcome
	result, err = f.service.HandleCallback(ctx, *other.Authority, "OK", "callback-unknown")
	if err != nil || result.Status != "verification_unknown" {
		t.Fatalf("payment=%+v error=%v", result, err)
	}
	f.gateway.NextVerify = Verification{Authority: *other.Authority, AmountIRR: other.AmountIRR, ReferenceID: "ref-2", GatewayCode: "100", Approved: true}
	resolved, err := f.service.ReconcileUnknown(ctx, 100)
	if err != nil || resolved != 1 {
		t.Fatalf("resolved=%d error=%v", resolved, err)
	}
	balance, err = f.wallet.Balance(ctx, f.userID)
	if err != nil || balance.AvailableIRR != 15_000 {
		t.Fatalf("balance after reconcile=%+v error=%v", balance, err)
	}
}

func TestPaymentDatabaseFailureAfterGatewaySuccessIsAtomicAndRetryable(t *testing.T) {
	f := newPaymentFixture(t)
	ctx := context.Background()
	payment, _, err := f.service.Start(ctx, f.userID, 20_000, "top-up-db-failure", "request-db-failure")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		CREATE FUNCTION reject_payment_success() RETURNS trigger AS $$
		BEGIN
			IF NEW.status = 'succeeded' THEN
				RAISE EXCEPTION 'forced payment result failure';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER reject_payment_success BEFORE UPDATE ON payment_attempts
		FOR EACH ROW EXECUTE FUNCTION reject_payment_success();
	`); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.HandleCallback(ctx, *payment.Authority, "OK", "callback-fail-db")
	if err == nil {
		t.Fatal("expected database failure")
	}
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM payment_attempts WHERE id = $1`, payment.ID).Scan(&status); err != nil || status != "verification_unknown" {
		t.Fatalf("status=%q error=%v", status, err)
	}
	balance, err := f.wallet.Balance(ctx, f.userID)
	if err != nil || balance.AvailableIRR != 0 {
		t.Fatalf("balance=%+v error=%v", balance, err)
	}
	if _, err := f.pool.Exec(ctx, `DROP TRIGGER reject_payment_success ON payment_attempts; DROP FUNCTION reject_payment_success()`); err != nil {
		t.Fatal(err)
	}
	resolved, err := f.service.ReconcileUnknown(ctx, 100)
	if err != nil || resolved != 1 {
		t.Fatalf("resolved=%d error=%v", resolved, err)
	}
	balance, err = f.wallet.Balance(ctx, f.userID)
	if err != nil || balance.AvailableIRR != 20_000 {
		t.Fatalf("balance after retry=%+v error=%v", balance, err)
	}
}

func TestPaymentCreateUnknownOutcomeIsNotRetried(t *testing.T) {
	f := newPaymentFixture(t)
	f.gateway.NextCreateErr = ErrGatewayUnknownOutcome
	payment, redirect, err := f.service.Start(context.Background(), f.userID, 10_000, "unknown-create", "request-unknown-create")
	if err != nil || payment.Status != "verification_unknown" || redirect != "" || f.gateway.CreateCalls != 1 {
		t.Fatalf("payment=%+v redirect=%q create calls=%d error=%v", payment, redirect, f.gateway.CreateCalls, err)
	}
	retry, redirect, err := f.service.Start(context.Background(), f.userID, 10_000, "unknown-create", "request-unknown-create-retry")
	if err != nil || retry.ID != payment.ID || retry.Status != "verification_unknown" || redirect != "" || f.gateway.CreateCalls != 1 {
		t.Fatalf("retry=%+v redirect=%q create calls=%d error=%v", retry, redirect, f.gateway.CreateCalls, err)
	}
	report, err := f.service.UnresolvedReport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.UnknownWithoutAuthority != 1 || report.UnknownWithAuthority != 0 || report.PendingWithoutAuthority != 0 || report.PendingWithAuthority != 0 {
		t.Fatalf("unresolved report=%+v", report)
	}
}

func TestPaymentStartRejectsAmountBelowDocumentedMinimum(t *testing.T) {
	f := newPaymentFixture(t)
	if _, _, err := f.service.Start(context.Background(), f.userID, OfficialMinimumAmountIRR-1, "small-amount", "request-small-amount"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("below-minimum amount error=%v", err)
	}
	if f.gateway.CreateCalls != 0 {
		t.Fatalf("gateway was called create=%d", f.gateway.CreateCalls)
	}
}

func TestPaymentStartIdempotencyAndAuthorization(t *testing.T) {
	f := newPaymentFixture(t)
	ctx := context.Background()
	first, _, err := f.service.Start(ctx, f.userID, 10_000, "same-key", "request-1")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := f.service.Start(ctx, f.userID, 10_000, "same-key", "request-2")
	if err != nil || first.ID != second.ID || f.gateway.CreateCalls != 1 {
		t.Fatalf("first=%+v second=%+v create calls=%d error=%v", first, second, f.gateway.CreateCalls, err)
	}
	if _, _, err := f.service.Start(ctx, f.userID, 10_001, "same-key", "request-3"); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("different payload error=%v", err)
	}

	app := fiber.New(fiber.Config{ErrorHandler: platform.ErrorHandler(slog.New(slog.NewTextHandler(io.Discard, nil)))})
	app.Use(platform.RequestID())
	NewHandler(f.service, nil).Register(app, testAccessSecret)
	response, err := app.Test(httptest.NewRequest(http.MethodPost, "/api/v1/payments/zarinpal", strings.NewReader(`{"amount_irr":10000}`)))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", response.StatusCode)
	}
	token, _, err := identity.IssueAccessToken(f.userID, "client@example.test", []string{"client"}, identity.AccessTokenConfig{Secret: testAccessSecret, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/payments/zarinpal", bytes.NewBufferString(`{"amount_irr":10000}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Idempotency-Key", "api-start")
	response, err = app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("authorized payment status=%d", response.StatusCode)
	}
}

type paymentFixture struct {
	pool    *pgxpool.Pool
	wallet  *wallet.Service
	service *Service
	gateway *FakeGateway
	userID  int64
}

func newPaymentFixture(t *testing.T) *paymentFixture {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set; skipping PostgreSQL payment integration test")
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
		t.Fatal(err)
	}
	schema := "payment_test_" + hex.EncodeToString(schemaBytes)
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() { _, _ = adminPool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	migrator, err := platform.NewMigrator(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	f := &paymentFixture{pool: pool, wallet: wallet.NewService(pool), gateway: &FakeGateway{}}
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ($1, 'test') RETURNING id`, "payment-"+hex.EncodeToString(schemaBytes)+"@example.test").Scan(&f.userID); err != nil {
		t.Fatal(err)
	}
	f.service = NewService(pool, f.wallet, f.gateway, "https://example.test/api/v1/payments/zarinpal/callback")
	return f
}

func strconvID(id int64) string {
	return fmt.Sprint(id)
}

var _ = identity.IssueAccessToken
