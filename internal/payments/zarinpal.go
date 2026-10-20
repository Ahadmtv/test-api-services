package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	zarinpalRequestPath = "/pg/v4/payment/request.json"
	zarinpalVerifyPath  = "/pg/v4/payment/verify.json"
	zarinpalStartPath   = "/pg/StartPay/"

	// Zarinpal sandbox accepts any UUID as merchant_id; production requires the
	// merchant identifier issued by Zarinpal.
	defaultSandboxMerchantID = "00000000-0000-4000-8000-000000000001"
)

type ZarinpalEnvironment string

const (
	ZarinpalSandbox    ZarinpalEnvironment = "sandbox"
	ZarinpalProduction ZarinpalEnvironment = "production"
)

type ZarinpalGateway struct {
	merchantID string
	baseURL    string
	client     *http.Client
}

func NewZarinpalGateway(merchantID string, environment ZarinpalEnvironment) (*ZarinpalGateway, error) {
	baseURL := "https://sandbox.zarinpal.com"
	switch environment {
	case ZarinpalSandbox:
	case ZarinpalProduction:
		baseURL = "https://payment.zarinpal.com"
	default:
		return nil, fmt.Errorf("invalid Zarinpal environment")
	}
	merchantID = strings.TrimSpace(merchantID)
	if merchantID == "" {
		if environment == ZarinpalSandbox {
			merchantID = defaultSandboxMerchantID
		} else {
			return nil, fmt.Errorf("%w: ZARINPAL_MERCHANT_ID is required in production", ErrGatewayUnavailable)
		}
	}
	if !validMerchantID(merchantID) {
		return nil, fmt.Errorf("%w: ZARINPAL_MERCHANT_ID must be a 36-character UUID", ErrGatewayUnavailable)
	}
	return &ZarinpalGateway{
		merchantID: merchantID,
		baseURL:    baseURL,
		client: &http.Client{
			Timeout: 12 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (g *ZarinpalGateway) Configured() bool {
	return g.merchantID != ""
}

func (g *ZarinpalGateway) Create(ctx context.Context, request CreateRequest) (CreatedPayment, error) {
	if !g.Configured() {
		return CreatedPayment{}, ErrGatewayUnavailable
	}
	if request.AmountIRR < OfficialMinimumAmountIRR || !validCallbackURL(request.CallbackURL) || request.Description == "" {
		return CreatedPayment{}, ErrInvalidInput
	}
	payload := map[string]any{
		"merchant_id":  g.merchantID,
		"amount":       request.AmountIRR,
		"callback_url": request.CallbackURL,
		"description":  request.Description,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return CreatedPayment{}, ErrGatewayUnknownOutcome
	}
	response, err := g.post(ctx, zarinpalRequestPath, body)
	if err != nil {
		return CreatedPayment{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(response))
	decoder.UseNumber()
	var envelope struct {
		Data struct {
			Code      json.Number `json:"code"`
			Authority string      `json:"authority"`
		} `json:"data"`
	}
	if decoder.Decode(&envelope) != nil {
		return CreatedPayment{}, ErrGatewayMalformed
	}
	code, err := envelope.Data.Code.Int64()
	if err != nil {
		return CreatedPayment{}, ErrGatewayMalformed
	}
	if code != 100 {
		return CreatedPayment{}, fmt.Errorf("%w: code %d", ErrGatewayRejected, code)
	}
	if !validAuthority(envelope.Data.Authority) {
		return CreatedPayment{}, ErrGatewayMalformed
	}
	return CreatedPayment{Authority: envelope.Data.Authority}, nil
}

func (g *ZarinpalGateway) Verify(ctx context.Context, request VerifyRequest) (Verification, error) {
	if !g.Configured() {
		return Verification{}, ErrGatewayUnavailable
	}
	if request.AmountIRR < OfficialMinimumAmountIRR || !validAuthority(request.Authority) {
		return Verification{}, ErrInvalidInput
	}
	payload := map[string]any{
		"merchant_id": g.merchantID,
		"amount":      request.AmountIRR,
		"authority":   request.Authority,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Verification{}, ErrGatewayUnknownOutcome
	}
	response, err := g.post(ctx, zarinpalVerifyPath, body)
	if err != nil {
		return Verification{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(response))
	decoder.UseNumber()
	var envelope struct {
		Data struct {
			Code  json.Number `json:"code"`
			RefID any         `json:"ref_id"`
		} `json:"data"`
	}
	if decoder.Decode(&envelope) != nil {
		return Verification{}, ErrGatewayMalformed
	}
	code, err := envelope.Data.Code.Int64()
	if err != nil {
		return Verification{}, ErrGatewayMalformed
	}
	// The verify response does not echo authority or amount; these fields reflect the request only.
	result := Verification{
		Authority:   request.Authority,
		AmountIRR:   request.AmountIRR,
		GatewayCode: envelope.Data.Code.String(),
	}
	switch code {
	case 100, 101:
		result.ReferenceID, err = scalarReference(envelope.Data.RefID)
		if err != nil {
			return Verification{}, ErrGatewayMalformed
		}
		result.Approved = true
	default:
		return Verification{}, fmt.Errorf("%w: code %d", ErrGatewayRejected, code)
	}
	return result, nil
}

func (g *ZarinpalGateway) RedirectURL(authority string) (string, error) {
	if !validAuthority(authority) {
		return "", ErrInvalidAuthority
	}
	return g.baseURL + zarinpalStartPath + url.PathEscape(authority), nil
}

func (g *ZarinpalGateway) post(ctx context.Context, path string, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, ErrGatewayTransport
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := g.client.Do(request)
	if err != nil {
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
			return nil, ErrGatewayTimeout
		}
		return nil, ErrGatewayTransport
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: read response body", ErrGatewayTransport)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode >= 400 && response.StatusCode < 500 &&
			response.StatusCode != http.StatusRequestTimeout &&
			response.StatusCode != http.StatusTooManyRequests {
			if code := rejectionCode(responseBody); code != "" {
				return nil, fmt.Errorf("%w: HTTP %d code %s", ErrGatewayRejected, response.StatusCode, code)
			}
			return nil, fmt.Errorf("%w: HTTP %d", ErrGatewayRejected, response.StatusCode)
		}
		return nil, fmt.Errorf("%w: HTTP %d", ErrGatewayHTTPStatus, response.StatusCode)
	}
	return responseBody, nil
}

func rejectionCode(body []byte) string {
	var envelope struct {
		Errors json.RawMessage `json:"errors"`
	}
	if json.Unmarshal(body, &envelope) != nil || len(envelope.Errors) == 0 {
		return ""
	}
	type apiError struct {
		Code json.Number `json:"code"`
	}
	var single apiError
	if json.Unmarshal(envelope.Errors, &single) == nil {
		return single.Code.String()
	}
	var list []apiError
	if json.Unmarshal(envelope.Errors, &list) == nil && len(list) > 0 {
		return list[0].Code.String()
	}
	return ""
}

func scalarReference(value any) (string, error) {
	switch reference := value.(type) {
	case string:
		if strings.TrimSpace(reference) == "" {
			return "", errors.New("empty gateway reference")
		}
		return reference, nil
	case json.Number:
		if _, err := reference.Int64(); err != nil {
			return "", err
		}
		return reference.String(), nil
	default:
		return "", errors.New("invalid gateway reference")
	}
}

func validMerchantID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if value[i] != '-' {
				return false
			}
			continue
		}
		character := value[i]
		if !((character >= '0' && character <= '9') ||
			(character >= 'a' && character <= 'f') ||
			(character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func validCallbackURL(raw string) bool {
	parsed, err := url.ParseRequestURI(raw)
	return err == nil && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https")
}
