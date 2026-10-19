package providers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Profile struct {
	ProviderID         int64  `json:"provider_id"`
	DisplayName        string `json:"display_name"`
	Description        string `json:"description"`
	TimeZone           string `json:"time_zone"`
	IsActive           bool   `json:"is_active"`
	VerificationStatus string `json:"verification_status"`
}

var ErrProfileNotFound = errors.New("provider profile not found")
var ErrInvalidProfile = errors.New("invalid provider profile")

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
	err := s.pool.QueryRow(ctx, `
		SELECT provider_id, display_name, description, time_zone, is_active, verification_status
		FROM provider_profiles WHERE provider_id = $1
	`, providerID).Scan(&profile.ProviderID, &profile.DisplayName, &profile.Description, &profile.TimeZone, &profile.IsActive, &profile.VerificationStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProfileNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get provider profile: %w", err)
	}
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
	return &profile, nil
}

func (s *Service) Put(ctx context.Context, providerID int64, displayName, description, timeZone string, active bool) (*Profile, error) {
	if err := ValidateProfile(displayName, timeZone); err != nil {
		return nil, err
	}
	var profile Profile
	err := s.pool.QueryRow(ctx, `
		INSERT INTO provider_profiles (provider_id, display_name, description, time_zone, is_active)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (provider_id) DO UPDATE SET
		display_name = EXCLUDED.display_name,
		description = EXCLUDED.description,
		time_zone = EXCLUDED.time_zone,
		is_active = EXCLUDED.is_active,
		updated_at = now()
		RETURNING provider_id, display_name, description, time_zone, is_active, verification_status
	`, providerID, strings.TrimSpace(displayName), description, timeZone, active).Scan(&profile.ProviderID, &profile.DisplayName, &profile.Description, &profile.TimeZone, &profile.IsActive, &profile.VerificationStatus)
	if err != nil {
		return nil, fmt.Errorf("save provider profile: %w", err)
	}
	return &profile, nil
}
