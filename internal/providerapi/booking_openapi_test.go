package providerapi

import (
	"os"
	"strings"
	"testing"
)

func TestOpenAPIContractIncludesBookingOperations(t *testing.T) {
	data, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read OpenAPI contract: %v", err)
	}
	spec := string(data)
	for _, obsolete := range []string{
		"/api/v1/provider/bookings/{bookingID}/accept:",
		"/api/v1/provider/bookings/{bookingID}/decline:",
		"pending_provider_acceptance",
		"BOOKING_PENDING_TTL",
		"\n        expires_at:",
	} {
		if strings.Contains(spec, obsolete) {
			t.Errorf("OpenAPI contract still contains obsolete approval field %q", obsolete)
		}
	}
	for _, operation := range []string{
		"/api/v1/bookings:",
		"/api/v1/bookings/{bookingID}:",
		"/api/v1/bookings/{bookingID}/cancel:",
		"/api/v1/provider/bookings:",
		"/api/v1/provider/bookings/{bookingID}/start:",
		"/api/v1/provider/bookings/{bookingID}/request-completion:",
		"/api/v1/provider/bookings/{bookingID}/cancel:",
		"/api/v1/bookings/{bookingID}/confirm-completion:",
		"/api/v1/bookings/{bookingID}/dispute:",
		"/api/v1/admin/bookings/{bookingID}/refund:",
		"/api/v1/admin/bookings/{bookingID}/dispute-resolution:",
		"/api/v1/provider/payouts:",
		"/api/v1/admin/payouts/{payoutID}/approve:",
		"/api/v1/admin/payouts/{payoutID}/reject:",
		"/api/v1/admin/payouts/{payoutID}/processing:",
		"/api/v1/admin/payouts/{payoutID}/paid:",
		"/api/v1/admin/payouts/{payoutID}/fail:",
		"PayoutRequest:",
		"PayoutPage:",
		"PayoutAdminInput:",
		"completion_pending",
		"disputed",
		"BookingCreateInput:",
		"BookingPage:",
		"Idempotency-Key",
	} {
		if !strings.Contains(spec, operation) {
			t.Errorf("OpenAPI contract is missing %q", operation)
		}
	}
}
