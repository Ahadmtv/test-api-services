package booking

import (
	"context"
	"crypto/sha256"
	"database/sql"
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
	ID int64 `json:"id"`
	// ClientID owns the booking; ProviderID is the provider user id.
	ClientID              int64      `json:"client_id"`
	ProviderID            int64      `json:"provider_id"`
	ServiceID             int64      `json:"service_id"`
	ServiceName           string     `json:"service_name"`
	StartsAt              time.Time  `json:"starts_at"`
	EndsAt                time.Time  `json:"ends_at"`
	DurationMinutes       int        `json:"duration_minutes"`
	PriceIRR              int64      `json:"price_irr"`
	BufferMinutes         int        `json:"buffer_minutes"`
	Status                string     `json:"status"`
	PaymentMethod         string     `json:"payment_method"`
	PaymentExpiresAt      *time.Time `json:"payment_expires_at"`
	CompletionRequestedAt *time.Time `json:"completion_requested_at"`
	CompletedAt           *time.Time `json:"completed_at"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

type Config struct {
	CancellationCutoff time.Duration
	// CompletionDisputeWindow is the time a client has to dispute a completion
	// request before the booking settles automatically.
	CompletionDisputeWindow time.Duration
	// PendingPaymentTTL is how long a gateway-funded booking reserves its slot
	// while the client completes payment.
	PendingPaymentTTL time.Duration
	ExpiryGrace       time.Duration
	Availability      scheduling.AvailabilityPolicy
}

const (
	PaymentMethodNone     = "none"
	PaymentMethodWallet   = "wallet"
	PaymentMethodZarinpal = "zarinpal"

	// GatewayMinimumAmountIRR is the provider's official minimum charge unit.
	GatewayMinimumAmountIRR int64 = 10_000

	defaultPendingPaymentTTL = 15 * time.Minute
)

type GatewayPaymentConfirmation struct {
	BookingID int64
	ClientID  int64
	PaymentID int64
	AmountIRR int64
	RequestID string
}

// DisputeResolution is the administrator decision for an open dispute.
type DisputeResolution string

const (
	// DisputeResolutionSettle pays the provider with the snapshotted commission.
	DisputeResolutionSettle DisputeResolution = "settle"
	// DisputeResolutionCancel releases the full hold back to the client.
	DisputeResolutionCancel DisputeResolution = "cancel"
)

type CreateInput struct {
	ServiceID      int64
	StartsAt       time.Time
	PaymentMethod  string
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
	config    Config
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func NewService(pool *pgxpool.Pool, providerProfiles *providers.Service, services *catalog.Store, schedule *scheduling.Store, config Config, walletServices ...*wallet.Service) *Service {
	walletService := wallet.NewService(pool)
	if len(walletServices) > 0 && walletServices[0] != nil {
		walletService = walletServices[0]
	}
	return &Service{Store: NewStore(pool), providers: providerProfiles, catalog: services, schedule: schedule, wallet: walletService, config: config}
}

var (
	ErrInvalidInput          = errors.New("invalid booking input")
	ErrServiceUnavailable    = errors.New("service unavailable for booking")
	ErrSlotUnavailable       = errors.New("requested booking slot unavailable")
	ErrBookingNotFound       = errors.New("booking not found")
	ErrForbidden             = errors.New("booking operation forbidden")
	ErrInvalidTransition     = errors.New("invalid booking transition")
	ErrCancellationClosed    = errors.New("booking cancellation window closed")
	ErrDisputeWindowClosed   = errors.New("booking dispute window closed")
	ErrInvalidResolution     = errors.New("invalid dispute resolution")
	ErrDisputeNotFound       = errors.New("booking dispute not found")
	ErrIdempotencyConflict   = errors.New("idempotency key payload conflict")
	ErrPaymentAmountMismatch = errors.New("payment amount does not match booking")
	ErrPaymentMinimum        = errors.New("gateway payment amount is below the provider minimum")
)

func (s *Service) Create(ctx context.Context, clientID int64, input CreateInput, config Config) (*Booking, error) {
	if clientID < 1 || input.ServiceID < 1 || input.StartsAt.IsZero() {
		return nil, ErrInvalidInput
	}
	paymentMethod := input.PaymentMethod
	if paymentMethod == "" {
		paymentMethod = PaymentMethodWallet
	}
	if paymentMethod != PaymentMethodWallet && paymentMethod != PaymentMethodZarinpal {
		return nil, ErrInvalidInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin booking creation: %w", err)
	}
	defer tx.Rollback(ctx)

	fingerprint := requestFingerprint(input.ServiceID, input.StartsAt, paymentMethod)
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
			current, err := scanBooking(tx.QueryRow(ctx, `
				SELECT id, client_id, provider_id, service_id, service_name, starts_at, ends_at,
					duration_minutes, price_irr, buffer_minutes, status, payment_method, payment_expires_at,
					completion_requested_at, completed_at, created_at, updated_at
				FROM bookings WHERE id = $1
			`, previous.ID))
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("refresh idempotent booking result: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return nil, fmt.Errorf("commit idempotent booking read: %w", err)
			}
			if current != nil {
				return current, nil
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
	if paymentMethod == PaymentMethodZarinpal && service.PriceIRR > 0 && service.PriceIRR < GatewayMinimumAmountIRR {
		return nil, ErrPaymentMinimum
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

	status := "confirmed"
	if service.PriceIRR == 0 {
		paymentMethod = PaymentMethodNone
	}
	var paymentExpiresAt *time.Time
	if paymentMethod == PaymentMethodZarinpal && service.PriceIRR > 0 {
		ttl := config.PendingPaymentTTL
		if ttl <= 0 {
			ttl = defaultPendingPaymentTTL
		}
		status = "pending_payment"
		expiresAt := now.Add(ttl)
		paymentExpiresAt = &expiresAt
	}
	item, err := scanBooking(tx.QueryRow(ctx, `
		INSERT INTO bookings (
			client_id, provider_id, service_id, service_name, starts_at, ends_at,
			duration_minutes, price_irr, buffer_minutes, status, payment_method, payment_expires_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id, client_id, provider_id, service_id, service_name, starts_at, ends_at,
			duration_minutes, price_irr, buffer_minutes, status, payment_method, payment_expires_at, completion_requested_at, completed_at, created_at, updated_at
	`, clientID, providerID, service.ID, service.Name, startAt, endAt,
		service.DurationMinutes, service.PriceIRR, service.BufferMinutes, status, paymentMethod, paymentExpiresAt))
	if err != nil {
		if isOverlapViolation(err) {
			return nil, ErrSlotUnavailable
		}
		return nil, fmt.Errorf("insert booking: %w", err)
	}
	if item.PriceIRR > 0 && paymentMethod == PaymentMethodWallet {
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
			duration_minutes, price_irr, buffer_minutes, status, payment_method, payment_expires_at, completion_requested_at, completed_at, created_at, updated_at
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

// AdminBookingListItem is the administrator view of a booking with the
// client contact, provider display name and dispute/refund indicators.
type AdminBookingListItem struct {
	ID                  int64     `json:"id"`
	Status              string    `json:"status"`
	ServiceName         string    `json:"service_name"`
	StartsAt            time.Time `json:"starts_at"`
	EndsAt              time.Time `json:"ends_at"`
	PriceIRR            int64     `json:"price_irr"`
	PaymentMethod       string    `json:"payment_method"`
	ClientEmail         string    `json:"client_email"`
	ClientPhone         *string   `json:"client_phone"`
	ProviderDisplayName string    `json:"provider_display_name"`
	HasOpenDispute      bool      `json:"has_open_dispute"`
	Refundable          bool      `json:"refundable"`
	CreatedAt           time.Time `json:"created_at"`
}

type AdminBookingPage struct {
	Items      []AdminBookingListItem `json:"items"`
	NextCursor *int64                 `json:"next_cursor"`
}

// AdminListFilter narrows the administrator booking list. RefundableOnly
// selects completed bookings whose settled hold has not been refunded.
type AdminListFilter struct {
	After          int64
	Limit          int
	Status         string
	Query          string
	RefundableOnly bool
}

// refundableHoldPredicate matches settled booking holds that have not been
// refunded yet; it is shared by the refundable filter and the item flag.
const refundableHoldPredicate = `EXISTS (
	SELECT 1 FROM wallet_holds wh
	WHERE wh.booking_id = b.id AND wh.status = 'settled'
	AND NOT EXISTS (
		SELECT 1 FROM wallet_holds refunded
		WHERE refunded.booking_id = b.id AND refunded.status = 'refunded'
	)
)`

// ListAdmin returns bookings across all users for administrators with keyset
// pagination, optional status and text filters, and a refundable-only view.
func (s *Store) ListAdmin(ctx context.Context, filter AdminListFilter) (*AdminBookingPage, error) {
	if filter.After < 0 || filter.Limit < 1 || filter.Limit > 100 {
		return nil, ErrInvalidInput
	}
	refundableFilter := ""
	if filter.RefundableOnly {
		refundableFilter = `AND b.status = 'completed' AND ` + refundableHoldPredicate
	}
	rows, err := s.pool.Query(ctx, `
		SELECT b.id, b.status, b.service_name, b.starts_at, b.ends_at, b.price_irr, b.payment_method,
		       u.email, u.phone, pp.display_name, b.created_at,
		       EXISTS (
		      	SELECT 1 FROM booking_disputes d
		      	WHERE d.booking_id = b.id AND d.resolved_at IS NULL
		       ) AS has_open_dispute,
		       (b.status = 'completed' AND `+refundableHoldPredicate+`) AS refundable
		FROM bookings b
		JOIN users u ON u.id = b.client_id
		JOIN provider_profiles pp ON pp.provider_id = b.provider_id
		WHERE ($1 = 0 OR b.id > $1)
		  AND ($2 = '' OR b.status = $2)
		  AND ($3 = '' OR b.service_name ILIKE '%' || $3 || '%'
		       OR u.email ILIKE '%' || $3 || '%'
		       OR pp.display_name ILIKE '%' || $3 || '%')
		  `+refundableFilter+`
		ORDER BY b.id LIMIT $4
	`, filter.After, filter.Status, filter.Query, filter.Limit+1)
	if err != nil {
		return nil, fmt.Errorf("list bookings for admin: %w", err)
	}
	defer rows.Close()

	items := make([]AdminBookingListItem, 0, filter.Limit)
	for rows.Next() {
		var item AdminBookingListItem
		var phone sql.NullString
		if err := rows.Scan(&item.ID, &item.Status, &item.ServiceName, &item.StartsAt, &item.EndsAt, &item.PriceIRR, &item.PaymentMethod, &item.ClientEmail, &phone, &item.ProviderDisplayName, &item.CreatedAt, &item.HasOpenDispute, &item.Refundable); err != nil {
			return nil, fmt.Errorf("scan booking for admin: %w", err)
		}
		if phone.Valid {
			item.ClientPhone = &phone.String
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate bookings for admin: %w", err)
	}
	page := AdminBookingPage{Items: items}
	if len(items) > filter.Limit {
		items = items[:filter.Limit]
		page.Items = items
		if len(items) > 0 {
			cursor := items[len(items)-1].ID
			page.NextCursor = &cursor
		}
	}
	return &page, nil
}

func (s *Store) list(ctx context.Context, predicate string, actorID, after int64, limit int) ([]Booking, bool, error) {
	query := `SELECT id, client_id, provider_id, service_id, service_name, starts_at, ends_at,
		duration_minutes, price_irr, buffer_minutes, status, payment_method, payment_expires_at, completion_requested_at, completed_at, created_at, updated_at
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

// CancelByProvider lets the owning provider cancel a confirmed booking before
// the scheduled service start; the hold is released to the client in the same
// transaction. Provider cancellation after start follows the dispute path.
func (s *Service) CancelByProvider(ctx context.Context, providerID, bookingID int64, requestID string) (*Booking, error) {
	return s.transition(ctx, providerID, bookingID, "cancelled", "provider_cancelled", requestID, func(current Booking) error {
		if current.ProviderID != providerID {
			return ErrBookingNotFound
		}
		if current.Status == "cancelled" {
			return nil
		}
		if current.Status != "confirmed" {
			return ErrInvalidTransition
		}
		if time.Now().UTC().After(current.StartsAt) {
			return ErrCancellationClosed
		}
		return nil
	}, func(tx pgx.Tx) error { return s.wallet.ReleaseBookingHoldTx(ctx, tx, bookingID, providerID, requestID) })
}

// Dispute lets the client open a dispute against a completion request inside
// the configured dispute window; booking funds stay on hold.
func (s *Service) Dispute(ctx context.Context, clientID, bookingID int64, reason, requestID string) (*Booking, error) {
	return s.transition(ctx, clientID, bookingID, "disputed", "client_disputed", requestID, func(current Booking) error {
		if current.ClientID != clientID {
			return ErrBookingNotFound
		}
		if current.Status == "disputed" {
			return nil
		}
		if current.Status != "completion_pending" {
			return ErrInvalidTransition
		}
		if current.CompletionRequestedAt == nil {
			return ErrInvalidTransition
		}
		if s.config.CompletionDisputeWindow > 0 &&
			time.Now().UTC().After(current.CompletionRequestedAt.UTC().Add(s.config.CompletionDisputeWindow)) {
			return ErrDisputeWindowClosed
		}
		return nil
	}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO booking_disputes (booking_id, opened_by, reason)
			VALUES ($1, $2, $3)
		`, bookingID, clientID, reason); err != nil {
			return fmt.Errorf("record booking dispute: %w", err)
		}
		return nil
	})
}

// ResolveDispute lets an administrator settle a disputed booking in favor of
// the provider (settle) or the client (cancel). The financial effect and the
// status change happen atomically and the dispute row is closed with the
// decision.
func (s *Service) ResolveDispute(ctx context.Context, adminID, bookingID int64, resolution DisputeResolution, note, requestID string) (*Booking, error) {
	if resolution != DisputeResolutionSettle && resolution != DisputeResolutionCancel {
		return nil, ErrInvalidResolution
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin dispute resolution: %w", err)
	}
	defer tx.Rollback(ctx)
	var isAdmin bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_roles WHERE user_id = $1 AND role = 'admin')`, adminID).Scan(&isAdmin); err != nil {
		return nil, fmt.Errorf("check dispute administrator role: %w", err)
	}
	if !isAdmin {
		return nil, ErrForbidden
	}
	target := "cancelled"
	if resolution == DisputeResolutionSettle {
		target = "completed"
	}
	var providerID int64
	if err := tx.QueryRow(ctx, `SELECT provider_id FROM bookings WHERE id = $1`, bookingID).Scan(&providerID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrBookingNotFound
		}
		return nil, fmt.Errorf("read dispute booking provider: %w", err)
	}
	if _, err := s.providers.LockForBookingTx(ctx, tx, providerID); err != nil {
		return nil, err
	}
	current, err := scanBooking(tx.QueryRow(ctx, `
		SELECT id, client_id, provider_id, service_id, service_name, starts_at, ends_at,
			duration_minutes, price_irr, buffer_minutes, status, payment_method, payment_expires_at, completion_requested_at, completed_at, created_at, updated_at
		FROM bookings WHERE id = $1 FOR UPDATE
	`, bookingID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBookingNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock booking for dispute resolution: %w", err)
	}
	if current.Status == target {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit idempotent dispute read: %w", err)
		}
		return current, nil
	}
	if current.Status != "disputed" {
		return nil, ErrInvalidTransition
	}
	disputeID, err := openDisputeIDTx(ctx, tx, bookingID)
	if err != nil {
		return nil, err
	}
	switch resolution {
	case DisputeResolutionSettle:
		if err := s.wallet.SettleBookingHoldTx(ctx, tx, bookingID, adminID, requestID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE bookings SET completed_at = COALESCE(completed_at, now()) WHERE id = $1`, bookingID); err != nil {
			return nil, fmt.Errorf("stamp dispute completion: %w", err)
		}
	case DisputeResolutionCancel:
		if err := s.wallet.ReleaseBookingHoldTx(ctx, tx, bookingID, adminID, requestID); err != nil {
			return nil, err
		}
	}
	if err := closeDisputeTx(ctx, tx, disputeID, adminID, string(resolution), note); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		SELECT set_config('app.actor_type', 'user', true),
		       set_config('app.actor_user_id', $1, true),
		       set_config('app.request_id', $2, true),
		       set_config('app.transition_reason', $3, true)
	`, fmt.Sprint(adminID), requestID, "dispute_resolved_"+string(resolution)); err != nil {
		return nil, fmt.Errorf("set dispute audit context: %w", err)
	}
	item, err := scanBooking(tx.QueryRow(ctx, `
		UPDATE bookings SET status = $1, updated_at = now()
		WHERE id = $2 AND status = 'disputed'
		RETURNING id, client_id, provider_id, service_id, service_name, starts_at, ends_at,
			duration_minutes, price_irr, buffer_minutes, status, payment_method, payment_expires_at, completion_requested_at, completed_at, created_at, updated_at
	`, target, bookingID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidTransition
	}
	if err != nil {
		return nil, fmt.Errorf("update booking after dispute resolution: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit dispute resolution: %w", err)
	}
	return item, nil
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

// RequestCompletion lets the owning provider mark a finished service as
// completion_pending; settlement waits for client confirmation, dispute
// resolution, or dispute-window expiry.
func (s *Service) RequestCompletion(ctx context.Context, providerID, bookingID int64, requestID string) (*Booking, error) {
	return s.transition(ctx, providerID, bookingID, "completion_pending", "service_complete_requested", requestID, func(current Booking) error {
		if current.ProviderID != providerID {
			return ErrBookingNotFound
		}
		if current.Status == "completion_pending" {
			return nil
		}
		if current.Status != "in_progress" {
			return ErrInvalidTransition
		}
		return nil
	}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE bookings SET completion_requested_at = now() WHERE id = $1`, bookingID); err != nil {
			return fmt.Errorf("stamp completion request: %w", err)
		}
		return nil
	})
}

// ConfirmCompletion lets the booking client settle the requested completion.
// The hold is settled and the booking becomes completed in one transaction;
// repeated confirmation requests replay the terminal state without a second
// settlement.
func (s *Service) ConfirmCompletion(ctx context.Context, clientID, bookingID int64, requestID string) (*Booking, error) {
	return s.transition(ctx, clientID, bookingID, "completed", "client_confirmed_completion", requestID, func(current Booking) error {
		if current.ClientID != clientID {
			return ErrBookingNotFound
		}
		if current.Status == "completed" {
			return nil
		}
		if current.Status != "completion_pending" {
			return ErrInvalidTransition
		}
		return nil
	}, s.finalizeCompletionEffect(ctx, bookingID, clientID, requestID))
}

// ConfirmGatewayPaymentTx captures verified gateway funds into the booking
// hold and confirms the reservation atomically. It is called by the payment
// callback inside the same transaction that marks the payment succeeded.
func (s *Service) ConfirmGatewayPaymentTx(ctx context.Context, tx pgx.Tx, input GatewayPaymentConfirmation) error {
	if input.BookingID < 1 || input.ClientID < 1 || input.PaymentID < 1 || input.AmountIRR < 1 {
		return ErrInvalidInput
	}
	var clientID, providerID, priceIRR int64
	var status, paymentMethod string
	err := tx.QueryRow(ctx, `
		SELECT client_id, provider_id, price_irr, status, payment_method
		FROM bookings WHERE id = $1 FOR UPDATE
	`, input.BookingID).Scan(&clientID, &providerID, &priceIRR, &status, &paymentMethod)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrBookingNotFound
	}
	if err != nil {
		return fmt.Errorf("lock booking for gateway payment: %w", err)
	}
	if clientID != input.ClientID {
		return ErrBookingNotFound
	}
	if status == "confirmed" && paymentMethod == PaymentMethodZarinpal {
		return nil
	}
	if status != "pending_payment" || paymentMethod != PaymentMethodZarinpal {
		return ErrInvalidTransition
	}
	var paymentWindowOpen bool
	if err := tx.QueryRow(ctx, `
		SELECT payment_expires_at + make_interval(secs => $2::double precision) > clock_timestamp()
		FROM bookings WHERE id = $1
	`, input.BookingID, s.config.ExpiryGrace.Seconds()).Scan(&paymentWindowOpen); err != nil {
		return fmt.Errorf("check gateway payment window: %w", err)
	}
	if !paymentWindowOpen {
		return ErrInvalidTransition
	}
	if priceIRR != input.AmountIRR {
		return ErrPaymentAmountMismatch
	}
	if _, err := s.wallet.CaptureBookingPaymentTx(ctx, tx, wallet.CaptureBookingPaymentInput{
		BookingID:  input.BookingID,
		CustomerID: clientID,
		ProviderID: providerID,
		PaymentID:  input.PaymentID,
		AmountIRR:  input.AmountIRR,
		RequestID:  input.RequestID,
	}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		SELECT set_config('app.actor_type', 'system', true),
		       set_config('app.actor_user_id', '', true),
		       set_config('app.request_id', $1, true),
		       set_config('app.transition_reason', 'gateway_payment_verified', true)
	`, input.RequestID); err != nil {
		return fmt.Errorf("set gateway payment audit context: %w", err)
	}
	result, err := tx.Exec(ctx, `
		UPDATE bookings SET status = 'confirmed', updated_at = now()
		WHERE id = $1 AND status = 'pending_payment'
	`, input.BookingID)
	if err != nil {
		return fmt.Errorf("confirm gateway paid booking: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrInvalidTransition
	}
	return nil
}

// FailGatewayPaymentTx cancels a pending gateway booking after a verified
// failed payment so the slot is released immediately.
func (s *Service) FailGatewayPaymentTx(ctx context.Context, tx pgx.Tx, bookingID, clientID int64, requestID string) error {
	var ownerID int64
	var status string
	if err := tx.QueryRow(ctx, `
		SELECT client_id, status FROM bookings WHERE id = $1 FOR UPDATE
	`, bookingID).Scan(&ownerID, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrBookingNotFound
		}
		return fmt.Errorf("lock booking for failed payment: %w", err)
	}
	if ownerID != clientID {
		return ErrBookingNotFound
	}
	if status == "cancelled" || status == "expired" {
		return nil
	}
	if status != "pending_payment" {
		return ErrInvalidTransition
	}
	if _, err := tx.Exec(ctx, `
		SELECT set_config('app.actor_type', 'system', true),
		       set_config('app.actor_user_id', '', true),
		       set_config('app.request_id', $1, true),
		       set_config('app.transition_reason', 'gateway_payment_failed', true)
	`, requestID); err != nil {
		return fmt.Errorf("set failed payment audit context: %w", err)
	}
	result, err := tx.Exec(ctx, `
		UPDATE bookings SET status = 'cancelled', updated_at = now()
		WHERE id = $1 AND status = 'pending_payment'
	`, bookingID)
	if err != nil {
		return fmt.Errorf("cancel failed payment booking: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrInvalidTransition
	}
	return nil
}

// ExpirePendingPayments releases slots after the payment deadline and grace,
// regardless of whether the gateway attempt has been resolved.
func (s *Service) ExpirePendingPayments(ctx context.Context, limit int) (int, error) {
	if limit < 1 {
		return 0, ErrInvalidInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin pending payment expiry: %w", err)
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT b.id FROM bookings b
		WHERE b.status = 'pending_payment'
		  AND b.payment_expires_at <= now() - make_interval(secs => $2::double precision)
		ORDER BY b.payment_expires_at, b.id
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, limit, s.config.ExpiryGrace.Seconds())
	if err != nil {
		return 0, fmt.Errorf("select expired pending payments: %w", err)
	}
	ids := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan expired pending payment: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("read expired pending payments: %w", err)
	}
	rows.Close()

	expired := 0
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `
			SELECT set_config('app.actor_type', 'system', true),
			       set_config('app.actor_user_id', '', true),
			       set_config('app.request_id', 'pending-payment-expiry', true),
			       set_config('app.transition_reason', 'payment_window_expired', true)
		`); err != nil {
			return 0, fmt.Errorf("set pending payment expiry context: %w", err)
		}
		result, err := tx.Exec(ctx, `
			UPDATE bookings SET status = 'expired', updated_at = now()
			WHERE id = $1 AND status = 'pending_payment'
			  AND payment_expires_at <= now() - make_interval(secs => $2::double precision)
		`, id, s.config.ExpiryGrace.Seconds())
		if err != nil {
			return 0, fmt.Errorf("expire pending payment booking %d: %w", id, err)
		}
		expired += int(result.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit pending payment expiry: %w", err)
	}
	return expired, nil
}

// finalizeCompletionEffect settles the booking hold (no-op without a hold),
// stamps completed_at, and leaves the status CAS to the caller's transition.
func (s *Service) finalizeCompletionEffect(ctx context.Context, bookingID, actorID int64, requestID string) func(pgx.Tx) error {
	return func(tx pgx.Tx) error {
		if err := s.wallet.SettleBookingHoldTx(ctx, tx, bookingID, actorID, requestID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE bookings SET completed_at = COALESCE(completed_at, now()) WHERE id = $1`, bookingID); err != nil {
			return fmt.Errorf("stamp booking completion: %w", err)
		}
		return nil
	}
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
			duration_minutes, price_irr, buffer_minutes, status, payment_method, payment_expires_at, completion_requested_at, completed_at, created_at, updated_at
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
			duration_minutes, price_irr, buffer_minutes, status, payment_method, payment_expires_at, completion_requested_at, completed_at, created_at, updated_at
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
		WHERE provider_id = $1 AND status IN ('pending_payment', 'confirmed', 'in_progress', 'completion_pending', 'disputed')
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
		&item.Status, &item.PaymentMethod, &item.PaymentExpiresAt, &item.CompletionRequestedAt, &item.CompletedAt, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		return nil, err
	}
	item.StartsAt = item.StartsAt.UTC()
	item.EndsAt = item.EndsAt.UTC()
	if item.PaymentExpiresAt != nil {
		expiresAt := item.PaymentExpiresAt.UTC()
		item.PaymentExpiresAt = &expiresAt
	}
	item.CreatedAt = item.CreatedAt.UTC()
	item.UpdatedAt = item.UpdatedAt.UTC()
	return &item, nil
}

func requestFingerprint(serviceID int64, startsAt time.Time, paymentMethod string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s", serviceID, startsAt.UTC().Format(time.RFC3339Nano), paymentMethod)))
	return hex.EncodeToString(sum[:])
}

func isOverlapViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23P01" && pgErr.ConstraintName == "bookings_no_overlapping_active_provider_slots"
}

// ExpireDisputeWindows finalizes completion_pending bookings whose dispute
// window elapsed without an open dispute. Each booking is settled in its own
// transaction with a conditional status update, so a booking can never settle
// twice, and bookings locked by a concurrent operator are skipped.
func (s *Service) ExpireDisputeWindows(ctx context.Context, window time.Duration, limit int) (int, error) {
	if window <= 0 || limit < 1 {
		return 0, ErrInvalidInput
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM bookings
		WHERE status = 'completion_pending'
			AND completion_requested_at IS NOT NULL
			AND completion_requested_at <= now() - $1::interval
		ORDER BY id
		LIMIT $2
		FOR UPDATE SKIP LOCKED
	`, window, limit)
	if err != nil {
		return 0, fmt.Errorf("select expired dispute windows: %w", err)
	}
	ids := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	settled := 0
	for _, id := range ids {
		ok, err := s.expireDisputeWindow(ctx, id, window)
		if err != nil {
			return settled, fmt.Errorf("expire dispute window booking %d: %w", id, err)
		}
		if ok {
			settled++
		}
	}
	return settled, nil
}

func (s *Service) expireDisputeWindow(ctx context.Context, bookingID int64, window time.Duration) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin dispute expiry: %w", err)
	}
	defer tx.Rollback(ctx)
	var providerID int64
	if err := tx.QueryRow(ctx, `SELECT provider_id FROM bookings WHERE id = $1`, bookingID).Scan(&providerID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("read expiry booking provider: %w", err)
	}
	if _, err := s.providers.LockForBookingTx(ctx, tx, providerID); err != nil {
		return false, err
	}
	current, err := scanBooking(tx.QueryRow(ctx, `
		SELECT id, client_id, provider_id, service_id, service_name, starts_at, ends_at,
			duration_minutes, price_irr, buffer_minutes, status, payment_method, payment_expires_at, completion_requested_at, completed_at, created_at, updated_at
		FROM bookings WHERE id = $1 FOR UPDATE
	`, bookingID))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock booking for dispute expiry: %w", err)
	}
	if current.Status != "completion_pending" {
		return false, nil
	}
	createdAt := current.CompletionRequestedAt
	if createdAt == nil || time.Since(createdAt.UTC()) < window {
		return false, nil
	}
	set_config, err := tx.Exec(ctx, `
		SELECT set_config('app.actor_type', 'system', true),
		       set_config('app.actor_user_id', '', true),
		       set_config('app.request_id', $1, true),
		       set_config('app.transition_reason', 'dispute_window_expired', true)
	`, "dispute-window-expiry")
	if err != nil {
		return false, fmt.Errorf("set expiry audit context: %w", err)
	}
	_ = set_config
	if _, err := tx.Exec(ctx, `
		UPDATE bookings SET completed_at = COALESCE(completed_at, now())
		WHERE id = $1
	`, bookingID); err != nil {
		return false, fmt.Errorf("stamp expiry completion: %w", err)
	}
	if err := s.wallet.SettleBookingHoldTx(ctx, tx, bookingID, 0, "dispute-window-expiry"); err != nil {
		return false, err
	}
	result, err := tx.Exec(ctx, `
		UPDATE bookings SET status = 'completed', updated_at = now()
		WHERE id = $1 AND status = 'completion_pending'
	`, bookingID)
	if err != nil {
		return false, fmt.Errorf("complete expiry booking: %w", err)
	}
	if result.RowsAffected() != 1 {
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit dispute expiry: %w", err)
	}
	return true, nil
}

func openDisputeIDTx(ctx context.Context, tx pgx.Tx, bookingID int64) (int64, error) {
	var disputeID int64
	if err := tx.QueryRow(ctx, `SELECT id FROM booking_disputes WHERE booking_id = $1`, bookingID).Scan(&disputeID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrDisputeNotFound
		}
		return 0, fmt.Errorf("read booking dispute: %w", err)
	}
	return disputeID, nil
}

func closeDisputeTx(ctx context.Context, tx pgx.Tx, disputeID, adminID int64, resolution, note string) error {
	result, err := tx.Exec(ctx, `
		UPDATE booking_disputes
		SET resolved_at = now(), resolved_by = $1, resolution = $2, resolution_note = $3
		WHERE id = $4 AND resolved_at IS NULL
	`, adminID, resolution, note, disputeID)
	if err != nil {
		return fmt.Errorf("close booking dispute: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrInvalidTransition
	}
	return nil
}
