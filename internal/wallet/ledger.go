package wallet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	kindCustomerAvailable  = "customer_available"
	kindCustomerHeld       = "customer_held"
	kindProviderEarnings   = "provider_earnings"
	kindPlatformCommission = "platform_commission"
	kindFundingClearing    = "funding_clearing"
	kindRefundClearing     = "refund_clearing"
)

const (
	sideDebit  = "debit"
	sideCredit = "credit"
)

const (
	txFunding       = "funding"
	txHoldPlace     = "hold_place"
	txHoldRelease   = "hold_release"
	txRefundRelease = "refund_release"
	txHoldSettle    = "hold_settle"
	txReversal      = "reversal"
)

// accountRef identifies a ledger account; ownerID is zero for platform-owned accounts.
type accountRef struct {
	ownerID int64
	kind    string
}

func (r accountRef) normalSide() string {
	if r.kind == kindFundingClearing {
		return sideDebit
	}
	return sideCredit
}

func (r accountRef) ownerValue() any {
	if r.ownerID == 0 {
		return nil
	}
	return r.ownerID
}

type entry struct {
	account accountRef
	side    string
	amount  int64
}

type posting struct {
	reference string
	kind      string
	reverses  int64
	actorID   int64
	requestID string
	entries   []entry
}

type lockedAccount struct {
	id      int64
	balance int64
}

// postTx writes one balanced ledger transaction inside the caller's database transaction.
// Account rows are locked in a canonical order so concurrent postings cannot deadlock.
func postTx(ctx context.Context, tx pgx.Tx, p posting) (int64, error) {
	if err := validatePosting(p.entries); err != nil {
		return 0, err
	}
	accounts, err := lockAccountsTx(ctx, tx, p.entries)
	if err != nil {
		return 0, err
	}
	if err := checkBalances(p.entries, accounts); err != nil {
		return 0, err
	}

	var transactionID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO ledger_transactions (reference, kind, reverses_transaction_id, actor_user_id, request_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`, p.reference, p.kind, nullableID(p.reverses), nullableID(p.actorID), p.requestID).Scan(&transactionID); err != nil {
		return 0, mapTransactionInsertError(err)
	}
	for _, e := range applicationOrder(p.entries) {
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger_entries (transaction_id, account_id, side, amount_irr)
			VALUES ($1, $2, $3, $4)
		`, transactionID, accounts[e.account].id, e.side, e.amount); err != nil {
			return 0, fmt.Errorf("insert ledger entry: %w", err)
		}
	}
	return transactionID, nil
}

func validatePosting(entries []entry) error {
	if len(entries) < 2 {
		return ErrUnbalancedPosting
	}
	var debits, credits int64
	var err error
	for _, e := range entries {
		if err := validateAmount(e.amount); err != nil {
			return err
		}
		switch e.side {
		case sideDebit:
			debits, err = addAmounts(debits, e.amount)
		case sideCredit:
			credits, err = addAmounts(credits, e.amount)
		default:
			return ErrInvalidInput
		}
		if err != nil {
			return err
		}
	}
	if debits != credits {
		return ErrUnbalancedPosting
	}
	return nil
}

// lockAccountsTx creates missing accounts and locks every referenced account row in key order.
func lockAccountsTx(ctx context.Context, tx pgx.Tx, entries []entry) (map[accountRef]lockedAccount, error) {
	seen := make(map[accountRef]struct{}, len(entries))
	refs := make([]accountRef, 0, len(entries))
	for _, e := range entries {
		if _, ok := seen[e.account]; ok {
			continue
		}
		seen[e.account] = struct{}{}
		refs = append(refs, e.account)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].ownerID != refs[j].ownerID {
			return refs[i].ownerID < refs[j].ownerID
		}
		return refs[i].kind < refs[j].kind
	})

	locked := make(map[accountRef]lockedAccount, len(refs))
	for _, ref := range refs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO wallet_accounts (owner_user_id, kind, normal_side)
			VALUES ($1, $2, $3)
			ON CONFLICT (owner_key, kind) DO NOTHING
		`, ref.ownerValue(), ref.kind, ref.normalSide()); err != nil {
			return nil, fmt.Errorf("ensure wallet account: %w", err)
		}
		var account lockedAccount
		if err := tx.QueryRow(ctx, `
			SELECT id, balance_irr FROM wallet_accounts
			WHERE owner_key = $1 AND kind = $2
			FOR UPDATE
		`, ref.ownerID, ref.kind).Scan(&account.id, &account.balance); err != nil {
			return nil, fmt.Errorf("lock wallet account: %w", err)
		}
		locked[ref] = account
	}
	return locked, nil
}

// checkBalances verifies that no account would become negative once the posting is applied.
func checkBalances(entries []entry, accounts map[accountRef]lockedAccount) error {
	increases := make(map[accountRef]int64, len(accounts))
	decreases := make(map[accountRef]int64, len(accounts))
	for _, e := range entries {
		var err error
		if e.side == e.account.normalSide() {
			increases[e.account], err = addAmounts(increases[e.account], e.amount)
		} else {
			decreases[e.account], err = addAmounts(decreases[e.account], e.amount)
		}
		if err != nil {
			return err
		}
	}
	for ref, account := range accounts {
		balance, err := addAmounts(account.balance, increases[ref])
		if err != nil {
			return err
		}
		if _, err := subtractAmounts(balance, decreases[ref]); err != nil {
			return err
		}
	}
	return nil
}

// applicationOrder applies balance-increasing entries first so intermediate balances never go negative.
func applicationOrder(entries []entry) []entry {
	ordered := make([]entry, 0, len(entries))
	for _, e := range entries {
		if e.side == e.account.normalSide() {
			ordered = append(ordered, e)
		}
	}
	for _, e := range entries {
		if e.side != e.account.normalSide() {
			ordered = append(ordered, e)
		}
	}
	return ordered
}

func mapTransactionInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if pgErr.ConstraintName == "ledger_transactions_reverses_transaction_id_key" {
			return ErrAlreadyReversed
		}
		return ErrDuplicatePosting
	}
	return fmt.Errorf("insert ledger transaction: %w", err)
}

func auditTx(ctx context.Context, tx pgx.Tx, actorID int64, action, targetType string, targetID int64, requestID string, metadata map[string]any) error {
	payload, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode audit metadata: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_events (actor_user_id, action, target_type, target_id, request_id, metadata)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb)
	`, actorID, action, targetType, targetID, requestID, string(payload)); err != nil {
		return fmt.Errorf("write audit event: %w", err)
	}
	return nil
}

func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func bookingReference(bookingID int64, operation string) string {
	return fmt.Sprintf("booking:%d:%s", bookingID, operation)
}
