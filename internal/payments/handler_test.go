package payments

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"

	"test-api/internal/platform"
)

func TestCallbackQueryAcceptsOfficialCapitalization(t *testing.T) {
	app := fiber.New()
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString(queryValue(c, "authority", "Authority") + "|" + queryValue(c, "status", "Status"))
	})
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/?Authority=A0000000000000000000000000006qpmlj8d&Status=OK", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "A0000000000000000000000000006qpmlj8d|OK" {
		t.Fatalf("callback query=%q", body)
	}
}

func TestPaymentErrorDistinguishesConfigurationFromUnknownOutcome(t *testing.T) {
	var unavailable *platform.AppError
	if !errors.As(paymentError(ErrGatewayUnavailable), &unavailable) ||
		unavailable.Code != "payment_provider_unavailable" ||
		unavailable.Status != 503 ||
		!errors.Is(unavailable, ErrGatewayUnavailable) {
		t.Fatalf("unavailable mapping=%+v", unavailable)
	}
	var unknown *platform.AppError
	if !errors.As(paymentError(ErrGatewayUnknownOutcome), &unknown) ||
		unknown.Code != "payment_verification_pending" ||
		unknown.Status != 503 ||
		!errors.Is(unknown, ErrGatewayUnknownOutcome) {
		t.Fatalf("unknown mapping=%+v", unknown)
	}
}
