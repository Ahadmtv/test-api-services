package catalog

import "testing"

func TestValidateService(t *testing.T) {
	if err := Validate("Consultation", 30, 0, 0); err != nil {
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
