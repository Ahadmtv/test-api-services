package wallet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Service exposes wallet reads, reconciliation, and transaction-scoped financial operations.
type Service struct{ pool *pgxpool.Pool }

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Balance reports customer funds and provider earnings in IRR.
type Balance struct {
	AvailableIRR        int64  `json:"available_irr"`
	HeldIRR             int64  `json:"held_irr"`
	ProviderEarningsIRR int64  `json:"provider_earnings_irr"`
	PayoutReservedIRR   int64  `json:"provider_payout_reserved_irr"`
	Currency            string `json:"currency"`
}

type Transaction struct {
	ID        int64     `json:"id"`
	Reference string    `json:"reference"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
	AmountIRR int64     `json:"amount_irr"`
}

func (s *Service) Balance(ctx context.Context, userID int64) (*Balance, error) {
	if userID < 1 {
		return nil, ErrInvalidInput
	}
	var b Balance
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(balance_irr) FILTER (WHERE kind = 'customer_available'), 0),
		       COALESCE(MAX(balance_irr) FILTER (WHERE kind = 'customer_held'), 0),
		       COALESCE(MAX(balance_irr) FILTER (WHERE kind = 'provider_earnings'), 0),
		       COALESCE(MAX(balance_irr) FILTER (WHERE kind = 'provider_payout_reserved'), 0)
		FROM wallet_accounts WHERE owner_user_id = $1`, userID).Scan(&b.AvailableIRR, &b.HeldIRR, &b.ProviderEarningsIRR, &b.PayoutReservedIRR)
	if err != nil {
		return nil, fmt.Errorf("read wallet balance: %w", err)
	}
	b.Currency = CurrencyIRR
	return &b, nil
}

// Transactions returns customer-affecting ledger documents after the opaque numeric cursor.
func (s *Service) Transactions(ctx context.Context, userID, after int64, limit int) ([]Transaction, bool, error) {
	if userID < 1 || after < 0 || limit < 1 || limit > 100 {
		return nil, false, ErrInvalidInput
	}
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.reference, t.kind, t.created_at, MAX(e.amount_irr)::bigint
		FROM ledger_transactions t
		JOIN ledger_entries e ON e.transaction_id = t.id
		JOIN wallet_accounts a ON a.id = e.account_id
		WHERE a.owner_user_id = $1 AND t.id > $2
		GROUP BY t.id, t.reference, t.kind, t.created_at
		ORDER BY t.id LIMIT $3`, userID, after, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("list wallet transactions: %w", err)
	}
	defer rows.Close()
	items := make([]Transaction, 0, limit)
	for rows.Next() {
		var item Transaction
		if err := rows.Scan(&item.ID, &item.Reference, &item.Kind, &item.CreatedAt, &item.AmountIRR); err != nil {
			return nil, false, err
		}
		item.CreatedAt = item.CreatedAt.UTC()
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	return items, more, nil
}

// Reconcile verifies that the stored account projection equals immutable ledger entries.
func (s *Service) Reconcile(ctx context.Context) error {
	var accountID int64
	err := s.pool.QueryRow(ctx, `
		SELECT a.id FROM wallet_accounts a
		LEFT JOIN ledger_entries e ON e.account_id = a.id
		GROUP BY a.id, a.balance_irr, a.normal_side
		HAVING a.balance_irr <> COALESCE(SUM(CASE WHEN e.side = a.normal_side THEN e.amount_irr ELSE -e.amount_irr END), 0)
		LIMIT 1`).Scan(&accountID)
	if err == nil {
		return fmt.Errorf("wallet account %d projection differs from ledger", accountID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("reconcile wallet account projections: %w", err)
	}

	var transactionID int64
	err = s.pool.QueryRow(ctx, `
		SELECT t.id FROM ledger_transactions t
		LEFT JOIN ledger_entries e ON e.transaction_id = t.id
		GROUP BY t.id
		HAVING COUNT(e.id) < 2
			OR COALESCE(SUM(e.amount_irr) FILTER (WHERE e.side = 'debit'), 0)
			<> COALESCE(SUM(e.amount_irr) FILTER (WHERE e.side = 'credit'), 0)
		LIMIT 1`).Scan(&transactionID)
	if err == nil {
		return fmt.Errorf("ledger transaction %d is not balanced", transactionID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("reconcile ledger transactions: %w", err)
	}

	var holdID int64
	err = s.pool.QueryRow(ctx, `
		SELECT h.id FROM wallet_holds h
		JOIN ledger_transactions placed ON placed.id = h.place_transaction_id
		LEFT JOIN ledger_transactions closing ON closing.id = h.closing_transaction_id
		LEFT JOIN ledger_transactions refund ON refund.id = h.refund_transaction_id
		WHERE placed.kind <> 'hold_place'
			OR (h.status = 'active' AND h.closing_transaction_id IS NOT NULL)
			OR (h.status = 'released' AND (closing.kind <> 'hold_release' OR h.refund_transaction_id IS NOT NULL))
			OR (h.status = 'settled' AND (closing.kind <> 'hold_settle' OR h.refund_transaction_id IS NOT NULL))
			OR (h.status = 'refunded' AND (closing.kind <> 'refund_release' OR refund.kind <> 'reversal' OR refund.reverses_transaction_id IS DISTINCT FROM (
				SELECT settled.id FROM ledger_transactions settled WHERE settled.kind = 'hold_settle' AND settled.reference = 'booking:' || h.booking_id || ':settle'
			)))
		LIMIT 1`).Scan(&holdID)
	if err == nil {
		return fmt.Errorf("wallet hold %d does not match its ledger transactions", holdID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("reconcile wallet holds: %w", err)
	}
	return nil
}

func (s *Service) PlaceBookingHoldTx(ctx context.Context, tx pgx.Tx, input PlaceBookingHoldInput) (*BookingHold, error) {
	return PlaceBookingHoldTx(ctx, tx, input)
}
func (s *Service) CaptureBookingPaymentTx(ctx context.Context, tx pgx.Tx, input CaptureBookingPaymentInput) (*BookingHold, error) {
	return CaptureBookingPaymentTx(ctx, tx, input)
}
func (s *Service) ReleaseBookingHoldTx(ctx context.Context, tx pgx.Tx, bookingID, actorID int64, requestID string) error {
	return ReleaseBookingHoldTx(ctx, tx, bookingID, actorID, requestID)
}
func (s *Service) SettleBookingHoldTx(ctx context.Context, tx pgx.Tx, bookingID, actorID int64, requestID string) error {
	return SettleBookingHoldTx(ctx, tx, bookingID, actorID, requestID)
}

func (s *Service) CreditPaymentTx(ctx context.Context, tx pgx.Tx, userID, paymentID, amountIRR int64, requestID string) error {
	if userID < 1 || paymentID < 1 || requestID == "" {
		return ErrInvalidInput
	}
	if err := validateAmount(amountIRR); err != nil {
		return err
	}
	transactionID, err := postTx(ctx, tx, posting{
		reference: fmt.Sprintf("payment:%d:funding", paymentID),
		kind:      txFunding,
		actorID:   userID,
		requestID: requestID,
		entries: []entry{
			{account: accountRef{kind: kindFundingClearing}, side: sideDebit, amount: amountIRR},
			{account: accountRef{ownerID: userID, kind: kindCustomerAvailable}, side: sideCredit, amount: amountIRR},
		},
	})
	if err != nil {
		return err
	}
	return auditTx(ctx, tx, userID, "wallet.funding.payment", "payment_attempt", paymentID, requestID, map[string]any{
		"amount_irr": amountIRR, "ledger_transaction_id": transactionID,
	})
}

// FundForDevelopment credits a customer only for local development and tests. It has no HTTP endpoint.
func (s *Service) FundForDevelopment(ctx context.Context, userID, amountIRR int64, requestID string) error {
	if userID < 1 || requestID == "" || len(requestID) > 128 {
		return ErrInvalidInput
	}
	if err := validateAmount(amountIRR); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin development funding: %w", err)
	}
	defer tx.Rollback(ctx)
	fingerprintInput := fmt.Sprintf("development-funding|%d|%d", userID, amountIRR)
	fingerprintSum := sha256.Sum256([]byte(fingerprintInput))
	fingerprint := hex.EncodeToString(fingerprintSum[:])
	var recordID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO idempotency_records (principal_id, operation, idempotency_key, request_fingerprint)
		VALUES ($1, 'wallet.development.funding', $2, $3)
		ON CONFLICT (principal_id, operation, idempotency_key) DO NOTHING
		RETURNING id
	`, userID, requestID, fingerprint).Scan(&recordID)
	if errors.Is(err, pgx.ErrNoRows) {
		var storedFingerprint string
		if err := tx.QueryRow(ctx, `
			SELECT request_fingerprint FROM idempotency_records
			WHERE principal_id = $1 AND operation = 'wallet.development.funding' AND idempotency_key = $2
			FOR UPDATE
		`, userID, requestID).Scan(&storedFingerprint); err != nil {
			return fmt.Errorf("read development funding idempotency record: %w", err)
		}
		if storedFingerprint != fingerprint {
			return ErrIdempotencyConflict
		}
		return tx.Commit(ctx)
	}
	if err != nil {
		return fmt.Errorf("reserve development funding idempotency key: %w", err)
	}
	referenceKey := sha256.Sum256([]byte(requestID))
	transactionID, err := postTx(ctx, tx, posting{reference: fmt.Sprintf("development:fund:%d:%s", userID, hex.EncodeToString(referenceKey[:])), kind: txFunding, actorID: userID, requestID: requestID, entries: []entry{{account: accountRef{kind: kindFundingClearing}, side: sideDebit, amount: amountIRR}, {account: accountRef{ownerID: userID, kind: kindCustomerAvailable}, side: sideCredit, amount: amountIRR}}})
	if err != nil {
		return err
	}
	if err := auditTx(ctx, tx, userID, "wallet.funding.development", "user", userID, requestID, map[string]any{"amount_irr": amountIRR, "ledger_transaction_id": transactionID}); err != nil {
		return err
	}
	result, err := json.Marshal(map[string]int64{"ledger_transaction_id": transactionID})
	if err != nil {
		return fmt.Errorf("encode development funding result: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE idempotency_records SET result = $1::jsonb WHERE id = $2`, string(result), recordID); err != nil {
		return fmt.Errorf("store development funding result: %w", err)
	}
	return tx.Commit(ctx)
}
