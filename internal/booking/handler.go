package booking

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"test-api/internal/auth"
	"test-api/internal/identity"
	"test-api/internal/platform"
	"test-api/internal/wallet"
)

type Handler struct {
	service *Service
	config  Config
}

func NewHandler(service *Service, config Config) *Handler {
	return &Handler{service: service, config: config}
}

func (h *Handler) Register(app *fiber.App, accessTokenSecret string, resolvers ...auth.PrincipalResolver) {
	bookings := app.Group("/api/v1/bookings")
	bookings.Use(auth.Authenticator(accessTokenSecret, resolvers...))
	bookings.Post("/", auth.RequireRoles("client"), h.Create)
	bookings.Get("/", auth.RequireRoles("client"), h.ClientHistory)
	bookings.Get("/:bookingID", auth.RequireRoles("client", "provider"), h.Get)
	bookings.Post("/:bookingID/cancel", auth.RequireRoles("client"), h.Cancel)

	provider := app.Group("/api/v1/provider/bookings")
	provider.Use(auth.Authenticator(accessTokenSecret, resolvers...))
	provider.Use(auth.RequireRoles("provider"))
	provider.Get("/", h.ProviderBookings)
	provider.Post("/:bookingID/start", h.Start)
	provider.Post("/:bookingID/request-completion", h.RequestCompletion)
	provider.Post("/:bookingID/cancel", h.CancelByProvider)

	clientActions := app.Group("/api/v1/bookings")
	clientActions.Use(auth.Authenticator(accessTokenSecret, resolvers...))
	clientActions.Post("/:bookingID/confirm-completion", auth.RequireRoles("client"), h.ConfirmCompletion)
	clientActions.Post("/:bookingID/dispute", auth.RequireRoles("client"), h.Dispute)

	admin := app.Group("/api/v1/admin")
	admin.Use(auth.Authenticator(accessTokenSecret, resolvers...), auth.RequireRoles("admin"))
	admin.Get("/bookings", h.AdminList)
	admin.Post("/bookings/:bookingID/refund", h.Refund)
	admin.Post("/bookings/:bookingID/dispute-resolution", h.ResolveDispute)
}

// adminBookingStatuses enumerates the booking statuses accepted by the
// administrator booking list filter.
var adminBookingStatuses = map[string]bool{
	"pending_payment": true, "pending_provider_acceptance": true,
	"confirmed": true, "in_progress": true, "completion_pending": true,
	"disputed": true, "completed": true, "cancelled": true, "expired": true,
}

// AdminList returns bookings across all users for administrators with
// keyset pagination, status and text filters, and a refundable-only view.
func (h *Handler) AdminList(c fiber.Ctx) error {
	after, limit, err := bookingPageParams(c)
	if err != nil {
		return err
	}
	filter := AdminListFilter{After: after, Limit: limit}
	if status := strings.TrimSpace(c.Query("status")); status != "" {
		if !adminBookingStatuses[status] {
			return platform.NewAppError("invalid_status", "وضعیت رزرو نامعتبر است", 400, nil)
		}
		filter.Status = status
	}
	if query := strings.TrimSpace(c.Query("q")); query != "" {
		if len([]rune(query)) > 100 {
			query = string([]rune(query)[:100])
		}
		filter.Query = query
	}
	switch strings.TrimSpace(c.Query("refundable")) {
	case "":
	case "true":
		filter.RefundableOnly = true
	case "false":
	default:
		return platform.NewAppError("invalid_refundable", "مقدار فیلتر بازپرداخت نامعتبر است", 400, nil)
	}
	page, err := h.service.ListAdmin(c.Context(), filter)
	if err != nil {
		if errors.Is(err, ErrInvalidInput) {
			return invalidBookingRequest()
		}
		return platform.Unexpected(err)
	}
	return c.JSON(page)
}

type createRequest struct {
	ServiceID     int64  `json:"service_id"`
	StartsAt      string `json:"starts_at"`
	PaymentMethod string `json:"payment_method"`
}

type disputeRequest struct {
	Reason string `json:"reason"`
}

type resolveDisputeRequest struct {
	Resolution string `json:"resolution"`
	Note       string `json:"note"`
}

func (h *Handler) Create(c fiber.Ctx) error {
	var req createRequest
	decoder := json.NewDecoder(bytes.NewReader(c.Body()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return invalidBookingRequest()
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return invalidBookingRequest()
	}
	if req.ServiceID < 1 {
		return invalidBookingRequest()
	}
	startsAt, err := time.Parse(time.RFC3339, req.StartsAt)
	if err != nil {
		return invalidBookingRequest()
	}
	key := strings.TrimSpace(c.Get("Idempotency-Key"))
	if len(key) > 128 {
		return invalidBookingRequest()
	}
	requestID, _ := c.Locals(platform.RequestIDKey).(string)
	item, err := h.service.Create(c.Context(), principal(c).UserID, CreateInput{
		ServiceID: req.ServiceID, StartsAt: startsAt, PaymentMethod: strings.TrimSpace(req.PaymentMethod),
		IdempotencyKey: key, RequestID: requestID,
	}, h.config)
	if err != nil {
		return bookingError(err)
	}
	return c.Status(fiber.StatusCreated).JSON(item)
}

func (h *Handler) Get(c fiber.Ctx) error {
	id, err := bookingID(c.Params("bookingID"))
	if err != nil {
		return invalidBookingRequest()
	}
	item, err := h.service.Get(c.Context(), principal(c).UserID, id)
	if err != nil {
		return bookingError(err)
	}
	return c.JSON(item)
}

func (h *Handler) ClientHistory(c fiber.Ctx) error {
	after, limit, err := bookingPageParams(c)
	if err != nil {
		return err
	}
	items, more, err := h.service.ListClient(c.Context(), principal(c).UserID, after, limit)
	if err != nil {
		return platform.Unexpected(err)
	}
	return c.JSON(bookingPage(items, more))
}

func (h *Handler) ProviderBookings(c fiber.Ctx) error {
	after, limit, err := bookingPageParams(c)
	if err != nil {
		return err
	}
	items, more, err := h.service.ListProvider(c.Context(), principal(c).UserID, after, limit)
	if err != nil {
		return platform.Unexpected(err)
	}
	return c.JSON(bookingPage(items, more))
}

func (h *Handler) Start(c fiber.Ctx) error {
	return h.providerTransition(c, h.service.Start)
}

func (h *Handler) RequestCompletion(c fiber.Ctx) error {
	return h.providerTransition(c, h.service.RequestCompletion)
}

func (h *Handler) CancelByProvider(c fiber.Ctx) error {
	return h.providerTransition(c, h.service.CancelByProvider)
}

func (h *Handler) ConfirmCompletion(c fiber.Ctx) error {
	id, err := bookingID(c.Params("bookingID"))
	if err != nil {
		return invalidBookingRequest()
	}
	requestID, _ := c.Locals(platform.RequestIDKey).(string)
	item, err := h.service.ConfirmCompletion(c.Context(), principal(c).UserID, id, requestID)
	if err != nil {
		return bookingError(err)
	}
	return c.JSON(item)
}

func (h *Handler) Dispute(c fiber.Ctx) error {
	id, err := bookingID(c.Params("bookingID"))
	if err != nil {
		return invalidBookingRequest()
	}
	var req disputeRequest
	decoder := json.NewDecoder(bytes.NewReader(c.Body()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return invalidBookingRequest()
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return invalidBookingRequest()
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" || len(reason) > 1000 {
		return platform.NewAppError("invalid_dispute_reason", "شرح اختلاف لازم است", 400, nil)
	}
	requestID, _ := c.Locals(platform.RequestIDKey).(string)
	item, err := h.service.Dispute(c.Context(), principal(c).UserID, id, reason, requestID)
	if err != nil {
		return bookingError(err)
	}
	return c.JSON(item)
}

func (h *Handler) ResolveDispute(c fiber.Ctx) error {
	id, err := bookingID(c.Params("bookingID"))
	if err != nil {
		return invalidBookingRequest()
	}
	var req resolveDisputeRequest
	decoder := json.NewDecoder(bytes.NewReader(c.Body()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return invalidBookingRequest()
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return invalidBookingRequest()
	}
	resolution := DisputeResolution(strings.TrimSpace(req.Resolution))
	if resolution != DisputeResolutionSettle && resolution != DisputeResolutionCancel {
		return invalidBookingRequest()
	}
	note := strings.TrimSpace(req.Note)
	if len(note) > 1000 {
		return invalidBookingRequest()
	}
	requestID, _ := c.Locals(platform.RequestIDKey).(string)
	item, err := h.service.ResolveDispute(c.Context(), principal(c).UserID, id, resolution, note, requestID)
	if err != nil {
		return bookingError(err)
	}
	return c.JSON(item)
}

func (h *Handler) Refund(c fiber.Ctx) error {
	id, err := bookingID(c.Params("bookingID"))
	if err != nil {
		return invalidBookingRequest()
	}
	key := strings.TrimSpace(c.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		return invalidBookingRequest()
	}
	requestID, _ := c.Locals(platform.RequestIDKey).(string)
	if err := h.service.Refund(c.Context(), principal(c).UserID, id, key, requestID); err != nil {
		return bookingError(err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) providerTransition(c fiber.Ctx, transition func(context.Context, int64, int64, string) (*Booking, error)) error {
	id, err := bookingID(c.Params("bookingID"))
	if err != nil {
		return invalidBookingRequest()
	}
	requestID, _ := c.Locals(platform.RequestIDKey).(string)
	item, err := transition(c.Context(), principal(c).UserID, id, requestID)
	if err != nil {
		return bookingError(err)
	}
	return c.JSON(item)
}

func (h *Handler) Cancel(c fiber.Ctx) error {
	id, err := bookingID(c.Params("bookingID"))
	if err != nil {
		return invalidBookingRequest()
	}
	requestID, _ := c.Locals(platform.RequestIDKey).(string)
	item, err := h.service.Cancel(c.Context(), principal(c).UserID, id, h.config.CancellationCutoff, requestID)
	if err != nil {
		return bookingError(err)
	}
	return c.JSON(item)
}

type bookingPageResponse struct {
	Items      []Booking `json:"items"`
	NextCursor *int64    `json:"next_cursor"`
}

func bookingPage(items []Booking, more bool) bookingPageResponse {
	page := bookingPageResponse{Items: items}
	if more && len(items) > 0 {
		cursor := items[len(items)-1].ID
		page.NextCursor = &cursor
	}
	return page
}

func bookingPageParams(c fiber.Ctx) (int64, int, error) {
	var after int64
	var err error
	if value := c.Query("cursor"); value != "" {
		after, err = bookingID(value)
		if err != nil {
			return 0, 0, invalidBookingRequest()
		}
	}
	limit := 50
	if value := c.Query("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 100 {
			return 0, 0, platform.NewAppError("invalid_limit", "اندازه صفحه باید بین ۱ تا ۱۰۰ باشد", 400, nil)
		}
	}
	return after, limit, nil
}

func bookingID(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 1 {
		return 0, errors.New("invalid booking id")
	}
	return id, nil
}

func principal(c fiber.Ctx) identity.Principal {
	return c.Locals(platform.PrincipalKey).(identity.Principal)
}

func invalidBookingRequest() error {
	return platform.NewAppError("invalid_booking_request", "درخواست رزرو نامعتبر است", 400, nil)
}

func bookingError(err error) error {
	switch {
	case errors.Is(err, ErrInvalidInput):
		return invalidBookingRequest()
	case errors.Is(err, ErrServiceUnavailable):
		return platform.NewAppError("service_unavailable", "خدمت برای رزرو در دسترس نیست", 404, nil)
	case errors.Is(err, ErrBookingNotFound):
		return platform.NewAppError("booking_not_found", "رزرو یافت نشد", 404, nil)
	case errors.Is(err, ErrForbidden):
		return platform.NewAppError("forbidden", "دسترسی به این عملیات مجاز نیست", 403, nil)
	case errors.Is(err, ErrSlotUnavailable):
		return platform.NewAppError("booking_conflict", "زمان درخواستی دیگر در دسترس نیست", 409, nil)
	case errors.Is(err, ErrIdempotencyConflict):
		return platform.NewAppError("idempotency_conflict", "کلید تکرار برای درخواست متفاوت استفاده شده است", 409, nil)
	case errors.Is(err, ErrInvalidTransition):
		return platform.NewAppError("invalid_booking_transition", "این تغییر وضعیت رزرو مجاز نیست", 409, nil)
	case errors.Is(err, ErrPaymentMinimum):
		return platform.NewAppError("payment_minimum_not_met", "مبلغ این خدمت کمتر از حداقل مجاز درگاه پرداخت است", 400, nil)
	case errors.Is(err, ErrPaymentAmountMismatch):
		return platform.NewAppError("payment_amount_mismatch", "مبلغ پرداخت با مبلغ رزرو هم‌خوانی ندارد", 409, nil)
	case errors.Is(err, ErrCancellationClosed):
		return platform.NewAppError("cancellation_window_closed", "مهلت لغو رزرو پایان یافته است", 409, nil)
	case errors.Is(err, ErrDisputeWindowClosed):
		return platform.NewAppError("dispute_window_closed", "مهلت ثبت اختلاف پایان یافته است", 409, nil)
	case errors.Is(err, ErrInvalidResolution), errors.Is(err, ErrDisputeNotFound):
		return platform.NewAppError("dispute_not_resolvable", "اختلاف رزرو قابل حل نیست", 409, nil)
	case errors.Is(err, wallet.ErrInsufficientFunds):
		return platform.NewAppError("insufficient_wallet_funds", "موجودی قابل استفاده کیف پول کافی نیست", 409, nil)
	case errors.Is(err, wallet.ErrInvalidInput), errors.Is(err, wallet.ErrInvalidAmount):
		return invalidBookingRequest()
	case errors.Is(err, wallet.ErrIdempotencyConflict):
		return platform.NewAppError("idempotency_conflict", "کلید تکرار برای درخواست متفاوت استفاده شده است", 409, nil)
	case errors.Is(err, wallet.ErrNotReversible), errors.Is(err, wallet.ErrAlreadyReversed):
		return platform.NewAppError("wallet_operation_conflict", "عملیات مالی برای این رزرو قابل بازگشت نیست", 409, nil)
	case errors.Is(err, wallet.ErrTransactionNotFound):
		return platform.NewAppError("wallet_hold_not_found", "قفل مالی رزرو یافت نشد", 404, nil)
	default:
		return platform.Unexpected(err)
	}
}
