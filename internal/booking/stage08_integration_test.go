package booking

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func newAdminFixture(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var adminID int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO users (email, password_hash) VALUES ($1, 'test') RETURNING id`, randomEmail(t)+".admin").Scan(&adminID); err != nil {
		t.Fatalf("create admin fixture: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO user_roles (user_id, role) VALUES ($1, 'admin')`, adminID); err != nil {
		t.Fatalf("grant admin fixture role: %v", err)
	}
	return adminID
}

func completeBookingSetup(t *testing.T) (*bookingFixture, context.Context, int64) {
	t.Helper()
	f := newBookingFixture(t)
	ctx := context.Background()
	// Exercise the commission snapshot path instead of the seeded zero rate.
	if _, err := f.pool.Exec(ctx, `INSERT INTO commission_policies (version, rate_bps, effective_from) VALUES (2, 1250, now())`); err != nil {
		t.Fatalf("seed commission policy: %v", err)
	}
	created, err := f.service.Create(ctx, f.clientID, CreateInput{ServiceID: f.serviceID, StartsAt: nextBookingStart()}, f.config)
	if err != nil {
		t.Fatalf("create booking: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE bookings SET starts_at = now() - interval '1 minute', ends_at = now() + interval '29 minutes' WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("move booking into service window: %v", err)
	}
	if _, err := f.service.Start(ctx, f.providerID, created.ID, "start-request"); err != nil {
		t.Fatalf("start service: %v", err)
	}
	return f, ctx, created.ID
}

func TestCompletionFlowSettlesHoldAtomically(t *testing.T) {
	f, ctx, bookingID := completeBookingSetup(t)

	held, err := f.service.wallet.Balance(ctx, f.clientID)
	if err != nil {
		t.Fatal(err)
	}
	if held.HeldIRR != 25_000 || held.AvailableIRR != 975_000 {
		t.Fatalf("balance before completion=%+v", held)
	}

	requested, err := f.service.RequestCompletion(ctx, f.providerID, bookingID, "completion-request")
	if err != nil {
		t.Fatalf("request completion: %v", err)
	}
	if requested.Status != "completion_pending" || requested.CompletionRequestedAt == nil {
		t.Fatalf("request completion booking=%+v", requested)
	}
	stillHeld, err := f.service.wallet.Balance(ctx, f.clientID)
	if err != nil {
		t.Fatal(err)
	}
	if stillHeld.HeldIRR != 25_000 {
		t.Fatalf("funds must stay on hold while completion pending: %+v", stillHeld)
	}

	if _, err := f.service.Dispute(ctx, f.otherClientID, bookingID, "not allowed", "foreign-dispute"); !errors.Is(err, ErrBookingNotFound) {
		t.Fatalf("foreign dispute error=%v; want ErrBookingNotFound", err)
	}
	completed, err := f.service.ConfirmCompletion(ctx, f.clientID, bookingID, "confirm-request")
	if err != nil {
		t.Fatalf("confirm completion: %v", err)
	}
	if completed.Status != "completed" || completed.CompletedAt == nil {
		t.Fatalf("confirmed booking=%+v", completed)
	}
	if _, err := f.service.ConfirmCompletion(ctx, f.clientID, bookingID, "confirm-retry"); err != nil {
		t.Fatalf("repeat confirmation: %v", err)
	}

	balance, err := f.service.wallet.Balance(ctx, f.clientID)
	if err != nil {
		t.Fatal(err)
	}
	if balance.AvailableIRR != 975_000 || balance.HeldIRR != 0 {
		t.Fatalf("balance after completion=%+v", balance)
	}
	var providerEarnings int64
	if err := f.pool.QueryRow(ctx, `SELECT balance_irr FROM wallet_accounts WHERE owner_user_id = $1 AND kind = 'provider_earnings'`, f.providerID).Scan(&providerEarnings); err != nil {
		t.Fatal(err)
	}
	if providerEarnings != 21_875 {
		t.Fatalf("provider earnings=%d; want 21875", providerEarnings)
	}
	assertBalancedLedger(t, f.pool)
}

func TestRequestCompletionRejectsInvalidStates(t *testing.T) {
	f, ctx, bookingID := completeBookingSetup(t)

	if _, err := f.service.RequestCompletion(ctx, f.providerID, bookingID+999, "missing-request"); !errors.Is(err, ErrBookingNotFound) {
		t.Fatalf("missing booking error=%v; want ErrBookingNotFound", err)
	}
	if _, err := f.service.RequestCompletion(ctx, f.otherProvider, bookingID, "foreign-request"); !errors.Is(err, ErrBookingNotFound) {
		t.Fatalf("foreign provider error=%v; want ErrBookingNotFound", err)
	}
	if _, err := f.service.ConfirmCompletion(ctx, f.clientID, bookingID, "early-confirm"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("confirm before request error=%v; want ErrInvalidTransition", err)
	}
	if _, err := f.service.RequestCompletion(ctx, f.providerID, bookingID, "request-request"); err != nil {
		t.Fatalf("request completion: %v", err)
	}
	if _, err := f.service.RequestCompletion(ctx, f.providerID, bookingID, "request-retry"); err != nil {
		t.Fatalf("repeat request completion: %v", err)
	}
	if _, err := f.service.Start(ctx, f.providerID, bookingID, "start-again"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("start from completion_pending error=%v; want ErrInvalidTransition", err)
	}
}

func TestDisputeRequiresOpenWindow(t *testing.T) {
	f, ctx, bookingID := completeBookingSetup(t)

	if _, err := f.service.RequestCompletion(ctx, f.providerID, bookingID, "request-completion"); err != nil {
		t.Fatalf("request completion: %v", err)
	}
	disputed, err := f.service.Dispute(ctx, f.clientID, bookingID, "service quality", "dispute-request")
	if err != nil {
		t.Fatalf("open dispute: %v", err)
	}
	if disputed.Status != "disputed" {
		t.Fatalf("disputed booking status=%s", disputed.Status)
	}
	held, err := f.service.wallet.Balance(ctx, f.clientID)
	if err != nil {
		t.Fatal(err)
	}
	if held.HeldIRR != 25_000 {
		t.Fatalf("funds must stay held during dispute: %+v", held)
	}

	var disputeReason string
	var resolution *string
	if err := f.pool.QueryRow(ctx, `SELECT reason, resolution FROM booking_disputes WHERE booking_id = $1`, bookingID).Scan(&disputeReason, &resolution); err != nil {
		t.Fatal(err)
	}
	if disputeReason != "service quality" || resolution != nil {
		t.Fatalf("dispute row reason=%q resolution=%v", disputeReason, resolution)
	}

	if _, err := f.service.ConfirmCompletion(ctx, f.clientID, bookingID, "confirm-disputed"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("confirm disputed booking error=%v; want ErrInvalidTransition", err)
	}
}

func TestDisputeWindowClosedBlocksClientActions(t *testing.T) {
	f, ctx, bookingID := completeBookingSetup(t)

	if _, err := f.service.RequestCompletion(ctx, f.providerID, bookingID, "request-completion"); err != nil {
		t.Fatalf("request completion: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE bookings SET completion_requested_at = now() - interval '25 hours' WHERE id = $1`, bookingID); err != nil {
		t.Fatalf("age completion request: %v", err)
	}
	if _, err := f.service.Dispute(ctx, f.clientID, bookingID, "late dispute", "late-dispute"); !errors.Is(err, ErrDisputeWindowClosed) {
		t.Fatalf("late dispute error=%v; want ErrDisputeWindowClosed", err)
	}

	// Client confirmation deliberately stays available beyond the window; the
	// window only gates disputes, and the expiry job settles instead only when
	// the client never confirms.
	completed, err := f.service.ConfirmCompletion(ctx, f.clientID, bookingID, "late-confirm")
	if err != nil {
		t.Fatalf("late confirmation: %v", err)
	}
	if completed.Status != "completed" {
		t.Fatalf("late completed booking status=%s", completed.Status)
	}
}

func TestResolveDisputeSettlePaysProvider(t *testing.T) {
	f, ctx, bookingID := completeBookingSetup(t)

	if _, err := f.service.RequestCompletion(ctx, f.providerID, bookingID, "request-completion"); err != nil {
		t.Fatalf("request completion: %v", err)
	}
	if _, err := f.service.Dispute(ctx, f.clientID, bookingID, "complaint", "dispute-request"); err != nil {
		t.Fatalf("open dispute: %v", err)
	}

	adminID := newAdminFixture(t, f.pool)
	if _, err := f.service.ResolveDispute(ctx, f.clientID, bookingID, DisputeResolutionSettle, "", "settle-request"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin resolution error=%v; want ErrForbidden", err)
	}
	resolved, err := f.service.ResolveDispute(ctx, adminID, bookingID, DisputeResolutionSettle, "client acknowledged", "settle-request")
	if err != nil {
		t.Fatalf("resolve dispute: %v", err)
	}
	if resolved.Status != "completed" || resolved.CompletedAt == nil {
		t.Fatalf("resolved booking=%+v", resolved)
	}
	if _, err := f.service.ResolveDispute(ctx, adminID, bookingID, DisputeResolutionSettle, "again", "settle-retry"); err != nil {
		t.Fatalf("repeat dispute resolution: %v", err)
	}
	if _, err := f.service.ResolveDispute(ctx, adminID, bookingID, DisputeResolutionCancel, "flip", "settle-flip"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("flipped resolution error=%v; want ErrInvalidTransition", err)
	}

	balance, err := f.service.wallet.Balance(ctx, f.clientID)
	if err != nil {
		t.Fatal(err)
	}
	if balance.HeldIRR != 0 || balance.AvailableIRR != 975_000 {
		t.Fatalf("balance after settle resolution=%+v", balance)
	}
	var providerEarnings int64
	if err := f.pool.QueryRow(ctx, `SELECT balance_irr FROM wallet_accounts WHERE owner_user_id = $1 AND kind = 'provider_earnings'`, f.providerID).Scan(&providerEarnings); err != nil {
		t.Fatal(err)
	}
	if providerEarnings != 21_875 {
		t.Fatalf("provider earnings=%d; want 21875", providerEarnings)
	}
	var resolution string
	var note string
	if err := f.pool.QueryRow(ctx, `SELECT resolution, resolution_note FROM booking_disputes WHERE booking_id = $1`, bookingID).Scan(&resolution, &note); err != nil {
		t.Fatal(err)
	}
	if resolution != string(DisputeResolutionSettle) || note != "client acknowledged" {
		t.Fatalf("dispute resolution=%q note=%q", resolution, note)
	}
	assertBalancedLedger(t, f.pool)
}

func TestResolveDisputeCancelRefundsClient(t *testing.T) {
	f, ctx, bookingID := completeBookingSetup(t)

	if _, err := f.service.RequestCompletion(ctx, f.providerID, bookingID, "request-completion"); err != nil {
		t.Fatalf("request completion: %v", err)
	}
	if _, err := f.service.Dispute(ctx, f.clientID, bookingID, "no service", "dispute-request"); err != nil {
		t.Fatalf("open dispute: %v", err)
	}

	adminID := newAdminFixture(t, f.pool)
	resolved, err := f.service.ResolveDispute(ctx, adminID, bookingID, DisputeResolutionCancel, "refund granted", "cancel-request")
	if err != nil {
		t.Fatalf("resolve dispute: %v", err)
	}
	if resolved.Status != "cancelled" {
		t.Fatalf("resolved booking status=%s; want cancelled", resolved.Status)
	}
	if _, err := f.service.ResolveDispute(ctx, adminID, bookingID, DisputeResolutionCancel, "retry", "cancel-retry"); err != nil {
		t.Fatalf("repeat dispute resolution: %v", err)
	}

	balance, err := f.service.wallet.Balance(ctx, f.clientID)
	if err != nil {
		t.Fatal(err)
	}
	if balance.AvailableIRR != 1_000_000 || balance.HeldIRR != 0 {
		t.Fatalf("balance after cancel resolution=%+v", balance)
	}
	var earnings int64
	var earningsCount int
	if err := f.pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(balance_irr), 0) FROM wallet_accounts WHERE owner_user_id = $1 AND kind = 'provider_earnings'`, f.providerID).Scan(&earningsCount, &earnings); err != nil {
		t.Fatal(err)
	}
	if earnings != 0 {
		t.Fatalf("provider earnings=%d; want 0", earnings)
	}
	assertBalancedLedger(t, f.pool)
}

func TestResolveDisputeWithoutOpenDisputeFails(t *testing.T) {
	f, ctx, bookingID := completeBookingSetup(t)

	if _, err := f.service.RequestCompletion(ctx, f.providerID, bookingID, "request-completion"); err != nil {
		t.Fatalf("request completion: %v", err)
	}
	adminID := newAdminFixture(t, f.pool)
	if _, err := f.service.ResolveDispute(ctx, adminID, bookingID, DisputeResolutionSettle, "", "no-dispute"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("resolution without dispute error=%v; want ErrInvalidTransition", err)
	}
}

func TestProviderCancelBeforeStartReleasesHold(t *testing.T) {
	f := newBookingFixture(t)
	ctx := context.Background()
	created, err := f.service.Create(ctx, f.clientID, CreateInput{ServiceID: f.serviceID, StartsAt: nextBookingStart()}, f.config)
	if err != nil {
		t.Fatalf("create booking: %v", err)
	}

	cancelled, err := f.service.CancelByProvider(ctx, f.providerID, created.ID, "provider-cancel")
	if err != nil {
		t.Fatalf("provider cancel: %v", err)
	}
	if cancelled.Status != "cancelled" {
		t.Fatalf("cancelled booking status=%s", cancelled.Status)
	}

	balance, err := f.service.wallet.Balance(ctx, f.clientID)
	if err != nil {
		t.Fatal(err)
	}
	if balance.AvailableIRR != 1_000_000 || balance.HeldIRR != 0 {
		t.Fatalf("balance after provider cancel=%+v", balance)
	}

	if _, err := f.service.CancelByProvider(ctx, f.providerID, created.ID, "provider-cancel-retry"); err != nil {
		t.Fatalf("repeat provider cancel: %v", err)
	}

	inProgress, err := f.service.Create(ctx, f.clientID, CreateInput{ServiceID: f.serviceID, StartsAt: nextBookingStart()}, f.config)
	if err != nil {
		t.Fatalf("create second booking: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE bookings SET starts_at = now() - interval '1 minute', ends_at = now() + interval '29 minutes' WHERE id = $1`, inProgress.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Start(ctx, f.providerID, inProgress.ID, "start-request"); err != nil {
		t.Fatalf("start service: %v", err)
	}
	if _, err := f.service.CancelByProvider(ctx, f.providerID, inProgress.ID, "late-cancel"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("cancel in_progress error=%v; want ErrInvalidTransition", err)
	}
	assertBalancedLedger(t, f.pool)
}

func TestExpireDisputeWindowsSettlesAgedRequests(t *testing.T) {
	f, ctx, bookingID := completeBookingSetup(t)

	if _, err := f.service.RequestCompletion(ctx, f.providerID, bookingID, "request-completion"); err != nil {
		t.Fatalf("request completion: %v", err)
	}

	settled, err := f.service.ExpireDisputeWindows(ctx, 24*time.Hour, 10)
	if err != nil {
		t.Fatalf("expire dispute windows: %v", err)
	}
	if settled != 0 {
		t.Fatalf("fresh completion settled=%d; want 0", settled)
	}

	if _, err := f.pool.Exec(ctx, `UPDATE bookings SET completion_requested_at = now() - interval '25 hours' WHERE id = $1`, bookingID); err != nil {
		t.Fatalf("age completion request: %v", err)
	}
	settled, err = f.service.ExpireDisputeWindows(ctx, 24*time.Hour, 10)
	if err != nil {
		t.Fatalf("expire aged dispute windows: %v", err)
	}
	if settled != 1 {
		t.Fatalf("aged completion settled=%d; want 1", settled)
	}
	settled, err = f.service.ExpireDisputeWindows(ctx, 24*time.Hour, 10)
	if err != nil {
		t.Fatalf("repeat expiry run: %v", err)
	}
	if settled != 0 {
		t.Fatalf("second expiry run settled=%d; want exactly-once", settled)
	}

	balance, err := f.service.wallet.Balance(ctx, f.clientID)
	if err != nil {
		t.Fatal(err)
	}
	if balance.HeldIRR != 0 || balance.AvailableIRR != 975_000 {
		t.Fatalf("balance after expiry settlement=%+v", balance)
	}
	var providerEarnings int64
	if err := f.pool.QueryRow(ctx, `SELECT balance_irr FROM wallet_accounts WHERE owner_user_id = $1 AND kind = 'provider_earnings'`, f.providerID).Scan(&providerEarnings); err != nil {
		t.Fatal(err)
	}
	if providerEarnings != 21_875 {
		t.Fatalf("provider earnings=%d; want 21875", providerEarnings)
	}
	var actorType string
	if err := f.pool.QueryRow(ctx, `
		SELECT from_transition.actor_type FROM booking_state_transitions from_transition
		JOIN bookings b ON b.id = from_transition.booking_id
		WHERE b.id = $1 AND from_transition.to_status = 'completed'
	`, bookingID).Scan(&actorType); err != nil {
		t.Fatalf("read expiry transition actor: %v", err)
	}
	if actorType != "system" {
		t.Fatalf("expiry transition actor_type=%q; want system", actorType)
	}
	assertBalancedLedger(t, f.pool)
}
