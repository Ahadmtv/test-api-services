package images

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	fiberstatic "github.com/gofiber/fiber/v3/middleware/static"

	"test-api/internal/identity"
	"test-api/internal/platform"
)

const testSecret = "01234567890123456789012345678901"

func testToken(t *testing.T, userID int64, roles []string) string {
	t.Helper()
	token, _, err := identity.IssueAccessToken(userID, "user@example.com", roles, identity.AccessTokenConfig{
		Secret: testSecret,
		TTL:    15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return token
}

func setupTestApp(t *testing.T, maxUploadBytes int64) (*fiber.App, *Service, *Storage, *memoryStore) {
	t.Helper()
	tempDir := t.TempDir()
	storage, err := NewStorage(tempDir)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	store := newMemoryStore()
	svc := NewService(ServiceConfig{
		Processor: NewProcessor(defaultTestProcessorConfig()),
		Storage:   storage,
		Store:     store,
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := fiber.New(fiber.Config{
		ErrorHandler: platform.ErrorHandler(logger),
	})

	app.Use(platform.BodyLimitGuard(1024, int(maxUploadBytes+65536), "/api/v1/me/images"))
	app.Use(platform.StaticImagePrefix, fiberstatic.New(storage.Dir(), fiberstatic.Config{
		ModifyResponse: func(c fiber.Ctx) error {
			c.Set("X-Content-Type-Options", "nosniff")
			return nil
		},
	}))

	handler := NewHandler(svc, maxUploadBytes)
	handler.Register(app, testSecret)

	return app, svc, storage, store
}

func createMultipartBody(fieldName, fileName, contentType string, content []byte) (*bytes.Buffer, string) {
	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="` + fieldName + `"; filename="` + fileName + `"`}
	if contentType != "" {
		h["Content-Type"] = []string{contentType}
	}
	part, _ := writer.CreatePart(h)
	_, _ = part.Write(content)
	_ = writer.Close()
	return body, writer.FormDataContentType()
}

func TestHandler_List(t *testing.T) {
	app, _, _, _ := setupTestApp(t, 5<<20)

	// Without auth -> 401
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/images", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}

	// With client token -> 200
	clientToken := testToken(t, 10, []string{"client"})
	req = httptest.NewRequest(http.MethodGet, "/api/v1/me/images", nil)
	req.Header.Set("Authorization", "Bearer "+clientToken)
	resp, err = app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var set Set
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if set.Profile != nil || set.Cover != nil {
		t.Errorf("expected empty set, got %+v", set)
	}
}

func TestHandler_UploadProfile(t *testing.T) {
	app, _, _, _ := setupTestApp(t, 5<<20)
	clientToken := testToken(t, 10, []string{"client"})
	imgData := createTestJPEG(t, 200, 200)

	// 1. Initial upload -> 201 Created
	body, contentType := createMultipartBody("file", "avatar.jpg", "image/jpeg", imgData)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/me/images/profile", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+clientToken)

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" || !strings.HasPrefix(loc, "/static/img/") {
		t.Errorf("expected Location header starting with /static/img/, got %q", loc)
	}

	var uploaded Image
	if err := json.NewDecoder(resp.Body).Decode(&uploaded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if uploaded.URL != loc {
		t.Errorf("uploaded URL %q does not match Location %q", uploaded.URL, loc)
	}

	// 2. Static serving test
	staticReq := httptest.NewRequest(http.MethodGet, loc, nil)
	staticResp, err := app.Test(staticReq)
	if err != nil {
		t.Fatalf("static fetch: %v", err)
	}
	if staticResp.StatusCode != http.StatusOK {
		t.Errorf("static status = %d, want 200", staticResp.StatusCode)
	}
	if staticResp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("expected X-Content-Type-Options: nosniff")
	}

	// 3. Replacement upload -> 200 OK
	imgData2 := createTestJPEG(t, 220, 220)
	body2, contentType2 := createMultipartBody("file", "avatar2.jpg", "image/jpeg", imgData2)
	req2 := httptest.NewRequest(http.MethodPut, "/api/v1/me/images/profile", body2)
	req2.Header.Set("Content-Type", contentType2)
	req2.Header.Set("Authorization", "Bearer "+clientToken)

	resp2, err := app.Test(req2)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 on replacement", resp2.StatusCode)
	}
}

func TestHandler_UploadProfile_Errors(t *testing.T) {
	app, _, _, _ := setupTestApp(t, 5<<20)
	clientToken := testToken(t, 10, []string{"client"})

	// Malformed multipart
	req := httptest.NewRequest(http.MethodPut, "/api/v1/me/images/profile", strings.NewReader("not-multipart"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	req.Header.Set("Authorization", "Bearer "+clientToken)
	resp, err := app.Test(req)
	if err == nil && resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed multipart status = %d, want 400", resp.StatusCode)
	}

	// Wrong field name (e.g. "image" instead of "file")
	body, contentType := createMultipartBody("wrong_field", "avatar.jpg", "image/jpeg", createTestJPEG(t, 200, 200))
	req = httptest.NewRequest(http.MethodPut, "/api/v1/me/images/profile", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+clientToken)
	resp, _ = app.Test(req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("wrong field status = %d, want 400", resp.StatusCode)
	}

	// Unsupported media type
	body, contentType = createMultipartBody("file", "doc.pdf", "application/pdf", []byte("%PDF-1.4 ..."))
	req = httptest.NewRequest(http.MethodPut, "/api/v1/me/images/profile", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+clientToken)
	resp, _ = app.Test(req)
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("unsupported type status = %d, want 415", resp.StatusCode)
	}

	// Image too small
	body, contentType = createMultipartBody("file", "tiny.jpg", "image/jpeg", createTestJPEG(t, 64, 64))
	req = httptest.NewRequest(http.MethodPut, "/api/v1/me/images/profile", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+clientToken)
	resp, _ = app.Test(req)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("too small status = %d, want 422", resp.StatusCode)
	}
}

func TestHandler_DeleteProfile(t *testing.T) {
	app, _, _, _ := setupTestApp(t, 5<<20)
	clientToken := testToken(t, 10, []string{"client"})

	// Delete without auth -> 401
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/me/images/profile", nil)
	resp, _ := app.Test(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}

	// Upload first
	body, contentType := createMultipartBody("file", "avatar.jpg", "image/jpeg", createTestJPEG(t, 200, 200))
	upReq := httptest.NewRequest(http.MethodPut, "/api/v1/me/images/profile", body)
	upReq.Header.Set("Content-Type", contentType)
	upReq.Header.Set("Authorization", "Bearer "+clientToken)
	_, _ = app.Test(upReq)

	// Delete with auth -> 204 No Content
	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/me/images/profile", nil)
	delReq.Header.Set("Authorization", "Bearer "+clientToken)
	delResp, err := app.Test(delReq)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if delResp.StatusCode != http.StatusNoContent {
		t.Errorf("delete status = %d, want 204", delResp.StatusCode)
	}
}

func TestHandler_CoverAuthorization(t *testing.T) {
	app, _, _, _ := setupTestApp(t, 5<<20)
	clientToken := testToken(t, 10, []string{"client"})
	providerToken := testToken(t, 20, []string{"client", "provider"})
	coverImg := createTestJPEG(t, 800, 450)

	// 1. Client attempts cover upload -> 403 Forbidden
	body, contentType := createMultipartBody("file", "cover.jpg", "image/jpeg", coverImg)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/me/images/cover", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+clientToken)
	resp, _ := app.Test(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("client cover upload status = %d, want 403", resp.StatusCode)
	}

	// 2. Client attempts cover delete -> 403 Forbidden
	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/me/images/cover", nil)
	delReq.Header.Set("Authorization", "Bearer "+clientToken)
	delResp, _ := app.Test(delReq)
	if delResp.StatusCode != http.StatusForbidden {
		t.Errorf("client cover delete status = %d, want 403", delResp.StatusCode)
	}

	// 3. Provider attempts cover upload -> 201 Created
	body2, contentType2 := createMultipartBody("file", "cover.jpg", "image/jpeg", coverImg)
	req2 := httptest.NewRequest(http.MethodPut, "/api/v1/me/images/cover", body2)
	req2.Header.Set("Content-Type", contentType2)
	req2.Header.Set("Authorization", "Bearer "+providerToken)
	resp2, err := app.Test(req2)
	if err != nil {
		t.Fatalf("provider cover upload: %v", err)
	}
	if resp2.StatusCode != http.StatusCreated {
		t.Errorf("provider cover upload status = %d, want 201", resp2.StatusCode)
	}

	// 4. Provider attempts cover delete -> 204 No Content
	delReq2 := httptest.NewRequest(http.MethodDelete, "/api/v1/me/images/cover", nil)
	delReq2.Header.Set("Authorization", "Bearer "+providerToken)
	delResp2, err := app.Test(delReq2)
	if err != nil {
		t.Fatalf("provider cover delete: %v", err)
	}
	if delResp2.StatusCode != http.StatusNoContent {
		t.Errorf("provider cover delete status = %d, want 204", delResp2.StatusCode)
	}
}

func TestHandler_ContentLengthTooLarge(t *testing.T) {
	app, _, _, _ := setupTestApp(t, 100) // very small limit: 100 bytes
	clientToken := testToken(t, 10, []string{"client"})

	body := bytes.Repeat([]byte("a"), 200000)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/me/images/profile", bytes.NewReader(body))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	req.Header.Set("Content-Length", "200000")
	req.Header.Set("Authorization", "Bearer "+clientToken)

	resp, _ := app.Test(req)
	if resp != nil && resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

func TestImageErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		code int
	}{
		{ErrEmptyFile, http.StatusBadRequest},
		{ErrFileTooLarge, http.StatusRequestEntityTooLarge},
		{ErrUnsupportedType, http.StatusUnsupportedMediaType},
		{ErrImageTooSmall, http.StatusUnprocessableEntity},
		{ErrInvalidImage, http.StatusBadRequest},
		{ErrInvalidPurpose, http.StatusBadRequest},
		{errors.New("unknown"), http.StatusInternalServerError},
	}

	for _, c := range cases {
		appErr := imageError(c.err)
		var apiErr *platform.AppError
		if errors.As(appErr, &apiErr) {
			if apiErr.Status != c.code {
				t.Errorf("imageError(%v).Status = %d, want %d", c.err, apiErr.Status, c.code)
			}
		} else {
			t.Errorf("expected AppError for %v, got %T", c.err, appErr)
		}
	}
}
