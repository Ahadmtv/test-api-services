package payments

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var (
	ErrGatewayUnknownOutcome = errors.New("payment gateway outcome is unknown")
	ErrGatewayRejected       = errors.New("payment gateway rejected the operation")
	ErrGatewayUnavailable    = errors.New("payment gateway is not configured")
	ErrGatewayTimeout        = errors.New("payment gateway request timed out")
	ErrGatewayTransport      = errors.New("payment gateway transport failed")
	ErrGatewayHTTPStatus     = errors.New("payment gateway returned an unexpected HTTP status")
	ErrGatewayMalformed      = errors.New("payment gateway response is malformed")
)

type Gateway interface {
	Create(context.Context, CreateRequest) (CreatedPayment, error)
	Verify(context.Context, VerifyRequest) (Verification, error)
	RedirectURL(string) (string, error)
}

type CreateRequest struct {
	AmountIRR   int64
	CallbackURL string
	Description string
}

type CreatedPayment struct {
	Authority string
}

type VerifyRequest struct {
	Authority string
	AmountIRR int64
}

type Verification struct {
	Authority   string
	AmountIRR   int64
	ReferenceID string
	GatewayCode string
	Approved    bool
}

type FakeGateway struct {
	mu                sync.Mutex
	NextCreateErr     error
	NextVerify        Verification
	NextVerifyErr     error
	CreateCalls       int
	VerificationCalls int
}

func (g *FakeGateway) Create(_ context.Context, _ CreateRequest) (CreatedPayment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.CreateCalls++
	if g.NextCreateErr != nil {
		err := g.NextCreateErr
		g.NextCreateErr = nil
		return CreatedPayment{}, err
	}
	return CreatedPayment{Authority: fmt.Sprintf("A%035d", g.CreateCalls)}, nil
}

func (g *FakeGateway) Verify(_ context.Context, request VerifyRequest) (Verification, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.VerificationCalls++
	if g.NextVerifyErr != nil {
		err := g.NextVerifyErr
		g.NextVerifyErr = nil
		return Verification{}, err
	}
	result := g.NextVerify
	if result.Authority == "" && result.AmountIRR == 0 && result.ReferenceID == "" && result.GatewayCode == "" && !result.Approved {
		result = Verification{ReferenceID: fmt.Sprintf("fake-ref-%d", g.VerificationCalls), GatewayCode: "100", Approved: true}
	}
	if result.Authority == "" {
		result.Authority = request.Authority
	}
	if result.AmountIRR == 0 {
		result.AmountIRR = request.AmountIRR
	}
	return result, nil
}

func (g *FakeGateway) RedirectURL(authority string) (string, error) {
	if !validAuthority(authority) {
		return "", ErrInvalidAuthority
	}
	return "https://sandbox.zarinpal.com/pg/StartPay/" + authority, nil
}
