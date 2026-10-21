package payouts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"test-api/internal/providers"
	"test-api/internal/wallet"
)

type Request struct {
	ID                        int64     `json:"id"`
	ProviderID                int64     `json:"provider_id"`
	AmountIRR                 int64     `json:"amount_irr"`
	Status                    string    `json:"status"`
	ExternalTransferReference *string   `json:"external_transfer_reference"`
	AdminNote                 string    `json:"admin_note"`
	CreatedAt                 time.Time `json:"created_at"`
	UpdatedAt                 time.Time `json:"updated_at"`
}

var (
	ErrInvalidInput        = errors.New("invalid payout input")
	ErrNotFound            = errors.New("payout request not found")
	ErrForbidden           = errors.New("payout operation forbidden")
	ErrAmountTooSmall      = errors.New("payout amount below configured minimum")
	ErrConflict            = errors.New("payout state conflict")
	ErrIdempotencyConflict = errors.New("idempotency key payload conflict")
)

const (
	StatusRequested  = "requested"
	StatusApproved   = "approved"
	StatusProcessing = "processing"
	StatusPaid       = "paid"
	StatusRejected   = "rejected"
	StatusFailed     = "failed"
)

// Service manages payout requests backed by ledger reservations.
type Service struct {
	pool         *pgxpool.Pool
	providers    *providers.Service
	minAmountIRR int64
}

func NewService(pool *pgxpool.Pool, providerProfiles *providers.Service, minAmountIRR int64) *Service {
	return &Service{pool: pool, providers: providerProfiles, minAmountIRR: minAmountIRR}
}

type CreateInput struct {
	ProviderID     int64
	AmountIRR      int64
	IdempotencyKey string
	RequestID      string
	Nonce          string
}

// Create reserves provider earnings and opens a payout request atomically.
// Duplicate keys replay the frozen result; a second active request for the
// same provider is rejected by a partial unique index inside the transaction,
// so concurrent requests can never reserve the same funds twice.
func (s *Service) Create(ctx context.Context, input CreateInput) (*Request, bool, error) {
	if input.ProviderID < 1 || input.AmountIRR < 1 {
		return nil, false, ErrInvalidInput
	}
	if len(input.IdempotencyKey) > 128 {
		return nil, false, ErrInvalidInput
	}
	if input.AmountIRR < s.minAmountIRR {
		return nil, false, fmt.Errorf("%w: minimum %d IRR", ErrAmountTooSmall, s.minAmountIRR)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin payout creation: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := s.providers.LockForBookingTx(ctx, tx, input.ProviderID); err != nil {
		if errors.Is(err, providers.ErrProfileNotFound) {
			return nil, false, ErrForbidden
		}
		return nil, false, err
	}

	// Keyed requests freeze their fingerprint: the per-request nonce is
	// deliberately ignored so a replay with the same idempotency key maps to
	// the same request key and ledger reference.
	nonce := input.Nonce
	if input.IdempotencyKey != "" {
		nonce = ""
	}
	requestKey := payoutRequestKey(input.ProviderID, input.AmountIRR, input.IdempotencyKey, nonce)
	if input.IdempotencyKey != "" {
		var inserted int64
		err := tx.QueryRow(ctx, `
			INSERT INTO idempotency_records (principal_id, operation, idempotency_key, request_fingerprint)
			VALUES ($1, 'provider.payout.create', $2, $3)
			ON CONFLICT (principal_id, operation, idempotency_key) DO NOTHING
			RETURNING id
		`, input.ProviderID, input.IdempotencyKey, requestKey).Scan(&inserted)
		if errors.Is(err, pgx.ErrNoRows) {
			var storedFingerprint string
			var result []byte
			if err := tx.QueryRow(ctx, `
				SELECT request_fingerprint, result FROM idempotency_records
				WHERE principal_id = $1 AND operation = 'provider.payout.create' AND idempotency_key = $2
				FOR UPDATE
			`, input.ProviderID, input.IdempotencyKey).Scan(&storedFingerprint, &result); err != nil {
				return nil, false, fmt.Errorf("read payout idempotency result: %w", err)
			}
			if storedFingerprint != requestKey {
				return nil, false, ErrIdempotencyConflict
			}
			var stored payoutResult
			if err := json.Unmarshal(result, &stored); err != nil {
				return nil, false, fmt.Errorf("decode payout idempotency result: %w", err)
			}
			item, err := lockPayoutTx(ctx, tx, stored.PayoutID)
			if err != nil {
				return nil, false, err
			}
			if err := tx.Commit(ctx); err != nil {
				return nil, false, fmt.Errorf("commit payout replay: %w", err)
			}
			return item, true, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("reserve payout idempotency key: %w", err)
		}
	}

	reserveID, err := wallet.ReserveProviderPayoutTx(ctx, tx, input.ProviderID, input.AmountIRR, requestKey, input.RequestID)
	if err != nil {
		return nil, false, err
	}
	item, err := insertPayoutRequest(ctx, tx, insertPayout{
		providerID: input.ProviderID,
		amountIRR:  input.AmountIRR,
		requestKey: requestKey,
		reserveID:  reserveID,
	})
	if err != nil {
		return nil, false, err
	}
	if err := auditPayout(ctx, tx, input.ProviderID, "payout.requested", item.ID, input.RequestID, map[string]any{
		"amount_irr":            input.AmountIRR,
		"ledger_transaction_id": reserveID,
	}); err != nil {
		return nil, false, err
	}
	if input.IdempotencyKey != "" {
		payload, err := encodePayoutResult(item)
		if err != nil {
			return nil, false, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE idempotency_records SET result = $1::jsonb
			WHERE principal_id = $2 AND operation = 'provider.payout.create' AND idempotency_key = $3
		`, payload, input.ProviderID, input.IdempotencyKey); err != nil {
			return nil, false, fmt.Errorf("store payout idempotency result: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		if isDuplicateActivePayout(err) {
			return nil, false, ErrConflict
		}
		return nil, false, fmt.Errorf("commit payout creation: %w", err)
	}
	return item, false, nil
}

func (s *Service) List(ctx context.Context, providerID, after int64, limit int) ([]Request, bool, error) {
	if providerID < 1 || after < 0 || limit < 1 || limit > 100 {
		return nil, false, ErrInvalidInput
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, provider_id, amount_irr, status, external_transfer_reference, admin_note, created_at, updated_at
		FROM payout_requests WHERE provider_id = $1 AND id > $2
		ORDER BY id LIMIT $3
	`, providerID, after, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("list payout requests: %w", err)
	}
	defer rows.Close()
	items := make([]Request, 0, limit)
	for rows.Next() {
		item, err := scanRequest(rows)
		if err != nil {
			return nil, false, err
		}
		items = append(items, *item)
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

// AdminPayout is the administrator view of a payout request with the
// provider's display name resolved for list screens.
type AdminPayout struct {
	Request
	ProviderDisplayName string `json:"provider_display_name"`
}

type AdminPayoutPage struct {
	Items      []AdminPayout `json:"items"`
	NextCursor *int64        `json:"next_cursor"`
}

// ListAll returns every payout request for administrators with keyset
// pagination and an optional status filter.
func (s *Service) ListAll(ctx context.Context, after int64, limit int, status string) (*AdminPayoutPage, error) {
	if after < 0 || limit < 1 || limit > 100 {
		return nil, ErrInvalidInput
	}
	if status != "" && !isValidPayoutStatus(status) {
		return nil, ErrInvalidInput
	}
	rows, err := s.pool.Query(ctx, `
		SELECT p.id, p.provider_id, p.amount_irr, p.status, p.external_transfer_reference, p.admin_note, p.created_at, p.updated_at,
		       pp.display_name
		FROM payout_requests p
		JOIN provider_profiles pp ON pp.provider_id = p.provider_id
		WHERE ($1 = 0 OR p.id > $1) AND ($2 = '' OR p.status = $2)
		ORDER BY p.id LIMIT $3
	`, after, status, limit+1)
	if err != nil {
		return nil, fmt.Errorf("list payout requests: %w", err)
	}
	defer rows.Close()
	items := make([]AdminPayout, 0, limit)
	for rows.Next() {
		var item AdminPayout
		var reference *string
		if err := rows.Scan(&item.ID, &item.ProviderID, &item.AmountIRR, &item.Status, &reference, &item.AdminNote, &item.CreatedAt, &item.UpdatedAt, &item.ProviderDisplayName); err != nil {
			return nil, err
		}
		item.ExternalTransferReference = reference
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	page := AdminPayoutPage{Items: items}
	if len(items) > limit {
		items = items[:limit]
		page.Items = items
		if len(items) > 0 {
			cursor := items[len(items)-1].ID
			page.NextCursor = &cursor
		}
	}
	return &page, nil
}

func isValidPayoutStatus(status string) bool {
	switch status {
	case StatusRequested, StatusApproved, StatusProcessing, StatusPaid, StatusRejected, StatusFailed:
		return true
	default:
		return false
	}
}

// Approve moves a requested payout to approved without touching funds.
func (s *Service) Approve(ctx context.Context, adminID, payoutID int64, note, requestID string) (*Request, error) {
	return s.simpleTransition(ctx, adminID, payoutID, note, requestID, StatusApproved, "requested")
}

// StartProcessing moves an approved payout to processing before an external
// administrative transfer is attempted.
func (s *Service) StartProcessing(ctx context.Context, adminID, payoutID int64, note, requestID string) (*Request, error) {
	return s.simpleTransition(ctx, adminID, payoutID, note, requestID, StatusProcessing, "approved")
}

// Reject releases reserved funds back to the provider and closes the request.
func (s *Service) Reject(ctx context.Context, adminID, payoutID int64, note, requestID string) (*Request, error) {
	return s.releaseTransition(ctx, adminID, payoutID, note, requestID, StatusRejected)
}

// MarkPaid records the manual settlement: the administrator attests that an
// approved external transfer completed by storing its reference and moving
// reserved funds into the payout clearing account.
func (s *Service) MarkPaid(ctx context.Context, adminID, payoutID int64, externalReference, note, requestID string) (*Request, error) {
	if externalReference == "" || len(externalReference) > 128 {
		return nil, ErrInvalidInput
	}
	return s.paidTransition(ctx, adminID, payoutID, externalReference, note, requestID)
}

// Fail releases reserved funds back to the provider after a processing
// attempt failed outside the database.
func (s *Service) Fail(ctx context.Context, adminID, payoutID int64, note, requestID string) (*Request, error) {
	return s.releaseTransition(ctx, adminID, payoutID, note, requestID, StatusFailed)
}

func (s *Service) simpleTransition(ctx context.Context, adminID, payoutID int64, note, requestID, target, from string) (*Request, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin payout transition: %w", err)
	}
	defer tx.Rollback(ctx)
	item, err := lockPayoutTx(ctx, tx, payoutID)
	if err != nil {
		return nil, err
	}
	if item.Status == target {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit payout replay: %w", err)
		}
		return item, nil
	}
	if item.Status != from {
		return nil, ErrConflict
	}
	item, err = applyPayoutTransition(ctx, tx, payoutID, target, note, nil, 0)
	if err != nil {
		return nil, err
	}
	if err := auditPayout(ctx, tx, adminID, "payout."+target, payoutID, requestID, map[string]any{
		"provider_id": item.ProviderID, "amount_irr": item.AmountIRR,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit payout transition: %w", err)
	}
	return item, nil
}

func (s *Service) releaseTransition(ctx context.Context, adminID, payoutID int64, note, requestID, target string) (*Request, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin payout transition: %w", err)
	}
	defer tx.Rollback(ctx)
	item, err := lockPayoutTx(ctx, tx, payoutID)
	if err != nil {
		return nil, err
	}
	if item.Status == target {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit payout replay: %w", err)
		}
		return item, nil
	}
	if item.Status != StatusRequested && item.Status != StatusProcessing {
		return nil, ErrConflict
	}
	if item.Status == StatusRequested && target != StatusRejected {
		return nil, ErrConflict
	}
	if item.Status == StatusProcessing && target != StatusFailed {
		return nil, ErrConflict
	}
	releaseID, err := wallet.ReleaseProviderPayoutTx(ctx, tx, item.ProviderID, item.ID, item.AmountIRR, adminID, requestID)
	if err != nil {
		return nil, err
	}
	item, err = applyPayoutTransition(ctx, tx, payoutID, target, note, nil, releaseID)
	if err != nil {
		return nil, err
	}
	if err := auditPayout(ctx, tx, adminID, "payout."+target, payoutID, requestID, map[string]any{
		"provider_id": item.ProviderID, "amount_irr": item.AmountIRR,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit payout transition: %w", err)
	}
	return item, nil
}

func (s *Service) paidTransition(ctx context.Context, adminID, payoutID int64, externalReference, note, requestID string) (*Request, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin payout transition: %w", err)
	}
	defer tx.Rollback(ctx)
	item, err := lockPayoutTx(ctx, tx, payoutID)
	if err != nil {
		return nil, err
	}
	if item.Status == StatusPaid {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit payout replay: %w", err)
		}
		return item, nil
	}
	if item.Status != StatusProcessing {
		return nil, ErrConflict
	}
	paidID, err := wallet.CompleteProviderPayoutTx(ctx, tx, item.ProviderID, item.ID, item.AmountIRR, adminID, requestID)
	if err != nil {
		return nil, err
	}
	item, err = applyPayoutTransition(ctx, tx, payoutID, StatusPaid, note, &externalReference, paidID)
	if err != nil {
		return nil, err
	}
	if err := auditPayout(ctx, tx, adminID, "payout.paid", payoutID, requestID, map[string]any{
		"provider_id": item.ProviderID, "amount_irr": item.AmountIRR,
		"external_transfer_reference": externalReference,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit payout transition: %w", err)
	}
	return item, nil
}

// Verify reconciles payout request rows against their ledger documents and
// reports the first inconsistency found.
func (s *Service) Verify(ctx context.Context) error {
	var payoutID int64
	err := s.pool.QueryRow(ctx, `
		SELECT p.id FROM payout_requests p
		LEFT JOIN ledger_transactions r ON r.id = p.reserve_transaction_id
		WHERE r.id IS NULL OR r.kind <> 'payout_reserve'
			OR r.reference <> 'payout:reserve:' || p.request_key
		LIMIT 1
	`).Scan(&payoutID)
	if err == nil {
		return fmt.Errorf("payout request %d reserve transaction does not match its ledger document", payoutID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("verify payout reserve documents: %w", err)
	}

	err = s.pool.QueryRow(ctx, `
		SELECT p.id FROM payout_requests p
		JOIN ledger_transactions closing ON closing.id = COALESCE(p.release_transaction_id, p.paid_transaction_id, 0)
		WHERE NOT (
			(p.status IN ('rejected', 'failed') AND closing.kind = 'payout_release'
				AND p.paid_transaction_id IS NULL) OR
			(p.status = 'paid' AND closing.kind = 'payout_paid')
		) LIMIT 1
	`).Scan(&payoutID)
	if err == nil {
		return fmt.Errorf("payout request %d closing transaction does not match its status", payoutID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("verify payout closing documents: %w", err)
	}

	err = s.pool.QueryRow(ctx, `
		SELECT p.id FROM payout_requests p
		WHERE p.status = 'paid' AND p.external_transfer_reference IS DISTINCT FROM (
			SELECT metadata->>'external_transfer_reference' FROM audit_events
			WHERE target_type = 'payout_request' AND target_id = p.id
				AND action = 'payout.paid'
			ORDER BY id LIMIT 1
		) LIMIT 1
	`).Scan(&payoutID)
	if err == nil {
		return fmt.Errorf("payout request %d paid without an audited external transfer reference", payoutID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("verify payout external references: %w", err)
	}
	return nil
}

func payoutRequestKey(providerID, amountIRR int64, idempotencyKey, nonce string) string {
	return wallet.PayoutRequestKey(providerID, amountIRR, idempotencyKey, nonce)
}
