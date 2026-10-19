package wallet

import (
	"errors"
	"math"
	"math/big"
)

const (
	CurrencyIRR      = "IRR"
	basisPointsScale = 10000
)

var (
	ErrInvalidInput          = errors.New("invalid wallet input")
	ErrInvalidAmount         = errors.New("invalid wallet amount")
	ErrAmountOverflow        = errors.New("wallet amount overflow")
	ErrInsufficientFunds     = errors.New("insufficient wallet funds")
	ErrUnbalancedPosting     = errors.New("unbalanced ledger posting")
	ErrDuplicatePosting      = errors.New("duplicate ledger posting")
	ErrNoCommissionPolicy    = errors.New("no active commission policy")
	ErrInvalidCommissionRate = errors.New("invalid commission rate")
	ErrInvalidHoldTransition = errors.New("invalid wallet hold transition")
	ErrTransactionNotFound   = errors.New("ledger transaction not found")
	ErrNotReversible         = errors.New("ledger transaction is not reversible")
	ErrAlreadyReversed       = errors.New("ledger transaction already reversed")
	ErrIdempotencyConflict   = errors.New("idempotency key payload conflict")
)

func validateAmount(amount int64) error {
	if amount <= 0 {
		return ErrInvalidAmount
	}
	return nil
}

// addAmounts adds two non-negative amounts and rejects int64 overflow.
func addAmounts(a, b int64) (int64, error) {
	if a > math.MaxInt64-b {
		return 0, ErrAmountOverflow
	}
	return a + b, nil
}

// subtractAmounts subtracts b from a and rejects results below zero.
func subtractAmounts(a, b int64) (int64, error) {
	if b > a {
		return 0, ErrInsufficientFunds
	}
	return a - b, nil
}

// CalculateCommission splits amount into the platform commission and the provider share.
// The commission is floor(amount * rateBPS / 10000), computed without floating-point arithmetic.
func CalculateCommission(amount int64, rateBPS int) (commission, providerShare int64, err error) {
	if err := validateAmount(amount); err != nil {
		return 0, 0, err
	}
	if rateBPS < 0 || rateBPS > basisPointsScale {
		return 0, 0, ErrInvalidCommissionRate
	}
	product := new(big.Int).Mul(big.NewInt(amount), big.NewInt(int64(rateBPS)))
	commission = product.Quo(product, big.NewInt(basisPointsScale)).Int64()
	return commission, amount - commission, nil
}
