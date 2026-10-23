package providers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"test-api/internal/platform"
)

type Profile struct {
	ProviderID         int64  `json:"provider_id"`
	DisplayName        string `json:"display_name"`
	Description        string `json:"description"`
	TimeZone           string `json:"time_zone"`
	IsActive           bool   `json:"is_active"`
	VerificationStatus string `json:"verification_status"`
	ProfileImageURL    string `json:"profile_image_url"`
	CoverImageURL      string `json:"cover_image_url"`
}

var ErrProfileNotFound = errors.New("provider profile not found")
var ErrInvalidProfile = errors.New("invalid provider profile")
var ErrProfileIncomplete = errors.New("provider profile incomplete")

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

func ValidateProfile(displayName, timeZone string) error {
	if strings.TrimSpace(displayName) == "" || len([]rune(displayName)) > 160 {
		return ErrInvalidProfile
	}
	if timeZone == "" || timeZone == "Local" {
		return ErrInvalidProfile
	}
	if _, err := time.LoadLocation(timeZone); err != nil {
		return ErrInvalidProfile
	}
	return nil
}

func (s *Service) Get(ctx context.Context, providerID int64) (*Profile, error) {
	var profile Profile
	var profileImageKey, coverImageKey string
	err := s.pool.QueryRow(ctx, `
		SELECT provider_id, display_name, description, time_zone, is_active, verification_status,
		       COALESCE((SELECT storage_key FROM user_images WHERE user_id = provider_profiles.provider_id AND purpose = 'profile'), ''),
		       COALESCE((SELECT storage_key FROM user_images WHERE user_id = provider_profiles.provider_id AND purpose = 'cover'), '')
		FROM provider_profiles WHERE provider_id = $1
	`, providerID).Scan(&profile.ProviderID, &profile.DisplayName, &profile.Description, &profile.TimeZone, &profile.IsActive, &profile.VerificationStatus, &profileImageKey, &coverImageKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProfileNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get provider profile: %w", err)
	}
	profile.ProfileImageURL = platform.PublicImageURL(profileImageKey)
	profile.CoverImageURL = platform.PublicImageURL(coverImageKey)
	return &profile, nil
}

func (s *Service) LockForBookingTx(ctx context.Context, tx pgx.Tx, providerID int64) (*Profile, error) {
	var profile Profile
	err := tx.QueryRow(ctx, `
		SELECT provider_id, display_name, description, time_zone, is_active, verification_status
		FROM provider_profiles WHERE provider_id = $1 FOR UPDATE
	`, providerID).Scan(&profile.ProviderID, &profile.DisplayName, &profile.Description, &profile.TimeZone, &profile.IsActive, &profile.VerificationStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProfileNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock provider profile for booking: %w", err)
	}
	return &profile, nil
}

func (s *Service) SetVerificationStatus(ctx context.Context, actorID, providerID int64, status, requestID string) (*Profile, error) {
	if status != "pending" && status != "verified" && status != "rejected" {
		return nil, ErrInvalidProfile
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin provider verification: %w", err)
	}
	defer tx.Rollback(ctx)
	var displayName string
	err = tx.QueryRow(ctx, `
		SELECT display_name FROM provider_profiles WHERE provider_id = $1 FOR UPDATE
	`, providerID).Scan(&displayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProfileNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock provider profile for verification: %w", err)
	}
	if status == "verified" && strings.TrimSpace(displayName) == "" {
		return nil, ErrProfileIncomplete
	}
	var profile Profile
	err = tx.QueryRow(ctx, `
		UPDATE provider_profiles SET verification_status = $1, updated_at = now()
		WHERE provider_id = $2
		RETURNING provider_id, display_name, description, time_zone, is_active, verification_status
	`, status, providerID).Scan(&profile.ProviderID, &profile.DisplayName, &profile.Description, &profile.TimeZone, &profile.IsActive, &profile.VerificationStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProfileNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("set provider verification status: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_events (actor_user_id, action, target_type, target_id, request_id, metadata)
		VALUES ($1, 'provider.verification.updated', 'provider_profile', $2, $3, jsonb_build_object('verification_status', $4::text))
	`, actorID, providerID, requestID, status); err != nil {
		return nil, fmt.Errorf("audit provider verification: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit provider verification: %w", err)
	}
	return s.Get(ctx, providerID)
}

func (s *Service) Put(ctx context.Context, providerID int64, displayName, description, timeZone string, active bool) (*Profile, error) {
	if err := ValidateProfile(displayName, timeZone); err != nil {
		return nil, err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO provider_profiles (provider_id, display_name, description, time_zone, is_active)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (provider_id) DO UPDATE SET
		display_name = EXCLUDED.display_name,
		description = EXCLUDED.description,
		time_zone = EXCLUDED.time_zone,
		is_active = EXCLUDED.is_active,
		updated_at = now()
	`, providerID, strings.TrimSpace(displayName), description, timeZone, active)
	if err != nil {
		return nil, fmt.Errorf("save provider profile: %w", err)
	}
	return s.Get(ctx, providerID)
}

// AdminProfile is the administrator view of a provider profile with the
// owner's safe contact details; it never exposes internal credentials.
type AdminProfile struct {
	ProviderID         int64     `json:"provider_id"`
	DisplayName        string    `json:"display_name"`
	VerificationStatus string    `json:"verification_status"`
	IsActive           bool      `json:"is_active"`
	ProfileCompleted   bool      `json:"profile_completed"`
	OwnerEmail         string    `json:"owner_email"`
	OwnerPhone         *string   `json:"owner_phone"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type AdminProfilePage struct {
	Items      []AdminProfile `json:"items"`
	NextCursor *int64         `json:"next_cursor"`
}

// AdminList returns every provider profile for administrators with keyset
// pagination, an optional verification_status filter and a text search over
// display name, owner email and owner phone.
func (s *Service) AdminList(ctx context.Context, after int64, limit int, verificationStatus, query string) (*AdminProfilePage, error) {
	if after < 0 || limit < 1 || limit > 100 {
		return nil, ErrInvalidProfile
	}
	rows, err := s.pool.Query(ctx, `
		SELECT pp.provider_id, pp.display_name, pp.verification_status, pp.is_active,
		       btrim(pp.display_name) <> '', u.email, u.phone, pp.created_at, pp.updated_at
		FROM provider_profiles pp
		JOIN users u ON u.id = pp.provider_id
		WHERE ($1 = 0 OR pp.provider_id > $1)
		  AND ($2 = '' OR pp.verification_status = $2)
		  AND ($3 = '' OR pp.display_name ILIKE '%' || $3 || '%'
		       OR u.email ILIKE '%' || $3 || '%'
		       OR COALESCE(u.phone, '') ILIKE '%' || $3 || '%')
		ORDER BY pp.provider_id LIMIT $4
	`, after, verificationStatus, query, limit+1)
	if err != nil {
		return nil, fmt.Errorf("list provider profiles for admin: %w", err)
	}
	defer rows.Close()

	items := make([]AdminProfile, 0, limit)
	for rows.Next() {
		var item AdminProfile
		var phone sql.NullString
		if err := rows.Scan(&item.ProviderID, &item.DisplayName, &item.VerificationStatus, &item.IsActive, &item.ProfileCompleted, &item.OwnerEmail, &phone, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan provider profile for admin: %w", err)
		}
		if phone.Valid {
			item.OwnerPhone = &phone.String
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate provider profiles for admin: %w", err)
	}
	page := AdminProfilePage{Items: items}
	if len(items) > limit {
		items = items[:limit]
		page.Items = items
		if len(items) > 0 {
			cursor := items[len(items)-1].ProviderID
			page.NextCursor = &cursor
		}
	}
	return &page, nil
}
