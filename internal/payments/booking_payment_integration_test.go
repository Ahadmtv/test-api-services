package payments

import (
	"context"
	"errors"
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
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM booking_holds WHERE booking_id = $1 AND status = 'active'`, created.ID).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	if holds != 1 {
		t.Fatalf("active holds=%d; want 1", holds)
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
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM booking_holds WHERE booking_id = $1`, created.ID).Scan(&holds); err != nil {
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

func TestPendingBookingExpirySkipsActiveAttempts(t *testing.T) {
	f, bookingService, _, serviceID := newDirectBookingPaymentFixture(t)
	ctx := context.Background()
	config := booking.Config{
		PendingPaymentTTL: 15 * time.Minute,
		Availability: scheduling.AvailabilityPolicy{
			SlotIncrement: 15 * time.Minute, MinimumNotice: 0, MaximumHorizon: 90 * 24 * time.Hour,
		},
	}
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
	if expired != 1 {
		t.Fatalf("expired=%d; want 1", expired)
	}
	var unpaidStatus, activeStatus string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM bookings WHERE id = $1`, unpaid.ID).Scan(&unpaidStatus); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT status FROM bookings WHERE id = $1`, active.ID).Scan(&activeStatus); err != nil {
		t.Fatal(err)
	}
	if unpaidStatus != "expired" || activeStatus != "pending_payment" {
		t.Fatalf("unpaid=%s active=%s", unpaidStatus, activeStatus)
	}
}
