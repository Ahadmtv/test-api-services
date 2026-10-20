package wallet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const (
	HoldActive   = "active"
	HoldReleased = "released"
	HoldSettled  = "settled"
	HoldRefunded = "refunded"
)

type BookingHold struct {
	ID                      int64
	BookingID               int64
	CustomerID              int64
	ProviderID              int64
	AmountIRR               int64
	CommissionPolicyVersion int
	CommissionRateBPS       int
	Status                  string
}

type PlaceBookingHoldInput struct {
	BookingID  int64
	CustomerID int64
	ProviderID int64
	AmountIRR  int64
	RequestID  string
}

type CaptureBookingPaymentInput struct {
	BookingID  int64
	CustomerID int64
	ProviderID int64
	PaymentID  int64
	AmountIRR  int64
	RequestID  string
}

type commissionPolicy struct {
	version int
	rateBPS int
}

// PlaceBookingHoldTx moves the booking amount from available to held funds and records the commission snapshot.
// It must run in the transaction that inserts the booking so that the two commit or roll back together.
func PlaceBookingHoldTx(ctx context.Context, tx pgx.Tx, input PlaceBookingHoldInput) (*BookingHold, error) {
	if input.BookingID < 1 || input.CustomerID < 1 || input.ProviderID < 1 {
		return nil, ErrInvalidInput
	}
	if err := validateAmount(input.AmountIRR); err != nil {
		return nil, err
	}
	policy, err := activeCommissionPolicyTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	transactionID, err := postTx(ctx, tx, posting{
		reference: bookingReference(input.BookingID, "hold"),
		kind:      txHoldPlace,
		actorID:   input.CustomerID,
		requestID: input.RequestID,
		entries: []entry{
			{account: accountRef{ownerID: input.CustomerID, kind: kindCustomerAvailable}, side: sideDebit, amount: input.AmountIRR},
			{account: accountRef{ownerID: input.CustomerID, kind: kindCustomerHeld}, side: sideCredit, amount: input.AmountIRR},
		},
	})
	if err != nil {
		return nil, err
	}

	var hold BookingHold
	if err := tx.QueryRow(ctx, `
		INSERT INTO wallet_holds (
			booking_id, customer_id, provider_id, amount_irr,
			commission_policy_version, commission_rate_bps, status, place_transaction_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, 'active', $7)
		RETURNING id, booking_id, customer_id, provider_id, amount_irr,
			commission_policy_version, commission_rate_bps, status
	`, input.BookingID, input.CustomerID, input.ProviderID, input.AmountIRR,
		policy.version, policy.rateBPS, transactionID).Scan(
		&hold.ID, &hold.BookingID, &hold.CustomerID, &hold.ProviderID, &hold.AmountIRR,
		&hold.CommissionPolicyVersion, &hold.CommissionRateBPS, &hold.Status,
	); err != nil {
		return nil, fmt.Errorf("insert wallet hold: %w", err)
	}

	if err := auditTx(ctx, tx, input.CustomerID, "wallet.hold.placed", "wallet_hold", hold.ID, input.RequestID, map[string]any{
		"booking_id":            input.BookingID,
		"amount_irr":            input.AmountIRR,
		"ledger_transaction_id": transactionID,
	}); err != nil {
		return nil, err
	}
	return &hold, nil
}

// CaptureBookingPaymentTx places a booking hold directly from verified gateway
// funds. The funding clearing account is debited and the customer's held
// balance is credited, so the payment never becomes spendable wallet money.
func CaptureBookingPaymentTx(ctx context.Context, tx pgx.Tx, input CaptureBookingPaymentInput) (*BookingHold, error) {
	if input.BookingID < 1 || input.CustomerID < 1 || input.ProviderID < 1 || input.PaymentID < 1 {
		return nil, ErrInvalidInput
	}
	if err := validateAmount(input.AmountIRR); err != nil {
		return nil, err
	}
	policy, err := activeCommissionPolicyTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	transactionID, err := postTx(ctx, tx, posting{
		reference: fmt.Sprintf("payment:%d:capture", input.PaymentID),
		kind:      txHoldPlace,
		actorID:   input.CustomerID,
		requestID: input.RequestID,
		entries: []entry{
			{account: accountRef{kind: kindFundingClearing}, side: sideDebit, amount: input.AmountIRR},
			{account: accountRef{ownerID: input.CustomerID, kind: kindCustomerHeld}, side: sideCredit, amount: input.AmountIRR},
		},
	})
	if err != nil {
		return nil, err
	}

	var hold BookingHold
	if err := tx.QueryRow(ctx, `
		INSERT INTO wallet_holds (
			booking_id, customer_id, provider_id, amount_irr,
			commission_policy_version, commission_rate_bps, status, place_transaction_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, 'active', $7)
		RETURNING id, booking_id, customer_id, provider_id, amount_irr,
			commission_policy_version, commission_rate_bps, status
	`, input.BookingID, input.CustomerID, input.ProviderID, input.AmountIRR,
		policy.version, policy.rateBPS, transactionID).Scan(
		&hold.ID, &hold.BookingID, &hold.CustomerID, &hold.ProviderID, &hold.AmountIRR,
		&hold.CommissionPolicyVersion, &hold.CommissionRateBPS, &hold.Status,
	); err != nil {
		return nil, fmt.Errorf("insert gateway payment hold: %w", err)
	}

	if err := auditTx(ctx, tx, input.CustomerID, "wallet.hold.captured", "wallet_hold", hold.ID, input.RequestID, map[string]any{
		"booking_id":            input.BookingID,
		"payment_id":            input.PaymentID,
		"amount_irr":            input.AmountIRR,
		"ledger_transaction_id": transactionID,
	}); err != nil {
		return nil, err
	}
	return &hold, nil
}

// ReleaseBookingHoldTx returns held funds to the customer. It is a no-op when the booking has no hold or the hold is already released.
func ReleaseBookingHoldTx(ctx context.Context, tx pgx.Tx, bookingID, actorID int64, requestID string) error {
	hold, err := lockBookingHoldTx(ctx, tx, bookingID)
	if err != nil {
		return err
	}
	if hold == nil || hold.Status == HoldReleased {
		return nil
	}
	if hold.Status != HoldActive {
		return ErrInvalidHoldTransition
	}

	transactionID, err := postTx(ctx, tx, posting{
		reference: bookingReference(bookingID, "release"),
		kind:      txHoldRelease,
		actorID:   actorID,
		requestID: requestID,
		entries: []entry{
			{account: accountRef{ownerID: hold.CustomerID, kind: kindCustomerHeld}, side: sideDebit, amount: hold.AmountIRR},
			{account: accountRef{ownerID: hold.CustomerID, kind: kindCustomerAvailable}, side: sideCredit, amount: hold.AmountIRR},
		},
	})
	if err != nil {
		return err
	}
	if err := closeHoldTx(ctx, tx, hold.ID, HoldReleased, transactionID); err != nil {
		return err
	}
	return auditTx(ctx, tx, actorID, "wallet.hold.released", "wallet_hold", hold.ID, requestID, map[string]any{
		"booking_id":            bookingID,
		"amount_irr":            hold.AmountIRR,
		"ledger_transaction_id": transactionID,
	})
}

// SettleBookingHoldTx moves held funds into provider earnings and platform commission.
// It is a no-op when the booking has no hold or the hold is already settled, so a settlement is never posted twice.
func SettleBookingHoldTx(ctx context.Context, tx pgx.Tx, bookingID, actorID int64, requestID string) error {
	hold, err := lockBookingHoldTx(ctx, tx, bookingID)
	if err != nil {
		return err
	}
	if hold == nil {
		return nil
	}
	switch hold.Status {
	case HoldSettled, HoldRefunded:
		return nil
	case HoldActive:
	default:
		return ErrInvalidHoldTransition
	}

	commission, providerShare, err := CalculateCommission(hold.AmountIRR, hold.CommissionRateBPS)
	if err != nil {
		return err
	}
	entries := []entry{
		{account: accountRef{ownerID: hold.CustomerID, kind: kindCustomerHeld}, side: sideDebit, amount: hold.AmountIRR},
	}
	if providerShare > 0 {
		entries = append(entries, entry{account: accountRef{ownerID: hold.ProviderID, kind: kindProviderEarnings}, side: sideCredit, amount: providerShare})
	}
	if commission > 0 {
		entries = append(entries, entry{account: accountRef{kind: kindPlatformCommission}, side: sideCredit, amount: commission})
	}

	transactionID, err := postTx(ctx, tx, posting{
		reference: bookingReference(bookingID, "settle"),
		kind:      txHoldSettle,
		actorID:   actorID,
		requestID: requestID,
		entries:   entries,
	})
	if err != nil {
		return err
	}
	if err := closeHoldTx(ctx, tx, hold.ID, HoldSettled, transactionID); err != nil {
		return err
	}
	return auditTx(ctx, tx, actorID, "wallet.hold.settled", "wallet_hold", hold.ID, requestID, map[string]any{
		"booking_id":                bookingID,
		"amount_irr":                hold.AmountIRR,
		"provider_share_irr":        providerShare,
		"commission_irr":            commission,
		"commission_rate_bps":       hold.CommissionRateBPS,
		"commission_policy_version": hold.CommissionPolicyVersion,
		"ledger_transaction_id":     transactionID,
	})
}

func RefundBookingHoldTx(ctx context.Context, tx pgx.Tx, bookingID, actorID int64, idempotencyKey, requestID string) error {
	if bookingID < 1 || actorID < 1 || idempotencyKey == "" || len(idempotencyKey) > 128 {
		return ErrInvalidInput
	}
	fingerprint := refundFingerprint(bookingID)
	var inserted int64
	err := tx.QueryRow(ctx, `
		INSERT INTO idempotency_records (principal_id, operation, idempotency_key, request_fingerprint)
		VALUES ($1, 'wallet.booking.refund', $2, $3)
		ON CONFLICT (principal_id, operation, idempotency_key) DO NOTHING
		RETURNING id
	`, actorID, idempotencyKey, fingerprint).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		var storedFingerprint string
		var result []byte
		if err := tx.QueryRow(ctx, `
			SELECT request_fingerprint, result FROM idempotency_records
			WHERE principal_id = $1 AND operation = 'wallet.booking.refund' AND idempotency_key = $2
			FOR UPDATE
		`, actorID, idempotencyKey).Scan(&storedFingerprint, &result); err != nil {
			return fmt.Errorf("read refund idempotency record: %w", err)
		}
		if storedFingerprint != fingerprint {
			return ErrIdempotencyConflict
		}
		var response map[string]json.RawMessage
		if err := json.Unmarshal(result, &response); err != nil {
			return fmt.Errorf("decode refund idempotency result: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("reserve refund idempotency key: %w", err)
	}

	hold, err := lockBookingHoldTx(ctx, tx, bookingID)
	if err != nil {
		return err
	}
	if hold == nil {
		return ErrTransactionNotFound
	}
	if hold.Status != HoldSettled {
		return ErrNotReversible
	}
	var settlementID int64
	if err := tx.QueryRow(ctx, `
		SELECT closing_transaction_id FROM wallet_holds WHERE id = $1
	`, hold.ID).Scan(&settlementID); err != nil {
		return fmt.Errorf("read settled wallet hold transaction: %w", err)
	}
	var settlementKind string
	if err := tx.QueryRow(ctx, `SELECT kind FROM ledger_transactions WHERE id = $1`, settlementID).Scan(&settlementKind); err != nil {
		return fmt.Errorf("read settlement: %w", err)
	}
	if settlementKind != txHoldSettle {
		return ErrNotReversible
	}

	rows, err := tx.Query(ctx, `
		SELECT a.owner_key, a.kind, e.side, e.amount_irr
		FROM ledger_entries e JOIN wallet_accounts a ON a.id = e.account_id
		WHERE e.transaction_id = $1 ORDER BY a.owner_key, a.kind
	`, settlementID)
	if err != nil {
		return fmt.Errorf("read settlement entries: %w", err)
	}
	var reversalEntries []entry
	for rows.Next() {
		var ownerID, amount int64
		var kind, side string
		if err := rows.Scan(&ownerID, &kind, &side, &amount); err != nil {
			rows.Close()
			return fmt.Errorf("scan settlement entry: %w", err)
		}
		reverseSide := sideDebit
		if side == sideDebit {
			reverseSide = sideCredit
		}
		reversalEntries = append(reversalEntries, entry{account: accountRef{ownerID: ownerID, kind: kind}, side: reverseSide, amount: amount})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read settlement entries: %w", err)
	}
	rows.Close()
	reversalID, err := postTx(ctx, tx, posting{
		reference: bookingReference(bookingID, "refund-reversal"),
		kind:      txReversal,
		reverses:  settlementID,
		actorID:   actorID,
		requestID: requestID,
		entries:   reversalEntries,
	})
	if err != nil {
		return err
	}

	releaseID, err := postTx(ctx, tx, posting{
		reference: bookingReference(bookingID, "refund-release"),
		kind:      txRefundRelease,
		actorID:   actorID,
		requestID: requestID,
		entries: []entry{
			{account: accountRef{ownerID: hold.CustomerID, kind: kindCustomerHeld}, side: sideDebit, amount: hold.AmountIRR},
			{account: accountRef{ownerID: hold.CustomerID, kind: kindCustomerAvailable}, side: sideCredit, amount: hold.AmountIRR},
		},
	})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE wallet_holds SET status = 'refunded', closing_transaction_id = $1,
			refund_transaction_id = $2, updated_at = now()
		WHERE id = $3 AND status = 'settled'
	`, releaseID, reversalID, hold.ID); err != nil {
		return fmt.Errorf("mark wallet hold refunded: %w", err)
	}
	if err := auditTx(ctx, tx, actorID, "wallet.hold.refunded", "wallet_hold", hold.ID, requestID, map[string]any{
		"booking_id": bookingID, "amount_irr": hold.AmountIRR,
		"reversal_transaction_id": reversalID, "release_transaction_id": releaseID,
	}); err != nil {
		return err
	}
	result, err := json.Marshal(map[string]int64{"booking_id": bookingID, "reversal_transaction_id": reversalID, "release_transaction_id": releaseID})
	if err != nil {
		return fmt.Errorf("encode refund idempotency result: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE idempotency_records SET result = $1::jsonb
		WHERE id = $2
	`, string(result), inserted); err != nil {
		return fmt.Errorf("store refund idempotency result: %w", err)
	}
	return nil
}

func refundFingerprint(bookingID int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("refund|%d", bookingID)))
	return hex.EncodeToString(sum[:])
}

func activeCommissionPolicyTx(ctx context.Context, tx pgx.Tx) (commissionPolicy, error) {
	var policy commissionPolicy
	err := tx.QueryRow(ctx, `
		SELECT version, rate_bps FROM commission_policies
		WHERE effective_from <= now()
		ORDER BY effective_from DESC, version DESC
		LIMIT 1
	`).Scan(&policy.version, &policy.rateBPS)
	if errors.Is(err, pgx.ErrNoRows) {
		return commissionPolicy{}, ErrNoCommissionPolicy
	}
	if err != nil {
		return commissionPolicy{}, fmt.Errorf("read commission policy: %w", err)
	}
	return policy, nil
}

// lockBookingHoldTx returns nil when the booking has no hold.
func lockBookingHoldTx(ctx context.Context, tx pgx.Tx, bookingID int64) (*BookingHold, error) {
	var hold BookingHold
	err := tx.QueryRow(ctx, `
		SELECT id, booking_id, customer_id, provider_id, amount_irr,
			commission_policy_version, commission_rate_bps, status
		FROM wallet_holds
		WHERE booking_id = $1
		FOR UPDATE
	`, bookingID).Scan(
		&hold.ID, &hold.BookingID, &hold.CustomerID, &hold.ProviderID, &hold.AmountIRR,
		&hold.CommissionPolicyVersion, &hold.CommissionRateBPS, &hold.Status,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock wallet hold: %w", err)
	}
	return &hold, nil
}

func closeHoldTx(ctx context.Context, tx pgx.Tx, holdID int64, status string, transactionID int64) error {
	result, err := tx.Exec(ctx, `
		UPDATE wallet_holds
		SET status = $1, closing_transaction_id = $2, updated_at = now()
		WHERE id = $3 AND status = 'active'
	`, status, transactionID, holdID)
	if err != nil {
		return fmt.Errorf("close wallet hold: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrInvalidHoldTransition
	}
	return nil
}
