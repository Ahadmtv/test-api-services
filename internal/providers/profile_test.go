package providers

import "testing"

func TestValidateProfile(t *testing.T) {
	if err := ValidateProfile("Studio", "Asia/Tehran"); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}
	for _, tc := range []struct {
		name, zone string
	}{
		{" ", "Asia/Tehran"},
		{"A", ""},
		{"A", "not/a-zone"},
		{"A", "Local"},
	} {
		if err := ValidateProfile(tc.name, tc.zone); err == nil {
			t.Errorf("ValidateProfile(%q, %q) should fail", tc.name, tc.zone)
		}
	}
}
