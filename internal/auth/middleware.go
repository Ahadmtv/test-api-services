package auth

import (
	"context"
	"errors"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"

	"test-api/internal/identity"
	"test-api/internal/platform"
)

// PrincipalResolver loads the caller's current server-side identity. When
// supplied, it replaces the roles embedded in the access token so role changes
// and deactivations take effect immediately.
type PrincipalResolver interface {
	ResolvePrincipal(ctx context.Context, userID int64) (identity.Principal, error)
}

// Authenticator validates a JWT access token and populates the request with the
// authenticated principal. Client-supplied roles/IDs are never trusted.
func Authenticator(accessTokenSecret string, resolvers ...PrincipalResolver) fiber.Handler {
	var resolver PrincipalResolver
	if len(resolvers) > 0 {
		resolver = resolvers[0]
	}
	return func(c fiber.Ctx) error {
		auth := c.Get("Authorization")
		if auth == "" {
			return platform.NewAppError("unauthenticated", "احراز هویت ضروری است", 401, nil)
		}

		parts := strings.SplitN(auth, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return platform.NewAppError("unauthenticated", "فرم اعتبارسنجی نامعتبر است", 401, nil)
		}

		claims, err := identity.ParseAccessToken(parts[1], accessTokenSecret)
		if err != nil {
			code, message := accessTokenError(err)
			return platform.NewAppError(code, message, 401, nil)
		}

		principal := identity.Principal{
			UserID: claims.UserID,
			Email:  claims.Email,
			Roles:  claims.Roles,
		}
		if resolver != nil {
			current, err := resolver.ResolvePrincipal(c.Context(), claims.UserID)
			if err != nil {
				if errors.Is(err, identity.ErrUserNotFound) || errors.Is(err, identity.ErrUserInactive) {
					return platform.NewAppError("unauthenticated", "احراز هویت ضروری است", 401, nil)
				}
				return platform.Unexpected(err)
			}
			principal = current
		}
		if principal.Roles == nil {
			principal.Roles = []string{}
		}

		c.Locals(platform.PrincipalKey, principal)
		return c.Next()
	}
}

func accessTokenError(err error) (code string, message string) {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return "token_expired", "توکن دسترسی منقضی شده است"
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return "invalid_signature", "امضای توکن نامعتبر است"
	case errors.Is(err, jwt.ErrTokenMalformed):
		return "malformed_token", "ساختار توکن نامعتبر است"
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		return "token_not_active", "توکن هنوز فعال نیست"
	default:
		return "invalid_token", "توکن نامعتبر است"
	}
}

// RequireRoles authorizes the request only if the principal holds at least one
// of the given roles. It must be placed after Authenticator.
func RequireRoles(allowed ...string) fiber.Handler {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, r := range allowed {
		allowedSet[r] = struct{}{}
	}

	return func(c fiber.Ctx) error {
		principal, ok := c.Locals(platform.PrincipalKey).(identity.Principal)
		if !ok {
			return platform.NewAppError("unauthenticated", "احراز هویت ضروری است", 401, nil)
		}

		for _, role := range principal.Roles {
			if _, ok := allowedSet[role]; ok {
				return c.Next()
			}
		}

		return platform.NewAppError("forbidden", "دسترسی به این Resource مجاز نیست", 403, nil)
	}
}
