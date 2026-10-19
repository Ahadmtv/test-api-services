package wallet

import (
	"errors"
	"math"
	"testing"
)

func TestCalculateCommissionUsesIntegerFloorArithmetic(t *testing.T) {
	commission, provider, err := CalculateCommission(25_000, 1250)
	if err != nil {
		t.Fatal(err)
	}
	if commission != 3_125 || provider != 21_875 {
		t.Fatalf("commission=%d provider=%d; want 3125/21875", commission, provider)
	}
	commission, provider, err = CalculateCommission(1, 1)
	if err != nil || commission != 0 || provider != 1 {
		t.Fatalf("fractional commission result=%d/%d error=%v", commission, provider, err)
	}
}

func TestMoneyRejectsInvalidAmountsAndOverflow(t *testing.T) {
	for _, amount := range []int64{0, -1} {
		if err := validateAmount(amount); !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("validateAmount(%d)=%v; want invalid amount", amount, err)
		}
	}
	if _, err := addAmounts(math.MaxInt64, 1); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("overflow add error=%v", err)
	}
	if _, _, err := CalculateCommission(1, -1); !errors.Is(err, ErrInvalidCommissionRate) {
		t.Fatalf("negative commission rate error=%v", err)
	}
	if _, _, err := CalculateCommission(math.MaxInt64, 10_000); err != nil {
		t.Fatalf("maximum valid commission amount error=%v", err)
	}
}
