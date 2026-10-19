package booking

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"test-api/internal/catalog"
	"test-api/internal/providers"
	"test-api/internal/scheduling"
	"test-api/internal/wallet"
)

type Booking struct {
	ID              int64     `json:"id"`
	ClientID        int64     `json:"client_id"`
	ProviderID      int64     `json:"provider_id"`
	ServiceID       int64     `json:"service_id"`
	ServiceName     string    `json:"service_name"`
	StartsAt        time.Time `json:"starts_at"`
	EndsAt          time.Time `json:"ends_at"`
	DurationMinutes int       `json:"duration_minutes"`
	PriceIRR        int64     `json:"price_irr"`
	BufferMinutes   int       `json:"buffer_minutes"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type Config struct {
	CancellationCutoff time.Duration
	Availability       scheduling.AvailabilityPolicy
}

type CreateInput struct {
	ServiceID      int64
	StartsAt       time.Time
	IdempotencyKey string
	RequestID      string
}

type Store struct {
	pool *pgxpool.Pool
}

type Service struct {
	*Store
	providers *providers.Service
	catalog   *catalog.Store
	schedule  *scheduling.Store
	wallet    *wallet.Service
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func NewService(pool *pgxpool.Pool, providerProfiles *providers.Service, services *catalog.Store, schedule *scheduling.Store, walletServices ...*wallet.Service) *Service {
	walletService := wallet.NewService(pool)
	if len(walletServices) > 0 && walletServices[0] != nil {
		walletService = walletServices[0]
	}
	return &Service{Store: NewStore(pool), providers: providerProfiles, catalog: services, schedule: schedule, wallet: walletService}
}

var (
	ErrInvalidInput        = errors.New("invalid booking input")
	ErrServiceUnavailable  = errors.New("service unavailable for booking")
	ErrSlotUnavailable     = errors.New("requested booking slot unavailable")
	ErrBookingNotFound     = errors.New("booking not found")
	ErrForbidden           = errors.New("booking operation forbidden")
	ErrInvalidTransition   = errors.New("invalid booking transition")
	ErrCancellationClosed  = errors.New("booking cancellation window closed")
	ErrIdempotencyConflict = errors.New("idempotency key payload conflict")
)

func (s *Service) Create(ctx context.Context, clientID int64, input CreateInput, config Config) (*Booking, error) {
	if clientID < 1 || input.ServiceID < 1 || input.StartsAt.IsZero() {
		return nil, ErrInvalidInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin booking creation: %w", err)
	}
	defer tx.Rollback(ctx)

	fingerprint := requestFingerprint(input.ServiceID, input.StartsAt)
	if input.IdempotencyKey != "" {
		var inserted int64
		err := tx.QueryRow(ctx, `
			INSERT INTO idempotency_records (principal_id, operation, idempotency_key, request_fingerprint)
			VALUES ($1, 'booking.create', $2, $3)
			ON CONFLICT (principal_id, operation, idempotency_key) DO NOTHING
			RETURNING id
		`, clientID, input.IdempotencyKey, fingerprint).Scan(&inserted)
		if errors.Is(err, pgx.ErrNoRows) {
			var storedFingerprint string
			var result []byte
			if err := tx.QueryRow(ctx, `
				SELECT request_fingerprint, result FROM idempotency_records
				WHERE principal_id = $1 AND operation = 'booking.create' AND idempotency_key = $2
				FOR UPDATE
			`, clientID, input.IdempotencyKey).Scan(&storedFingerprint, &result); err != nil {
				return nil, fmt.Errorf("read idempotent booking result: %w", err)
			}
			if storedFingerprint != fingerprint {
				return nil, ErrIdempotencyConflict
			}
			var previous Booking
			if err := json.Unmarshal(result, &previous); err != nil {
				return nil, fmt.Errorf("decode idempotent booking result: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return nil, fmt.Errorf("commit idempotent booking read: %w", err)
			}
			return &previous, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reserve booking idempotency key: %w", err)
		}
	}

	providerID, err := s.catalog.BookingProviderIDTx(ctx, tx, input.ServiceID)
	if err != nil {
		if errors.Is(err, catalog.ErrServiceNotFound) {
			return nil, ErrServiceUnavailable
		}
		return nil, err
	}
	provider, err := s.providers.LockForBookingTx(ctx, tx, providerID)
	if err != nil {
		if errors.Is(err, providers.ErrProfileNotFound) {
			return nil, ErrServiceUnavailable
		}
		return nil, err
	}
	if !provider.IsActive || provider.VerificationStatus != "verified" {
		return nil, ErrServiceUnavailable
	}
	service, err := s.catalog.GetForBookingTx(ctx, tx, providerID, input.ServiceID)
	if err != nil {
		if errors.Is(err, catalog.ErrServiceNotFound) {
			return nil, ErrServiceUnavailable
		}
		return nil, err
	}
	startAt := input.StartsAt.UTC()
	endAt := startAt.Add(time.Duration(service.DurationMinutes) * time.Minute)
	location, err := time.LoadLocation(provider.TimeZone)
	if err != nil {
		return nil, fmt.Errorf("load provider time zone: %w", err)
	}
	localDate := startAt.In(location).Format("2006-01-02")
	dayRange, err := scheduling.LocalDayRange(localDate, provider.TimeZone)
	if err != nil {
		return nil, ErrSlotUnavailable
	}
	workingHours, err := s.schedule.GetWorkingHoursTx(ctx, tx, providerID)
	if err != nil {
		return nil, err
	}
	blocked, err := s.schedule.ListAvailabilityIntervalsTx(ctx, tx, providerID, dayRange.StartAt, dayRange.EndAt)
	if err != nil {
		return nil, err
	}
	occupied, err := listOccupiedTx(ctx, tx, providerID, dayRange.StartAt, dayRange.EndAt)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	slots, err := scheduling.CalculateAvailability(
		localDate, provider.TimeZone,
		workingHours, blocked, occupied, service.DurationMinutes, service.BufferMinutes,
		config.Availability, now,
	)
	if err != nil {
		return nil, fmt.Errorf("validate requested booking slot: %w", err)
	}
	available := false
	for _, slot := range slots {
		if slot.StartAt.Equal(startAt) && slot.EndAt.Equal(endAt) {
			available = true
			break
		}
	}
	if !available {
		return nil, ErrSlotUnavailable
	}

	if _, err := tx.Exec(ctx, `
		SELECT set_config('app.actor_type', 'user', true),
		       set_config('app.actor_user_id', $1, true),
		       set_config('app.request_id', $2, true),
		       set_config('app.transition_reason', 'created', true)
	`, fmt.Sprint(clientID), input.RequestID); err != nil {
		return nil, fmt.Errorf("set booking audit context: %w", err)
	}

	paymentMethod := "wallet"
	if service.PriceIRR == 0 {
		paymentMethod = "none"
	}
	item, err := scanBooking(tx.QueryRow(ctx, `
		INSERT INTO bookings (
			client_id, provider_id, service_id, service_name, starts_at, ends_at,
			duration_minutes, price_irr, buffer_minutes, status, payment_method
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'confirmed', $10)
		RETURNING id, client_id, provider_id, service_id, service_name, starts_at, ends_at,
			duration_minutes, price_irr, buffer_minutes, status, created_at, updated_at
	`, clientID, providerID, service.ID, service.Name, startAt, endAt,
		service.DurationMinutes, service.PriceIRR, service.BufferMinutes, paymentMethod))
	if err != nil {
		if isOverlapViolation(err) {
			return nil, ErrSlotUnavailable
		}
		return nil, fmt.Errorf("insert booking: %w", err)
	}
	if item.PriceIRR > 0 {
		if _, err := s.wallet.PlaceBookingHoldTx(ctx, tx, wallet.PlaceBookingHoldInput{BookingID: item.ID, CustomerID: clientID, ProviderID: providerID, AmountIRR: item.PriceIRR, RequestID: input.RequestID}); err != nil {
			return nil, err
		}
	}
	if input.IdempotencyKey != "" {
		result, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("encode booking idempotency result: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE idempotency_records SET booking_id = $1, result = $2::jsonb
			WHERE principal_id = $3 AND operation = 'booking.create' AND idempotency_key = $4
		`, item.ID, string(result), clientID, input.IdempotencyKey); err != nil {
			return nil, fmt.Errorf("store booking idempotency result: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		if isOverlapViolation(err) {
			return nil, ErrSlotUnavailable
		}
		return nil, fmt.Errorf("commit booking creation: %w", err)
	}
	return item, nil
}

func (s *Store) Get(ctx context.Context, actorID, bookingID int64) (*Booking, error) {
	item, err := scanBooking(s.pool.QueryRow(ctx, `
		SELECT id, client_id, provider_id, service_id, service_name, starts_at, ends_at,
			duration_minutes, price_irr, buffer_minutes, status, created_at, updated_at
		FROM bookings WHERE id = $1 AND (client_id = $2 OR provider_id = $2)
	`, bookingID, actorID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBookingNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get booking: %w", err)
	}
	return item, nil
}

func (s *Store) ListClient(ctx context.Context, clientID, after int64, limit int) ([]Booking, bool, error) {
	return s.list(ctx, `client_id = $1`, clientID, after, limit)
}

func (s *Store) ListProvider(ctx context.Context, providerID, after int64, limit int) ([]Booking, bool, error) {
	return s.list(ctx, `provider_id = $1`, providerID, after, limit)
}

func (s *Store) list(ctx context.Context, predicate string, actorID, after int64, limit int) ([]Booking, bool, error) {
	query := `SELECT id, client_id, provider_id, service_id, service_name, starts_at, ends_at,
		duration_minutes, price_irr, buffer_minutes, status, created_at, updated_at
		FROM bookings WHERE ` + predicate + ` AND id > $2 ORDER BY id LIMIT $3`
	rows, err := s.pool.Query(ctx, query, actorID, after, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("list bookings: %w", err)
	}
	defer rows.Close()
	items := make([]Booking, 0, limit)
	for rows.Next() {
		item, err := scanBooking(rows)
		if err != nil {
			return nil, false, err
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	return items, hasMore, nil
}

func (s *Service) Start(ctx context.Context, providerID, bookingID int64, requestID string) (*Booking, error) {
	return s.transition(ctx, providerID, bookingID, "in_progress", "service_started", requestID, func(current Booking) error {
		if current.ProviderID != providerID {
			return ErrBookingNotFound
		}
		if current.Status == "in_progress" {
			return nil
		}
		if current.Status != "confirmed" {
			return ErrInvalidTransition
		}
		if time.Now().UTC().Before(current.StartsAt) {
			return ErrInvalidTransition
		}
		return nil
	})
}

func (s *Service) Cancel(ctx context.Context, clientID, bookingID int64, cutoff time.Duration, requestID string) (*Booking, error) {
	return s.transition(ctx, clientID, bookingID, "cancelled", "client_cancelled", requestID, func(current Booking) error {
		if current.ClientID != clientID {
			return ErrBookingNotFound
		}
		if current.Status == "cancelled" {
			return nil
		}
		if current.Status != "confirmed" {
			return ErrInvalidTransition
		}
		if !time.Now().UTC().Add(cutoff).Before(current.StartsAt) {
			return ErrCancellationClosed
		}
		return nil
	}, func(tx pgx.Tx) error { return s.wallet.ReleaseBookingHoldTx(ctx, tx, bookingID, clientID, requestID) })
}

func (s *Service) Refund(ctx context.Context, adminID, bookingID int64, idempotencyKey, requestID string) error {
	if adminID < 1 || bookingID < 1 {
		return wallet.ErrInvalidInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin booking refund: %w", err)
	}
	defer tx.Rollback(ctx)
	var isAdmin bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_roles WHERE user_id = $1 AND role = 'admin')`, adminID).Scan(&isAdmin); err != nil {
		return fmt.Errorf("check refund administrator role: %w", err)
	}
	if !isAdmin {
		return ErrForbidden
	}
	var providerID int64
	if err := tx.QueryRow(ctx, `SELECT provider_id FROM bookings WHERE id = $1`, bookingID).Scan(&providerID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrBookingNotFound
		}
		return fmt.Errorf("read refund booking provider: %w", err)
	}
	if _, err := s.providers.LockForBookingTx(ctx, tx, providerID); err != nil {
		return err
	}
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM bookings WHERE id = $1 FOR UPDATE`, bookingID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrBookingNotFound
		}
		return fmt.Errorf("lock booking for refund: %w", err)
	}
	if status != "completed" {
		return wallet.ErrNotReversible
	}
	if err := wallet.RefundBookingHoldTx(ctx, tx, bookingID, adminID, idempotencyKey, requestID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit booking refund: %w", err)
	}
	return nil
}

// Complete allows the owning provider to settle an in-progress wallet booking.
func (s *Service) Complete(ctx context.Context, providerID, bookingID int64, requestID string) (*Booking, error) {
	return s.transition(ctx, providerID, bookingID, "completed", "service_completed", requestID, func(current Booking) error {
		if current.ProviderID != providerID {
			return ErrBookingNotFound
		}
		if current.Status == "completed" {
			return nil
		}
		if current.Status != "in_progress" {
			return ErrInvalidTransition
		}
		return nil
	}, func(tx pgx.Tx) error { return s.wallet.SettleBookingHoldTx(ctx, tx, bookingID, providerID, requestID) })
}

func (s *Service) transition(ctx context.Context, actorID, bookingID int64, target, reason, requestID string, validate func(Booking) error, effects ...func(pgx.Tx) error) (*Booking, error) {
	effect := func(pgx.Tx) error { return nil }
	if len(effects) > 0 {
		effect = effects[0]
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin booking transition: %w", err)
	}
	defer tx.Rollback(ctx)
	// Preserve profile -> booking -> accounts locking order for wallet transitions.
	var providerID int64
	if err := tx.QueryRow(ctx, `SELECT provider_id FROM bookings WHERE id = $1`, bookingID).Scan(&providerID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrBookingNotFound
		}
		return nil, fmt.Errorf("read booking provider: %w", err)
	}
	if _, err := s.providers.LockForBookingTx(ctx, tx, providerID); err != nil {
		return nil, err
	}
	current, err := scanBooking(tx.QueryRow(ctx, `
		SELECT id, client_id, provider_id, service_id, service_name, starts_at, ends_at,
			duration_minutes, price_irr, buffer_minutes, status, created_at, updated_at
		FROM bookings WHERE id = $1 FOR UPDATE
	`, bookingID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBookingNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock booking for transition: %w", err)
	}
	if err := validate(*current); err != nil {
		return nil, err
	}
	if current.Status == target {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit idempotent booking transition: %w", err)
		}
		return current, nil
	}
	if err := effect(tx); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		SELECT set_config('app.actor_type', 'user', true),
		       set_config('app.actor_user_id', $1, true),
		       set_config('app.request_id', $2, true),
		       set_config('app.transition_reason', $3, true)
	`, fmt.Sprint(actorID), requestID, reason); err != nil {
		return nil, fmt.Errorf("set booking transition audit context: %w", err)
	}
	item, err := scanBooking(tx.QueryRow(ctx, `
		UPDATE bookings SET status = $1, updated_at = now()
		WHERE id = $2 AND status = $3
		RETURNING id, client_id, provider_id, service_id, service_name, starts_at, ends_at,
			duration_minutes, price_irr, buffer_minutes, status, created_at, updated_at
	`, target, bookingID, current.Status))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidTransition
	}
	if err != nil {
		return nil, fmt.Errorf("update booking state: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit booking transition: %w", err)
	}
	return item, nil
}

func listOccupiedTx(ctx context.Context, tx pgx.Tx, providerID int64, startAt, endAt time.Time) ([]scheduling.TimeInterval, error) {
	rows, err := tx.Query(ctx, `
		SELECT starts_at, ends_at + buffer_minutes * INTERVAL '1 minute'
		FROM bookings
		WHERE provider_id = $1 AND status IN ('confirmed', 'in_progress')
		  AND starts_at < $3 AND ends_at + buffer_minutes * INTERVAL '1 minute' > $2
		ORDER BY starts_at
	`, providerID, startAt.UTC(), endAt.UTC())
	if err != nil {
		return nil, fmt.Errorf("list occupied bookings: %w", err)
	}
	defer rows.Close()
	intervals := make([]scheduling.TimeInterval, 0)
	for rows.Next() {
		var interval scheduling.TimeInterval
		if err := rows.Scan(&interval.StartAt, &interval.EndAt); err != nil {
			return nil, err
		}
		interval.StartAt = interval.StartAt.UTC()
		interval.EndAt = interval.EndAt.UTC()
		intervals = append(intervals, interval)
	}
	return intervals, rows.Err()
}

func scanBooking(row pgx.Row) (*Booking, error) {
	var item Booking
	err := row.Scan(&item.ID, &item.ClientID, &item.ProviderID, &item.ServiceID, &item.ServiceName,
		&item.StartsAt, &item.EndsAt, &item.DurationMinutes, &item.PriceIRR, &item.BufferMinutes,
		&item.Status, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		return nil, err
	}
	item.StartsAt = item.StartsAt.UTC()
	item.EndsAt = item.EndsAt.UTC()
	item.CreatedAt = item.CreatedAt.UTC()
	item.UpdatedAt = item.UpdatedAt.UTC()
	return &item, nil
}

func requestFingerprint(serviceID int64, startsAt time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s", serviceID, startsAt.UTC().Format(time.RFC3339Nano))))
	return hex.EncodeToString(sum[:])
}

func isOverlapViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23P01" && pgErr.ConstraintName == "bookings_no_overlapping_active_provider_slots"
}
