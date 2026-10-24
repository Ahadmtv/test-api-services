package auth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"

	"test-api/internal/identity"
	"test-api/internal/platform"
)

func setupTestHandler(t *testing.T) (*mockService, *fiber.App) {
	t.Helper()
	ms := newMockService()
	rateLimiter := NewRateLimiter(1000)
	h := NewHandler(ms, testLogger(), rateLimiter, 900)

	app := fiber.New(fiber.Config{ErrorHandler: platform.ErrorHandler(testLogger())})
	app.Post("/register", h.Register)
	app.Post("/login", h.Login)
	app.Post("/refresh", h.Refresh)
	app.Post("/logout", h.Logout)

	return ms, app
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func postJSON(t *testing.T, app *fiber.App, path string, body any) (*http.Response, error) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, "http://test.local"+path, io.NopCloser(bytesReaderData(data)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(data))
	return app.Test(req)
}

type bytesReader struct {
	b   []byte
	pos int
}

func bytesReaderData(b []byte) *bytesReader { return &bytesReader{b: b} }

func (r *bytesReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}

func TestRegisterSuccess(t *testing.T) {
	_, app := setupTestHandler(t)

	resp, err := postJSON(t, app, "/register", map[string]string{
		"email":    "new@example.com",
		"phone":    "+989120000000",
		"password": "secret1234",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
}

func TestRegisterValidation(t *testing.T) {
	_, app := setupTestHandler(t)

	resp, err := postJSON(t, app, "/register", map[string]string{
		"email":    "not-an-email",
		"password": "short",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestLoginSuccess(t *testing.T) {
	ms, app := setupTestHandler(t)
	ms.Register(context.Background(), "user@example.com", "+989120000000", "secret1234")
	ms.addRefreshToken("refresh-user@example.com")

	resp, err := postJSON(t, app, "/login", map[string]string{
		"email":    "user@example.com",
		"password": "secret1234",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.AccessToken == "" || result.RefreshToken == "" {
		t.Fatal("expected tokens")
	}
}

func TestLoginWrongPassword(t *testing.T) {
	ms, app := setupTestHandler(t)
	ms.Register(context.Background(), "user@example.com", "+989120000000", "secret1234")

	resp, err := postJSON(t, app, "/login", map[string]string{
		"email":    "user@example.com",
		"password": "wrongpassword",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestRefresh(t *testing.T) {
	ms, app := setupTestHandler(t)
	ms.Register(context.Background(), "user@example.com", "+989120000000", "secret1234")
	ms.addRefreshToken("refresh-user@example.com")

	resp, err := postJSON(t, app, "/refresh", map[string]string{
		"refresh_token": "refresh-user@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !contains(string(body), "access-new") {
		t.Errorf("expected rotated token, got %s", body)
	}
}

func TestRefreshInProgress(t *testing.T) {
	ms, app := setupTestHandler(t)
	ms.refreshErr = identity.ErrRefreshInProgress

	resp, err := postJSON(t, app, "/refresh", map[string]string{
		"refresh_token": "refresh-user@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var problem problemBody
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "refresh_in_progress" {
		t.Errorf("code = %q, want refresh_in_progress", problem.Code)
	}
}

func TestRefreshUnknownToken(t *testing.T) {
	_, app := setupTestHandler(t)

	resp, err := postJSON(t, app, "/refresh", map[string]string{
		"refresh_token": "unknown-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestMeUnauthenticated(t *testing.T) {
	ms, _ := setupTestHandler(t)

	app := fiber.New(fiber.Config{ErrorHandler: platform.ErrorHandler(testLogger())})
	h := NewHandler(ms, testLogger(), NewRateLimiter(1000), 900)
	app.Get("/me", h.Me)

	req, err := http.NewRequest(http.MethodGet, "http://test.local/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestMeAuthenticated(t *testing.T) {
	ms, _ := setupTestHandler(t)
	ms.Register(context.Background(), "user@example.com", "+989120000000", "secret1234")

	app := fiber.New(fiber.Config{ErrorHandler: platform.ErrorHandler(testLogger())})
	app.Use(func(c fiber.Ctx) error {
		c.Locals(platform.PrincipalKey, identity.Principal{
			UserID: 1,
			Email:  "user@example.com",
			Roles:  []string{"client"},
		})
		return c.Next()
	})
	h := NewHandler(ms, testLogger(), NewRateLimiter(1000), 900)
	app.Get("/me", h.Me)

	req, err := http.NewRequest(http.MethodGet, "http://test.local/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !contains(string(body), "user@example.com") {
		t.Errorf("expected email in body, got %s", body)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
