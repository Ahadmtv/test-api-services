package booking

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"test-api/internal/catalog"
	"test-api/internal/identity"
	"test-api/internal/platform"
	"test-api/internal/providers"
	"test-api/internal/scheduling"
	"test-api/internal/wallet"
)

const integrationSecret = "01234567890123456789012345678901"

type bookingFixture struct {
	pool          *pgxpool.Pool
	service       *Service
	providerID    int64
	otherProvider int64
	clientID      int64
	otherClientID int64
	serviceID     int64
	otherService  int64
	config        Config
}

func newBookingFixture(t *testing.T) *bookingFixture {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set; skipping PostgreSQL booking integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to PostgreSQL: %v", err)
	}
	t.Cleanup(adminPool.Close)
	schemaBytes := make([]byte, 8)
	if _, err := rand.Read(schemaBytes); err != nil {
		t.Fatalf("generate schema name: %v", err)
	}
	schema := "booking_test_" + hex.EncodeToString(schemaBytes)
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() { _, _ = adminPool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatalf("connect to test schema: %v", err)
	}
	t.Cleanup(pool.Close)
	migrator, err := platform.NewMigrator(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("create migrator: %v", err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	f := &bookingFixture{pool: pool}
	for _, user := range []*int64{&f.clientID, &f.otherClientID, &f.providerID, &f.otherProvider} {
		if err := pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ($1, 'test') RETURNING id`, randomEmail(t)).Scan(user); err != nil {
			t.Fatalf("create fixture user: %v", err)
		}
	}
	for _, providerID := range []int64{f.providerID, f.otherProvider} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO provider_profiles (provider_id, display_name, is_active, verification_status)
			VALUES ($1, 'Provider', TRUE, 'verified')
		`, providerID); err != nil {
			t.Fatalf("create provider profile: %v", err)
		}
		for weekday := 1; weekday <= 7; weekday++ {
			if _, err := pool.Exec(ctx, `
				INSERT INTO weekly_working_hours (provider_id, weekday, opens_at, closes_at)
				VALUES ($1, $2, '00:00', '23:59')
			`, providerID, weekday); err != nil {
				t.Fatalf("create provider hours: %v", err)
			}
		}
	}
	if err := pool.QueryRow(ctx, `INSERT INTO services (provider_id, name, duration_minutes, price_irr) VALUES ($1, 'Service', 30, 25000) RETURNING id`, f.providerID).Scan(&f.serviceID); err != nil {
		t.Fatalf("create service: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO services (provider_id, name, duration_minutes, price_irr) VALUES ($1, 'Other provider service', 30, 30000) RETURNING id`, f.otherProvider).Scan(&f.otherService); err != nil {
		t.Fatalf("create second service: %v", err)
	}
	walletService := wallet.NewService(pool)
	for _, clientID := range []int64{f.clientID, f.otherClientID} {
		if err := walletService.FundForDevelopment(ctx, clientID, 1_000_000, "fixture-"+strconv.FormatInt(clientID, 10)); err != nil {
			t.Fatalf("fund fixture wallet: %v", err)
		}
	}
	f.service = NewService(pool, providers.NewService(pool), catalog.NewStore(pool), scheduling.NewStore(pool), Config{
		Availability: scheduling.AvailabilityPolicy{
			SlotIncrement: 15 * time.Minute, MinimumNotice: 0, MaximumHorizon: 90 * 24 * time.Hour,
		},
		CompletionDisputeWindow: 24 * time.Hour,
	}, walletService)
	f.config = Config{
		Availability: scheduling.AvailabilityPolicy{
			SlotIncrement: 15 * time.Minute, MinimumNotice: 0, MaximumHorizon: 90 * 24 * time.Hour,
		},
	}
	return f
}

func TestConcurrentOverlappingBookingRequests(t *testing.T) {
	for attempt := 0; attempt < 3; attempt++ {
		t.Run(time.Now().UTC().Format("150405.000000000"), func(t *testing.T) {
			f := newBookingFixture(t)
			start := nextBookingStart()
			const count = 10
			var wg sync.WaitGroup
			var mu sync.Mutex
			succeeded, conflicts := 0, 0
			unexpected := make([]error, 0)
			for i := 0; i < count; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := f.service.Create(context.Background(), f.clientID, CreateInput{ServiceID: f.serviceID, StartsAt: start}, f.config)
					mu.Lock()
					defer mu.Unlock()
					if err == nil {
						succeeded++
					} else if errors.Is(err, ErrSlotUnavailable) {
						conflicts++
					} else {
						unexpected = append(unexpected, err)
					}
				}()
			}
			wg.Wait()
			if len(unexpected) > 0 {
				t.Fatalf("unexpected booking errors: %v", unexpected)
			}
			if succeeded != 1 || conflicts != count-1 {
				t.Fatalf("successes=%d conflicts=%d; want 1 and %d", succeeded, conflicts, count-1)
			}
			var total int
			if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM bookings WHERE provider_id = $1`, f.providerID).Scan(&total); err != nil {
				t.Fatalf("count bookings: %v", err)
			}
			if total != 1 {
				t.Fatalf("stored overlapping bookings=%d, want 1", total)
			}
		})
	}
}

func TestDatabaseExclusionConstraintRejectsOverlap(t *testing.T) {
	f := newBookingFixture(t)
	start := nextBookingStart()
	created, err := f.service.Create(context.Background(), f.clientID, CreateInput{ServiceID: f.serviceID, StartsAt: start}, f.config)
	if err != nil {
		t.Fatalf("create first booking: %v", err)
	}
	_, err = f.pool.Exec(context.Background(), `
		INSERT INTO bookings (client_id, provider_id, service_id, service_name, starts_at, ends_at, duration_minutes, price_irr, buffer_minutes)
		VALUES ($1, $2, $3, 'Service', $4, $5, 30, 25000, 0)
	`, f.otherClientID, f.providerID, f.serviceID, start.Add(time.Minute), start.Add(31*time.Minute))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23P01" || pgErr.ConstraintName != "bookings_no_overlapping_active_provider_slots" {
		t.Fatalf("overlap insert error = %v; want exclusion constraint violation", err)
	}
	if created.Status != "confirmed" {
		t.Fatalf("initial status = %s, want confirmed", created.Status)
	}
}

func TestInstantBookingMigrationCancelsExistingPendingRows(t *testing.T) {
	f := newBookingFixture(t)
	ctx := context.Background()
	var pendingBookingID int64
	for _, migration := range []string{"0006_instant_confirmed_bookings.down.sql", "0006_instant_confirmed_bookings.up.sql"} {
		sql, err := os.ReadFile("../../migrations/" + migration)
		if err != nil {
			t.Fatalf("read migration %s: %v", migration, err)
		}
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin migration %s: %v", migration, err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("apply migration %s: %v", migration, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit migration %s: %v", migration, err)
		}
		if migration == "0006_instant_confirmed_bookings.down.sql" {
			start := nextBookingStart()
			if err := f.pool.QueryRow(ctx, `
				INSERT INTO bookings (client_id, provider_id, service_id, service_name, starts_at, ends_at, duration_minutes, price_irr, buffer_minutes, status, expires_at)
				VALUES ($1, $2, $3, 'Service', $4, $5, 30, 25000, 0, 'pending_provider_acceptance', now() + interval '15 minutes')
				RETURNING id
			`, f.clientID, f.providerID, f.serviceID, start, start.Add(30*time.Minute)).Scan(&pendingBookingID); err != nil {
				t.Fatalf("insert legacy pending booking: %v", err)
			}
		}
	}
	var status, fromStatus, toStatus, actorType string
	var legacyExpirationIsNull bool
	if err := f.pool.QueryRow(ctx, `
		SELECT b.status, b.legacy_expires_at IS NULL, t.from_status, t.to_status, t.actor_type
		FROM bookings b JOIN booking_state_transitions t ON t.booking_id = b.id
		WHERE b.id = $1 AND t.from_status = 'pending_provider_acceptance'
	`, pendingBookingID).Scan(&status, &legacyExpirationIsNull, &fromStatus, &toStatus, &actorType); err != nil {
		t.Fatalf("read migrated pending booking: %v", err)
	}
	if status != "cancelled" || legacyExpirationIsNull || fromStatus != "pending_provider_acceptance" || toStatus != "cancelled" || actorType != "system" {
		t.Fatalf("migration result status=%s legacy expiry null=%t transition=%s->%s actor=%s", status, legacyExpirationIsNull, fromStatus, toStatus, actorType)
	}
}

func TestAdjacentAndDifferentProviderBookings(t *testing.T) {
	f := newBookingFixture(t)
	start := nextBookingStart()
	first, err := f.service.Create(context.Background(), f.clientID, CreateInput{ServiceID: f.serviceID, StartsAt: start}, f.config)
	if err != nil {
		t.Fatalf("create first adjacent booking: %v", err)
	}
	if _, err := f.service.Create(context.Background(), f.otherClientID, CreateInput{ServiceID: f.serviceID, StartsAt: first.EndsAt}, f.config); err != nil {
		t.Fatalf("create adjacent booking: %v", err)
	}
	if _, err := f.service.Create(context.Background(), f.otherClientID, CreateInput{ServiceID: f.otherService, StartsAt: start}, f.config); err != nil {
		t.Fatalf("create same-time booking for a different provider: %v", err)
	}
}

func TestBookingRespectsServiceAndLocalAvailability(t *testing.T) {
	f := newBookingFixture(t)
	ctx := context.Background()
	location, err := time.LoadLocation("Pacific/Auckland")
	if err != nil {
		t.Fatalf("load provider timezone: %v", err)
	}
	day := time.Now().In(location).AddDate(0, 0, 3)
	weekday := int(day.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	localStart := time.Date(day.Year(), day.Month(), day.Day(), 11, 0, 0, 0, location)
	start := localStart.UTC()
	if _, err := f.pool.Exec(ctx, `UPDATE provider_profiles SET time_zone = 'Pacific/Auckland' WHERE provider_id = $1`, f.providerID); err != nil {
		t.Fatalf("set provider timezone: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM weekly_working_hours WHERE provider_id = $1`, f.providerID); err != nil {
		t.Fatalf("clear provider hours: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO weekly_working_hours (provider_id, weekday, opens_at, closes_at) VALUES ($1, $2, '10:00', '13:00')`, f.providerID, weekday); err != nil {
		t.Fatalf("set provider local hours: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE services SET buffer_minutes = 15 WHERE id = $1`, f.serviceID); err != nil {
		t.Fatalf("set service buffer: %v", err)
	}
	created, err := f.service.Create(ctx, f.clientID, CreateInput{ServiceID: f.serviceID, StartsAt: start}, f.config)
	if err != nil {
		t.Fatalf("create booking in provider timezone: %v", err)
	}
	if created.DurationMinutes != 30 || created.BufferMinutes != 15 || created.PriceIRR != 25000 {
		t.Fatalf("booking snapshot=%+v; want trusted service duration, buffer, and price", created)
	}
	if _, err := f.service.Create(ctx, f.otherClientID, CreateInput{ServiceID: f.serviceID, StartsAt: start.Add(30 * time.Minute)}, f.config); !errors.Is(err, ErrSlotUnavailable) {
		t.Fatalf("booking overlapping service buffer error=%v, want ErrSlotUnavailable", err)
	}
	if _, err := f.service.Cancel(ctx, f.clientID, created.ID, 0, ""); err != nil {
		t.Fatalf("cancel booking before start: %v", err)
	}
	if _, err := scheduling.NewStore(f.pool).CreateTimeOff(ctx, f.providerID, "blocked", start.Add(-time.Minute), start.Add(20*time.Minute), ""); err != nil {
		t.Fatalf("create provider time off: %v", err)
	}
	if _, err := f.service.Create(ctx, f.otherClientID, CreateInput{ServiceID: f.serviceID, StartsAt: start}, f.config); !errors.Is(err, ErrSlotUnavailable) {
		t.Fatalf("booking during time off error=%v, want ErrSlotUnavailable", err)
	}
	if _, err := f.service.Create(ctx, f.otherClientID, CreateInput{ServiceID: f.serviceID, StartsAt: start.Add(2 * time.Hour)}, f.config); !errors.Is(err, ErrSlotUnavailable) {
		t.Fatalf("booking outside working hours error=%v, want ErrSlotUnavailable", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE services SET name = 'Changed', price_irr = 99999, duration_minutes = 60 WHERE id = $1`, f.serviceID); err != nil {
		t.Fatalf("change service after booking: %v", err)
	}
	stored, err := f.service.Get(ctx, f.clientID, created.ID)
	if err != nil {
		t.Fatalf("read booking snapshot: %v", err)
	}
	if stored.ServiceName != "Service" || stored.DurationMinutes != 30 || stored.PriceIRR != 25000 || stored.BufferMinutes != 15 {
		t.Fatalf("stored snapshot changed after service update: %+v", stored)
	}
}

func TestInactiveServiceCannotBeBooked(t *testing.T) {
	f := newBookingFixture(t)
	if _, err := f.pool.Exec(context.Background(), `UPDATE services SET is_active = FALSE WHERE id = $1`, f.serviceID); err != nil {
		t.Fatalf("deactivate service: %v", err)
	}
	if _, err := f.service.Create(context.Background(), f.clientID, CreateInput{ServiceID: f.serviceID, StartsAt: nextBookingStart(), IdempotencyKey: "inactive-service"}, f.config); !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("inactive service booking error=%v, want ErrServiceUnavailable", err)
	}
	var count int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM bookings`).Scan(&count); err != nil {
		t.Fatalf("count bookings after invalid request: %v", err)
	}
	if count != 0 {
		t.Fatalf("invalid service left %d bookings; want none", count)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM idempotency_records WHERE operation = 'booking.create'`).Scan(&count); err != nil {
		t.Fatalf("count idempotency records after rejected request: %v", err)
	}
	if count != 0 {
		t.Fatalf("invalid service left %d booking.create idempotency records; want none", count)
	}
}

func TestConfirmedCancellationCutoff(t *testing.T) {
	f := newBookingFixture(t)
	f.config.CancellationCutoff = 4 * 24 * time.Hour
	created, err := f.service.Create(context.Background(), f.clientID, CreateInput{ServiceID: f.serviceID, StartsAt: nextBookingStart()}, f.config)
	if err != nil {
		t.Fatalf("create near-term booking: %v", err)
	}
	if _, err := f.service.Cancel(context.Background(), f.clientID, created.ID, f.config.CancellationCutoff, ""); !errors.Is(err, ErrCancellationClosed) {
		t.Fatalf("cancel inside configured cutoff error=%v, want ErrCancellationClosed", err)
	}
}

func TestBookingIdempotency(t *testing.T) {
	f := newBookingFixture(t)
	start := nextBookingStart()
	input := CreateInput{ServiceID: f.serviceID, StartsAt: start, IdempotencyKey: "booking-unique-key"}
	first, err := f.service.Create(context.Background(), f.clientID, input, f.config)
	if err != nil {
		t.Fatalf("create booking: %v", err)
	}
	replay, err := f.service.Create(context.Background(), f.clientID, input, f.config)
	if err != nil {
		t.Fatalf("replay booking: %v", err)
	}
	if replay.ID != first.ID {
		t.Fatalf("replay booking id=%d, want %d", replay.ID, first.ID)
	}
	input.StartsAt = input.StartsAt.Add(15 * time.Minute)
	if _, err := f.service.Create(context.Background(), f.clientID, input, f.config); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("reuse key with different payload error=%v, want ErrIdempotencyConflict", err)
	}
}

func TestInstantBookingHasNoApprovalExpiration(t *testing.T) {
	f := newBookingFixture(t)
	created, err := f.service.Create(context.Background(), f.clientID, CreateInput{ServiceID: f.serviceID, StartsAt: nextBookingStart()}, f.config)
	if err != nil {
		t.Fatalf("create confirmed booking: %v", err)
	}
	var status string
	var legacyExpiryIsNull bool
	if err := f.pool.QueryRow(context.Background(), `SELECT status, legacy_expires_at IS NULL FROM bookings WHERE id = $1`, created.ID).Scan(&status, &legacyExpiryIsNull); err != nil {
		t.Fatalf("read booking state: %v", err)
	}
	if status != "confirmed" || !legacyExpiryIsNull {
		t.Fatalf("status=%s legacy expiry null=%t; want confirmed without expiry", status, legacyExpiryIsNull)
	}
	stored, err := f.service.Get(context.Background(), f.clientID, created.ID)
	if err != nil || stored.Status != "confirmed" {
		t.Fatalf("confirmed booking=%+v error=%v", stored, err)
	}
	var transitions int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM booking_state_transitions WHERE booking_id = $1`, created.ID).Scan(&transitions); err != nil {
		t.Fatalf("count transition audit records: %v", err)
	}
	if transitions != 1 {
		t.Fatalf("transition audit records=%d, want initial confirmation only", transitions)
	}
}

func TestTransitionsCancellationAndScheduleConflicts(t *testing.T) {
	f := newBookingFixture(t)
	start := nextBookingStart()
	confirmed, err := f.service.Create(context.Background(), f.clientID, CreateInput{ServiceID: f.serviceID, StartsAt: start}, f.config)
	if err != nil {
		t.Fatalf("create confirmed booking: %v", err)
	}
	if _, err := f.service.Start(context.Background(), f.providerID, confirmed.ID, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("start before service time error=%v, want ErrInvalidTransition", err)
	}
	_, err = providers.NewService(f.pool).Put(context.Background(), f.providerID, "Provider", "", "UTC", true)
	var timezoneErr *pgconn.PgError
	if !errors.As(err, &timezoneErr) || timezoneErr.ConstraintName != "provider_profiles_time_zone_booking_guard" {
		t.Fatalf("timezone change with active booking error=%v, want booking guard violation", err)
	}
	if _, err := f.service.Start(context.Background(), f.otherProvider, confirmed.ID, ""); !errors.Is(err, ErrBookingNotFound) {
		t.Fatalf("start by another provider error=%v, want ErrBookingNotFound", err)
	}
	schedule := scheduling.NewStore(f.pool)
	if _, err := schedule.CreateTimeOff(context.Background(), f.providerID, "blocked", start.Add(-time.Minute), start.Add(time.Hour), ""); !errors.Is(err, scheduling.ErrScheduleConflict) {
		t.Fatalf("time off overlapping confirmed booking error=%v, want ErrScheduleConflict", err)
	}
	if err := schedule.PutWorkingHours(context.Background(), f.providerID, nil); !errors.Is(err, scheduling.ErrScheduleConflict) {
		t.Fatalf("working-hours update excluding confirmed booking error=%v, want ErrScheduleConflict", err)
	}
	if _, err := f.service.Cancel(context.Background(), f.clientID, confirmed.ID, 0, ""); err != nil {
		t.Fatalf("cancel confirmed booking before start: %v", err)
	}
	if _, err := f.service.Create(context.Background(), f.otherClientID, CreateInput{ServiceID: f.serviceID, StartsAt: start}, f.config); err != nil {
		t.Fatalf("cancelled booking did not release its slot: %v", err)
	}
}

func TestProviderCanStartConfirmedService(t *testing.T) {
	f := newBookingFixture(t)
	created, err := f.service.Create(context.Background(), f.clientID, CreateInput{ServiceID: f.serviceID, StartsAt: nextBookingStart()}, f.config)
	if err != nil {
		t.Fatalf("create booking: %v", err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		UPDATE bookings SET starts_at = now() - interval '1 minute', ends_at = now() + interval '29 minutes'
		WHERE id = $1
	`, created.ID); err != nil {
		t.Fatalf("move booking into its scheduled service window: %v", err)
	}
	started, err := f.service.Start(context.Background(), f.providerID, created.ID, "")
	if err != nil {
		t.Fatalf("start confirmed booking: %v", err)
	}
	if started.Status != "in_progress" {
		t.Fatalf("started status=%s, want in_progress", started.Status)
	}
}

func TestInvalidDatabaseTransitionIsRejected(t *testing.T) {
	f := newBookingFixture(t)
	created, err := f.service.Create(context.Background(), f.clientID, CreateInput{ServiceID: f.serviceID, StartsAt: nextBookingStart()}, f.config)
	if err != nil {
		t.Fatalf("create booking: %v", err)
	}
	_, err = f.pool.Exec(context.Background(), `UPDATE bookings SET status = 'in_progress' WHERE id = $1`, created.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "bookings_start_deadline_check" {
		t.Fatalf("early service start error=%v; want database deadline violation", err)
	}
	_, err = f.pool.Exec(context.Background(), `UPDATE bookings SET status = 'completed' WHERE id = $1`, created.ID)
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "bookings_state_transition_check" {
		t.Fatalf("invalid transition error=%v; want transition guard violation", err)
	}
}

func TestBookingAPIAuthorizationAndOwnership(t *testing.T) {
	f := newBookingFixture(t)
	app := fiber.New(fiber.Config{ErrorHandler: platform.ErrorHandler(slog.New(slog.NewTextHandler(io.Discard, nil)))})
	app.Use(platform.RequestID())
	NewHandler(f.service, f.config).Register(app, integrationSecret)
	clientToken := bookingToken(t, f.clientID, []string{"client"})
	otherClientToken := bookingToken(t, f.otherClientID, []string{"client"})
	providerToken := bookingToken(t, f.providerID, []string{"provider"})
	otherProviderToken := bookingToken(t, f.otherProvider, []string{"provider"})
	start := nextBookingStart().Format(time.RFC3339)
	body := `{"service_id":` + jsonNumber(f.serviceID) + `,"starts_at":"` + start + `"}`

	resp := bookingRequest(t, app, http.MethodPost, "/api/v1/bookings", "", body, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous create status=%d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
	resp = bookingRequest(t, app, http.MethodPost, "/api/v1/bookings", providerToken, body, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("provider create status=%d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
	untrustedBody := strings.TrimSuffix(body, "}") + `,"price_irr":1,"provider_id":` + jsonNumber(f.otherProvider) + `}`
	resp = bookingRequest(t, app, http.MethodPost, "/api/v1/bookings", clientToken, untrustedBody, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("client-provided booking price/provider status=%d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	resp = bookingRequest(t, app, http.MethodPost, "/api/v1/bookings", clientToken, body, "api-test-key")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("client create status=%d, want 201", resp.StatusCode)
	}
	var created Booking
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode created booking: %v", err)
	}
	resp.Body.Close()
	resp = bookingRequest(t, app, http.MethodPost, "/api/v1/provider/bookings/"+jsonNumber(created.ID)+"/start", otherProviderToken, "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("other provider booking start status=%d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	resp = bookingRequest(t, app, http.MethodGet, "/api/v1/bookings/"+jsonNumber(created.ID), otherClientToken, "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("other client booking detail status=%d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	resp = bookingRequest(t, app, http.MethodGet, "/api/v1/bookings", clientToken, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client history status=%d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	resp = bookingRequest(t, app, http.MethodGet, "/api/v1/provider/bookings", providerToken, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("provider management list status=%d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdminBookingListAPI(t *testing.T) {
	f := newBookingFixture(t)
	app := fiber.New(fiber.Config{ErrorHandler: platform.ErrorHandler(slog.New(slog.NewTextHandler(io.Discard, nil)))})
	app.Use(platform.RequestID())
	NewHandler(f.service, f.config).Register(app, integrationSecret)

	start := nextBookingStart()
	created, err := f.service.Create(context.Background(), f.clientID, CreateInput{ServiceID: f.serviceID, StartsAt: start}, f.config)
	if err != nil {
		t.Fatalf("create booking: %v", err)
	}

	clientToken := bookingToken(t, f.clientID, []string{"client"})
	providerToken := bookingToken(t, f.providerID, []string{"provider"})
	adminToken := bookingToken(t, f.providerID, []string{"admin"})

	type adminBookingItem struct {
		ID                  int64  `json:"id"`
		Status              string `json:"status"`
		ServiceName         string `json:"service_name"`
		PriceIRR            int64  `json:"price_irr"`
		ClientEmail         string `json:"client_email"`
		ProviderDisplayName string `json:"provider_display_name"`
		HasOpenDispute      bool   `json:"has_open_dispute"`
		Refundable          bool   `json:"refundable"`
	}
	type adminBookingPage struct {
		Items      []adminBookingItem `json:"items"`
		NextCursor *int64             `json:"next_cursor"`
	}

	resp := bookingRequest(t, app, http.MethodGet, "/api/v1/admin/bookings", "", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous admin booking list status=%d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
	resp = bookingRequest(t, app, http.MethodGet, "/api/v1/admin/bookings", clientToken, "", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("client admin booking list status=%d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
	resp = bookingRequest(t, app, http.MethodGet, "/api/v1/admin/bookings", providerToken, "", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("provider admin booking list status=%d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
	resp = bookingRequest(t, app, http.MethodGet, "/api/v1/admin/bookings?status=bogus", adminToken, "", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid status filter status=%d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	resp = bookingRequest(t, app, http.MethodGet, "/api/v1/admin/bookings?refundable=maybe", adminToken, "", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid refundable filter status=%d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	resp = bookingRequest(t, app, http.MethodGet, "/api/v1/admin/bookings", adminToken, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin booking list status=%d, want 200", resp.StatusCode)
	}
	var page adminBookingPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("decode admin booking page: %v", err)
	}
	resp.Body.Close()
	if page.NextCursor != nil {
		t.Fatalf("single booking must not have next cursor, got %d", *page.NextCursor)
	}
	if len(page.Items) != 1 {
		t.Fatalf("admin sees %d bookings, want 1", len(page.Items))
	}
	item := page.Items[0]
	if item.ID != created.ID || item.Status != "confirmed" || item.PriceIRR != created.PriceIRR {
		t.Fatalf("admin booking item=%+v, want booking %d confirmed", item, created.ID)
	}
	if item.HasOpenDispute || item.Refundable {
		t.Fatalf("fresh booking flags=%+v, want dispute=false refundable=false", item)
	}
	if item.ClientEmail == "" || item.ProviderDisplayName != "Provider" {
		t.Fatalf("admin booking contact fields=%+v", item)
	}

	resp = bookingRequest(t, app, http.MethodGet, "/api/v1/admin/bookings?status=cancelled", adminToken, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancelled filter status=%d, want 200", resp.StatusCode)
	}
	var cancelled adminBookingPage
	if err := json.NewDecoder(resp.Body).Decode(&cancelled); err != nil {
		t.Fatalf("decode cancelled filter page: %v", err)
	}
	resp.Body.Close()
	if len(cancelled.Items) != 0 {
		t.Fatalf("cancelled filter returned %+v, want empty", cancelled.Items)
	}

	resp = bookingRequest(t, app, http.MethodGet, "/api/v1/admin/bookings?q="+created.ServiceName, adminToken, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("text filter status=%d, want 200", resp.StatusCode)
	}
	var byService adminBookingPage
	if err := json.NewDecoder(resp.Body).Decode(&byService); err != nil {
		t.Fatalf("decode text filter page: %v", err)
	}
	resp.Body.Close()
	if len(byService.Items) != 1 || byService.Items[0].ID != created.ID {
		t.Fatalf("text filter returned %+v, want booking %d", byService.Items, created.ID)
	}

	resp = bookingRequest(t, app, http.MethodGet, "/api/v1/admin/bookings?refundable=true", adminToken, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refundable filter status=%d, want 200", resp.StatusCode)
	}
	var refundable adminBookingPage
	if err := json.NewDecoder(resp.Body).Decode(&refundable); err != nil {
		t.Fatalf("decode refundable filter page: %v", err)
	}
	resp.Body.Close()
	if len(refundable.Items) != 0 {
		t.Fatalf("refundable filter returned %+v, want empty for unsettled hold", refundable.Items)
	}
}

func nextBookingStart() time.Time {
	location, _ := time.LoadLocation("Asia/Tehran")
	day := time.Now().In(location).AddDate(0, 0, 3)
	start := time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, location)
	return start.UTC()
}

func randomEmail(t *testing.T) string {
	t.Helper()
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		t.Fatalf("generate email suffix: %v", err)
	}
	return hex.EncodeToString(bytes) + "@booking.test"
}

func bookingToken(t *testing.T, userID int64, roles []string) string {
	t.Helper()
	token, _, err := identity.IssueAccessToken(userID, "booking@example.test", roles, identity.AccessTokenConfig{Secret: integrationSecret, TTL: time.Hour})
	if err != nil {
		t.Fatalf("issue booking test token: %v", err)
	}
	return token
}

func bookingRequest(t *testing.T, app *fiber.App, method, path, token, body, idempotencyKey string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, "http://test.local"+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("create booking request: %v", err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := app.Test(request)
	if err != nil {
		t.Fatalf("send booking request: %v", err)
	}
	return response
}

func jsonNumber(value int64) string {
	return strconv.FormatInt(value, 10)
}
