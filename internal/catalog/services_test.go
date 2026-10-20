package catalog

import "testing"

func TestValidateService(t *testing.T) {
	if err := Validate("کوتاهی مو", 30, 0, 0); err != nil {
		t.Fatalf("valid service rejected: %v", err)
	}
	for _, tc := range []struct {
		name     string
		duration int
		price    int64
		buffer   int
	}{
		{" ", 30, 100, 0},
		{"Long", 0, 100, 0},
		{"Long", 1441, 100, 0},
		{"Long", 30, -1, 0},
		{"Long", 30, 100, -1},
		{"Long", 30, 100, 1441},
	} {
		if err := Validate(tc.name, tc.duration, tc.price, tc.buffer); err == nil {
			t.Errorf("Validate(%q, %d, %d, %d) should fail", tc.name, tc.duration, tc.price, tc.buffer)
		}
	}
}

func TestNormalizeCategory(t *testing.T) {
	for _, category := range serviceCategories {
		normalized, err := NormalizeCategory(category)
		if err != nil || normalized != category {
			t.Fatalf("NormalizeCategory(%q) = %q, %v", category, normalized, err)
		}
	}
	if normalized, err := NormalizeCategory(""); err != nil || normalized != CategoryOther {
		t.Fatalf("empty category = %q, %v; want %q", normalized, err, CategoryOther)
	}
	for _, category := range []string{"dermatology", "dentistry", "internal-medicine", "hair cut"} {
		if _, err := NormalizeCategory(category); err == nil {
			t.Fatalf("medical or unknown category %q must be rejected", category)
		}
	}
}
