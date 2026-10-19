package payments

import (
	"os"
	"strings"
	"testing"
)

func TestOpenAPIContainsZarinpalPaymentOperations(t *testing.T) {
	data, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	spec := string(data)
	for _, required := range []string{
		"/api/v1/payments/zarinpal:",
		"/api/v1/payments/zarinpal/callback:",
		"/api/v1/payments/{paymentID}:",
		"PaymentCreateInput:",
		"PaymentCreateResponse:",
		"verification_unknown",
		"status فقط داده بازگشت است",
	} {
		if !strings.Contains(spec, required) {
			t.Errorf("OpenAPI contract is missing %q", required)
		}
	}
}
