package wallet

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const (
	kindProviderPayoutReserved = "provider_payout_reserved"
	kindPayoutClearing         = "payout_clearing"
)

const (
	txPayoutReserve = "payout_reserve"
	txPayoutRelease = "payout_release"
	txPayoutPaid    = "payout_paid"
)

// ReserveProviderPayoutTx moves the payout amount from provider earnings into
// reserved payout funds so a single reserve cannot fund two withdrawal
// requests. The posting happens inside the caller's transaction; the caller
// is responsible for locking the provider profile first and for inserting the
// payout request row that references the returned transaction.
func ReserveProviderPayoutTx(ctx context.Context, tx pgx.Tx, providerID, amountIRR int64, requestKey, requestID string) (int64, error) {
	if providerID < 1 || requestKey == "" {
		return 0, ErrInvalidInput
	}
	if err := validateAmount(amountIRR); err != nil {
		return 0, err
	}
	transactionID, err := postTx(ctx, tx, posting{
		reference: "payout:reserve:" + requestKey,
		kind:      txPayoutReserve,
		actorID:   providerID,
		requestID: requestID,
		entries: []entry{
			{account: accountRef{ownerID: providerID, kind: kindProviderEarnings}, side: sideDebit, amount: amountIRR},
			{account: accountRef{ownerID: providerID, kind: kindProviderPayoutReserved}, side: sideCredit, amount: amountIRR},
		},
	})
	if err != nil {
		return 0, err
	}
	return transactionID, nil
}

// ReleaseProviderPayoutTx returns reserved funds to provider earnings when a
// payout request is rejected or fails. Its reference is unique per payout, so
// one request can never release its reserve twice.
func ReleaseProviderPayoutTx(ctx context.Context, tx pgx.Tx, providerID, payoutID, amountIRR, actorID int64, requestID string) (int64, error) {
	return payoutInternalTransfer(ctx, tx, payoutTransfer{
		providerID: providerID,
		payoutID:   payoutID,
		amountIRR:  amountIRR,
		actorID:    actorID,
		requestID:  requestID,
		reference:  fmt.Sprintf("payout:release:%d", payoutID),
		kind:       txPayoutRelease,
		audit:      "wallet.payout.released",
	})
}

// CompleteProviderPayoutTx records the manual settlement of a payout: reserved
// funds are credited to the platform payout clearing account. The caller must
// have stored the external transfer reference that documents the administrative
// transfer before this posting is committed.
func CompleteProviderPayoutTx(ctx context.Context, tx pgx.Tx, providerID, payoutID, amountIRR, actorID int64, requestID string) (int64, error) {
	if providerID < 1 || payoutID < 1 {
		return 0, ErrInvalidInput
	}
	if err := validateAmount(amountIRR); err != nil {
		return 0, err
	}
	transactionID, err := postTx(ctx, tx, posting{
		reference: fmt.Sprintf("payout:paid:%d", payoutID),
		kind:      txPayoutPaid,
		actorID:   actorID,
		requestID: requestID,
		entries: []entry{
			{account: accountRef{ownerID: providerID, kind: kindProviderPayoutReserved}, side: sideDebit, amount: amountIRR},
			{account: accountRef{kind: kindPayoutClearing}, side: sideCredit, amount: amountIRR},
		},
	})
	if err != nil {
		return 0, err
	}
	if err := auditTx(ctx, tx, actorID, "wallet.payout.paid", "payout_request", payoutID, requestID, map[string]any{
		"provider_id":           providerID,
		"amount_irr":            amountIRR,
		"ledger_transaction_id": transactionID,
	}); err != nil {
		return 0, err
	}
	return transactionID, nil
}

type payoutTransfer struct {
	providerID int64
	payoutID   int64
	amountIRR  int64
	actorID    int64
	requestID  string
	reference  string
	kind       string
	audit      string
}

func payoutInternalTransfer(ctx context.Context, tx pgx.Tx, t payoutTransfer) (int64, error) {
	if t.providerID < 1 || t.payoutID < 1 {
		return 0, ErrInvalidInput
	}
	if err := validateAmount(t.amountIRR); err != nil {
		return 0, err
	}
	transactionID, err := postTx(ctx, tx, posting{
		reference: t.reference,
		kind:      t.kind,
		actorID:   t.actorID,
		requestID: t.requestID,
		entries: []entry{
			{account: accountRef{ownerID: t.providerID, kind: kindProviderPayoutReserved}, side: sideDebit, amount: t.amountIRR},
			{account: accountRef{ownerID: t.providerID, kind: kindProviderEarnings}, side: sideCredit, amount: t.amountIRR},
		},
	})
	if err != nil {
		return 0, err
	}
	if err := auditTx(ctx, tx, t.actorID, t.audit, "payout_request", t.payoutID, t.requestID, map[string]any{
		"provider_id":           t.providerID,
		"amount_irr":            t.amountIRR,
		"ledger_transaction_id": transactionID,
	}); err != nil {
		return 0, err
	}
	return transactionID, nil
}

// PayoutRequestKey derives the deterministic payout reservation key from the
// provider, amount, and idempotency context. The payouts service shares this
// helper so stored request keys always match ledger references.
func PayoutRequestKey(providerID, amountIRR int64, idempotencyKey, nonce string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("payout|%d|%d|%s|%s", providerID, amountIRR, idempotencyKey, nonce)))
	return fmt.Sprintf("%x", sum)
}
