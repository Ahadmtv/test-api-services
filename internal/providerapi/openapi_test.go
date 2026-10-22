package providerapi

import (
	"os"
	"strings"
	"testing"
)

func TestOpenAPI3ContractIncludesBeautyServiceCategory(t *testing.T) {
	data, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read OpenAPI contract: %v", err)
	}
	spec := string(data)
	for _, needle := range []string{
		"    ServiceCategory:\n",
		"        - haircut\n",
		"        - other\n",
		"name: category",
	} {
		if !strings.Contains(spec, needle) {
			t.Errorf("OpenAPI contract is missing beauty category marker %q", needle)
		}
	}
}

func TestOpenAPI3ServiceRequiresProviderDisplayName(t *testing.T) {
	data, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read OpenAPI contract: %v", err)
	}
	spec := string(data)
	for _, needle := range []string{
		"required: [id, provider_id, name, description, category, duration_minutes, price_irr, buffer_minutes, is_active, provider_display_name]",
		"provider_display_name:\n",
		"مقدار خالی یعنی نمایه ارائه‌دهنده هنوز تکمیل نشده است.",
	} {
		if !strings.Contains(spec, needle) {
			t.Errorf("OpenAPI Service contract is missing provider display name marker %q", needle)
		}
	}
}

func TestOpenAPI3ContractIncludesStage03Operations(t *testing.T) {
	data, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read OpenAPI contract: %v", err)
	}
	spec := string(data)
	if !strings.Contains(spec, "openapi: 3.1.0") {
		t.Fatal("OpenAPI contract must use version 3.1.0")
	}
	operations := map[string][]string{
		"/api/v1/services:":                                  {"get"},
		"/api/v1/services/{serviceID}:":                      {"get"},
		"/api/v1/provider/profile:":                          {"get", "put"},
		"/api/v1/provider/services:":                         {"get", "post"},
		"/api/v1/provider/services/{serviceID}:":             {"patch", "delete"},
		"/api/v1/provider/working-hours:":                    {"get", "put"},
		"/api/v1/provider/time-off:":                         {"get", "post"},
		"/api/v1/admin/providers/{providerID}/verification:": {"patch"},
	}
	for route, methods := range operations {
		marker := "  " + route + "\n"
		start := strings.Index(spec, marker)
		if start < 0 {
			t.Errorf("OpenAPI contract is missing route %s", route)
			continue
		}
		start += len(marker)
		end := strings.Index(spec[start:], "\n  /")
		if end < 0 {
			end = strings.Index(spec[start:], "\ncomponents:")
		}
		if end < 0 {
			end = len(spec) - start
		}
		section := spec[start : start+end]
		for _, method := range methods {
			needle := "\n    " + method + ":"
			if !strings.Contains(section, needle) && !strings.HasPrefix(section, "    "+method+":") {
				t.Errorf("OpenAPI route %s is missing %s operation", route, method)
			}
		}
	}
}
