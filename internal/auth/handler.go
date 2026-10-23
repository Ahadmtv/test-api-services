package auth

import (
	"context"
	"errors"
	"regexp"

	"github.com/gofiber/fiber/v3"
	"log/slog"

	"test-api/internal/identity"
	"test-api/internal/platform"
)

var emailRegexp = regexp.MustCompile(`^[a-zA-Z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)

// serviceInterface is the subset of identity.Service used by the HTTP layer.
type serviceInterface interface {
	Register(ctx context.Context, email, phone, password string) (*identity.TokenPair, error)
	Login(ctx context.Context, email, password string) (*identity.TokenPair, error)
	Refresh(ctx context.Context, refreshToken string) (*identity.TokenPair, error)
	Logout(ctx context.Context, refreshToken string) error
	Me(ctx context.Context, userID int64) (*identity.User, error)
}

// Config for the auth HTTP layer.
type Config struct {
	Service        serviceInterface
	Logger         *slog.Logger
	RateLimiter    *RateLimiter
	AccessTokenTTL int64
}

// Handler routes authentication requests.
type Handler struct {
	service     serviceInterface
	logger      *slog.Logger
	rateLimiter *RateLimiter
	accessTTL   int64
}

// NewHandler builds an auth handler.
func NewHandler(service serviceInterface, logger *slog.Logger, rateLimiter *RateLimiter, accessTokenTTL int64) *Handler {
	return &Handler{
		service:     service,
		logger:      logger,
		rateLimiter: rateLimiter,
		accessTTL:   accessTokenTTL,
	}
}

type registerRequest struct {
	Email    string `json:"email"`
	Phone    string `json:"phone,omitempty"`
	Password string `json:"password"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type logoutRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type tokenPairResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

type meResponse struct {
	ID              int64    `json:"id"`
	Email           string   `json:"email"`
	Phone           *string  `json:"phone,omitempty"`
	IsActive        bool     `json:"is_active"`
	Roles           []string `json:"roles"`
	ProfileImageURL string   `json:"profile_image_url"`
}

// Register godoc
func (h *Handler) Register(c fiber.Ctx) error {
	if h.rateLimiter != nil {
		if !h.rateLimiter.Allow(c.IP(), "auth:register") {
			return platform.NewAppError("rate_limited", "تعداد درخواست‌ها بیش از حد مجاز است", 429, nil)
		}
	}

	var req registerRequest
	if err := c.Bind().Body(&req); err != nil {
		return platform.NewAppError("invalid_request", "بدنه درخواست نامعتبر است", 400, err)
	}

	if req.Email == "" || !isValidEmail(req.Email) {
		return platform.NewAppError("invalid_email", "ایمیل نامعتبر است", 400, nil)
	}
	if req.Password == "" || len(req.Password) < 8 {
		return platform.NewAppError("invalid_password", "رمز عبور باید حداقل ۸ کاراکتر باشد", 400, nil)
	}

	_, err := h.service.Register(c.Context(), req.Email, req.Phone, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrUserAlreadyExists):
			// Return the same generic error to avoid account enumeration.
			return platform.NewAppError("invalid_credentials", "ایمیل یا رمز عبور نامعتبر است", 401, nil)
		default:
			return platform.Unexpected(err)
		}
	}

	return c.Status(201).Type("application/json").JSON(map[string]string{
		"status":  "created",
		"message": "حساب کاربری با موفقیت ایجاد شد",
	})
}

// Login godoc
func (h *Handler) Login(c fiber.Ctx) error {
	if h.rateLimiter != nil {
		if !h.rateLimiter.Allow(c.IP(), "auth:login") {
			return platform.NewAppError("rate_limited", "تعداد درخواست‌ها بیش از حد مجاز است", 429, nil)
		}
	}

	var req loginRequest
	if err := c.Bind().Body(&req); err != nil {
		return platform.NewAppError("invalid_request", "بدنه درخواست نامعتبر است", 400, err)
	}

	pair, err := h.service.Login(c.Context(), req.Email, req.Password)
	if err != nil {
		// Same generic error for unknown user / wrong password / inactive.
		return platform.NewAppError("invalid_credentials", "ایمیل یا رمز عبور نامعتبر است", 401, nil)
	}

	return c.Status(200).Type("application/json").JSON(tokenPairResponse{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		ExpiresIn:    h.accessTTL,
	})
}

// Refresh godoc
func (h *Handler) Refresh(c fiber.Ctx) error {
	if h.rateLimiter != nil {
		if !h.rateLimiter.Allow(c.IP(), "auth:refresh") {
			return platform.NewAppError("rate_limited", "تعداد درخواست‌ها بیش از حد مجاز است", 429, nil)
		}
	}

	var req refreshRequest
	if err := c.Bind().Body(&req); err != nil {
		return platform.NewAppError("invalid_request", "بدنه درخواست نامعتبر است", 400, err)
	}
	if req.RefreshToken == "" {
		return platform.NewAppError("invalid_request", "توکنRefresh الزامی است", 400, nil)
	}

	pair, err := h.service.Refresh(c.Context(), req.RefreshToken)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrTokenReplayDetected):
			return platform.NewAppError("token_replay", "ورود مجدد شناسایی شد، کلیه جلسات لغو شد", 401, nil)
		default:
			return platform.NewAppError("invalid_credentials", "توکنRefresh نامعتبر است", 401, nil)
		}
	}

	return c.Status(200).Type("application/json").JSON(tokenPairResponse{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		ExpiresIn:    h.accessTTL,
	})
}

// Logout godoc
func (h *Handler) Logout(c fiber.Ctx) error {
	var req logoutRequest
	if err := c.Bind().Body(&req); err != nil {
		return platform.NewAppError("invalid_request", "بدنه درخواست نامعتبر است", 400, err)
	}
	if req.RefreshToken != "" {
		_ = h.service.Logout(c.Context(), req.RefreshToken)
	}
	return c.Status(200).Type("application/json").JSON(map[string]string{
		"status":  "ok",
		"message": "خروج با موفقیت انجام شد",
	})
}

// Me godoc
func (h *Handler) Me(c fiber.Ctx) error {
	principal, ok := c.Locals(platform.PrincipalKey).(identity.Principal)
	if !ok {
		return platform.NewAppError("unauthenticated", "احراز هویت ضروری است", 401, nil)
	}

	user, err := h.service.Me(c.Context(), principal.UserID)
	if err != nil {
		return platform.Unexpected(err)
	}

	return c.Status(200).Type("application/json").JSON(meResponse{
		ID:              user.ID,
		Email:           user.Email,
		Phone:           user.Phone,
		IsActive:        user.IsActive,
		Roles:           user.Roles,
		ProfileImageURL: user.ProfileImageURL,
	})
}

func isValidEmail(email string) bool {
	if len(email) < 3 || len(email) > 254 {
		return false
	}
	return emailRegexp.MatchString(email)
}
