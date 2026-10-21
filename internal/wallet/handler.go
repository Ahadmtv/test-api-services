package wallet

import (
	"errors"
	"strconv"

	"github.com/gofiber/fiber/v3"

	"test-api/internal/auth"
	"test-api/internal/identity"
	"test-api/internal/platform"
)

type Handler struct{ service *Service }

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) Register(app *fiber.App, accessTokenSecret string, resolvers ...auth.PrincipalResolver) {
	group := app.Group("/api/v1/wallet")
	group.Use(auth.Authenticator(accessTokenSecret, resolvers...), auth.RequireRoles("client", "provider"))
	group.Get("/", h.Balance)
	group.Get("/transactions", h.Transactions)
}
func (h *Handler) Balance(c fiber.Ctx) error {
	item, err := h.service.Balance(c.Context(), walletPrincipal(c).UserID)
	if err != nil {
		return walletError(err)
	}
	return c.JSON(item)
}
func (h *Handler) Transactions(c fiber.Ctx) error {
	var after int64
	var err error
	if raw := c.Query("cursor"); raw != "" {
		after, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || after < 1 {
			return invalidWalletRequest()
		}
	}
	limit := 50
	if raw := c.Query("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			return invalidWalletRequest()
		}
	}
	items, more, err := h.service.Transactions(c.Context(), walletPrincipal(c).UserID, after, limit)
	if err != nil {
		return walletError(err)
	}
	response := struct {
		Items      []Transaction `json:"items"`
		NextCursor *int64        `json:"next_cursor"`
	}{Items: items}
	if more && len(items) > 0 {
		cursor := items[len(items)-1].ID
		response.NextCursor = &cursor
	}
	return c.JSON(response)
}
func walletPrincipal(c fiber.Ctx) identity.Principal {
	return c.Locals(platform.PrincipalKey).(identity.Principal)
}
func invalidWalletRequest() error {
	return platform.NewAppError("invalid_wallet_request", "درخواست کیف پول نامعتبر است", 400, nil)
}
func walletError(err error) error {
	if errors.Is(err, ErrInvalidInput) {
		return invalidWalletRequest()
	}
	return platform.Unexpected(err)
}
