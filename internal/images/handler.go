package images

import (
	"errors"

	"github.com/gofiber/fiber/v3"

	"test-api/internal/auth"
	"test-api/internal/identity"
	"test-api/internal/platform"
)

const multipartOverhead = 64 << 10

type Handler struct {
	service        *Service
	maxUploadBytes int64
}

func NewHandler(service *Service, maxUploadBytes int64) *Handler {
	return &Handler{service: service, maxUploadBytes: maxUploadBytes}
}

func (h *Handler) Register(app *fiber.App, accessTokenSecret string, resolvers ...auth.PrincipalResolver) {
	authenticate := auth.Authenticator(accessTokenSecret, resolvers...)

	app.Get("/api/v1/me/images", authenticate, h.List)
	app.Put("/api/v1/me/images/profile", authenticate, h.UploadProfile)
	app.Delete("/api/v1/me/images/profile", authenticate, h.DeleteProfile)
	app.Put("/api/v1/me/images/cover", authenticate, auth.RequireRoles("provider"), h.UploadCover)
	app.Delete("/api/v1/me/images/cover", authenticate, auth.RequireRoles("provider"), h.DeleteCover)
}

func (h *Handler) List(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	set, err := h.service.List(c.Context(), principal(c).UserID)
	if err != nil {
		return platform.Unexpected(err)
	}
	return c.JSON(set)
}

func (h *Handler) UploadProfile(c fiber.Ctx) error {
	return h.upload(c, PurposeProfile)
}

func (h *Handler) UploadCover(c fiber.Ctx) error {
	return h.upload(c, PurposeCover)
}

func (h *Handler) DeleteProfile(c fiber.Ctx) error {
	return h.delete(c, PurposeProfile)
}

func (h *Handler) DeleteCover(c fiber.Ctx) error {
	return h.delete(c, PurposeCover)
}

func (h *Handler) upload(c fiber.Ctx, purpose Purpose) error {
	if int64(c.Request().Header.ContentLength()) > h.maxUploadBytes+multipartOverhead {
		return imageError(ErrFileTooLarge)
	}
	form, err := c.MultipartForm()
	if err != nil || form == nil || form.File == nil {
		return imageError(ErrInvalidImage)
	}
	files := form.File["file"]
	if len(files) != 1 {
		return imageError(ErrInvalidImage)
	}
	header := files[0]
	if header.Size > h.maxUploadBytes {
		return imageError(ErrFileTooLarge)
	}
	file, err := header.Open()
	if err != nil {
		return imageError(ErrInvalidImage)
	}
	defer file.Close()

	requestID, _ := c.Locals(platform.RequestIDKey).(string)
	result, err := h.service.Upload(c.Context(), principal(c).UserID, purpose, file, header.Header.Get("Content-Type"), requestID)
	if err != nil {
		return imageError(err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	c.Set(fiber.HeaderLocation, result.Image.URL)
	if result.Created {
		return c.Status(fiber.StatusCreated).JSON(result.Image)
	}
	return c.Status(fiber.StatusOK).JSON(result.Image)
}

func (h *Handler) delete(c fiber.Ctx, purpose Purpose) error {
	requestID, _ := c.Locals(platform.RequestIDKey).(string)
	if err := h.service.Delete(c.Context(), principal(c).UserID, purpose, requestID); err != nil {
		return imageError(err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.SendStatus(fiber.StatusNoContent)
}

func principal(c fiber.Ctx) identity.Principal {
	return c.Locals(platform.PrincipalKey).(identity.Principal)
}

func imageError(err error) error {
	switch {
	case errors.Is(err, ErrEmptyFile):
		return platform.NewAppError("empty_image", "فایل تصویر خالی است", fiber.StatusBadRequest, nil)
	case errors.Is(err, ErrFileTooLarge):
		return platform.NewAppError("image_too_large", "حجم فایل تصویر بیش از حد مجاز است", fiber.StatusRequestEntityTooLarge, nil)
	case errors.Is(err, ErrUnsupportedType):
		return platform.NewAppError("unsupported_image_type", "فقط تصاویر JPEG، PNG و WebP پشتیبانی می‌شوند", fiber.StatusUnsupportedMediaType, nil)
	case errors.Is(err, ErrImageTooSmall):
		return platform.NewAppError("image_too_small", "ابعاد تصویر برای این بخش کافی نیست", fiber.StatusUnprocessableEntity, nil)
	case errors.Is(err, ErrInvalidImage):
		return platform.NewAppError("invalid_image", "فایل تصویر معتبر نیست", fiber.StatusBadRequest, nil)
	case errors.Is(err, ErrInvalidPurpose):
		return platform.NewAppError("invalid_image_purpose", "نوع تصویر معتبر نیست", fiber.StatusBadRequest, nil)
	default:
		return platform.Unexpected(err)
	}
}
