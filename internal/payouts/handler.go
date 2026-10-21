package payouts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"

	"test-api/internal/auth"
	"test-api/internal/identity"
	"test-api/internal/platform"
	"test-api/internal/wallet"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) Register(app *fiber.App, accessTokenSecret string, resolvers ...auth.PrincipalResolver) {
	providerGroup := app.Group("/api/v1/provider/payouts")
	providerGroup.Use(auth.Authenticator(accessTokenSecret, resolvers...))
	providerGroup.Use(auth.RequireRoles("provider"))
	providerGroup.Post("/", h.Create)
	providerGroup.Get("/", h.List)

	adminGroup := app.Group("/api/v1/admin/payouts")
	adminGroup.Use(auth.Authenticator(accessTokenSecret, resolvers...))
	adminGroup.Use(auth.RequireRoles("admin"))
	adminGroup.Get("/", h.AdminList)
	adminGroup.Post("/:payoutID/approve", adminAction(h.service.Approve))
	adminGroup.Post("/:payoutID/reject", adminAction(h.service.Reject))
	adminGroup.Post("/:payoutID/processing", adminAction(h.service.StartProcessing))
	adminGroup.Post("/:payoutID/paid", h.MarkPaid)
	adminGroup.Post("/:payoutID/fail", adminAction(h.service.Fail))
}

type createRequest struct {
	AmountIRR *int64 `json:"amount_irr"`
}

func (h *Handler) Create(c fiber.Ctx) error {
	var req createRequest
	decoder := json.NewDecoder(bytes.NewReader(c.Body()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return invalidPayoutRequest()
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return invalidPayoutRequest()
	}
	if req.AmountIRR == nil {
		return invalidPayoutRequest()
	}
	key := strings.TrimSpace(c.Get("Idempotency-Key"))
	requestID, _ := c.Locals(platform.RequestIDKey).(string)
	item, replay, err := h.service.Create(c.Context(), CreateInput{
		ProviderID:     principal(c).UserID,
		AmountIRR:      *req.AmountIRR,
		IdempotencyKey: key,
		RequestID:      requestID,
		Nonce:          requestID,
	})
	if err != nil {
		return payoutError(err)
	}
	status := fiber.StatusCreated
	if replay {
		status = fiber.StatusOK
	}
	return c.Status(status).JSON(item)
}

func (h *Handler) List(c fiber.Ctx) error {
	after, limit, err := pageParams(c)
	if err != nil {
		return err
	}
	items, more, err := h.service.List(c.Context(), principal(c).UserID, after, limit)
	if err != nil {
		return payoutError(err)
	}
	return c.JSON(payoutPage(items, more))
}

// AdminList returns every payout request for administrators, optionally
// filtered by status, with the provider display name resolved.
func (h *Handler) AdminList(c fiber.Ctx) error {
	after, limit, err := pageParams(c)
	if err != nil {
		return err
	}
	status := strings.TrimSpace(c.Query("status"))
	if status != "" && !isValidPayoutStatus(status) {
		return platform.NewAppError("invalid_status", "وضعیت درخواست تسویه نامعتبر است", 400, nil)
	}
	page, err := h.service.ListAll(c.Context(), after, limit, status)
	if err != nil {
		return payoutError(err)
	}
	return c.JSON(page)
}

func pageParams(c fiber.Ctx) (int64, int, error) {
	var after int64
	var err error
	if value := c.Query("cursor"); value != "" {
		after, err = strconv.ParseInt(value, 10, 64)
		if err != nil || after < 0 {
			return 0, 0, invalidPayoutRequest()
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

type payoutAction func(context.Context, int64, int64, string, string) (*Request, error)

type adminRequest struct {
	Note string `json:"note"`
}

func adminAction(action payoutAction) fiber.Handler {
	return func(c fiber.Ctx) error {
		id, err := parsePayoutID(c)
		if err != nil {
			return invalidPayoutRequest()
		}
		note, requestID, err := parseAdminBody(c)
		if err != nil {
			return err
		}
		item, err := action(c.Context(), principal(c).UserID, id, note, requestID)
		if err != nil {
			return payoutError(err)
		}
		return c.JSON(item)
	}
}

func parseAdminBody(c fiber.Ctx) (string, string, error) {
	requestID, _ := c.Locals(platform.RequestIDKey).(string)
	if len(c.Body()) == 0 {
		return "", requestID, nil
	}
	var req adminRequest
	decoder := json.NewDecoder(bytes.NewReader(c.Body()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return "", "", invalidPayoutRequest()
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", "", invalidPayoutRequest()
	}
	note := strings.TrimSpace(req.Note)
	if len(note) > 1000 {
		return "", "", platform.NewAppError("invalid_note", "یادداشت مدیر بیش از اندازه است", 400, nil)
	}
	return note, requestID, nil
}

func (h *Handler) MarkPaid(c fiber.Ctx) error {
	id, err := parsePayoutID(c)
	if err != nil {
		return invalidPayoutRequest()
	}
	var req markPaidRequest
	requestID, _ := c.Locals(platform.RequestIDKey).(string)
	if len(c.Body()) == 0 {
		return invalidPayoutRequest()
	}
	decoder := json.NewDecoder(bytes.NewReader(c.Body()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return invalidPayoutRequest()
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return invalidPayoutRequest()
	}
	reference := strings.TrimSpace(req.ExternalTransferReference)
	if reference == "" || len(reference) > 128 {
		return platform.NewAppError("invalid_external_reference", "ارجاع انتقال بیرونی برای ثبت پرداخت لازم است", 400, nil)
	}
	item, err := h.service.MarkPaid(c.Context(), principal(c).UserID, id, reference, strings.TrimSpace(req.Note), requestID)
	if err != nil {
		return payoutError(err)
	}
	return c.JSON(item)
}

type markPaidRequest struct {
	ExternalTransferReference string `json:"external_transfer_reference"`
	Note                      string `json:"note"`
}

func payoutPage(items []Request, more bool) map[string]any {
	page := map[string]any{"items": items}
	if more && len(items) > 0 {
		page["next_cursor"] = items[len(items)-1].ID
	}
	return page
}

func parsePayoutID(c fiber.Ctx) (int64, error) {
	id, err := strconv.ParseInt(c.Params("payoutID"), 10, 64)
	if err != nil || id < 1 {
		return 0, errors.New("invalid payout id")
	}
	return id, nil
}

func principal(c fiber.Ctx) identity.Principal {
	return c.Locals(platform.PrincipalKey).(identity.Principal)
}

func invalidPayoutRequest() error {
	return platform.NewAppError("invalid_payout_request", "درخواست تسویه نامعتبر است", 400, nil)
}

func payoutError(err error) error {
	switch {
	case errors.Is(err, ErrInvalidInput):
		return invalidPayoutRequest()
	case errors.Is(err, ErrNotFound):
		return platform.NewAppError("payout_not_found", "درخواست تسویه یافت نشد", 404, nil)
	case errors.Is(err, ErrForbidden):
		return platform.NewAppError("forbidden", "دسترسی به این عملیات مجاز نیست", 403, nil)
	case errors.Is(err, ErrAmountTooSmall), errors.Is(err, wallet.ErrInvalidAmount):
		return platform.NewAppError("payout_amount_invalid", "مبلغ درخواست تسویه کمتر از حد مجاز است", 400, nil)
	case errors.Is(err, ErrConflict):
		return platform.NewAppError("payout_conflict", "درخواست تسویه هم‌زمان دیگری فعال است یا تغییر وضعیت مجاز نیست", 409, nil)
	case errors.Is(err, ErrIdempotencyConflict), errors.Is(err, wallet.ErrIdempotencyConflict):
		return platform.NewAppError("idempotency_conflict", "کلید تکرار برای درخواست متفاوت استفاده شده است", 409, nil)
	case errors.Is(err, wallet.ErrInsufficientFunds):
		return platform.NewAppError("insufficient_provider_funds", "درآمد قابل تسویه ارائه‌دهنده کافی نیست", 409, nil)
	default:
		return platform.Unexpected(err)
	}
}
