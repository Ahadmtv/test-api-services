package platform

import (
	"os"
	"strings"
	"testing"
)

// TestOpenAPIContractIncludesStage09AuthOperations verifies the OpenAPI 3.1
// contract documents the authentication and identity surface that stage 09
// audited, so the published contract stays aligned with the router.
func TestOpenAPIContractIncludesStage09AuthOperations(t *testing.T) {
	data, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read OpenAPI contract: %v", err)
	}
	spec := string(data)

	if !strings.Contains(spec, "openapi: 3.1.0") {
		t.Error("OpenAPI contract must declare version 3.1.0")
	}
	for _, needle := range []string{
		"/api/v1/auth/register:",
		"/api/v1/auth/login:",
		"/api/v1/auth/refresh:",
		"/api/v1/auth/logout:",
		"/api/v1/me:",
		"AuthRegisterInput:",
		"AuthLoginInput:",
		"AuthRefreshInput:",
		"TokenPair:",
		"OperationResult:",
		"bearerAuth:",
	} {
		if !strings.Contains(spec, needle) {
			t.Errorf("OpenAPI contract is missing %q", needle)
		}
	}
}
