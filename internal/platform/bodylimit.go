package platform

import (
	"strings"

	"github.com/gofiber/fiber/v3"
)

// BodyLimitGuard keeps the small default request body cap for ordinary routes
// while allowing the explicitly exempted upload routes to receive one image.
// The transport-level cap is raised separately so Fiber can parse the upload.
func BodyLimitGuard(defaultLimit, uploadLimit int, uploadPrefixes ...string) fiber.Handler {
	return func(c fiber.Ctx) error {
		limit := defaultLimit
		exempt := false
		for _, prefix := range uploadPrefixes {
			if strings.HasPrefix(c.Path(), prefix) {
				exempt = true
				limit = uploadLimit
				break
			}
		}

		contentLength := c.Request().Header.ContentLength()
		if contentLength > limit {
			return payloadTooLarge()
		}
		if !exempt && contentLength < 0 && hasRequestBody(c.Method()) {
			return payloadTooLarge()
		}
		return c.Next()
	}
}

func hasRequestBody(method string) bool {
	switch method {
	case fiber.MethodPost, fiber.MethodPut, fiber.MethodPatch:
		return true
	default:
		return false
	}
}

func payloadTooLarge() error {
	return NewAppError("payload_too_large", "حجم درخواست بیش از حد مجاز است", fiber.StatusRequestEntityTooLarge, nil)
}
