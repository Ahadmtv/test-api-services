package payments

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"test-api/internal/booking"
	"test-api/internal/catalog"
	"test-api/internal/providers"
	"test-api/internal/scheduling"
)

func newDirectBookingPaymentFixture(t *testing.T) (*paymentFixture, *booking.Service, int64, int64) {
	t.Helper()
	f := newPaymentFixture(t)
	ctx := context.Background()
	var providerUserID int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ('provider-payment@example.test', 'test') RETURNING id`).Scan(&providerUserID); err != nil {
		t.Fatalf("create provider user: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO provider_profiles (provider_id, display_name, is_active, verification_status)
		VALUES ($1, 'Provider', TRUE, 'verified')
	`, providerUserID); err != nil {
		t.Fatalf("create provider profile: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'provider')`, providerUserID); err != nil {
		t.Fatalf("grant provider role: %v", err)
	}
	for weekday := 1; weekday <= 7; weekday++ {
		if _, err := f.pool.Exec(ctx, `
			INSERT INTO weekly_working_hours (provider_id, weekday, opens_at, closes_at)
			VALUES ($1, $2, '00:00', '23:59')
		`, providerUserID, weekday); err != nil {
			t.Fatalf("create provider hours: %v", err)
		}
	}
	var serviceID int64
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO services (provider_id, name, duration_minutes, price_irr)
		VALUES ($1, 'Gateway service', 30, 25000) RETURNING id
	`, providerUserID).Scan(&serviceID); err != nil {
		t.Fatalf("create service: %v", err)
	}
	bookingService := booking.NewService(
		f.pool, providers.NewService(f.pool), catalog.NewStore(f.pool), scheduling.NewStore(f.pool),
		booking.Config{
			PendingPaymentTTL: 15 * time.Minute,
			ExpiryGrace:       15 * time.Minute,
			Availability: scheduling.AvailabilityPolicy{
				SlotIncrement: 15 * time.Minute, MinimumNotice: 0, MaximumHorizon: 90 * 24 * time.Hour,
			},
		},
		f.wallet,
	)
	f.service = NewService(f.pool, f.wallet, f.gateway, "https://example.test/api/v1/payments/zarinpal/callback", bookingService)
	return f, bookingService, providerUserID, serviceID
}

func bookingPaymentStartTime() time.Time {
	return time.Now().UTC().Add(48 * time.Hour).Truncate(time.Hour)
}

func TestBookingPaymentCapturesWithoutCreditingWallet(t *testing.T) {
	f, bookingService, _, serviceID := newDirectBookingPaymentFixture(t)
	ctx := context.Background()
	created, err := bookingService.Create(ctx, f.userID, booking.CreateInput{
		ServiceID: serviceID, StartsAt: bookingPaymentStartTime(), PaymentMethod: booking.PaymentMethodZarinpal,
	}, booking.Config{
		PendingPaymentTTL: 15 * time.Minute,
		Availability: scheduling.AvailabilityPolicy{
			SlotIncrement: 15 * time.Minute, MinimumNotice: 0, MaximumHorizon: 90 * 24 * time.Hour,
		},
	})
	if err != nil {
		t.Fatalf("create gateway booking: %v", err)
	}
	if created.Status != "pending_payment" || created.PaymentExpiresAt == nil {
		t.Fatalf("created status=%s expires=%v; want pending_payment with expiry", created.Status, created.PaymentExpiresAt)
	}
	occupied, err := scheduling.NewOccupancyStore(f.pool).ListOccupied(
		ctx, created.ProviderID, created.StartsAt.Add(-time.Minute), created.EndsAt.Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("list occupied intervals: %v", err)
	}
	if len(occupied) != 1 {
		t.Fatalf("pending payment occupies %d intervals; want 1", len(occupied))
	}
	if _, err := bookingService.Create(ctx, f.userID, booking.CreateInput{
		ServiceID: serviceID, StartsAt: created.StartsAt,
	}, booking.Config{
		Availability: scheduling.AvailabilityPolicy{
			SlotIncrement: 15 * time.Minute, MinimumNotice: 0, MaximumHorizon: 90 * 24 * time.Hour,
		},
	}); !errors.Is(err, booking.ErrSlotUnavailable) {
		t.Fatalf("overlap with pending payment error=%v; want slot unavailable", err)
	}
	before, err := f.wallet.Balance(ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	payment, redirect, err := f.service.StartBooking(ctx, f.userID, created.ID, "booking-start", "req")
	if err != nil || redirect == "" {
		t.Fatalf("start booking payment: %v redirect=%q", err, redirect)
	}
	if payment.Purpose != PurposeBooking || payment.BookingID == nil || *payment.BookingID != created.ID || payment.AmountIRR != 25000 {
		t.Fatalf("payment = %+v", payment)
	}
	replayed, _, err := f.service.StartBooking(ctx, f.userID, created.ID, "booking-start-retry", "retry-req")
	if err != nil || replayed.ID != payment.ID || f.gateway.CreateCalls != 1 {
		t.Fatalf("duplicate payment start=%+v err=%v create calls=%d; want same attempt and one gateway request", replayed, err, f.gateway.CreateCalls)
	}
	if _, err := f.service.HandleCallback(ctx, *payment.Authority, "OK", "callback"); err != nil {
		t.Fatalf("handle callback: %v", err)
	}
	after, err := f.wallet.Balance(ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	if after.AvailableIRR != before.AvailableIRR || after.HeldIRR != before.HeldIRR+25000 {
		t.Fatalf("available=%d->%d held=%d->%d", before.AvailableIRR, after.AvailableIRR, before.HeldIRR, after.HeldIRR)
	}
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM bookings WHERE id = $1`, created.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "confirmed" {
		t.Fatalf("booking status=%s; want confirmed", status)
	}
	occupied, err = scheduling.NewOccupancyStore(f.pool).ListOccupied(
		ctx, created.ProviderID, created.StartsAt.Add(-time.Minute), created.EndsAt.Add(time.Minute),
	)
	if err != nil || len(occupied) != 1 {
		t.Fatalf("confirmed booking occupancy=%d err=%v; want slot retained", len(occupied), err)
	}
	var ledgerReference string
	if err := f.pool.QueryRow(ctx, `
		SELECT t.reference FROM payment_attempts p JOIN ledger_transactions t ON t.id = p.ledger_transaction_id
		WHERE p.id = $1
	`, payment.ID).Scan(&ledgerReference); err != nil {
		t.Fatal(err)
	}
	if ledgerReference != "payment:"+strconvID(payment.ID)+":capture" {
		t.Fatalf("ledger reference=%s", ledgerReference)
	}
	if _, err := f.service.HandleCallback(ctx, *payment.Authority, "OK", "callback-again"); err != nil {
		t.Fatalf("repeat callback: %v", err)
	}
	var holds int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM wallet_holds WHERE booking_id = $1 AND status = 'active'`, created.ID).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	if holds != 1 {
		t.Fatalf("active holds=%d; want 1", holds)
	}
	if err := f.wallet.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile ledger: %v", err)
	}
}

func TestConcurrentBookingPaymentCallbacksCaptureOnce(t *testing.T) {
	f, bookingService, _, serviceID := newDirectBookingPaymentFixture(t)
	ctx := context.Background()
	created, err := bookingService.Create(ctx, f.userID, booking.CreateInput{
		ServiceID: serviceID, StartsAt: bookingPaymentStartTime(), PaymentMethod: booking.PaymentMethodZarinpal,
	}, bookingPaymentConfig())
	if err != nil {
		t.Fatalf("create booking: %v", err)
	}
	payment, _, err := f.service.StartBooking(ctx, f.userID, created.ID, "concurrent-callback-start", "req")
	if err != nil {
		t.Fatal(err)
	}
	f.gateway.NextVerify = Verification{GatewayCode: "100", ReferenceID: "concurrent-ref", Approved: true}

	start := make(chan struct{})
	errorsFound := make(chan error, 2)
	var callbacks sync.WaitGroup
	for range 2 {
		callbacks.Add(1)
		go func() {
			defer callbacks.Done()
			<-start
			_, err := f.service.HandleCallback(ctx, *payment.Authority, "OK", "concurrent-callback")
			errorsFound <- err
		}()
	}
	close(start)
	callbacks.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatalf("handle duplicate callback: %v", err)
		}
	}

	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM bookings WHERE id = $1`, created.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "confirmed" {
		t.Fatalf("booking status=%s; want confirmed", status)
	}
	var captures int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE reference = $1`,
		"payment:"+strconvID(payment.ID)+":capture").Scan(&captures); err != nil {
		t.Fatal(err)
	}
	if captures != 1 {
		t.Fatalf("capture ledger documents=%d; want 1", captures)
	}
	if err := f.wallet.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile ledger: %v", err)
	}
}

func TestFailedBookingPaymentCancelsPendingBooking(t *testing.T) {
	f, bookingService, _, serviceID := newDirectBookingPaymentFixture(t)
	ctx := context.Background()
	created, err := bookingService.Create(ctx, f.userID, booking.CreateInput{
		ServiceID: serviceID, StartsAt: bookingPaymentStartTime(), PaymentMethod: booking.PaymentMethodZarinpal,
	}, booking.Config{
		PendingPaymentTTL: 15 * time.Minute,
		Availability: scheduling.AvailabilityPolicy{
			SlotIncrement: 15 * time.Minute, MinimumNotice: 0, MaximumHorizon: 90 * 24 * time.Hour,
		},
	})
	if err != nil {
		t.Fatalf("create gateway booking: %v", err)
	}
	payment, _, err := f.service.StartBooking(ctx, f.userID, created.ID, "failed-start", "req")
	if err != nil {
		t.Fatal(err)
	}
	f.gateway.NextVerify = Verification{GatewayCode: "failed", Approved: false}
	if _, err := f.service.HandleCallback(ctx, *payment.Authority, "NOK", "callback"); err != nil {
		t.Fatalf("handle failed callback: %v", err)
	}
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM bookings WHERE id = $1`, created.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" {
		t.Fatalf("booking status=%s; want cancelled", status)
	}
	var holds int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM wallet_holds WHERE booking_id = $1`, created.ID).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	if holds != 0 {
		t.Fatalf("holds=%d; want 0", holds)
	}
}

func TestRejectedGatewayCreateCancelsPendingBooking(t *testing.T) {
	f, bookingService, _, serviceID := newDirectBookingPaymentFixture(t)
	ctx := context.Background()
	created, err := bookingService.Create(ctx, f.userID, booking.CreateInput{
		ServiceID: serviceID, StartsAt: bookingPaymentStartTime(), PaymentMethod: booking.PaymentMethodZarinpal,
	}, booking.Config{
		PendingPaymentTTL: 15 * time.Minute,
		Availability: scheduling.AvailabilityPolicy{
			SlotIncrement: 15 * time.Minute, MinimumNotice: 0, MaximumHorizon: 90 * 24 * time.Hour,
		},
	})
	if err != nil {
		t.Fatalf("create gateway booking: %v", err)
	}
	f.gateway.NextCreateErr = ErrGatewayRejected
	payment, _, err := f.service.StartBooking(ctx, f.userID, created.ID, "rejected-start", "req")
	if err != nil {
		t.Fatalf("start rejected booking payment: %v", err)
	}
	if payment.Status != "failed" {
		t.Fatalf("payment status=%s; want failed", payment.Status)
	}
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM bookings WHERE id = $1`, created.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" {
		t.Fatalf("booking status=%s; want cancelled", status)
	}
}

func bookingPaymentConfig() booking.Config {
	return booking.Config{
		PendingPaymentTTL: 15 * time.Minute,
		ExpiryGrace:       15 * time.Minute,
		Availability: scheduling.AvailabilityPolicy{
			SlotIncrement: 15 * time.Minute, MinimumNotice: 0, MaximumHorizon: 90 * 24 * time.Hour,
		},
	}
}

func TestPendingBookingExpiryWithinGraceKeepsSlotReserved(t *testing.T) {
	f, bookingService, _, serviceID := newDirectBookingPaymentFixture(t)
	ctx := context.Background()
	config := bookingPaymentConfig()
	unpaid, err := bookingService.Create(ctx, f.userID, booking.CreateInput{
		ServiceID: serviceID, StartsAt: bookingPaymentStartTime(), PaymentMethod: booking.PaymentMethodZarinpal,
	}, config)
	if err != nil {
		t.Fatalf("create unpaid booking: %v", err)
	}
	active, err := bookingService.Create(ctx, f.userID, booking.CreateInput{
		ServiceID: serviceID, StartsAt: bookingPaymentStartTime().Add(2 * time.Hour), PaymentMethod: booking.PaymentMethodZarinpal,
	}, config)
	if err != nil {
		t.Fatalf("create booking with active attempt: %v", err)
	}
	if _, _, err := f.service.StartBooking(ctx, f.userID, active.ID, "active-start", "req"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		UPDATE bookings SET payment_expires_at = now() - interval '1 minute'
		WHERE id = ANY($1)
	`, []int64{unpaid.ID, active.ID}); err != nil {
		t.Fatal(err)
	}
	expired, err := bookingService.ExpirePendingPayments(ctx, 100)
	if err != nil {
		t.Fatalf("expire pending payments: %v", err)
	}
	if expired != 0 {
		t.Fatalf("expired=%d; want 0 within grace", expired)
	}
	var unpaidStatus, activeStatus, attemptStatus string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM bookings WHERE id = $1`, unpaid.ID).Scan(&unpaidStatus); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT status FROM bookings WHERE id = $1`, active.ID).Scan(&activeStatus); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT status FROM payment_attempts WHERE booking_id = $1`, active.ID).Scan(&attemptStatus); err != nil {
		t.Fatal(err)
	}
	if unpaidStatus != "pending_payment" || activeStatus != "pending_payment" || attemptStatus != "pending" {
		t.Fatalf("unpaid=%s active=%s attempt=%s", unpaidStatus, activeStatus, attemptStatus)
	}
}

func TestPendingBookingExpiryAfterGraceReleasesSlot(t *testing.T) {
	f, bookingService, _, serviceID := newDirectBookingPaymentFixture(t)
	ctx := context.Background()
	config := bookingPaymentConfig()
	abandoned, err := bookingService.Create(ctx, f.userID, booking.CreateInput{
		ServiceID: serviceID, StartsAt: bookingPaymentStartTime(), PaymentMethod: booking.PaymentMethodZarinpal,
	}, config)
	if err != nil {
		t.Fatalf("create abandoned booking: %v", err)
	}
	// Simulate an abandoned payment: a live gateway attempt (pending, authority
	// stored) whose payment window passed beyond TTL + grace.
	payment, _, err := f.service.StartBooking(ctx, f.userID, abandoned.ID, "abandoned-start", "req")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		UPDATE bookings SET payment_expires_at = now() - interval '20 minutes'
		WHERE id = $1
	`, abandoned.ID); err != nil {
		t.Fatal(err)
	}
	expired, err := bookingService.ExpirePendingPayments(ctx, 100)
	if err != nil {
		t.Fatalf("expire pending payments: %v", err)
	}
	if expired != 1 {
		t.Fatalf("expired=%d; want 1", expired)
	}
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM bookings WHERE id = $1`, abandoned.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "expired" {
		t.Fatalf("booking status=%s; want expired", status)
	}
	// The abandoned attempt itself stays in place for the unresolved report;
	// it must never resurrect the expired booking. Retrying payment is blocked.
	if err := f.pool.QueryRow(ctx, `SELECT status FROM payment_attempts WHERE id = $1`, payment.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("attempt status=%s; want pending (manual follow-up)", status)
	}
	if _, _, err := f.service.StartBooking(ctx, f.userID, abandoned.ID, "retry-start", "req"); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("retry after expiry error=%v; want payment conflict", err)
	}
	// The released slot can be booked again without double-booking conflicts.
	if _, err := bookingService.Create(ctx, f.userID, booking.CreateInput{
		ServiceID: serviceID, StartsAt: bookingPaymentStartTime(), PaymentMethod: booking.PaymentMethodZarinpal,
	}, config); err != nil {
		t.Fatalf("rebook released slot: %v", err)
	}
}

func TestConcurrentPendingBookingExpiryReleasesOnce(t *testing.T) {
	f, bookingService, _, serviceID := newDirectBookingPaymentFixture(t)
	ctx := context.Background()
	created, err := bookingService.Create(ctx, f.userID, booking.CreateInput{
		ServiceID: serviceID, StartsAt: bookingPaymentStartTime(), PaymentMethod: booking.PaymentMethodZarinpal,
	}, bookingPaymentConfig())
	if err != nil {
		t.Fatalf("create booking: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		UPDATE bookings SET payment_expires_at = now() - interval '20 minutes'
		WHERE id = $1
	`, created.ID); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan int, 2)
	errorsFound := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			expired, err := bookingService.ExpirePendingPayments(ctx, 100)
			results <- expired
			errorsFound <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatalf("expire pending payment concurrently: %v", err)
		}
	}
	var expired int
	for count := range results {
		expired += count
	}
	if expired != 1 {
		t.Fatalf("concurrent expiry count=%d; want exactly 1", expired)
	}
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM bookings WHERE id = $1`, created.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "expired" {
		t.Fatalf("booking status=%s; want expired", status)
	}
}

func TestLateVerifiedPaymentAfterExpiryDoesNotConfirmBooking(t *testing.T) {
	f, bookingService, _, serviceID := newDirectBookingPaymentFixture(t)
	ctx := context.Background()
	config := bookingPaymentConfig()
	created, err := bookingService.Create(ctx, f.userID, booking.CreateInput{
		ServiceID: serviceID, StartsAt: bookingPaymentStartTime(), PaymentMethod: booking.PaymentMethodZarinpal,
	}, config)
	if err != nil {
		t.Fatalf("create booking: %v", err)
	}
	payment, _, err := f.service.StartBooking(ctx, f.userID, created.ID, "late-start", "req")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		UPDATE bookings SET payment_expires_at = now() - interval '20 minutes'
		WHERE id = $1
	`, created.ID); err != nil {
		t.Fatal(err)
	}
	// Verification may succeed after TTL + grace but before the worker runs;
	// the deadline must still prevent confirmation and capture.
	f.gateway.NextVerify = Verification{GatewayCode: "100", ReferenceID: "late-ref", Approved: true}
	if _, err := f.service.HandleCallback(ctx, *payment.Authority, "OK", "late-callback"); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("late callback error=%v; want payment conflict", err)
	}
	if expired, err := bookingService.ExpirePendingPayments(ctx, 100); err != nil || expired != 1 {
		t.Fatalf("expiry expired=%d err=%v; want 1 booking expired", expired, err)
	}
	var bookingStatus, attemptStatus string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM bookings WHERE id = $1`, created.ID).Scan(&bookingStatus); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT status FROM payment_attempts WHERE id = $1`, payment.ID).Scan(&attemptStatus); err != nil {
		t.Fatal(err)
	}
	if bookingStatus != "expired" {
		t.Fatalf("booking status=%s; want expired", bookingStatus)
	}
	if attemptStatus != "verification_unknown" {
		t.Fatalf("attempt status=%s; want verification_unknown for operator follow-up", attemptStatus)
	}
	var captures int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE reference = $1`,
		"payment:"+strconvID(payment.ID)+":capture").Scan(&captures); err != nil {
		t.Fatal(err)
	}
	if captures != 0 {
		t.Fatalf("capture ledger documents=%d; want 0", captures)
	}
	if err := f.wallet.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile ledger: %v", err)
	}
}
