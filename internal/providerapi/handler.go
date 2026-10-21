package providerapi

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgconn"

	"test-api/internal/auth"
	"test-api/internal/catalog"
	"test-api/internal/identity"
	"test-api/internal/platform"
	"test-api/internal/providers"
	"test-api/internal/scheduling"
)

type Handler struct {
	profiles  *providers.Service
	services  *catalog.Store
	schedule  *scheduling.Store
	occupancy scheduling.OccupancyReader
	policy    scheduling.AvailabilityPolicy
}

func NewHandler(profiles *providers.Service, services *catalog.Store, schedule *scheduling.Store, occupancy scheduling.OccupancyReader, policy scheduling.AvailabilityPolicy) *Handler {
	return &Handler{profiles: profiles, services: services, schedule: schedule, occupancy: occupancy, policy: policy}
}

func (h *Handler) Register(app *fiber.App, accessTokenSecret string, resolvers ...auth.PrincipalResolver) {
	app.Get("/api/v1/providers/:providerID/availability", h.GetAvailability)
	app.Get("/api/v1/services", h.ListPublicServices)
	app.Get("/api/v1/services/:serviceID", h.GetPublicService)

	provider := app.Group("/api/v1/provider")
	provider.Use(authenticator(accessTokenSecret, resolvers...))
	provider.Use(requireProviderRole())
	provider.Get("/profile", h.GetProfile)
	provider.Put("/profile", h.PutProfile)
	provider.Post("/services", h.CreateService)
	provider.Get("/services", h.ListOwnedServices)
	provider.Patch("/services/:serviceID", h.UpdateService)
	provider.Delete("/services/:serviceID", h.DeleteService)
	provider.Get("/working-hours", h.GetWorkingHours)
	provider.Put("/working-hours", h.PutWorkingHours)
	provider.Post("/time-off", h.CreateTimeOff)
	provider.Get("/time-off", h.ListTimeOff)

	admin := app.Group("/api/v1/admin")
	admin.Use(authenticator(accessTokenSecret, resolvers...))
	admin.Use(auth.RequireRoles("admin"))
	admin.Get("/providers", h.AdminListProviders)
	admin.Patch("/providers/:providerID/verification", h.SetVerificationStatus)
}

// AdminListProviders returns every provider profile for administrators with
// an optional verification_status filter and a text search.
func (h *Handler) AdminListProviders(c fiber.Ctx) error {
	after, limit, err := pageParams(c)
	if err != nil {
		return err
	}
	status := strings.TrimSpace(c.Query("verification_status"))
	if status != "" && status != "pending" && status != "verified" && status != "rejected" {
		return platform.NewAppError("invalid_verification_status", "وضعیت تأیید نامعتبر است", 400, nil)
	}
	query := strings.TrimSpace(c.Query("q"))
	if len([]rune(query)) > 100 {
		query = string([]rune(query)[:100])
	}
	page, err := h.profiles.AdminList(c.Context(), after, limit, status, query)
	if err != nil {
		if errors.Is(err, providers.ErrInvalidProfile) {
			return invalidRequest()
		}
		return platform.Unexpected(err)
	}
	return c.JSON(page)
}

func (h *Handler) GetAvailability(c fiber.Ctx) error {
	providerID, err := positiveID(c.Params("providerID"))
	if err != nil {
		return invalidID()
	}
	serviceID, err := positiveID(c.Query("service_id"))
	if err != nil {
		return platform.NewAppError("invalid_service_id", "شناسه خدمت نامعتبر است", 400, nil)
	}
	date := c.Query("date")
	if _, err := time.Parse("2006-01-02", date); err != nil || len(date) != len("2006-01-02") {
		return platform.NewAppError("invalid_date", "تاریخ باید به قالب میلادی YYYY-MM-DD باشد", 400, nil)
	}
	profile, err := h.profiles.Get(c.Context(), providerID)
	if err != nil {
		return profileError(err)
	}
	if !profile.IsActive || profile.VerificationStatus != "verified" {
		return platform.NewAppError("provider_unavailable", "ارائه‌دهنده در دسترس نیست", 404, nil)
	}
	service, err := h.services.GetPublicForProvider(c.Context(), providerID, serviceID)
	if err != nil {
		return catalogError(err)
	}
	dayRange, err := scheduling.LocalDayRange(date, profile.TimeZone)
	if err != nil {
		return availabilityError(err)
	}
	workingHours, err := h.schedule.GetWorkingHours(c.Context(), providerID)
	if err != nil {
		return platform.Unexpected(err)
	}
	blocked, err := h.schedule.ListAvailabilityIntervals(c.Context(), providerID, dayRange.StartAt, dayRange.EndAt)
	if err != nil {
		return platform.Unexpected(err)
	}
	occupied, err := h.occupancy.ListOccupied(c.Context(), providerID, dayRange.StartAt, dayRange.EndAt)
	if err != nil {
		return platform.Unexpected(err)
	}
	slots, err := scheduling.CalculateAvailability(date, profile.TimeZone, workingHours, blocked, occupied, service.DurationMinutes, service.BufferMinutes, h.policy, time.Now().UTC())
	if err != nil {
		return availabilityError(err)
	}
	return c.JSON(map[string]any{"provider_id": providerID, "service_id": serviceID, "date": date, "time_zone": profile.TimeZone, "slots": slots})
}

type profileRequest struct {
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	TimeZone    string `json:"time_zone"`
	IsActive    *bool  `json:"is_active"`
}

func (h *Handler) GetProfile(c fiber.Ctx) error {
	profile, err := h.profiles.Get(c.Context(), principal(c).UserID)
	if err != nil {
		return profileError(err)
	}
	return c.JSON(profile)
}

func (h *Handler) PutProfile(c fiber.Ctx) error {
	var req profileRequest
	if err := c.Bind().Body(&req); err != nil || req.IsActive == nil {
		return invalidRequest()
	}
	timeZone := req.TimeZone
	if timeZone == "" {
		timeZone = "Asia/Tehran"
	}
	profile, err := h.profiles.Put(c.Context(), principal(c).UserID, req.DisplayName, req.Description, timeZone, *req.IsActive)
	if err != nil {
		return profileError(err)
	}
	return c.JSON(profile)
}

func (h *Handler) SetVerificationStatus(c fiber.Ctx) error {
	providerID, err := positiveID(c.Params("providerID"))
	if err != nil {
		return invalidID()
	}
	var req struct {
		Status string `json:"verification_status"`
	}
	if err := c.Bind().Body(&req); err != nil {
		return invalidRequest()
	}
	caller := principal(c)
	requestID, _ := c.Locals(platform.RequestIDKey).(string)
	profile, err := h.profiles.SetVerificationStatus(c.Context(), caller.UserID, providerID, req.Status, requestID)
	if err != nil {
		return profileError(err)
	}
	return c.JSON(profile)
}

type serviceRequest struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	Category        string `json:"category"`
	DurationMinutes int    `json:"duration_minutes"`
	PriceIRR        *int64 `json:"price_irr"`
	BufferMinutes   int    `json:"buffer_minutes"`
	IsActive        *bool  `json:"is_active"`
}

func (h *Handler) CreateService(c fiber.Ctx) error {
	var req serviceRequest
	if err := c.Bind().Body(&req); err != nil || req.PriceIRR == nil {
		return invalidRequest()
	}
	active := true
	if req.IsActive != nil {
		active = *req.IsActive
	}
	item, err := h.services.Create(c.Context(), principal(c).UserID, catalog.CreateInput{
		Name: req.Name, Description: req.Description, Category: req.Category, DurationMinutes: req.DurationMinutes,
		PriceIRR: *req.PriceIRR, BufferMinutes: req.BufferMinutes, IsActive: active,
	})
	if err != nil {
		return catalogError(err)
	}
	return c.Status(fiber.StatusCreated).JSON(item)
}

func (h *Handler) ListOwnedServices(c fiber.Ctx) error {
	after, limit, err := pageParams(c)
	if err != nil {
		return err
	}
	items, more, err := h.services.ListOwned(c.Context(), principal(c).UserID, after, limit)
	if err != nil {
		return platform.Unexpected(err)
	}
	return c.JSON(makePage(items, more, func(item catalog.ServiceItem) int64 { return item.ID }))
}

func (h *Handler) ListPublicServices(c fiber.Ctx) error {
	after, limit, err := pageParams(c)
	if err != nil {
		return err
	}
	category := strings.TrimSpace(c.Query("category"))
	if category != "" && !catalog.IsValidCategory(category) {
		return invalidRequest()
	}
	items, more, err := h.services.ListPublic(c.Context(), category, after, limit)
	if err != nil {
		return platform.Unexpected(err)
	}
	return c.JSON(makePage(items, more, func(item catalog.ServiceItem) int64 { return item.ID }))
}

func (h *Handler) GetPublicService(c fiber.Ctx) error {
	id, err := positiveID(c.Params("serviceID"))
	if err != nil {
		return invalidID()
	}
	item, err := h.services.GetPublic(c.Context(), id)
	if err != nil {
		return catalogError(err)
	}
	return c.JSON(item)
}

type servicePatch struct {
	Name            *string `json:"name"`
	Description     *string `json:"description"`
	Category        *string `json:"category"`
	DurationMinutes *int    `json:"duration_minutes"`
	PriceIRR        *int64  `json:"price_irr"`
	BufferMinutes   *int    `json:"buffer_minutes"`
	IsActive        *bool   `json:"is_active"`
}

func (h *Handler) UpdateService(c fiber.Ctx) error {
	id, err := positiveID(c.Params("serviceID"))
	if err != nil {
		return invalidID()
	}
	var req servicePatch
	if err := c.Bind().Body(&req); err != nil {
		return invalidRequest()
	}
	if req.Name == nil && req.Description == nil && req.Category == nil && req.DurationMinutes == nil && req.PriceIRR == nil && req.BufferMinutes == nil && req.IsActive == nil {
		return invalidRequest()
	}
	item, err := h.services.Update(c.Context(), principal(c).UserID, id, catalog.UpdateInput{
		Name: req.Name, Description: req.Description, Category: req.Category, DurationMinutes: req.DurationMinutes,
		PriceIRR: req.PriceIRR, BufferMinutes: req.BufferMinutes, IsActive: req.IsActive,
	})
	if err != nil {
		return catalogError(err)
	}
	return c.JSON(item)
}

func (h *Handler) DeleteService(c fiber.Ctx) error {
	id, err := positiveID(c.Params("serviceID"))
	if err != nil {
		return invalidID()
	}
	if err := h.services.Deactivate(c.Context(), principal(c).UserID, id); err != nil {
		return catalogError(err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handler) GetWorkingHours(c fiber.Ctx) error {
	items, err := h.schedule.GetWorkingHours(c.Context(), principal(c).UserID)
	if err != nil {
		return platform.Unexpected(err)
	}
	return c.JSON(map[string]any{"intervals": items})
}

func (h *Handler) PutWorkingHours(c fiber.Ctx) error {
	var req struct {
		Intervals []scheduling.WorkingInterval `json:"intervals"`
	}
	if err := c.Bind().Body(&req); err != nil || req.Intervals == nil {
		return invalidRequest()
	}
	if err := h.schedule.PutWorkingHours(c.Context(), principal(c).UserID, req.Intervals); err != nil {
		return schedulingError(err)
	}
	return h.GetWorkingHours(c)
}

type timeOffRequest struct {
	Kind        string  `json:"kind"`
	StartsAt    string  `json:"starts_at"`
	EndsAt      string  `json:"ends_at"`
	Reason      string  `json:"reason"`
	HolidayDate *string `json:"holiday_date"`
}

func (h *Handler) CreateTimeOff(c fiber.Ctx) error {
	var req timeOffRequest
	if err := c.Bind().Body(&req); err != nil {
		return invalidRequest()
	}
	if req.Kind == "holiday" {
		if req.HolidayDate == nil || req.StartsAt != "" || req.EndsAt != "" || req.Reason != "" {
			return invalidRequest()
		}
		item, err := h.schedule.CreateHoliday(c.Context(), principal(c).UserID, *req.HolidayDate)
		if err != nil {
			return schedulingError(err)
		}
		return c.Status(fiber.StatusCreated).JSON(item)
	}
	if req.HolidayDate != nil {
		return invalidRequest()
	}
	startsAt, err := parseRFC3339(req.StartsAt)
	if err != nil {
		return timeOffInvalid()
	}
	endsAt, err := parseRFC3339(req.EndsAt)
	if err != nil {
		return timeOffInvalid()
	}
	item, err := h.schedule.CreateTimeOff(c.Context(), principal(c).UserID, req.Kind, startsAt, endsAt, req.Reason)
	if err != nil {
		return schedulingError(err)
	}
	return c.Status(fiber.StatusCreated).JSON(item)
}

func (h *Handler) ListTimeOff(c fiber.Ctx) error {
	after, limit, err := pageParams(c)
	if err != nil {
		return err
	}
	items, more, err := h.schedule.ListTimeOff(c.Context(), principal(c).UserID, after, limit)
	if err != nil {
		return platform.Unexpected(err)
	}
	return c.JSON(makePage(items, more, func(item scheduling.TimeOff) int64 { return item.ID }))
}

type pageResponse[T any] struct {
	Items      []T    `json:"items"`
	NextCursor *int64 `json:"next_cursor"`
}

func makePage[T any](items []T, more bool, lastID func(T) int64) pageResponse[T] {
	page := pageResponse[T]{Items: items}
	if more && len(items) > 0 {
		cursor := lastID(items[len(items)-1])
		page.NextCursor = &cursor
	}
	return page
}

func pageParams(c fiber.Ctx) (int64, int, error) {
	var after int64
	var err error
	if value := c.Query("cursor"); value != "" {
		after, err = positiveID(value)
		if err != nil {
			return 0, 0, platform.NewAppError("invalid_cursor", "نشانگر صفحه نامعتبر است", 400, nil)
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

func positiveID(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 1 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

func parseRFC3339(value string) (time.Time, error) {
	return time.Parse(time.RFC3339, value)
}

func authenticator(secret string, resolvers ...auth.PrincipalResolver) fiber.Handler {
	return auth.Authenticator(secret, resolvers...)
}

func requireProviderRole() fiber.Handler {
	return auth.RequireRoles("provider")
}

func principal(c fiber.Ctx) identity.Principal {
	return c.Locals(platform.PrincipalKey).(identity.Principal)
}

func availabilityError(err error) error {
	switch {
	case errors.Is(err, scheduling.ErrInvalidLocalDate):
		return platform.NewAppError("invalid_date", "تاریخ محلی نامعتبر است", 400, nil)
	case errors.Is(err, scheduling.ErrInvalidTimeZone):
		return platform.NewAppError("invalid_provider_time_zone", "منطقه زمانی ارائه‌دهنده نامعتبر است", 409, nil)
	case errors.Is(err, scheduling.ErrInvalidAvailability):
		return platform.NewAppError("invalid_availability_configuration", "پیکربندی زمان‌بندی ارائه‌دهنده نامعتبر است", 409, nil)
	default:
		return platform.Unexpected(err)
	}
}

func profileError(err error) error {
	switch {
	case errors.Is(err, providers.ErrProfileNotFound):
		return platform.NewAppError("provider_profile_not_found", "نمایه ارائه‌دهنده یافت نشد", 404, nil)
	case errors.Is(err, providers.ErrInvalidProfile):
		return platform.NewAppError("invalid_provider_profile", "اطلاعات نمایه ارائه‌دهنده نامعتبر است", 400, nil)
	default:
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "provider_profiles_time_zone_booking_guard" {
			return platform.NewAppError("schedule_conflict", "منطقه زمانی هنگام وجود رزرو فعال قابل تغییر نیست", 409, nil)
		}
		return platform.Unexpected(err)
	}
}

func catalogError(err error) error {
	switch {
	case errors.Is(err, catalog.ErrServiceNotFound):
		return platform.NewAppError("service_not_found", "خدمت یافت نشد", 404, nil)
	case errors.Is(err, catalog.ErrInvalidService):
		return platform.NewAppError("invalid_service", "اطلاعات خدمت نامعتبر است", 400, nil)
	default:
		return databaseConflictError(err)
	}
}

func schedulingError(err error) error {
	switch {
	case errors.Is(err, scheduling.ErrInvalidSchedule):
		return platform.NewAppError("invalid_working_hours", "بازه ساعات کاری نامعتبر است", 400, nil)
	case errors.Is(err, scheduling.ErrProviderNotFound):
		return platform.NewAppError("provider_profile_not_found", "پیش از تنظیم برنامه، نمایه ارائه‌دهنده را ثبت کنید", 404, nil)
	case errors.Is(err, scheduling.ErrInvalidTimeOff):
		return timeOffInvalid()
	case errors.Is(err, scheduling.ErrScheduleConflict):
		return platform.NewAppError("schedule_conflict", "تغییر برنامه با رزرو فعال تداخل دارد", 409, nil)
	case errors.Is(err, scheduling.ErrInvalidLocalDate):
		return platform.NewAppError("invalid_holiday_date", "تاریخ محلی تعطیلی نامعتبر است", 400, nil)
	default:
		return databaseConflictError(err)
	}
}

func databaseConflictError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23P01", "23505":
			return platform.NewAppError("interval_conflict", "بازه با پیکربندی موجود هم‌پوشانی دارد", 409, nil)
		case "23503":
			return platform.NewAppError("provider_profile_required", "ابتدا نمایه ارائه‌دهنده را ثبت کنید", 409, nil)
		case "23514", "22007", "22008":
			return platform.NewAppError("invalid_configuration", "پیکربندی نامعتبر است", 400, nil)
		}
	}
	return platform.Unexpected(err)
}

func invalidRequest() error {
	return platform.NewAppError("invalid_request", "بدنه درخواست نامعتبر است", 400, nil)
}

func invalidID() error {
	return platform.NewAppError("invalid_id", "شناسه نامعتبر است", 400, nil)
}

func timeOffInvalid() error {
	return platform.NewAppError("invalid_time_off", "بازه باید معتبر و پایان آن پس از آغاز باشد", 400, nil)
}
