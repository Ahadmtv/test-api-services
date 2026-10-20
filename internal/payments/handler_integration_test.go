package payments

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"test-api/internal/booking"
	"test-api/internal/identity"
	"test-api/internal/platform"
	"test-api/internal/scheduling"
)

func TestBookingPaymentCreateBodyUsesServerPrice(t *testing.T) {
	f, bookingService, _, serviceID := newDirectBookingPaymentFixture(t)
	created, err := bookingService.Create(context.Background(), f.userID, booking.CreateInput{
		ServiceID: serviceID, StartsAt: bookingPaymentStartTime(), PaymentMethod: booking.PaymentMethodZarinpal,
	}, booking.Config{
		PendingPaymentTTL: 15 * time.Minute,
		Availability: scheduling.AvailabilityPolicy{
			SlotIncrement: 15 * time.Minute, MinimumNotice: 0, MaximumHorizon: 90 * 24 * time.Hour,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := identity.IssueAccessToken(f.userID, "client@example.test", []string{"client"}, identity.AccessTokenConfig{Secret: testAccessSecret, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	app := fiber.New(fiber.Config{ErrorHandler: platform.ErrorHandler(slog.New(slog.NewTextHandler(io.Discard, nil)))})
	app.Use(platform.RequestID())
	NewHandler(f.service, nil, "").Register(app, testAccessSecret)

	request := httptest.NewRequest(http.MethodPost, "/api/v1/payments/zarinpal", bytes.NewBufferString(
		`{"purpose":"booking","booking_id":`+strconvID(created.ID)+`}`,
	))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Idempotency-Key", "booking-api-start")
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	response.Body.Close()

	invalid := httptest.NewRequest(http.MethodPost, "/api/v1/payments/zarinpal", bytes.NewBufferString(
		`{"purpose":"booking","booking_id":`+strconvID(created.ID)+`,"amount_irr":1000}`,
	))
	invalid.Header.Set("Authorization", "Bearer "+token)
	invalid.Header.Set("Idempotency-Key", "booking-api-invalid")
	response, err = app.Test(invalid)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("amount override status=%d body=%s", response.StatusCode, body)
	}
	response.Body.Close()
}

func TestPaymentCallbackRedirectsToConfiguredReturnURL(t *testing.T) {
	f := newPaymentFixture(t)
	payment, _, err := f.service.Start(context.Background(), f.userID, 20_000, "return-top-up", "return-request")
	if err != nil {
		t.Fatal(err)
	}

	const returnURL = "https://app.example.test/wallet/payment/result"
	app := fiber.New()
	app.Use(platform.RequestID())
	app.Get("/callback", NewHandler(f.service, nil, returnURL).Callback)

	for attempt := 1; attempt <= 2; attempt++ {
		response, err := app.Test(httptest.NewRequest(
			http.MethodGet,
			"/callback?Authority="+*payment.Authority+"&Status=OK",
			nil,
		))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusSeeOther {
			t.Fatalf("attempt %d status=%d body=%s", attempt, response.StatusCode, body)
		}
		if location := response.Header.Get("Location"); location != returnURL {
			t.Fatalf("attempt %d Location=%q", attempt, location)
		}
	}

	balance, err := f.wallet.Balance(context.Background(), f.userID)
	if err != nil || balance.AvailableIRR != 20_000 {
		t.Fatalf("balance=%+v error=%v", balance, err)
	}
}
