package images

import (
	"os"
	"strings"
	"testing"
)

func TestOpenAPIContractIncludesImageEndpoints(t *testing.T) {
	data, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	spec := strings.ReplaceAll(string(data), "\r\n", "\n")

	requiredNeedles := []string{
		"/api/v1/me/images:",
		"/api/v1/me/images/profile:",
		"/api/v1/me/images/cover:",
		"Image:",
		"ImageSet:",
		"profile_image_url:",
		"cover_image_url:",
		"provider_profile_image_url:",
	}

	for _, needle := range requiredNeedles {
		if !strings.Contains(spec, needle) {
			t.Errorf("openapi.yaml is missing required marker %q", needle)
		}
	}
}
