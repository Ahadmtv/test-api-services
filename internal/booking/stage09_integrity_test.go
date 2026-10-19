package booking

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"test-api/internal/wallet"
)

// Stage 09 integration tests target the two money-moving booking transitions
// that are most dangerous when executed concurrently: client confirmation of a
// completion request (settlement) and administrator refund (reversal). Both
// must apply their ledger effect exactly once even under a burst of parallel
// requests, and the ledger must stay balanced.

func TestConcurrentConfirmCompletionSettlesOnce(t *testing.T) {
	f, ctx, bookingID := completeBookingSetup(t)
	if _, err := f.service.RequestCompletion(ctx, f.providerID, bookingID, "race-request-completion"); err != nil {
		t.Fatalf("request completion: %v", err)
	}

	const workers = 8
	var wg sync.WaitGroup
	var successes atomic.Int64
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := f.service.ConfirmCompletion(ctx, f.clientID, bookingID, fmt.Sprintf("race-confirm-%d", i)); err != nil {
				errs[i] = err
				return
			}
			successes.Add(1)
		}(i)
	}
	wg.Wait()

	if successes.Load() == 0 {
		t.Fatalf("no confirm-completion request succeeded: %v", errs)
	}
	for _, err := range errs {
		if err != nil && !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("unexpected confirm-completion error: %v", err)
		}
	}

	var settlementCount int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE reference = $1`, fmt.Sprintf("booking:%d:settle", bookingID)).Scan(&settlementCount); err != nil {
		t.Fatalf("count settlement transactions: %v", err)
	}
	if settlementCount != 1 {
		t.Fatalf("settlement transactions=%d; want exactly 1", settlementCount)
	}

	var providerEarnings int64
	if err := f.pool.QueryRow(ctx, `SELECT balance_irr FROM wallet_accounts WHERE owner_user_id = $1 AND kind = 'provider_earnings'`, f.providerID).Scan(&providerEarnings); err != nil {
		t.Fatalf("read provider earnings: %v", err)
	}
	if providerEarnings != 21_875 {
		t.Fatalf("provider earnings=%d; want 21875", providerEarnings)
	}

	balance, err := f.service.wallet.Balance(ctx, f.clientID)
	if err != nil {
		t.Fatal(err)
	}
	if balance.HeldIRR != 0 || balance.AvailableIRR != 975_000 {
		t.Fatalf("client balance after concurrent settlement=%+v", balance)
	}
	assertBalancedLedger(t, f.pool)
}

func TestConcurrentRefundReversesOnce(t *testing.T) {
	f, ctx, bookingID := completeBookingSetup(t)
	if _, err := f.service.RequestCompletion(ctx, f.providerID, bookingID, "refund-race-completion"); err != nil {
		t.Fatalf("request completion: %v", err)
	}
	if _, err := f.service.ConfirmCompletion(ctx, f.clientID, bookingID, "refund-race-confirm"); err != nil {
		t.Fatalf("confirm completion: %v", err)
	}
	adminID := newAdminFixture(t, f.pool)

	const workers = 8
	var wg sync.WaitGroup
	var successes atomic.Int64
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("race-refund-%d", i)
			if err := f.service.Refund(ctx, adminID, bookingID, key, fmt.Sprintf("race-refund-request-%d", i)); err != nil {
				errs[i] = err
				return
			}
			successes.Add(1)
		}(i)
	}
	wg.Wait()

	if successes.Load() == 0 {
		t.Fatalf("no refund request succeeded: %v", errs)
	}
	for _, err := range errs {
		if err != nil && !errors.Is(err, wallet.ErrNotReversible) && !errors.Is(err, wallet.ErrAlreadyReversed) {
			t.Fatalf("unexpected refund error: %v", err)
		}
	}

	var reversalCount int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE reference = $1`, fmt.Sprintf("booking:%d:refund-reversal", bookingID)).Scan(&reversalCount); err != nil {
		t.Fatalf("count reversal transactions: %v", err)
	}
	if reversalCount != 1 {
		t.Fatalf("reversal transactions=%d; want exactly 1", reversalCount)
	}

	balance, err := f.service.wallet.Balance(ctx, f.clientID)
	if err != nil {
		t.Fatal(err)
	}
	if balance.AvailableIRR != 1_000_000 || balance.HeldIRR != 0 {
		t.Fatalf("client balance after concurrent refund=%+v", balance)
	}

	var providerEarnings, commissionBalance int64
	if err := f.pool.QueryRow(ctx, `SELECT balance_irr FROM wallet_accounts WHERE owner_user_id = $1 AND kind = 'provider_earnings'`, f.providerID).Scan(&providerEarnings); err != nil {
		t.Fatalf("read provider earnings: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT balance_irr FROM wallet_accounts WHERE owner_user_id IS NULL AND kind = 'platform_commission'`).Scan(&commissionBalance); err != nil {
		t.Fatalf("read platform commission: %v", err)
	}
	if providerEarnings != 0 || commissionBalance != 0 {
		t.Fatalf("balancing after reversal: provider=%d commission=%d", providerEarnings, commissionBalance)
	}
	assertBalancedLedger(t, f.pool)
	if err := f.service.wallet.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile ledger: %v", err)
	}
}
