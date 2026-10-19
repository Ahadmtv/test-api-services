package payouts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type insertPayout struct {
	providerID int64
	amountIRR  int64
	requestKey string
	reserveID  int64
}

func insertPayoutRequest(ctx context.Context, tx pgx.Tx, input insertPayout) (*Request, error) {
	item, err := scanRequest(tx.QueryRow(ctx, `
		INSERT INTO payout_requests (provider_id, amount_irr, status, request_key, reserve_transaction_id)
		VALUES ($1, $2, 'requested', $3, $4)
		RETURNING id, provider_id, amount_irr, status, external_transfer_reference, admin_note, created_at, updated_at
	`, input.providerID, input.amountIRR, input.requestKey, input.reserveID))
	if err != nil {
		if isDuplicateActivePayout(err) {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("insert payout request: %w", err)
	}
	return item, nil
}

func isDuplicateActivePayout(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23505" &&
		(pgErr.ConstraintName == "payout_requests_active_unique" ||
			pgErr.ConstraintName == "payout_requests_reserve_transaction_id_key" ||
			pgErr.ConstraintName == "payout_requests_release_transaction_id_key" ||
			pgErr.ConstraintName == "payout_requests_paid_transaction_id_key")
}

func lockPayoutTx(ctx context.Context, tx pgx.Tx, payoutID int64) (*Request, error) {
	item, err := scanRequest(tx.QueryRow(ctx, `
		SELECT id, provider_id, amount_irr, status, external_transfer_reference, admin_note, created_at, updated_at
		FROM payout_requests WHERE id = $1 FOR UPDATE
	`, payoutID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock payout request: %w", err)
	}
	return item, nil
}

// applyPayoutTransition performs the guarded status update; ledger documents
// must be posted by the caller before this runs so the transition triggers
// observe a consistent state. closingID links the closing ledger document
// (release or paid) to the row.
func applyPayoutTransition(ctx context.Context, tx pgx.Tx, payoutID int64, target, note string, externalReference *string, closingID int64) (*Request, error) {
	query := `
		UPDATE payout_requests
		SET status = $1, admin_note = $2,
		    external_transfer_reference = COALESCE($3, external_transfer_reference),
		    updated_at = now()
		WHERE id = $4 AND status = $5
		RETURNING id, provider_id, amount_irr, status, external_transfer_reference, admin_note, created_at, updated_at
	`
	switch target {
	case StatusRejected, StatusFailed:
		query = `
			UPDATE payout_requests
			SET status = $1, admin_note = $2, release_transaction_id = $3, updated_at = now()
			WHERE id = $4 AND status = $5
			RETURNING id, provider_id, amount_irr, status, external_transfer_reference, admin_note, created_at, updated_at
		`
	case StatusPaid:
		query = `
			UPDATE payout_requests
			SET status = $1, admin_note = $2,
			    external_transfer_reference = $3, paid_transaction_id = $4, updated_at = now()
			WHERE id = $5 AND status = $6
			RETURNING id, provider_id, amount_irr, status, external_transfer_reference, admin_note, created_at, updated_at
		`
	}
	var row pgx.Row
	switch target {
	case StatusRejected, StatusFailed:
		row = tx.QueryRow(ctx, query, target, note, closingID, payoutID, currentStatus(target))
	case StatusPaid:
		var reference string
		if externalReference != nil {
			reference = *externalReference
		}
		row = tx.QueryRow(ctx, query, target, note, reference, closingID, payoutID, currentStatus(target))
	default:
		row = tx.QueryRow(ctx, query, target, note, externalReference, payoutID, currentStatus(target))
	}
	item, err := scanRequest(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrConflict
	}
	if err != nil {
		return nil, fmt.Errorf("update payout request: %w", err)
	}
	return item, nil
}

func currentStatus(target string) string {
	switch target {
	case StatusApproved, StatusRejected:
		return StatusRequested
	case StatusProcessing:
		return StatusApproved
	case StatusPaid, StatusFailed:
		return StatusProcessing
	default:
		return ""
	}
}

func scanRequest(row pgx.Row) (*Request, error) {
	var item Request
	if err := row.Scan(&item.ID, &item.ProviderID, &item.AmountIRR, &item.Status,
		&item.ExternalTransferReference, &item.AdminNote, &item.CreatedAt, &item.UpdatedAt); err != nil {
		return nil, err
	}
	item.CreatedAt = item.CreatedAt.UTC()
	item.UpdatedAt = item.UpdatedAt.UTC()
	return &item, nil
}

type payoutResult struct {
	PayoutID int64 `json:"payout_id"`
}

func encodePayoutResult(item *Request) (string, error) {
	payload, err := json.Marshal(payoutResult{PayoutID: item.ID})
	if err != nil {
		return "", fmt.Errorf("encode payout result: %w", err)
	}
	return string(payload), nil
}

func auditPayout(ctx context.Context, tx pgx.Tx, actorID int64, action string, payoutID int64, requestID string, metadata map[string]any) error {
	payload, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode payout audit metadata: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_events (actor_user_id, action, target_type, target_id, request_id, metadata)
		VALUES ($1, $2, 'payout_request', $3, $4, $5::jsonb)
	`, actorID, action, payoutID, requestID, string(payload)); err != nil {
		return fmt.Errorf("write payout audit event: %w", err)
	}
	return nil
}
