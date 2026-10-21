package platform

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"
)

const (
	RequestIDKey       = "request_id"
	requestIDHeader    = "X-Request-ID"
	problemContentType = "application/problem+json"

	// PrincipalKey is the Fiber locals key under which the authenticated
	// identity.Principal is stored after successful JWT validation.
	PrincipalKey = "principal"
)

type problemResponse struct {
	Type        string       `json:"type"`
	Title       string       `json:"title"`
	Status      int          `json:"status"`
	Code        string       `json:"code"`
	RequestID   string       `json:"request_id"`
	FieldErrors []FieldError `json:"field_errors,omitempty"`
}

// RequestID ensures every request carries an X-Request-ID, generating one when
// the client did not provide it. The ID is available through
// c.Locals(RequestIDKey) and echoed in the response header.
func RequestID() fiber.Handler {
	return func(c fiber.Ctx) error {
		id := c.Get(requestIDHeader)
		if id == "" {
			id = newRequestID()
		}
		c.Set(requestIDHeader, id)
		c.Locals(RequestIDKey, id)
		return c.Next()
	}
}

// AccessLogger logs every handled request in structured form.
func AccessLogger(logger *slog.Logger) fiber.Handler {
	return func(c fiber.Ctx) error {
		start := time.Now()
		err := c.Next()
		fields := []any{
			"request_id", requestID(c),
			"method", c.Method(),
			"path", c.Path(),
			"status", c.Response().StatusCode(),
			"duration", time.Since(start).String(),
			"client_ip", c.IP(),
		}
		if err != nil {
			fields = append(fields, "error", err)
			logger.Error("request handled with error", fields...)
		} else {
			logger.Info("request handled", fields...)
		}
		return err
	}
}

// Recovery converts panics into a 500 problem response without leaking
// internals to the client.
func Recovery(logger *slog.Logger) fiber.Handler {
	return func(c fiber.Ctx) error {
		defer func() {
			if r := recover(); r != nil {
				err, ok := r.(error)
				if !ok {
					err = fmt.Errorf("%v", r)
				}
				fields := []any{
					"request_id", requestID(c),
					"error", err,
				}
				logger.Error("panic recovered", fields...)
				_ = writeProblem(c, 500, Unexpected(fmt.Errorf("recovered panic: %w", err)))
			}
		}()
		return c.Next()
	}
}

// ErrorHandler converts handler errors into consistent problem+json responses.
func ErrorHandler(logger *slog.Logger) fiber.ErrorHandler {
	return func(c fiber.Ctx, err error) error {
		fields := []any{"request_id", requestID(c)}

		var appErr *AppError
		switch {
		case errors.Is(err, fiber.ErrNotFound):
			appErr = RouteNotFound()
		case errors.Is(err, fiber.ErrMethodNotAllowed):
			appErr = MethodNotAllowed()
		default:
			if !errors.As(err, &appErr) {
				appErr = Unexpected(err)
			}
		}

		if appErr.Status >= 500 || appErr.Err != nil {
			fields = append(fields, "code", appErr.Code, "error", appErr.Err)
			logger.Error("request failed", fields...)
		} else {
			fields = append(fields, "code", appErr.Code)
			logger.Warn("request failed", fields...)
		}

		return writeProblem(c, appErr.Status, appErr)
	}
}

func writeProblem(c fiber.Ctx, status int, appErr *AppError) error {
	body, err := json.Marshal(problemResponse{
		Type:        "about:blank",
		Title:       appErr.Message,
		Status:      status,
		Code:        appErr.Code,
		RequestID:   requestID(c),
		FieldErrors: appErr.FieldErrors,
	})
	if err != nil {
		c.Status(500)
		c.Set("Content-Type", problemContentType)
		return c.SendString(`{"type":"about:blank","title":"خطای داخلی سرور","status":500,"code":"internal_error","request_id":"` + requestID(c) + `"}`)
	}
	c.Status(status)
	c.Set("Content-Type", problemContentType)
	return c.SendString(string(body))
}

func requestID(c fiber.Ctx) string {
	if id, ok := c.Locals(RequestIDKey).(string); ok {
		return id
	}
	return c.Get(requestIDHeader)
}

func newRequestID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
