package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	zarinpalRequestPath = "/pg/v4/payment/request.json"
	zarinpalVerifyPath  = "/pg/v4/payment/verify.json"
	zarinpalStartPath   = "/pg/StartPay/"
)

type ZarinpalEnvironment string

const (
	ZarinpalSandbox     ZarinpalEnvironment = "sandbox"
	ZarinpalProduction  ZarinpalEnvironment = "production"
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
	return &ZarinpalGateway{
		merchantID: strings.TrimSpace(merchantID),
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
	defer response.Close()
	decoder := json.NewDecoder(io.LimitReader(response, 1<<20))
	decoder.UseNumber()
	var envelope struct {
		Data struct {
			Code      json.Number `json:"code"`
			Authority string      `json:"authority"`
		} `json:"data"`
	}
	if decoder.Decode(&envelope) != nil {
		return CreatedPayment{}, ErrGatewayUnknownOutcome
	}
	code, err := envelope.Data.Code.Int64()
	if err != nil || code != 100 || !validAuthority(envelope.Data.Authority) {
		return CreatedPayment{}, ErrGatewayUnknownOutcome
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
	defer response.Close()
	decoder := json.NewDecoder(io.LimitReader(response, 1<<20))
	decoder.UseNumber()
	var envelope struct {
		Data struct {
			Code  json.Number `json:"code"`
			RefID any         `json:"ref_id"`
		} `json:"data"`
	}
	if decoder.Decode(&envelope) != nil {
		return Verification{}, ErrGatewayUnknownOutcome
	}
	code, err := envelope.Data.Code.Int64()
	if err != nil {
		return Verification{}, ErrGatewayUnknownOutcome
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
			return Verification{}, ErrGatewayUnknownOutcome
		}
		result.Approved = true
	default:
		return Verification{}, ErrGatewayUnknownOutcome
	}
	return result, nil
}

func (g *ZarinpalGateway) RedirectURL(authority string) (string, error) {
	if !validAuthority(authority) {
		return "", ErrInvalidAuthority
	}
	return g.baseURL + zarinpalStartPath + url.PathEscape(authority), nil
}

func (g *ZarinpalGateway) post(ctx context.Context, path string, body []byte) (io.ReadCloser, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, ErrGatewayUnknownOutcome
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := g.client.Do(request)
	if err != nil {
		return nil, ErrGatewayUnknownOutcome
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		return nil, ErrGatewayUnknownOutcome
	}
	return response.Body, nil
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

func validCallbackURL(raw string) bool {
	parsed, err := url.ParseRequestURI(raw)
	return err == nil && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https")
}
