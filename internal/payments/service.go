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

	"test-api/internal/booking"
	"test-api/internal/wallet"
)

const OfficialMinimumAmountIRR int64 = 10_000

var (
	ErrInvalidInput     = errors.New("invalid payment input")
	ErrPaymentNotFound  = errors.New("payment not found")
	ErrPaymentConflict  = errors.New("payment state conflict")
	ErrInvalidAuthority = errors.New("invalid payment authority")
	ErrPaymentMismatch  = errors.New("verified payment does not match local payment")
)

const (
	PurposeWalletTopUp = "wallet_topup"
	PurposeBooking     = "booking"
)

type Payment struct {
	ID                 int64      `json:"id"`
	AmountIRR          int64      `json:"amount_irr"`
	Purpose            string     `json:"purpose"`
	BookingID          *int64     `json:"booking_id,omitempty"`
	Status             string     `json:"status"`
	Authority          *string    `json:"authority,omitempty"`
	GatewayReferenceID *string    `json:"gateway_reference_id,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	SucceededAt        *time.Time `json:"succeeded_at,omitempty"`
}

type BookingPaymentConfirmer interface {
	ConfirmGatewayPaymentTx(ctx context.Context, tx pgx.Tx, input booking.GatewayPaymentConfirmation) error
	FailGatewayPaymentTx(ctx context.Context, tx pgx.Tx, bookingID, clientID int64, requestID string) error
}

type Service struct {
	pool           *pgxpool.Pool
	wallet         *wallet.Service
	gateway        Gateway
	callbackURL    string
	bookingPayment BookingPaymentConfirmer
}

func NewService(pool *pgxpool.Pool, walletService *wallet.Service, gateway Gateway, callbackURL string, bookingPayment ...BookingPaymentConfirmer) *Service {
	service := &Service{pool: pool, wallet: walletService, gateway: gateway, callbackURL: callbackURL}
	if len(bookingPayment) > 0 {
		service.bookingPayment = bookingPayment[0]
	}
	return service
}

func (s *Service) Start(ctx context.Context, userID, amountIRR int64, idempotencyKey, requestID string) (Payment, string, error) {
	return s.start(ctx, userID, amountIRR, PurposeWalletTopUp, 0, idempotencyKey, requestID)
}

func (s *Service) StartBooking(ctx context.Context, userID, bookingID int64, idempotencyKey, requestID string) (Payment, string, error) {
	if bookingID < 1 {
		return Payment{}, "", ErrInvalidInput
	}
	return s.start(ctx, userID, 0, PurposeBooking, bookingID, idempotencyKey, requestID)
}

func (s *Service) start(ctx context.Context, userID, amountIRR int64, purpose string, bookingID int64, idempotencyKey, requestID string) (Payment, string, error) {
	if readiness, ok := s.gateway.(interface{ Configured() bool }); ok && !readiness.Configured() {
		return Payment{}, "", ErrGatewayUnavailable
	}
	if userID < 1 || len(idempotencyKey) == 0 || len(idempotencyKey) > 128 || requestID == "" {
		return Payment{}, "", ErrInvalidInput
	}
	if purpose != PurposeWalletTopUp && purpose != PurposeBooking {
		return Payment{}, "", ErrInvalidInput
	}
	if purpose == PurposeWalletTopUp && amountIRR < OfficialMinimumAmountIRR {
		return Payment{}, "", ErrInvalidInput
	}
	parsedCallback, err := url.ParseRequestURI(s.callbackURL)
	if err != nil || parsedCallback.Host == "" || (parsedCallback.Scheme != "http" && parsedCallback.Scheme != "https") {
		return Payment{}, "", ErrGatewayUnavailable
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Payment{}, "", fmt.Errorf("begin payment creation: %w", err)
	}
	defer tx.Rollback(ctx)

	var existing *Payment
	if purpose == PurposeBooking {
		var ownerID, priceIRR int64
		var status, method string
		var expiresAt time.Time
		if err := tx.QueryRow(ctx, `
			SELECT client_id, price_irr, status, payment_method, payment_expires_at
			FROM bookings WHERE id = $1 FOR UPDATE
		`, bookingID).Scan(&ownerID, &priceIRR, &status, &method, &expiresAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return Payment{}, "", ErrPaymentNotFound
			}
			return Payment{}, "", fmt.Errorf("lock booking for payment: %w", err)
		}
		if ownerID != userID || status != "pending_payment" || method != "zarinpal" {
			return Payment{}, "", ErrPaymentConflict
		}
		if priceIRR < OfficialMinimumAmountIRR || !expiresAt.After(time.Now().UTC()) {
			return Payment{}, "", ErrPaymentConflict
		}
		amountIRR = priceIRR

		var found Payment
		err = tx.QueryRow(ctx, `
			SELECT id, amount_irr, purpose, booking_id, status, authority, gateway_reference_id, created_at, updated_at, succeeded_at
			FROM payment_attempts
			WHERE booking_id = $1 AND purpose = 'booking'
			  AND status IN ('created', 'pending', 'verification_unknown')
			FOR UPDATE
		`, bookingID).Scan(
			&found.ID, &found.AmountIRR, &found.Purpose, &found.BookingID, &found.Status,
			&found.Authority, &found.GatewayReferenceID, &found.CreatedAt, &found.UpdatedAt, &found.SucceededAt,
		)
		if err == nil {
			existing = &found
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return Payment{}, "", fmt.Errorf("read active booking payment: %w", err)
		}
	}

	var payment Payment
	var inserted bool
	if existing == nil {
		fingerprint := paymentFingerprint(purpose, bookingID, amountIRR, s.callbackURL)
		var bookingValue any
		if bookingID > 0 {
			bookingValue = bookingID
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO payment_attempts (
				client_id, idempotency_key, request_fingerprint, amount_irr, purpose, booking_id,
				callback_url, status, request_id, request_started_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'created', $8, now())
			ON CONFLICT (client_id, idempotency_key) DO NOTHING
			RETURNING id, amount_irr, purpose, booking_id, status, authority, gateway_reference_id, created_at, updated_at, succeeded_at
		`, userID, idempotencyKey, fingerprint, amountIRR, purpose, bookingValue, s.callbackURL, requestID).Scan(
			&payment.ID, &payment.AmountIRR, &payment.Purpose, &payment.BookingID, &payment.Status,
			&payment.Authority, &payment.GatewayReferenceID, &payment.CreatedAt, &payment.UpdatedAt, &payment.SucceededAt,
		)
		if err == nil {
			inserted = true
		} else if errors.Is(err, pgx.ErrNoRows) {
			var storedFingerprint string
			if err := tx.QueryRow(ctx, `
				SELECT id, request_fingerprint, amount_irr, purpose, booking_id, status, authority, gateway_reference_id, created_at, updated_at, succeeded_at
				FROM payment_attempts WHERE client_id = $1 AND idempotency_key = $2 FOR UPDATE
			`, userID, idempotencyKey).Scan(
				&payment.ID, &storedFingerprint, &payment.AmountIRR, &payment.Purpose, &payment.BookingID,
				&payment.Status, &payment.Authority, &payment.GatewayReferenceID, &payment.CreatedAt, &payment.UpdatedAt, &payment.SucceededAt,
			); err != nil {
				return Payment{}, "", fmt.Errorf("read idempotent payment: %w", err)
			}
			if storedFingerprint != fingerprint {
				return Payment{}, "", ErrPaymentConflict
			}
		} else {
			return Payment{}, "", fmt.Errorf("create payment record: %w", err)
		}
	} else {
		payment = *existing
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
		description := fmt.Sprintf("Wallet top-up %d", payment.ID)
		if purpose == PurposeBooking {
			description = fmt.Sprintf("Booking %d payment %d", bookingID, payment.ID)
		}
		created, gatewayErr := s.gateway.Create(ctx, CreateRequest{
			AmountIRR:   amountIRR,
			CallbackURL: s.callbackURL,
			Description: description,
		})
		if gatewayErr != nil {
			status, code := "verification_unknown", "request_outcome_unknown"
			if errors.Is(gatewayErr, ErrGatewayRejected) {
				status, code = "failed", "request_rejected"
			}
			if err := s.markRequestResult(payment.ID, status, code, ""); err != nil {
				return Payment{}, "", err
			}
			if purpose == PurposeBooking && status == "failed" {
				if err := s.cancelUnpaidBooking(ctx, bookingID, userID, requestID); err != nil {
					return Payment{}, "", err
				}
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

// UnresolvedReport makes payment attempts that cannot be resolved automatically
// visible to operators.
type UnresolvedReport struct {
	PendingWithAuthority    int64 `json:"pending_with_authority"`
	PendingWithoutAuthority int64 `json:"pending_without_authority"`
	UnknownWithAuthority    int64 `json:"unknown_with_authority"`
	UnknownWithoutAuthority int64 `json:"unknown_without_authority"`
}

// UnresolvedReport counts payment attempts still awaiting an outcome. Zarinpal
// offers no documented way to identify attempts whose authority was never
// stored, so those rows stay unresolved until an operator reconciles them
// manually; the reconciliation command never credits or fails them.
func (s *Service) UnresolvedReport(ctx context.Context) (UnresolvedReport, error) {
	var report UnresolvedReport
	err := s.pool.QueryRow(ctx, `
		SELECT
			COALESCE(count(*) FILTER (WHERE status = 'pending' AND authority IS NOT NULL), 0),
			COALESCE(count(*) FILTER (WHERE status = 'pending' AND authority IS NULL), 0),
			COALESCE(count(*) FILTER (WHERE status = 'verification_unknown' AND authority IS NOT NULL), 0),
			COALESCE(count(*) FILTER (WHERE status = 'verification_unknown' AND authority IS NULL), 0)
		FROM payment_attempts
		WHERE status IN ('pending', 'verification_unknown')
	`).Scan(&report.PendingWithAuthority, &report.PendingWithoutAuthority, &report.UnknownWithAuthority, &report.UnknownWithoutAuthority)
	if err != nil {
		return UnresolvedReport{}, fmt.Errorf("summarize unresolved payments: %w", err)
	}
	return report, nil
}

func (s *Service) cancelUnpaidBooking(ctx context.Context, bookingID, userID int64, requestID string) error {
	if s.bookingPayment == nil {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin failed booking payment cancellation: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := s.bookingPayment.FailGatewayPaymentTx(ctx, tx, bookingID, userID, requestID); err != nil {
		return fmt.Errorf("cancel booking after gateway rejection: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *Service) finishVerification(ctx context.Context, paymentID int64, authority string, result Verification, requestID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin payment verification result: %w", err)
	}
	defer tx.Rollback(ctx)

	var purpose string
	var bookingID *int64
	if err := tx.QueryRow(ctx, `
		SELECT purpose, booking_id FROM payment_attempts WHERE id = $1
	`, paymentID).Scan(&purpose, &bookingID); err != nil {
		return fmt.Errorf("read payment purpose: %w", err)
	}
	if purpose == PurposeBooking && bookingID != nil {
		var lockedID int64
		if err := tx.QueryRow(ctx, `SELECT id FROM bookings WHERE id = $1 FOR UPDATE`, *bookingID).Scan(&lockedID); err != nil {
			return fmt.Errorf("lock booking for payment result: %w", err)
		}
	}

	var payment Payment
	var userID int64
	var gatewayReference *string
	if err := tx.QueryRow(ctx, `
		SELECT id, client_id, amount_irr, purpose, booking_id, status, authority, gateway_reference_id, created_at, updated_at, succeeded_at
		FROM payment_attempts WHERE id = $1 FOR UPDATE
	`, paymentID).Scan(
		&payment.ID, &userID, &payment.AmountIRR, &payment.Purpose, &payment.BookingID, &payment.Status,
		&payment.Authority, &gatewayReference, &payment.CreatedAt, &payment.UpdatedAt, &payment.SucceededAt,
	); err != nil {
		return fmt.Errorf("lock payment verification result: %w", err)
	}
	if payment.Status == "succeeded" || payment.Status == "failed" {
		return tx.Commit(ctx)
	}
	if payment.Status != "verification_unknown" || payment.Authority == nil || *payment.Authority != authority {
		return ErrPaymentConflict
	}
	if result.Approved {
		ledgerReference := fmt.Sprintf("payment:%d:funding", paymentID)
		if payment.Purpose == PurposeBooking {
			if s.bookingPayment == nil || payment.BookingID == nil {
				return ErrPaymentConflict
			}
			err := s.bookingPayment.ConfirmGatewayPaymentTx(ctx, tx, booking.GatewayPaymentConfirmation{
				BookingID: *payment.BookingID,
				ClientID:  userID,
				PaymentID: paymentID,
				AmountIRR: payment.AmountIRR,
				RequestID: requestID,
			})
			if errors.Is(err, booking.ErrPaymentAmountMismatch) {
				return ErrPaymentMismatch
			}
			if errors.Is(err, booking.ErrInvalidTransition) || errors.Is(err, booking.ErrBookingNotFound) {
				return ErrPaymentConflict
			}
			if err != nil {
				return fmt.Errorf("confirm booking payment: %w", err)
			}
			ledgerReference = fmt.Sprintf("payment:%d:capture", paymentID)
		} else if err := s.wallet.CreditPaymentTx(ctx, tx, userID, paymentID, payment.AmountIRR, requestID); err != nil {
			return fmt.Errorf("credit verified wallet payment: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE payment_attempts SET status = 'succeeded', gateway_reference_id = NULLIF($1, ''),
				gateway_code = $2, ledger_transaction_id = (
					SELECT id FROM ledger_transactions WHERE reference = $3
				), succeeded_at = now(), updated_at = now()
			WHERE id = $4
		`, result.ReferenceID, result.GatewayCode, ledgerReference, paymentID); err != nil {
			return fmt.Errorf("mark payment succeeded: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, `
			UPDATE payment_attempts SET status = 'failed', gateway_code = $1, updated_at = now()
			WHERE id = $2
		`, result.GatewayCode, paymentID); err != nil {
			return fmt.Errorf("mark payment failed: %w", err)
		}
		if payment.Purpose == PurposeBooking && payment.BookingID != nil {
			if s.bookingPayment == nil {
				return ErrPaymentConflict
			}
			if err := s.bookingPayment.FailGatewayPaymentTx(ctx, tx, *payment.BookingID, userID, requestID); err != nil {
				return fmt.Errorf("release failed booking payment: %w", err)
			}
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
		SELECT id, amount_irr, purpose, booking_id, status, authority, gateway_reference_id, created_at, updated_at, succeeded_at
		FROM payment_attempts WHERE client_id = $1 AND id = $2
	`, userID, paymentID).Scan(&payment.ID, &payment.AmountIRR, &payment.Purpose, &payment.BookingID, &payment.Status, &payment.Authority, &payment.GatewayReferenceID, &payment.CreatedAt, &payment.UpdatedAt, &payment.SucceededAt)
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
		SELECT id, amount_irr, purpose, booking_id, status, authority, gateway_reference_id, created_at, updated_at, succeeded_at
		FROM payment_attempts WHERE authority = $1
	`, authority).Scan(&payment.ID, &payment.AmountIRR, &payment.Purpose, &payment.BookingID, &payment.Status, &payment.Authority, &payment.GatewayReferenceID, &payment.CreatedAt, &payment.UpdatedAt, &payment.SucceededAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, ErrPaymentNotFound
	}
	if err != nil {
		return Payment{}, fmt.Errorf("read payment by authority: %w", err)
	}
	return payment, nil
}

func paymentFingerprint(purpose string, bookingID, amountIRR int64, callbackURL string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("zarinpal|%s|%d|%d|%s", purpose, bookingID, amountIRR, callbackURL)))
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
