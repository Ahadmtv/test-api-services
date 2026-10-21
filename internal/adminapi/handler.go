package adminapi

import (
	"errors"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"

	"test-api/internal/auth"
	"test-api/internal/identity"
	"test-api/internal/platform"
)

// Handler exposes administrator account and role management.
type Handler struct {
	service *identity.Service
}

func NewHandler(service *identity.Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) Register(app *fiber.App, accessTokenSecret string, resolvers ...auth.PrincipalResolver) {
	group := app.Group("/api/v1/admin")
	group.Use(auth.Authenticator(accessTokenSecret, resolvers...))
	group.Use(auth.RequireRoles("admin"))
	group.Get("/users", h.ListUsers)
	group.Put("/users/:userID/roles/:role", h.SetRole)
}

func (h *Handler) ListUsers(c fiber.Ctx) error {
	var after int64
	if raw := c.Query("cursor"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 1 {
			return platform.NewAppError("invalid_cursor", "مکان‌نمای صفحه نامعتبر است", 400, nil)
		}
		after = parsed
	}
	limit := 50
	if raw := c.Query("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			return platform.NewAppError("invalid_limit", "تعداد نتایج باید بین ۱ تا ۱۰۰ باشد", 400, nil)
		}
		limit = parsed
	}
	query := strings.TrimSpace(c.Query("q"))
	if len([]rune(query)) > 100 {
		query = string([]rune(query)[:100])
	}
	page, err := h.service.AdminListUsers(c.Context(), after, limit, query, strings.TrimSpace(c.Query("role")))
	if err != nil {
		return adminError(err)
	}
	return c.Status(fiber.StatusOK).JSON(page)
}

type setRoleRequest struct {
	Granted *bool `json:"granted"`
}

func (h *Handler) SetRole(c fiber.Ctx) error {
	principal, ok := c.Locals(platform.PrincipalKey).(identity.Principal)
	if !ok {
		return platform.NewAppError("unauthenticated", "احراز هویت ضروری است", 401, nil)
	}
	targetID, err := strconv.ParseInt(c.Params("userID"), 10, 64)
	if err != nil || targetID < 1 {
		return platform.NewAppError("invalid_user_id", "شناسه کاربر نامعتبر است", 400, nil)
	}
	var input setRoleRequest
	if err := c.Bind().Body(&input); err != nil {
		return platform.NewAppError("invalid_request", "بدنه درخواست نامعتبر است", 400, nil)
	}
	if input.Granted == nil {
		return platform.NewAppError("invalid_request", "وضعیت نقش الزامی است", 400, nil)
	}
	requestID, _ := c.Locals(platform.RequestIDKey).(string)
	user, err := h.service.AdminSetRole(c.Context(), principal.UserID, targetID, c.Params("role"), *input.Granted, requestID)
	if err != nil {
		return adminError(err)
	}
	return c.Status(fiber.StatusOK).JSON(user)
}

func adminError(err error) error {
	switch {
	case errors.Is(err, identity.ErrInvalidInput):
		return platform.NewAppError("invalid_request", "درخواست نامعتبر است", 400, nil)
	case errors.Is(err, identity.ErrInvalidRole):
		return platform.NewAppError("invalid_role", "نقش انتخاب‌شده مجاز نیست", 400, nil)
	case errors.Is(err, identity.ErrUserNotFound):
		return platform.NewAppError("user_not_found", "کاربر پیدا نشد", 404, nil)
	case errors.Is(err, identity.ErrUserInactive):
		return platform.NewAppError("user_inactive", "حساب کاربر غیرفعال است", 409, nil)
	case errors.Is(err, identity.ErrSelfRoleChange):
		return platform.NewAppError("self_role_change_forbidden", "تغییر نقش حساب خود مجاز نیست", 409, nil)
	case errors.Is(err, identity.ErrLastAdmin):
		return platform.NewAppError("last_admin_protected", "حداقل یک مدیر فعال باید باقی بماند", 409, nil)
	default:
		return platform.Unexpected(err)
	}
}
