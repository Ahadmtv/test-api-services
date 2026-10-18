package platform

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

type stubPinger struct {
	err error
}

func (s stubPinger) Ping(context.Context) error { return s.err }

func getRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, mustURL(path).String(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return req
}

func TestHealthLive(t *testing.T) {
	app := fiber.New()
	app.Use(RequestID())
	app.Get("/health/live", Health{}.Live)

	resp, err := app.Test(getRequest(t, "/health/live"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("live status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != `{"status":"alive"}` {
		t.Fatalf("live body = %s, want {\"status\":\"alive\"}", body)
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("missing X-Request-ID response header")
	}
}

func TestHealthReadyOK(t *testing.T) {
	app := fiber.New()
	app.Get("/health/ready", Health{DB: stubPinger{}}.Ready)

	resp, err := app.Test(getRequest(t, "/health/ready"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("ready status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != `{"status":"ready"}` {
		t.Fatalf("ready body = %s", body)
	}
}

func TestHealthReadyDBDown(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler(logger)})
	app.Use(RequestID())
	app.Use(AccessLogger(logger))
	app.Use(Recovery(logger))
	app.Get("/health/ready", Health{DB: stubPinger{err: context.DeadlineExceeded}}.Ready)

	resp, err := app.Test(getRequest(t, "/health/ready"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 503 {
		t.Fatalf("ready status = %d, want 503", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content-type = %q, want application/problem+json", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"code":"database_unavailable"`) {
		t.Errorf("body does not contain database_unavailable code: %s", body)
	}
}

func mustURL(path string) *url.URL {
	return &url.URL{Scheme: "http", Host: "test.local", Path: path}
}
