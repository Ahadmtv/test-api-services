package payments

import (
	"errors"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"

	"test-api/internal/auth"
	"test-api/internal/identity"
	"test-api/internal/platform"
)

type Handler struct {
	service     *Service
	rateLimiter *auth.RateLimiter
	returnURL   string
}

func NewHandler(service *Service, rateLimiter *auth.RateLimiter, returnURL string) *Handler {
	return &Handler{service: service, rateLimiter: rateLimiter, returnURL: returnURL}
}

func (h *Handler) Register(app *fiber.App, accessTokenSecret string) {
	app.Get("/api/v1/payments/zarinpal/callback", h.Callback)
	group := app.Group("/api/v1/payments")
	group.Use(auth.Authenticator(accessTokenSecret), auth.RequireRoles("client"))
	group.Post("/zarinpal", h.rateLimited(h.Create))
	group.Get("/:paymentID", h.Get)
}

// rateLimited applies a per-IP token bucket to externally-visible payment
// creation so a single client cannot flood the gateway.
func (h *Handler) rateLimited(next fiber.Handler) fiber.Handler {
	return func(c fiber.Ctx) error {
		if h.rateLimiter != nil && !h.rateLimiter.Allow(c.IP(), "payments:create") {
			return platform.NewAppError("rate_limited", "تعداد درخواست‌ها بیش از حد مجاز است", 429, nil)
		}
		return next(c)
	}
}

func (h *Handler) Create(c fiber.Ctx) error {
	var input struct {
		Purpose   string `json:"purpose"`
		AmountIRR *int64 `json:"amount_irr"`
		BookingID *int64 `json:"booking_id"`
	}
	if err := c.Bind().JSON(&input); err != nil {
		return invalidPaymentRequest()
	}
	key := strings.TrimSpace(c.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		return invalidPaymentRequest()
	}
	userID := paymentPrincipal(c).UserID
	var payment Payment
	var redirectURL string
	var err error
	switch input.Purpose {
	case "", PurposeWalletTopUp:
		if input.AmountIRR == nil || *input.AmountIRR < OfficialMinimumAmountIRR || input.BookingID != nil {
			return invalidPaymentRequest()
		}
		payment, redirectURL, err = h.service.Start(c.Context(), userID, *input.AmountIRR, key, requestID(c))
	case PurposeBooking:
		if input.AmountIRR != nil || input.BookingID == nil || *input.BookingID < 1 {
			return invalidPaymentRequest()
		}
		payment, redirectURL, err = h.service.StartBooking(c.Context(), userID, *input.BookingID, key, requestID(c))
	default:
		return invalidPaymentRequest()
	}
	if err != nil {
		return paymentError(err)
	}
	status := fiber.StatusAccepted
	switch payment.Status {
	case "pending":
		status = fiber.StatusCreated
	case "succeeded", "failed":
		status = fiber.StatusOK
	}
	return c.Status(status).JSON(struct {
		Payment     Payment `json:"payment"`
		RedirectURL string  `json:"redirect_url,omitempty"`
	}{Payment: payment, RedirectURL: redirectURL})
}

func (h *Handler) Get(c fiber.Ctx) error {
	paymentID, err := strconv.ParseInt(c.Params("paymentID"), 10, 64)
	if err != nil || paymentID < 1 {
		return invalidPaymentRequest()
	}
	payment, err := h.service.Get(c.Context(), paymentPrincipal(c).UserID, paymentID)
	if err != nil {
		return paymentError(err)
	}
	return c.JSON(payment)
}

func (h *Handler) Callback(c fiber.Ctx) error {
	authority := queryValue(c, "authority", "Authority")
	callbackStatus := queryValue(c, "status", "Status")
	if _, err := h.service.HandleCallback(c.Context(), authority, callbackStatus, requestID(c)); err != nil {
		return paymentError(err)
	}
	if h.returnURL == "" {
		return c.SendStatus(fiber.StatusNoContent)
	}
	return c.Redirect().To(h.returnURL)
}

func queryValue(c fiber.Ctx, names ...string) string {
	for _, name := range names {
		if value := c.Query(name); value != "" {
			return value
		}
	}
	return ""
}

func paymentPrincipal(c fiber.Ctx) identity.Principal {
	return c.Locals(platform.PrincipalKey).(identity.Principal)
}

func requestID(c fiber.Ctx) string {
	if id, ok := c.Locals(platform.RequestIDKey).(string); ok {
		return id
	}
	return ""
}

func invalidPaymentRequest() error {
	return platform.NewAppError("invalid_payment_request", "درخواست پرداخت نامعتبر است", 400, nil)
}

func paymentError(err error) error {
	switch {
	case errors.Is(err, ErrInvalidInput), errors.Is(err, ErrInvalidAuthority):
		return invalidPaymentRequest()
	case errors.Is(err, ErrPaymentNotFound):
		return platform.NewAppError("payment_not_found", "پرداخت پیدا نشد", 404, nil)
	case errors.Is(err, ErrPaymentConflict):
		return platform.NewAppError("payment_conflict", "وضعیت پرداخت امکان این عملیات را نمی‌دهد", 409, nil)
	case errors.Is(err, ErrGatewayUnavailable):
		return platform.NewAppError("payment_provider_unavailable", "درگاه پرداخت در دسترس نیست", 503, err)
	case errors.Is(err, ErrGatewayUnknownOutcome):
		return platform.NewAppError("payment_verification_pending", "نتیجه پرداخت در حال بررسی است", 503, err)
	default:
		return platform.Unexpected(err)
	}
}
