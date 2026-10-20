package payments

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"

	"test-api/internal/platform"
)

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
