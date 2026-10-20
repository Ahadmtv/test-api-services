package auth

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"test-api/internal/identity"
	"test-api/internal/platform"
)

const testAccessTokenSecret = "01234567890123456789012345678901"

type problemBody struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
}

func middlewareTestApp() *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: platform.ErrorHandler(testLogger())})
	app.Get("/protected", Authenticator(testAccessTokenSecret), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})
	app.Get("/provider", Authenticator(testAccessTokenSecret), RequireRoles("provider"), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})
	return app
}

func issueTestToken(t *testing.T, roles []string, ttl time.Duration) string {
	t.Helper()
	token, _, err := identity.IssueAccessToken(7, "user@example.com", roles, identity.AccessTokenConfig{
		Secret: testAccessTokenSecret,
		TTL:    ttl,
	})
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return token
}

func getWithAuth(t *testing.T, app *fiber.App, path, authorization string) (*http.Response, problemBody) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://test.local"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	var problem problemBody
	if resp.StatusCode != http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
			t.Fatalf("decode problem: %v", err)
		}
	}
	resp.Body.Close()
	return resp, problem
}

func TestAuthenticatorRejectsMissingToken(t *testing.T) {
	resp, problem := getWithAuth(t, middlewareTestApp(), "/protected", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if problem.Code != "unauthenticated" {
		t.Errorf("code = %q, want unauthenticated", problem.Code)
	}
}

func TestAuthenticatorRejectsExpiredToken(t *testing.T) {
	token := issueTestToken(t, []string{"client"}, -time.Minute)
	resp, problem := getWithAuth(t, middlewareTestApp(), "/protected", "Bearer "+token)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if problem.Code != "token_expired" {
		t.Errorf("code = %q, want token_expired", problem.Code)
	}
}

func TestAuthenticatorRejectsWrongSignature(t *testing.T) {
	token, _, err := identity.IssueAccessToken(7, "user@example.com", []string{"client"}, identity.AccessTokenConfig{
		Secret: "another-secret-012345678901234567",
		TTL:    time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, problem := getWithAuth(t, middlewareTestApp(), "/protected", "Bearer "+token)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if problem.Code != "invalid_signature" {
		t.Errorf("code = %q, want invalid_signature", problem.Code)
	}
}

func TestAuthenticatorRejectsMalformedToken(t *testing.T) {
	resp, problem := getWithAuth(t, middlewareTestApp(), "/protected", "Bearer not-a-jwt")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if problem.Code != "malformed_token" {
		t.Errorf("code = %q, want malformed_token", problem.Code)
	}
}

func TestRequireRolesAllowsProvider(t *testing.T) {
	token := issueTestToken(t, []string{"client", "provider"}, time.Minute)
	resp, _ := getWithAuth(t, middlewareTestApp(), "/provider", "Bearer "+token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRequireRolesRejectsClient(t *testing.T) {
	token := issueTestToken(t, []string{"client"}, time.Minute)
	resp, problem := getWithAuth(t, middlewareTestApp(), "/provider", "Bearer "+token)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if problem.Code != "forbidden" {
		t.Errorf("code = %q, want forbidden", problem.Code)
	}
}
