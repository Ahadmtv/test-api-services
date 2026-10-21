package platform

import (
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestErrorHandlerMapsRouterNotFound(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler(logger)})
	app.Get("/known", func(c fiber.Ctx) error { return c.SendString("ok") })

	for path, wantStatus := range map[string]int{
		"/unknown":        404,
		"/unknown/nested": 404,
	} {
		resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
		if err != nil {
			t.Fatalf("request %s: %v", path, err)
		}
		if resp.StatusCode != wantStatus {
			t.Fatalf("%s: status = %d, want %d", path, resp.StatusCode, wantStatus)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(resp.Header.Get("Content-Type")) != problemContentType {
			t.Fatalf("%s: content-type = %q, want %q", path, resp.Header.Get("Content-Type"), problemContentType)
		}
		if !errors.Is(fiber.ErrNotFound, fiber.ErrNotFound) {
			t.Fatal("sanity")
		}
		if len(body) == 0 {
			t.Fatalf("%s: empty body", path)
		}
	}
}

func TestErrorHandlerMapsMethodNotAllowed(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler(logger)})
	app.Get("/only-get", func(c fiber.Ctx) error { return c.SendString("ok") })

	resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/only-get", nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != 405 {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != problemContentType {
		t.Fatalf("content-type = %q, want %q", got, problemContentType)
	}
}

func TestUnexpectedErrorStillInternal(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler(logger)})
	app.Get("/boom", func(c fiber.Ctx) error { return errors.New("boom") })

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/boom", nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != 500 {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}
