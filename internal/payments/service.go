package payments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"test-api/internal/wallet"
)

const OfficialMinimumAmountIRR int64 = 10_000

var (
	ErrInvalidInput      = errors.New("invalid payment input")
	ErrPaymentNotFound   = errors.New("payment not found")
	ErrPaymentConflict   = errors.New("payment state conflict")
	ErrInvalidAuthority  = errors.New("invalid payment authority")
	ErrPaymentMismatch   = errors.New("verified payment does not match local payment")
)

type Payment struct {
	ID                 int64      `json:"id"`
	AmountIRR          int64      `json:"amount_irr"`
	Status             string     `json:"status"`
	Authority          *string    `json:"authority,omitempty"`
	GatewayReferenceID *string    `json:"gateway_reference_id,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	SucceededAt        *time.Time `json:"succeeded_at,omitempty"`
}

type Service struct {
	pool        *pgxpool.Pool
	wallet      *wallet.Service
	gateway     Gateway
	callbackURL string
}

func NewService(pool *pgxpool.Pool, walletService *wallet.Service, gateway Gateway, callbackURL string) *Service {
	return &Service{pool: pool, wallet: walletService, gateway: gateway, callbackURL: callbackURL}
}

func (s *Service) Start(ctx context.Context, userID, amountIRR int64, idempotencyKey, requestID string) (Payment, string, error) {
	if readiness, ok := s.gateway.(interface{ Configured() bool }); ok && !readiness.Configured() {
		return Payment{}, "", ErrGatewayUnavailable
	}
	if userID < 1 || amountIRR < OfficialMinimumAmountIRR || len(idempotencyKey) == 0 || len(idempotencyKey) > 128 || requestID == "" {
		return Payment{}, "", ErrInvalidInput
	}
	parsedCallback, err := url.ParseRequestURI(s.callbackURL)
	if err != nil || parsedCallback.Host == "" || (parsedCallback.Scheme != "http" && parsedCallback.Scheme != "https") {
		return Payment{}, "", ErrGatewayUnavailable
	}
	fingerprint := paymentFingerprint(amountIRR, s.callbackURL)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Payment{}, "", fmt.Errorf("begin payment creation: %w", err)
	}
	defer tx.Rollback(ctx)

	var payment Payment
	var inserted bool
	err = tx.QueryRow(ctx, `
		INSERT INTO payment_attempts (client_id, idempotency_key, request_fingerprint, amount_irr, callback_url, status, request_id, request_started_at)
		VALUES ($1, $2, $3, $4, $5, 'created', $6, now())
		ON CONFLICT (client_id, idempotency_key) DO NOTHING
		RETURNING id, amount_irr, status, authority, gateway_reference_id, created_at, updated_at, succeeded_at
	`, userID, idempotencyKey, fingerprint, amountIRR, s.callbackURL, requestID).Scan(
		&payment.ID, &payment.AmountIRR, &payment.Status, &payment.Authority, &payment.GatewayReferenceID, &payment.CreatedAt, &payment.UpdatedAt, &payment.SucceededAt,
	)
	if err == nil {
		inserted = true
	} else if errors.Is(err, pgx.ErrNoRows) {
		var storedFingerprint string
		if err := tx.QueryRow(ctx, `
			SELECT id, request_fingerprint, amount_irr, status, authority, gateway_reference_id, created_at, updated_at, succeeded_at
			FROM payment_attempts WHERE client_id = $1 AND idempotency_key = $2 FOR UPDATE
		`, userID, idempotencyKey).Scan(
			&payment.ID, &storedFingerprint, &payment.AmountIRR, &payment.Status, &payment.Authority, &payment.GatewayReferenceID, &payment.CreatedAt, &payment.UpdatedAt, &payment.SucceededAt,
		); err != nil {
			return Payment{}, "", fmt.Errorf("read idempotent payment: %w", err)
		}
		if storedFingerprint != fingerprint {
			return Payment{}, "", ErrPaymentConflict
		}
	} else {
		return Payment{}, "", fmt.Errorf("create payment record: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Payment{}, "", fmt.Errorf("commit payment record: %w", err)
	}

	if inserted || payment.Status == "created" {
		claimed, err := s.claimPaymentRequest(payment.ID)
		if err != nil {
			return Payment{}, "", err
		}
		if !claimed {
			payment, err = s.getByID(ctx, userID, payment.ID)
			return payment, "", err
		}
		created, gatewayErr := s.gateway.Create(ctx, CreateRequest{
			AmountIRR:   amountIRR,
			CallbackURL: s.callbackURL,
			Description: fmt.Sprintf("Wallet top-up %d", payment.ID),
		})
		if gatewayErr != nil {
			status, code := "verification_unknown", "request_outcome_unknown"
			if errors.Is(gatewayErr, ErrGatewayRejected) {
				status, code = "failed", "request_rejected"
			}
			if err := s.markRequestResult(payment.ID, status, code, ""); err != nil {
				return Payment{}, "", err
			}
			payment, err = s.getByID(ctx, userID, payment.ID)
			return payment, "", err
		}
		if !validAuthority(created.Authority) {
			if err := s.markRequestResult(payment.ID, "verification_unknown", "malformed_request_response", ""); err != nil {
				return Payment{}, "", err
			}
			payment, err = s.getByID(ctx, userID, payment.ID)
			return payment, "", err
		}
		if err := s.markRequestResult(payment.ID, "pending", "", created.Authority); err != nil {
			return Payment{}, "", err
		}
		payment, err = s.getByID(ctx, userID, payment.ID)
		if err != nil {
			return Payment{}, "", err
		}
	}

	redirectURL := ""
	if payment.Status == "pending" && payment.Authority != nil {
		redirectURL, err = s.gateway.RedirectURL(*payment.Authority)
		if err != nil {
			return Payment{}, "", fmt.Errorf("build gateway redirect URL: %w", err)
		}
	}
	return payment, redirectURL, nil
}

func (s *Service) Get(ctx context.Context, userID, paymentID int64) (Payment, error) {
	return s.getByID(ctx, userID, paymentID)
}

func (s *Service) HandleCallback(ctx context.Context, authority, callbackStatus, requestID string) (Payment, error) {
	if !validAuthority(authority) || len(callbackStatus) > 32 {
		return Payment{}, ErrInvalidAuthority
	}
	payment, err := s.getByAuthority(ctx, authority)
	if err != nil {
		return Payment{}, err
	}
	if payment.Status == "succeeded" || payment.Status == "failed" {
		return payment, nil
	}
	if payment.Status != "pending" && payment.Status != "verification_unknown" {
		return Payment{}, ErrPaymentConflict
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE payment_attempts SET status = 'verification_unknown', callback_status = $1,
			verification_attempts = verification_attempts + 1, verification_started_at = now(), updated_at = now()
		WHERE id = $2 AND status IN ('pending', 'verification_unknown')
	`, callbackStatus, payment.ID); err != nil {
		return Payment{}, fmt.Errorf("record verification attempt: %w", err)
	}

	verified, err := s.gateway.Verify(ctx, VerifyRequest{Authority: authority, AmountIRR: payment.AmountIRR})
	if err != nil {
		return s.getByAuthority(ctx, authority)
	}
	if verified.Authority != authority || verified.AmountIRR != payment.AmountIRR || (verified.Approved && verified.ReferenceID == "") {
		return s.getByAuthority(ctx, authority)
	}
	if !verified.Approved {
		if err := s.finishVerification(ctx, payment.ID, authority, verified, requestID); err != nil {
			return Payment{}, err
		}
		return s.getByAuthority(ctx, authority)
	}
	if err := s.finishVerification(ctx, payment.ID, authority, verified, requestID); err != nil {
		return Payment{}, err
	}
	return s.getByAuthority(ctx, authority)
}

func (s *Service) ReconcileUnknown(ctx context.Context, limit int) (int, error) {
	if readiness, ok := s.gateway.(interface{ Configured() bool }); ok && !readiness.Configured() {
		return 0, ErrGatewayUnavailable
	}
	if limit < 1 || limit > 100 {
		return 0, ErrInvalidInput
	}
	rows, err := s.pool.Query(ctx, `
		SELECT authority, callback_status FROM payment_attempts
		WHERE status = 'verification_unknown' AND authority IS NOT NULL
		ORDER BY updated_at, id LIMIT $1
	`, limit)
	if err != nil {
		return 0, fmt.Errorf("list uncertain payments: %w", err)
	}
	var candidates []struct{ authority, status string }
	for rows.Next() {
		var item struct{ authority, status string }
		if err := rows.Scan(&item.authority, &item.status); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	resolved := 0
	for _, item := range candidates {
		payment, err := s.HandleCallback(ctx, item.authority, item.status, "wallet-reconcile")
		if err != nil {
			return resolved, err
		}
		if payment.Status == "succeeded" || payment.Status == "failed" {
			resolved++
		}
	}
	return resolved, nil
}

func (s *Service) finishVerification(ctx context.Context, paymentID int64, authority string, result Verification, requestID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin payment verification result: %w", err)
	}
	defer tx.Rollback(ctx)
	var payment Payment
	var userID int64
	var gatewayReference *string
	if err := tx.QueryRow(ctx, `
		SELECT id, client_id, amount_irr, status, authority, gateway_reference_id, created_at, updated_at, succeeded_at
		FROM payment_attempts WHERE id = $1 FOR UPDATE
	`, paymentID).Scan(&payment.ID, &userID, &payment.AmountIRR, &payment.Status, &payment.Authority, &gatewayReference, &payment.CreatedAt, &payment.UpdatedAt, &payment.SucceededAt); err != nil {
		return fmt.Errorf("lock payment verification result: %w", err)
	}
	if payment.Status == "succeeded" || payment.Status == "failed" {
		return tx.Commit(ctx)
	}
	if payment.Status != "verification_unknown" || payment.Authority == nil || *payment.Authority != authority {
		return ErrPaymentConflict
	}
	if result.Approved {
		if err := s.wallet.CreditPaymentTx(ctx, tx, userID, paymentID, payment.AmountIRR, requestID); err != nil {
			return fmt.Errorf("credit verified wallet payment: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE payment_attempts SET status = 'succeeded', gateway_reference_id = NULLIF($1, ''),
				gateway_code = $2, ledger_transaction_id = (
					SELECT id FROM ledger_transactions WHERE reference = $3
				), succeeded_at = now(), updated_at = now()
			WHERE id = $4
		`, result.ReferenceID, result.GatewayCode, fmt.Sprintf("payment:%d:funding", paymentID), paymentID); err != nil {
			return fmt.Errorf("mark payment succeeded: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, `
			UPDATE payment_attempts SET status = 'failed', gateway_code = $1, updated_at = now()
			WHERE id = $2
		`, result.GatewayCode, paymentID); err != nil {
			return fmt.Errorf("mark payment failed: %w", err)
		}
	}
	return tx.Commit(ctx)
}

func (s *Service) claimPaymentRequest(paymentID int64) (bool, error) {
	command, err := s.pool.Exec(context.Background(), `
		UPDATE payment_attempts SET status = 'verification_unknown', updated_at = now()
		WHERE id = $1 AND status = 'created'
	`, paymentID)
	if err != nil {
		return false, fmt.Errorf("claim payment request: %w", err)
	}
	return command.RowsAffected() == 1, nil
}

func (s *Service) markRequestResult(paymentID int64, status, gatewayCode, authority string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var authorityValue any
	if authority != "" {
		authorityValue = authority
	}
	command, err := s.pool.Exec(ctx, `
		UPDATE payment_attempts SET status = $1, gateway_code = NULLIF($2, ''), authority = $3, updated_at = now()
		WHERE id = $4 AND status = 'verification_unknown'
	`, status, gatewayCode, authorityValue, paymentID)
	if err != nil {
		return fmt.Errorf("persist gateway payment request result: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrPaymentConflict
	}
	return nil
}

func (s *Service) getByID(ctx context.Context, userID, paymentID int64) (Payment, error) {
	var payment Payment
	err := s.pool.QueryRow(ctx, `
		SELECT id, amount_irr, status, authority, gateway_reference_id, created_at, updated_at, succeeded_at
		FROM payment_attempts WHERE client_id = $1 AND id = $2
	`, userID, paymentID).Scan(&payment.ID, &payment.AmountIRR, &payment.Status, &payment.Authority, &payment.GatewayReferenceID, &payment.CreatedAt, &payment.UpdatedAt, &payment.SucceededAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrPaymentNotFound
	}
	if err != nil {
		return Payment{}, fmt.Errorf("read payment: %w", err)
	}
	return payment, nil
}

func (s *Service) getByAuthority(ctx context.Context, authority string) (Payment, error) {
	var payment Payment
	err := s.pool.QueryRow(ctx, `
		SELECT id, amount_irr, status, authority, gateway_reference_id, created_at, updated_at, succeeded_at
		FROM payment_attempts WHERE authority = $1
	`, authority).Scan(&payment.ID, &payment.AmountIRR, &payment.Status, &payment.Authority, &payment.GatewayReferenceID, &payment.CreatedAt, &payment.UpdatedAt, &payment.SucceededAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrPaymentNotFound
	}
	if err != nil {
		return Payment{}, fmt.Errorf("read payment by authority: %w", err)
	}
	return payment, nil
}

func paymentFingerprint(amountIRR int64, callbackURL string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("zarinpal|%d|%s", amountIRR, callbackURL)))
	return hex.EncodeToString(sum[:])
}

func validAuthority(authority string) bool {
	if len(authority) != 36 || (authority[0] != 'A' && authority[0] != 'S') {
		return false
	}
	for _, r := range authority[1:] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}
