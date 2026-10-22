package providerapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgxpool"

	"test-api/internal/catalog"
	"test-api/internal/identity"
	"test-api/internal/platform"
	"test-api/internal/providers"
	"test-api/internal/scheduling"
)

const integrationSecret = "01234567890123456789012345678901"

func TestStage03APIIntegration(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set; skipping provider API integration test")
	}
	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to database: %v", err)
	}
	defer adminPool.Close()

	schemaBytes := make([]byte, 8)
	if _, err := rand.Read(schemaBytes); err != nil {
		t.Fatalf("generate test schema: %v", err)
	}
	schema := "provider_test_" + hex.EncodeToString(schemaBytes)
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	defer adminPool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")

	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse database URL: %v", err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatalf("connect to test schema: %v", err)
	}
	defer pool.Close()

	migrator, err := platform.NewMigrator(pool, nilLogger())
	if err != nil {
		t.Fatalf("create migrator: %v", err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("migrate test schema: %v", err)
	}

	var ownerID, otherID int64
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ('owner@example.test', 'x') RETURNING id`).Scan(&ownerID); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ('other@example.test', 'x') RETURNING id`).Scan(&otherID); err != nil {
		t.Fatalf("create other provider: %v", err)
	}
	for _, profile := range []struct {
		id   int64
		name string
	}{
		{ownerID, "Owner Provider"},
		{otherID, "Other Provider"},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO provider_profiles (provider_id, display_name, is_active, verification_status) VALUES ($1, $2, TRUE, 'verified')`, profile.id, profile.name); err != nil {
			t.Fatalf("create profile %d: %v", profile.id, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'provider')`, profile.id); err != nil {
			t.Fatalf("grant provider role %d: %v", profile.id, err)
		}
	}
	var ownerServiceID, otherServiceID int64
	if err := pool.QueryRow(ctx, `INSERT INTO services (provider_id, name, category, duration_minutes, price_irr) VALUES ($1, 'Owned service', 'haircut', 30, 10000) RETURNING id`, ownerID).Scan(&ownerServiceID); err != nil {
		t.Fatalf("create owner service: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO services (provider_id, name, category, duration_minutes, price_irr) VALUES ($1, 'Other service', 'nails', 45, 20000) RETURNING id`, otherID).Scan(&otherServiceID); err != nil {
		t.Fatalf("create other service: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TABLE historical_service_snapshot_fixture (
			service_id BIGINT NOT NULL REFERENCES services(id) ON DELETE RESTRICT,
			service_name TEXT NOT NULL,
			duration_minutes INTEGER NOT NULL,
			price_irr BIGINT NOT NULL,
			currency CHAR(3) NOT NULL,
			buffer_minutes INTEGER NOT NULL
		)
	`); err != nil {
		t.Fatalf("create historical snapshot fixture table: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO historical_service_snapshot_fixture
		SELECT id, name, duration_minutes, price_irr, 'IRR', buffer_minutes
		FROM services WHERE id = $1
	`, otherServiceID); err != nil {
		t.Fatalf("create historical snapshot fixture: %v", err)
	}

	app := fiber.New(fiber.Config{ErrorHandler: platform.ErrorHandler(nilLogger())})
	app.Use(platform.RequestID())
	occupancy := scheduling.NewOccupancyStore(pool)
	policy := scheduling.AvailabilityPolicy{
		SlotIncrement:  15 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	NewHandler(providers.NewService(pool), catalog.NewStore(pool), scheduling.NewStore(pool), occupancy, policy).Register(app, integrationSecret)

	ownerToken := integrationToken(t, ownerID)
	resp := doRequest(t, app, http.MethodPut, "/api/v1/provider/profile", ownerToken, `{"display_name":"Missing status","time_zone":"Asia/Tehran"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("profile without is_active status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doRequest(t, app, http.MethodPost, "/api/v1/provider/services", ownerToken, `{"name":"Missing price","duration_minutes":30}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("service without price status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	clientToken := integrationTokenWithRoles(t, otherID, []string{"client"})
	resp = doRequest(t, app, http.MethodGet, "/api/v1/provider/services", clientToken, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("client provider-route status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doRequest(t, app, http.MethodGet, "/api/v1/provider/services", ownerToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list owner services status = %d", resp.StatusCode)
	}
	var page struct {
		Items []catalog.ServiceItem `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("decode service page: %v", err)
	}
	resp.Body.Close()
	if len(page.Items) != 1 || page.Items[0].ID != ownerServiceID {
		t.Fatalf("owner services = %+v, want only service %d", page.Items, ownerServiceID)
	}

	resp = doRequest(t, app, http.MethodPatch, "/api/v1/provider/services/"+jsonNumber(otherServiceID), ownerToken, `{"name":"stolen update"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-owner update status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	otherToken := integrationToken(t, otherID)
	resp = doRequest(t, app, http.MethodPatch, "/api/v1/provider/services/"+jsonNumber(otherServiceID), otherToken, `{"name":"Updated service","category":"makeup","duration_minutes":60,"price_irr":25000,"buffer_minutes":10}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner update status = %d", resp.StatusCode)
	}
	var updated catalog.ServiceItem
	if err := json.NewDecoder(resp.Body).Decode(&updated); err != nil {
		t.Fatalf("decode updated service: %v", err)
	}
	resp.Body.Close()
	if updated.ID != otherServiceID || updated.Name != "Updated service" || updated.Category != catalog.CategoryMakeup || updated.DurationMinutes != 60 || updated.PriceIRR != 25000 || updated.BufferMinutes != 10 {
		t.Fatalf("unexpected updated service: %+v", updated)
	}

	resp = doRequest(t, app, http.MethodGet, "/api/v1/services?category=makeup", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("filter public services by category status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read filtered services: %v", err)
	}
	resp.Body.Close()
	var filtered struct {
		Items []catalog.ServiceItem `json:"items"`
	}
	if err := json.Unmarshal(body, &filtered); err != nil {
		t.Fatalf("decode filtered services: %v", err)
	}
	if len(filtered.Items) != 1 || filtered.Items[0].ID != otherServiceID {
		t.Fatalf("filtered services = %+v, want only service %d", filtered.Items, otherServiceID)
	}
	if filtered.Items[0].ProviderDisplayName != "Other Provider" {
		t.Fatalf("public list provider_display_name = %q, want Other Provider", filtered.Items[0].ProviderDisplayName)
	}
	if filtered.Items[0].Name != "Updated service" {
		t.Fatalf("public list service name = %q, want Updated service", filtered.Items[0].Name)
	}
	assertPublicServiceHidesPrivateFields(t, body)

	resp = doRequest(t, app, http.MethodGet, "/api/v1/services/"+jsonNumber(otherServiceID), "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public service detail status = %d", resp.StatusCode)
	}
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read public service detail: %v", err)
	}
	resp.Body.Close()
	var publicDetail catalog.ServiceItem
	if err := json.Unmarshal(body, &publicDetail); err != nil {
		t.Fatalf("decode public service detail: %v", err)
	}
	if publicDetail.ProviderDisplayName != "Other Provider" {
		t.Fatalf("public detail provider_display_name = %q, want Other Provider", publicDetail.ProviderDisplayName)
	}
	assertPublicServiceHidesPrivateFields(t, body)

	resp = doRequest(t, app, http.MethodGet, "/api/v1/services?category=dermatology", "", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("medical category filter status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	var snapshotName, snapshotCurrency string
	var snapshotDuration, snapshotBuffer int
	var snapshotPrice int64
	if err := pool.QueryRow(ctx, `SELECT service_name, duration_minutes, price_irr, currency, buffer_minutes FROM historical_service_snapshot_fixture WHERE service_id = $1`, otherServiceID).Scan(&snapshotName, &snapshotDuration, &snapshotPrice, &snapshotCurrency, &snapshotBuffer); err != nil {
		t.Fatalf("read historical service snapshot: %v", err)
	}
	if snapshotName != "Other service" || snapshotDuration != 45 || snapshotPrice != 20000 || snapshotCurrency != "IRR" || snapshotBuffer != 0 {
		t.Fatalf("historical service snapshot changed: %q %d %d %s %d", snapshotName, snapshotDuration, snapshotPrice, snapshotCurrency, snapshotBuffer)
	}

	resp = doRequest(t, app, http.MethodPut, "/api/v1/provider/working-hours", ownerToken, `{"intervals":[{"weekday":1,"opens_at":"09:00","closes_at":"12:00"},{"weekday":1,"opens_at":"13:00","closes_at":"17:00"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("working-hours update status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	var intervals []scheduling.WorkingInterval
	rows, err := pool.Query(ctx, `SELECT weekday, to_char(opens_at, 'HH24:MI'), to_char(closes_at, 'HH24:MI') FROM weekly_working_hours WHERE provider_id = $1 ORDER BY opens_at`, ownerID)
	if err != nil {
		t.Fatalf("list stored hours: %v", err)
	}
	for rows.Next() {
		var interval scheduling.WorkingInterval
		if err := rows.Scan(&interval.Weekday, &interval.OpensAt, &interval.ClosesAt); err != nil {
			t.Fatalf("scan stored hours: %v", err)
		}
		intervals = append(intervals, interval)
	}
	rows.Close()
	if len(intervals) != 2 {
		t.Fatalf("stored intervals = %d, want 2", len(intervals))
	}
	resp = doRequest(t, app, http.MethodGet, "/api/v1/provider/working-hours", otherToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("other provider working-hours status = %d", resp.StatusCode)
	}
	var otherHours struct {
		Intervals []scheduling.WorkingInterval `json:"intervals"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&otherHours); err != nil {
		t.Fatalf("decode other provider hours: %v", err)
	}
	resp.Body.Close()
	if len(otherHours.Intervals) != 0 {
		t.Fatalf("other provider can see working hours: %+v", otherHours.Intervals)
	}

	resp = doRequest(t, app, http.MethodPut, "/api/v1/provider/working-hours", ownerToken, `{"intervals":[{"weekday":1,"opens_at":"22:00","closes_at":"02:00"}]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("overnight interval status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doRequest(t, app, http.MethodPost, "/api/v1/provider/time-off", ownerToken, `{"kind":"blocked","starts_at":"2030-03-20T09:00:00+03:30","ends_at":"2030-03-20T10:00:00+03:30","reason":"private"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create time-off status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doRequest(t, app, http.MethodGet, "/api/v1/provider/time-off", otherToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list other time-off status = %d", resp.StatusCode)
	}
	var timeOffPage struct {
		Items []scheduling.TimeOff `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&timeOffPage); err != nil {
		t.Fatalf("decode other time-off: %v", err)
	}
	resp.Body.Close()
	if len(timeOffPage.Items) != 0 {
		t.Fatalf("other provider can see time-off records: %+v", timeOffPage.Items)
	}

	resp = doRequest(t, app, http.MethodDelete, "/api/v1/provider/services/"+jsonNumber(otherServiceID), otherToken, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("service deactivation status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	var active bool
	if err := pool.QueryRow(ctx, `SELECT is_active FROM services WHERE id = $1`, otherServiceID).Scan(&active); err != nil {
		t.Fatalf("check soft deactivation: %v", err)
	}
	if active {
		t.Fatal("service should be deactivated without deleting its row")
	}
	var storedServiceCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM services WHERE id = $1`, otherServiceID).Scan(&storedServiceCount); err != nil {
		t.Fatalf("check service retained after deactivation: %v", err)
	}
	if storedServiceCount != 1 {
		t.Fatal("soft deletion must preserve the service row for historical references")
	}

	resp = doRequest(t, app, http.MethodPatch, "/api/v1/admin/providers/"+jsonNumber(otherID)+"/verification", ownerToken, `{"verification_status":"rejected"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("provider verification attempt status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
	adminToken := integrationTokenWithRoles(t, ownerID, []string{"admin"})
	resp = doRequest(t, app, http.MethodPatch, "/api/v1/admin/providers/"+jsonNumber(otherID)+"/verification", adminToken, `{"verification_status":"rejected"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin verification status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE actor_user_id = $1 AND target_id = $2`, ownerID, otherID).Scan(&auditCount); err != nil {
		t.Fatalf("check verification audit: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("verification audit rows = %d, want 1", auditCount)
	}
}

func TestAdminProviderListAPI(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set; skipping provider API integration test")
	}
	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to database: %v", err)
	}
	defer adminPool.Close()

	schemaBytes := make([]byte, 8)
	if _, err := rand.Read(schemaBytes); err != nil {
		t.Fatalf("generate test schema: %v", err)
	}
	schema := "provider_admin_test_" + hex.EncodeToString(schemaBytes)
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	defer adminPool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")

	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse database URL: %v", err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatalf("connect to test schema: %v", err)
	}
	defer pool.Close()

	migrator, err := platform.NewMigrator(pool, nilLogger())
	if err != nil {
		t.Fatalf("create migrator: %v", err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("migrate test schema: %v", err)
	}

	var p1ID, p2ID, clientID, incompleteID int64
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ('first@example.test', 'x') RETURNING id`).Scan(&p1ID); err != nil {
		t.Fatalf("create first provider: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ('second@example.test', 'x') RETURNING id`).Scan(&p2ID); err != nil {
		t.Fatalf("create second provider: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ('client@example.test', 'x') RETURNING id`).Scan(&clientID); err != nil {
		t.Fatalf("create client: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ('incomplete@example.test', 'x') RETURNING id`).Scan(&incompleteID); err != nil {
		t.Fatalf("create incomplete provider: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'provider')`, incompleteID); err != nil {
		t.Fatalf("grant provider role: %v", err)
	}
	for _, row := range []struct {
		id     int64
		name   string
		status string
	}{
		{p1ID, "Alpha Salon", "pending"},
		{p2ID, "Beta Studio", "verified"},
		{incompleteID, "", "pending"},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO provider_profiles (provider_id, display_name, is_active, verification_status) VALUES ($1, $2, TRUE, $3)`, row.id, row.name, row.status); err != nil {
			t.Fatalf("create profile %d: %v", row.id, err)
		}
	}

	app := fiber.New(fiber.Config{ErrorHandler: platform.ErrorHandler(nilLogger())})
	app.Use(platform.RequestID())
	occupancy := scheduling.NewOccupancyStore(pool)
	policy := scheduling.AvailabilityPolicy{SlotIncrement: 15 * time.Minute, MinimumNotice: 2 * time.Hour, MaximumHorizon: 90 * 24 * time.Hour}
	NewHandler(providers.NewService(pool), catalog.NewStore(pool), scheduling.NewStore(pool), occupancy, policy).Register(app, integrationSecret)

	adminToken := integrationTokenWithRoles(t, p1ID, []string{"admin"})
	clientToken := integrationTokenWithRoles(t, clientID, []string{"client"})

	type adminProviderItem struct {
		ProviderID         int64  `json:"provider_id"`
		DisplayName        string `json:"display_name"`
		VerificationStatus string `json:"verification_status"`
		ProfileCompleted   bool   `json:"profile_completed"`
		OwnerEmail         string `json:"owner_email"`
	}
	type adminProviderPage struct {
		Items      []adminProviderItem `json:"items"`
		NextCursor *int64              `json:"next_cursor"`
	}

	resp := doRequest(t, app, http.MethodGet, "/api/v1/admin/providers", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous admin provider list status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doRequest(t, app, http.MethodGet, "/api/v1/admin/providers", clientToken, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("client admin provider list status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doRequest(t, app, http.MethodGet, "/api/v1/admin/providers?verification_status=bogus", adminToken, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid verification filter status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doRequest(t, app, http.MethodGet, "/api/v1/admin/providers", adminToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin provider list status = %d, want 200", resp.StatusCode)
	}
	var page adminProviderPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("decode admin provider page: %v", err)
	}
	resp.Body.Close()
	if page.NextCursor != nil {
		t.Fatalf("three profiles cannot produce next cursor, got %d", *page.NextCursor)
	}
	if len(page.Items) != 3 {
		t.Fatalf("admin sees %d providers, want 3", len(page.Items))
	}
	if page.Items[0].OwnerEmail != "first@example.test" || page.Items[0].VerificationStatus != "pending" || !page.Items[0].ProfileCompleted {
		t.Fatalf("admin provider item[0]=%+v", page.Items[0])
	}
	if page.Items[1].DisplayName != "Beta Studio" || page.Items[1].VerificationStatus != "verified" {
		t.Fatalf("admin provider item[1]=%+v", page.Items[1])
	}
	if page.Items[2].DisplayName != "" || page.Items[2].ProfileCompleted || page.Items[2].OwnerEmail != "incomplete@example.test" {
		t.Fatalf("incomplete provider item[2]=%+v, want empty name with email fallback and profile_completed=false", page.Items[2])
	}

	patchBody := func(status string) string { return `{"verification_status":"` + status + `"}` }
	resp = doRequest(t, app, http.MethodPatch, "/api/v1/admin/providers/"+jsonNumber(incompleteID)+"/verification", adminToken, patchBody("verified"))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("verify incomplete profile status = %d, want 409", resp.StatusCode)
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		t.Fatalf("decode verify problem: %v", err)
	}
	resp.Body.Close()
	if problem.Code != "profile_incomplete" {
		t.Fatalf("verify incomplete problem code = %q, want profile_incomplete", problem.Code)
	}
	resp = doRequest(t, app, http.MethodPatch, "/api/v1/admin/providers/"+jsonNumber(incompleteID)+"/verification", adminToken, patchBody("pending"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pending verification on incomplete profile status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doRequest(t, app, http.MethodGet, "/api/v1/admin/providers?verification_status=verified", adminToken, "")
	var verified adminProviderPage
	if err := json.NewDecoder(resp.Body).Decode(&verified); err != nil {
		t.Fatalf("decode verified filter page: %v", err)
	}
	resp.Body.Close()
	if len(verified.Items) != 1 || verified.Items[0].ProviderID != p2ID {
		t.Fatalf("verified filter returned %+v, want only provider %d", verified.Items, p2ID)
	}

	resp = doRequest(t, app, http.MethodGet, "/api/v1/admin/providers?q=beta", adminToken, "")
	var byName adminProviderPage
	if err := json.NewDecoder(resp.Body).Decode(&byName); err != nil {
		t.Fatalf("decode name filter page: %v", err)
	}
	resp.Body.Close()
	if len(byName.Items) != 1 || byName.Items[0].ProviderID != p2ID {
		t.Fatalf("name filter returned %+v, want only provider %d", byName.Items, p2ID)
	}

	resp = doRequest(t, app, http.MethodGet, "/api/v1/admin/providers?limit=1", adminToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("limited admin provider list status = %d, want 200", resp.StatusCode)
	}
	var limited adminProviderPage
	if err := json.NewDecoder(resp.Body).Decode(&limited); err != nil {
		t.Fatalf("decode limited page: %v", err)
	}
	resp.Body.Close()
	if len(limited.Items) != 1 || limited.Items[0].ProviderID != p1ID || limited.NextCursor == nil || *limited.NextCursor != p1ID {
		t.Fatalf("limited page=%+v next=%v, want first item with its own cursor", limited.Items, limited.NextCursor)
	}

	var serviceID int64
	if err := pool.QueryRow(ctx, `INSERT INTO services (provider_id, name, category, duration_minutes, price_irr) VALUES ($1, 'Alpha service', 'haircut', 30, 10000) RETURNING id`, p1ID).Scan(&serviceID); err != nil {
		t.Fatalf("create provider service: %v", err)
	}
	resp = doRequest(t, app, http.MethodGet, "/api/v1/provider/services", integrationToken(t, p1ID), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("provider services list status = %d, want 200", resp.StatusCode)
	}
	var services struct {
		Items []catalog.ServiceItem `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&services); err != nil {
		t.Fatalf("decode provider services: %v", err)
	}
	resp.Body.Close()
	if len(services.Items) != 1 || services.Items[0].ID != serviceID || services.Items[0].ProviderDisplayName != "Alpha Salon" {
		t.Fatalf("provider services=%+v, want service %d with display name of provider", services.Items, serviceID)
	}
}

func integrationToken(t *testing.T, userID int64) string {
	return integrationTokenWithRoles(t, userID, []string{"provider"})
}

func integrationTokenWithRoles(t *testing.T, userID int64, roles []string) string {
	t.Helper()
	token, _, err := identity.IssueAccessToken(userID, "provider@example.test", roles, identity.AccessTokenConfig{Secret: integrationSecret, TTL: time.Hour})
	if err != nil {
		t.Fatalf("issue integration token: %v", err)
	}
	return token
}

func doRequest(t *testing.T, app *fiber.App, method, path, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, "http://test.local"+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	return resp
}

func jsonNumber(value int64) string {
	return strconv.FormatInt(value, 10)
}

func nilLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func assertPublicServiceHidesPrivateFields(t *testing.T, body []byte) {
	t.Helper()
	payload := string(body)
	for _, field := range []string{"owner_email", "owner_phone", "email", "phone", "password_hash"} {
		if strings.Contains(payload, `"`+field+`"`) {
			t.Errorf("public service response exposes private field %q: %s", field, payload)
		}
	}
}
