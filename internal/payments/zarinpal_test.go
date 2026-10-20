package payments

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testMerchantID = "00000000-0000-4000-8000-000000000002"

func TestZarinpalCreateUsesDocumentedIRRRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != zarinpalRequestPath {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["merchant_id"] != "merchant-test" || body["amount"] != float64(10_000) || body["callback_url"] != "https://example.test/payments/callback" {
			t.Fatalf("request body=%v", body)
		}
		_, _ = w.Write([]byte(`{"data":{"code":100,"authority":"A0000000000000000000000000006qpmlj8d"}}`))
	}))
	defer server.Close()

	gateway := testGateway(server)
	created, err := gateway.Create(context.Background(), CreateRequest{AmountIRR: 10_000, CallbackURL: "https://example.test/payments/callback", Description: "Wallet top-up 1"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Authority != "A0000000000000000000000000006qpmlj8d" {
		t.Fatalf("authority=%q", created.Authority)
	}
}

func TestZarinpalVerifyParsesOfficialResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != zarinpalVerifyPath {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["merchant_id"] != "merchant-test" || body["authority"] != "A0000000000000000000000000006qpmlj8d" || body["amount"] != float64(10_000) {
			t.Fatalf("request body=%v", body)
		}
		_, _ = w.Write([]byte(`{"data":{"code":100,"ref_id":123456789}}`))
	}))
	defer server.Close()

	gateway := testGateway(server)
	result, err := gateway.Verify(context.Background(), VerifyRequest{Authority: "A0000000000000000000000000006qpmlj8d", AmountIRR: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Approved || result.AmountIRR != 10_000 || result.ReferenceID != "123456789" {
		t.Fatalf("verification=%+v", result)
	}
}

func TestZarinpalMalformedResponsesAreNotAccepted(t *testing.T) {
	for _, response := range []struct {
		name string
		body string
	}{
		{name: "malformed create response", body: `{"data":`},
		{name: "missing create authority", body: `{"data":{"code":100}}`},
	} {
		t.Run(response.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(response.body)) }))
			defer server.Close()
			gateway := testGateway(server)
			_, err := gateway.Create(context.Background(), CreateRequest{AmountIRR: 10_000, CallbackURL: "https://example.test/callback", Description: "test"})
			if !errors.Is(err, ErrGatewayMalformed) {
				t.Fatalf("create error=%v", err)
			}
		})
	}
	for _, response := range []struct {
		name string
		body string
	}{
		{name: "malformed verify response", body: `{"data":`},
		{name: "missing verify code", body: `{"data":{}}`},
		{name: "already verified without reference", body: `{"data":{"code":101}}`},
	} {
		t.Run(response.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(response.body)) }))
			defer server.Close()
			gateway := testGateway(server)
			result, err := gateway.Verify(context.Background(), VerifyRequest{Authority: "A0000000000000000000000000006qpmlj8d", AmountIRR: 10_000})
			if !errors.Is(err, ErrGatewayMalformed) {
				t.Fatalf("verify result=%+v error=%v", result, err)
			}
		})
	}
}

func TestZarinpalVerifyAcceptsAlreadyVerifiedResponseWithReference(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"code":101,"ref_id":555111}}`))
	}))
	defer server.Close()

	gateway := testGateway(server)
	result, err := gateway.Verify(context.Background(), VerifyRequest{Authority: "A0000000000000000000000000006qpmlj8d", AmountIRR: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Approved || result.ReferenceID != "555111" || result.GatewayCode != "101" {
		t.Fatalf("verification=%+v", result)
	}
}

func TestZarinpalStructuredGatewayRejectionIsDefinitive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"data":{},"errors":{"code":-9,"message":"The merchant id field is required.","validations":[]}}`))
	}))
	defer server.Close()

	gateway := testGateway(server)
	_, err := gateway.Create(context.Background(), CreateRequest{AmountIRR: 10_000, CallbackURL: "https://example.test/callback", Description: "test"})
	if !errors.Is(err, ErrGatewayRejected) {
		t.Fatalf("create error=%v", err)
	}
	if errors.Is(err, ErrGatewayUnknownOutcome) {
		t.Fatalf("rejection classified as unknown: %v", err)
	}
}

func TestZarinpalTransportErrorsAreClassified(t *testing.T) {
	t.Run("http error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
		defer server.Close()
		gateway := testGateway(server)
		_, err := gateway.Create(context.Background(), CreateRequest{AmountIRR: 10_000, CallbackURL: "https://example.test/callback", Description: "test"})
		if !errors.Is(err, ErrGatewayHTTPStatus) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(50 * time.Millisecond)
			_, _ = w.Write([]byte(`{"data":{"code":100,"authority":"A0000000000000000000000000006qpmlj8d"}}`))
		}))
		defer server.Close()
		gateway := testGateway(server)
		gateway.client.Timeout = time.Millisecond
		_, err := gateway.Create(context.Background(), CreateRequest{AmountIRR: 10_000, CallbackURL: "https://example.test/callback", Description: "test"})
		if !errors.Is(err, ErrGatewayTimeout) {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestZarinpalRedirectUsesExplicitEnvironment(t *testing.T) {
	for _, test := range []struct{ environment, expected string }{
		{string(ZarinpalSandbox), "https://sandbox.zarinpal.com/pg/StartPay/A0000000000000000000000000006qpmlj8d"},
		{string(ZarinpalProduction), "https://payment.zarinpal.com/pg/StartPay/A0000000000000000000000000006qpmlj8d"},
	} {
		gateway, err := NewZarinpalGateway(testMerchantID, ZarinpalEnvironment(test.environment))
		if err != nil {
			t.Fatal(err)
		}
		redirect, err := gateway.RedirectURL("A0000000000000000000000000006qpmlj8d")
		if err != nil || redirect != test.expected {
			t.Fatalf("redirect=%q error=%v", redirect, err)
		}
	}
	if _, err := NewZarinpalGateway(testMerchantID, "unknown"); err == nil {
		t.Fatal("invalid environment accepted")
	}
}

func TestZarinpalSandboxUsesDocumentedPlaceholderMerchant(t *testing.T) {
	gateway, err := NewZarinpalGateway("", ZarinpalSandbox)
	if err != nil {
		t.Fatal(err)
	}
	if !gateway.Configured() || gateway.merchantID != defaultSandboxMerchantID {
		t.Fatalf("sandbox merchant configured=%v", gateway.Configured())
	}
}

func TestZarinpalProductionRequiresMerchantID(t *testing.T) {
	if _, err := NewZarinpalGateway("", ZarinpalProduction); !errors.Is(err, ErrGatewayUnavailable) {
		t.Fatalf("production missing merchant error=%v", err)
	}
	if _, err := NewZarinpalGateway("merchant-test", ZarinpalSandbox); !errors.Is(err, ErrGatewayUnavailable) {
		t.Fatalf("malformed merchant error=%v", err)
	}
}

func TestGatewayRequiresDocumentedMinimumAmount(t *testing.T) {
	gateway, err := NewZarinpalGateway(testMerchantID, ZarinpalSandbox)
	if err != nil {
		t.Fatal(err)
	}
	for _, amount := range []int64{1, 9999} {
		if _, err := gateway.Create(context.Background(), CreateRequest{AmountIRR: amount, CallbackURL: "https://example.test/callback", Description: "test"}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("amount %d error=%v", amount, err)
		}
	}
	if !strings.HasPrefix(gateway.baseURL, "https://sandbox.zarinpal.com") {
		t.Fatalf("sandbox URL=%q", gateway.baseURL)
	}
}

func testGateway(server *httptest.Server) *ZarinpalGateway {
	return &ZarinpalGateway{merchantID: "merchant-test", baseURL: server.URL, client: server.Client()}
}
