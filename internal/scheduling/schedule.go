package scheduling

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type WorkingInterval struct {
	Weekday  int    `json:"weekday"`
	OpensAt  string `json:"opens_at"`
	ClosesAt string `json:"closes_at"`
}

type TimeOff struct {
	ID          int64     `json:"id"`
	Kind        string    `json:"kind"`
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      time.Time `json:"ends_at"`
	Reason      string    `json:"reason"`
	HolidayDate *string   `json:"holiday_date,omitempty"`
}

type OccupancyReader interface {
	ListOccupied(context.Context, int64, time.Time, time.Time) ([]TimeInterval, error)
}

type EmptyOccupancyReader struct{}

func (EmptyOccupancyReader) ListOccupied(context.Context, int64, time.Time, time.Time) ([]TimeInterval, error) {
	return []TimeInterval{}, nil
}

type OccupancyStore struct {
	pool *pgxpool.Pool
}

func NewOccupancyStore(pool *pgxpool.Pool) *OccupancyStore {
	return &OccupancyStore{pool: pool}
}

func (o *OccupancyStore) ListOccupied(ctx context.Context, providerID int64, startAt, endAt time.Time) ([]TimeInterval, error) {
	rows, err := o.pool.Query(ctx, `
		SELECT starts_at, ends_at + buffer_minutes * INTERVAL '1 minute'
		FROM bookings
		WHERE provider_id = $1 AND status IN ('pending_payment', 'confirmed', 'in_progress', 'completion_pending', 'disputed')
		  AND starts_at < $3 AND ends_at + buffer_minutes * INTERVAL '1 minute' > $2
		ORDER BY starts_at
	`, providerID, startAt.UTC(), endAt.UTC())
	if err != nil {
		return nil, fmt.Errorf("list occupied intervals: %w", err)
	}
	defer rows.Close()
	intervals := make([]TimeInterval, 0)
	for rows.Next() {
		var interval TimeInterval
		if err := rows.Scan(&interval.StartAt, &interval.EndAt); err != nil {
			return nil, err
		}
		interval.StartAt = interval.StartAt.UTC()
		interval.EndAt = interval.EndAt.UTC()
		intervals = append(intervals, interval)
	}
	return intervals, rows.Err()
}

var ErrInvalidSchedule = errors.New("invalid schedule")
var ErrProviderNotFound = errors.New("provider profile not found")
var ErrInvalidTimeOff = errors.New("invalid time-off interval")
var ErrScheduleConflict = errors.New("schedule conflicts with an active booking")

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func ValidateWorkingHours(intervals []WorkingInterval) error {
	byDay := make(map[int][]WorkingInterval)
	for _, interval := range intervals {
		if interval.Weekday < 1 || interval.Weekday > 7 {
			return ErrInvalidSchedule
		}
		open, err := time.Parse("15:04", interval.OpensAt)
		if err != nil {
			return ErrInvalidSchedule
		}
		close, err := time.Parse("15:04", interval.ClosesAt)
		if err != nil || !open.Before(close) {
			return ErrInvalidSchedule
		}
		byDay[interval.Weekday] = append(byDay[interval.Weekday], interval)
	}
	for _, day := range byDay {
		sort.Slice(day, func(i, j int) bool { return day[i].OpensAt < day[j].OpensAt })
		for i := 1; i < len(day); i++ {
			if day[i].OpensAt < day[i-1].ClosesAt {
				return ErrInvalidSchedule
			}
		}
	}
	return nil
}

func (s *Store) GetWorkingHours(ctx context.Context, providerID int64) ([]WorkingInterval, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT weekday, to_char(opens_at, 'HH24:MI'), to_char(closes_at, 'HH24:MI')
		FROM weekly_working_hours WHERE provider_id = $1
		ORDER BY weekday, opens_at
	`, providerID)
	if err != nil {
		return nil, fmt.Errorf("get weekly working hours: %w", err)
	}
	defer rows.Close()
	intervals := make([]WorkingInterval, 0)
	for rows.Next() {
		var interval WorkingInterval
		if err := rows.Scan(&interval.Weekday, &interval.OpensAt, &interval.ClosesAt); err != nil {
			return nil, err
		}
		intervals = append(intervals, interval)
	}
	return intervals, rows.Err()
}

func (s *Store) GetWorkingHoursTx(ctx context.Context, tx pgx.Tx, providerID int64) ([]WorkingInterval, error) {
	rows, err := tx.Query(ctx, `
		SELECT weekday, to_char(opens_at, 'HH24:MI'), to_char(closes_at, 'HH24:MI')
		FROM weekly_working_hours WHERE provider_id = $1
		ORDER BY weekday, opens_at
	`, providerID)
	if err != nil {
		return nil, fmt.Errorf("get weekly working hours in booking transaction: %w", err)
	}
	defer rows.Close()
	intervals := make([]WorkingInterval, 0)
	for rows.Next() {
		var interval WorkingInterval
		if err := rows.Scan(&interval.Weekday, &interval.OpensAt, &interval.ClosesAt); err != nil {
			return nil, err
		}
		intervals = append(intervals, interval)
	}
	return intervals, rows.Err()
}

func (s *Store) PutWorkingHours(ctx context.Context, providerID int64, intervals []WorkingInterval) error {
	if err := ValidateWorkingHours(intervals); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin weekly hours update: %w", err)
	}
	defer tx.Rollback(ctx)
	var lockedProviderID int64
	var timeZone string
	if err := tx.QueryRow(ctx, `SELECT provider_id, time_zone FROM provider_profiles WHERE provider_id = $1 FOR UPDATE`, providerID).Scan(&lockedProviderID, &timeZone); errors.Is(err, pgx.ErrNoRows) {
		return ErrProviderNotFound
	} else if err != nil {
		return fmt.Errorf("lock provider profile: %w", err)
	}
	if err := activeBookingsFitHours(ctx, tx, providerID, timeZone, intervals); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM weekly_working_hours WHERE provider_id = $1`, providerID); err != nil {
		return fmt.Errorf("replace weekly working hours: %w", err)
	}
	for _, interval := range intervals {
		if _, err := tx.Exec(ctx, `
			INSERT INTO weekly_working_hours (provider_id, weekday, opens_at, closes_at)
			VALUES ($1, $2, $3::time, $4::time)
		`, providerID, interval.Weekday, interval.OpensAt, interval.ClosesAt); err != nil {
			return fmt.Errorf("insert weekly working hours: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit weekly hours update: %w", err)
	}
	return nil
}

func ValidateTimeOff(kind string, startsAt, endsAt time.Time, reason string) error {
	if (kind != "time_off" && kind != "blocked") || !startsAt.Before(endsAt) || len([]rune(reason)) > 500 {
		return ErrInvalidTimeOff
	}
	return nil
}

func (s *Store) CreateHoliday(ctx context.Context, providerID int64, date string) (*TimeOff, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin provider holiday creation: %w", err)
	}
	defer tx.Rollback(ctx)
	var timeZone string
	if err := tx.QueryRow(ctx, `SELECT time_zone FROM provider_profiles WHERE provider_id = $1 FOR UPDATE`, providerID).Scan(&timeZone); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProviderNotFound
	} else if err != nil {
		return nil, fmt.Errorf("lock provider profile: %w", err)
	}
	dayRange, err := LocalDayRange(date, timeZone)
	if err != nil {
		return nil, ErrInvalidLocalDate
	}
	if err := ensureNoActiveBookingConflict(ctx, tx, providerID, dayRange.StartAt, dayRange.EndAt); err != nil {
		return nil, err
	}
	var item TimeOff
	err = tx.QueryRow(ctx, `
		INSERT INTO provider_time_off (provider_id, kind, starts_at, ends_at, reason, holiday_date)
		VALUES ($1, 'holiday', $2, $3, '', $4::date)
		RETURNING id, kind, starts_at, ends_at, reason, holiday_date::text
	`, providerID, dayRange.StartAt, dayRange.EndAt, date).Scan(&item.ID, &item.Kind, &item.StartsAt, &item.EndsAt, &item.Reason, &item.HolidayDate)
	if err != nil {
		return nil, fmt.Errorf("create provider holiday: %w", err)
	}
	item.StartsAt = item.StartsAt.UTC()
	item.EndsAt = item.EndsAt.UTC()
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit provider holiday: %w", err)
	}
	return &item, nil
}

func (s *Store) ListAvailabilityIntervals(ctx context.Context, providerID int64, startAt, endAt time.Time) ([]TimeInterval, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT starts_at, ends_at
		FROM provider_time_off
		WHERE provider_id = $1 AND starts_at < $3 AND ends_at > $2
		ORDER BY starts_at
	`, providerID, startAt.UTC(), endAt.UTC())
	if err != nil {
		return nil, fmt.Errorf("list provider availability intervals: %w", err)
	}
	defer rows.Close()
	intervals := make([]TimeInterval, 0)
	for rows.Next() {
		var interval TimeInterval
		if err := rows.Scan(&interval.StartAt, &interval.EndAt); err != nil {
			return nil, err
		}
		interval.StartAt = interval.StartAt.UTC()
		interval.EndAt = interval.EndAt.UTC()
		intervals = append(intervals, interval)
	}
	return intervals, rows.Err()
}

func (s *Store) ListAvailabilityIntervalsTx(ctx context.Context, tx pgx.Tx, providerID int64, startAt, endAt time.Time) ([]TimeInterval, error) {
	rows, err := tx.Query(ctx, `
		SELECT starts_at, ends_at
		FROM provider_time_off
		WHERE provider_id = $1 AND starts_at < $3 AND ends_at > $2
		ORDER BY starts_at
	`, providerID, startAt.UTC(), endAt.UTC())
	if err != nil {
		return nil, fmt.Errorf("list provider availability intervals in booking transaction: %w", err)
	}
	defer rows.Close()
	intervals := make([]TimeInterval, 0)
	for rows.Next() {
		var interval TimeInterval
		if err := rows.Scan(&interval.StartAt, &interval.EndAt); err != nil {
			return nil, err
		}
		interval.StartAt = interval.StartAt.UTC()
		interval.EndAt = interval.EndAt.UTC()
		intervals = append(intervals, interval)
	}
	return intervals, rows.Err()
}

func (s *Store) CreateTimeOff(ctx context.Context, providerID int64, kind string, startsAt, endsAt time.Time, reason string) (*TimeOff, error) {
	if err := ValidateTimeOff(kind, startsAt, endsAt, reason); err != nil {
		return nil, err
	}
	startsAt = startsAt.UTC()
	endsAt = endsAt.UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin time-off creation: %w", err)
	}
	defer tx.Rollback(ctx)
	var lockedProviderID int64
	if err := tx.QueryRow(ctx, `SELECT provider_id FROM provider_profiles WHERE provider_id = $1 FOR UPDATE`, providerID).Scan(&lockedProviderID); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProviderNotFound
	} else if err != nil {
		return nil, fmt.Errorf("lock provider profile: %w", err)
	}
	if err := ensureNoActiveBookingConflict(ctx, tx, providerID, startsAt, endsAt); err != nil {
		return nil, err
	}
	var item TimeOff
	var holidayDate sql.NullString
	err = tx.QueryRow(ctx, `
		INSERT INTO provider_time_off (provider_id, kind, starts_at, ends_at, reason)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, kind, starts_at, ends_at, reason, holiday_date::text
	`, providerID, kind, startsAt, endsAt, reason).Scan(&item.ID, &item.Kind, &item.StartsAt, &item.EndsAt, &item.Reason, &holidayDate)
	if holidayDate.Valid {
		item.HolidayDate = &holidayDate.String
	}
	if err != nil {
		return nil, fmt.Errorf("create provider time-off interval: %w", err)
	}
	item.StartsAt = item.StartsAt.UTC()
	item.EndsAt = item.EndsAt.UTC()
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit time-off creation: %w", err)
	}
	return &item, nil
}

func (s *Store) ListTimeOff(ctx context.Context, providerID, after int64, limit int) ([]TimeOff, bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, starts_at, ends_at, reason, holiday_date::text
		FROM provider_time_off WHERE provider_id = $1 AND id > $2
		ORDER BY id LIMIT $3
	`, providerID, after, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("list provider time-off: %w", err)
	}
	defer rows.Close()
	items := make([]TimeOff, 0, limit)
	for rows.Next() {
		var item TimeOff
		var holidayDate sql.NullString
		if err := rows.Scan(&item.ID, &item.Kind, &item.StartsAt, &item.EndsAt, &item.Reason, &holidayDate); err != nil {
			return nil, false, err
		}
		item.StartsAt = item.StartsAt.UTC()
		item.EndsAt = item.EndsAt.UTC()
		if holidayDate.Valid {
			item.HolidayDate = &holidayDate.String
		}
		items = append(items, item)
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

func activeBookingsFitHours(ctx context.Context, tx pgx.Tx, providerID int64, timeZone string, hours []WorkingInterval) error {
	location, err := time.LoadLocation(timeZone)
	if err != nil {
		return ErrInvalidSchedule
	}
	rows, err := tx.Query(ctx, `
		SELECT starts_at, ends_at + buffer_minutes * INTERVAL '1 minute'
		FROM bookings
		WHERE provider_id = $1
		  AND status IN ('pending_payment', 'confirmed', 'in_progress', 'completion_pending', 'disputed')
		  AND ends_at + buffer_minutes * INTERVAL '1 minute' > now()
	`, providerID)
	if err != nil {
		return fmt.Errorf("check bookings against working-hours update: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var startsAt, occupiedUntil time.Time
		if err := rows.Scan(&startsAt, &occupiedUntil); err != nil {
			return err
		}
		startLocal := startsAt.In(location)
		endLocal := occupiedUntil.In(location)
		if startLocal.Year() != endLocal.Year() || startLocal.YearDay() != endLocal.YearDay() {
			return ErrScheduleConflict
		}
		weekday := int(startLocal.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		startMinute := startLocal.Hour()*60 + startLocal.Minute()
		endMinute := endLocal.Hour()*60 + endLocal.Minute()
		if endLocal.Second() != 0 || endLocal.Nanosecond() != 0 {
			endMinute++
		}
		fits := false
		for _, interval := range hours {
			if interval.Weekday != weekday {
				continue
			}
			open := parseMinuteOfDay(interval.OpensAt)
			close := parseMinuteOfDay(interval.ClosesAt)
			if startMinute >= open && endMinute <= close {
				fits = true
				break
			}
		}
		if !fits {
			return ErrScheduleConflict
		}
	}
	return rows.Err()
}

func ensureNoActiveBookingConflict(ctx context.Context, tx pgx.Tx, providerID int64, startsAt, endsAt time.Time) error {
	var conflict bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM bookings
			WHERE provider_id = $1
			  AND status IN ('pending_payment', 'confirmed', 'in_progress', 'completion_pending', 'disputed')
			  AND starts_at < $3
			  AND ends_at + buffer_minutes * INTERVAL '1 minute' > $2
		)
	`, providerID, startsAt.UTC(), endsAt.UTC()).Scan(&conflict)
	if err != nil {
		return fmt.Errorf("check time-off against active bookings: %w", err)
	}
	if conflict {
		return ErrScheduleConflict
	}
	return nil
}
