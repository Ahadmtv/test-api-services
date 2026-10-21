package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	CategoryHaircut     = "haircut"
	CategoryHairColor   = "hair-color"
	CategoryHairCare    = "hair-care"
	CategoryMakeup      = "makeup"
	CategoryEyebrowLash = "eyebrow-lash"
	CategoryNails       = "nails"
	CategorySkinCare    = "skin-care"
	CategoryWaxing      = "waxing"
	CategoryMassage     = "massage"
	CategorySpa         = "spa"
	CategoryBridal      = "bridal"
	CategoryBarbering   = "barbering"
	CategoryOther       = "other"
)

var serviceCategories = []string{
	CategoryHaircut, CategoryHairColor, CategoryHairCare, CategoryMakeup,
	CategoryEyebrowLash, CategoryNails, CategorySkinCare, CategoryWaxing,
	CategoryMassage, CategorySpa, CategoryBridal, CategoryBarbering, CategoryOther,
}

type ServiceItem struct {
	ID                  int64  `json:"id"`
	ProviderID          int64  `json:"provider_id"`
	Name                string `json:"name"`
	Description         string `json:"description"`
	Category            string `json:"category"`
	DurationMinutes     int    `json:"duration_minutes"`
	PriceIRR            int64  `json:"price_irr"`
	BufferMinutes       int    `json:"buffer_minutes"`
	IsActive            bool   `json:"is_active"`
	ProviderDisplayName string `json:"provider_display_name"`
}

type CreateInput struct {
	Name            string
	Description     string
	Category        string
	DurationMinutes int
	PriceIRR        int64
	BufferMinutes   int
	IsActive        bool
}

type UpdateInput struct {
	Name            *string
	Description     *string
	Category        *string
	DurationMinutes *int
	PriceIRR        *int64
	BufferMinutes   *int
	IsActive        *bool
}

var ErrServiceNotFound = errors.New("service not found")
var ErrInvalidService = errors.New("invalid service")

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func IsValidCategory(category string) bool {
	for _, candidate := range serviceCategories {
		if category == candidate {
			return true
		}
	}
	return false
}

// NormalizeCategory keeps older clients working while classifying services
// whose category has not been reviewed as explicitly "other".
func NormalizeCategory(category string) (string, error) {
	normalized := strings.TrimSpace(category)
	if normalized == "" {
		return CategoryOther, nil
	}
	if !IsValidCategory(normalized) {
		return "", ErrInvalidService
	}
	return normalized, nil
}

func Validate(name string, duration int, price int64, buffer int) error {
	if strings.TrimSpace(name) == "" || len([]rune(name)) > 160 || duration < 1 || duration > 1440 || price < 0 || buffer < 0 || buffer > 1440 {
		return ErrInvalidService
	}
	return nil
}

func (s *Store) Create(ctx context.Context, providerID int64, input CreateInput) (*ServiceItem, error) {
	validated := input
	validated.Name = strings.TrimSpace(input.Name)
	category, err := NormalizeCategory(input.Category)
	if err != nil {
		return nil, err
	}
	validated.Category = category
	if err := Validate(validated.Name, validated.DurationMinutes, validated.PriceIRR, validated.BufferMinutes); err != nil {
		return nil, err
	}
	var item ServiceItem
	err = s.pool.QueryRow(ctx, `
		INSERT INTO services (provider_id, name, description, category, duration_minutes, price_irr, buffer_minutes, is_active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, provider_id, name, description, category, duration_minutes, price_irr, buffer_minutes, is_active,
			(SELECT pp.display_name FROM provider_profiles pp WHERE pp.provider_id = services.provider_id)
	`, providerID, validated.Name, validated.Description, validated.Category, validated.DurationMinutes, validated.PriceIRR, validated.BufferMinutes, validated.IsActive).
		Scan(&item.ID, &item.ProviderID, &item.Name, &item.Description, &item.Category, &item.DurationMinutes, &item.PriceIRR, &item.BufferMinutes, &item.IsActive, &item.ProviderDisplayName)
	if err != nil {
		return nil, fmt.Errorf("create service: %w", err)
	}
	return &item, nil
}

func (s *Store) GetPublic(ctx context.Context, id int64) (*ServiceItem, error) {
	item, err := scanService(s.pool.QueryRow(ctx, `
		SELECT s.id, s.provider_id, s.name, s.description, s.category, s.duration_minutes, s.price_irr, s.buffer_minutes, s.is_active, p.display_name
		FROM services s
		JOIN provider_profiles p ON p.provider_id = s.provider_id
		WHERE s.id = $1 AND s.is_active AND p.is_active AND p.verification_status = 'verified'
	`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrServiceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get public service: %w", err)
	}
	return item, nil
}

func (s *Store) GetPublicForProvider(ctx context.Context, providerID, serviceID int64) (*ServiceItem, error) {
	item, err := scanService(s.pool.QueryRow(ctx, `
		SELECT s.id, s.provider_id, s.name, s.description, s.category, s.duration_minutes, s.price_irr, s.buffer_minutes, s.is_active, p.display_name
		FROM services s
		JOIN provider_profiles p ON p.provider_id = s.provider_id
		WHERE s.id = $1 AND s.provider_id = $2
			AND s.is_active AND p.is_active AND p.verification_status = 'verified'
	`, serviceID, providerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrServiceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get availability service: %w", err)
	}
	return item, nil
}

func (s *Store) BookingProviderIDTx(ctx context.Context, tx pgx.Tx, serviceID int64) (int64, error) {
	var providerID int64
	if err := tx.QueryRow(ctx, `SELECT provider_id FROM services WHERE id = $1`, serviceID).Scan(&providerID); errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrServiceNotFound
	} else if err != nil {
		return 0, fmt.Errorf("get service provider for booking: %w", err)
	}
	return providerID, nil
}

func (s *Store) GetForBookingTx(ctx context.Context, tx pgx.Tx, providerID, serviceID int64) (*ServiceItem, error) {
	item, err := scanService(tx.QueryRow(ctx, `
		SELECT s.id, s.provider_id, s.name, s.description, s.category, s.duration_minutes, s.price_irr, s.buffer_minutes, s.is_active, p.display_name
		FROM services s
		JOIN provider_profiles p ON p.provider_id = s.provider_id
		WHERE s.id = $1 AND s.provider_id = $2
			AND s.is_active AND p.is_active AND p.verification_status = 'verified'
		FOR SHARE OF s
	`, serviceID, providerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrServiceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get service snapshot for booking: %w", err)
	}
	return item, nil
}

func (s *Store) ListPublic(ctx context.Context, category string, after int64, limit int) ([]ServiceItem, bool, error) {
	return s.list(ctx, nil, category, after, limit)
}

func (s *Store) ListOwned(ctx context.Context, providerID, after int64, limit int) ([]ServiceItem, bool, error) {
	return s.list(ctx, &providerID, "", after, limit)
}

func (s *Store) list(ctx context.Context, providerID *int64, category string, after int64, limit int) ([]ServiceItem, bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.id, s.provider_id, s.name, s.description, s.category, s.duration_minutes, s.price_irr, s.buffer_minutes, s.is_active, p.display_name
		FROM services s
		JOIN provider_profiles p ON p.provider_id = s.provider_id
		WHERE s.id > $1 AND ($2::bigint IS NULL OR s.provider_id = $2)
			AND ($3 = '' OR s.category = $3)
			AND ($2::bigint IS NOT NULL OR (s.is_active AND p.is_active AND p.verification_status = 'verified'))
		ORDER BY s.id LIMIT $4
	`, after, providerID, category, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("list services: %w", err)
	}
	defer rows.Close()

	items := make([]ServiceItem, 0, limit)
	for rows.Next() {
		var item ServiceItem
		if err := rows.Scan(&item.ID, &item.ProviderID, &item.Name, &item.Description, &item.Category, &item.DurationMinutes, &item.PriceIRR, &item.BufferMinutes, &item.IsActive, &item.ProviderDisplayName); err != nil {
			return nil, false, err
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

func (s *Store) Deactivate(ctx context.Context, providerID, serviceID int64) error {
	result, err := s.pool.Exec(ctx, `
		UPDATE services SET is_active = FALSE, updated_at = now()
		WHERE id = $1 AND provider_id = $2
	`, serviceID, providerID)
	if err != nil {
		return fmt.Errorf("deactivate service: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrServiceNotFound
	}
	return nil
}

func (s *Store) Update(ctx context.Context, providerID, serviceID int64, input UpdateInput) (*ServiceItem, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin service update: %w", err)
	}
	defer tx.Rollback(ctx)

	item, err := scanService(tx.QueryRow(ctx, `
		SELECT s.id, s.provider_id, s.name, s.description, s.category, s.duration_minutes, s.price_irr, s.buffer_minutes, s.is_active,
			(SELECT pp.display_name FROM provider_profiles pp WHERE pp.provider_id = s.provider_id)
		FROM services s WHERE s.id = $1 AND s.provider_id = $2 FOR UPDATE
	`, serviceID, providerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrServiceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read service for update: %w", err)
	}
	if input.Name != nil {
		item.Name = *input.Name
	}
	if input.Description != nil {
		item.Description = *input.Description
	}
	if input.Category != nil {
		category := strings.TrimSpace(*input.Category)
		if !IsValidCategory(category) {
			return nil, ErrInvalidService
		}
		item.Category = category
	}
	if input.DurationMinutes != nil {
		item.DurationMinutes = *input.DurationMinutes
	}
	if input.PriceIRR != nil {
		item.PriceIRR = *input.PriceIRR
	}
	if input.BufferMinutes != nil {
		item.BufferMinutes = *input.BufferMinutes
	}
	if input.IsActive != nil {
		item.IsActive = *input.IsActive
	}
	if err := Validate(item.Name, item.DurationMinutes, item.PriceIRR, item.BufferMinutes); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE services SET name = $1, description = $2, category = $3, duration_minutes = $4, price_irr = $5, buffer_minutes = $6, is_active = $7, updated_at = now()
		WHERE id = $8 AND provider_id = $9
	`, strings.TrimSpace(item.Name), item.Description, item.Category, item.DurationMinutes, item.PriceIRR, item.BufferMinutes, item.IsActive, serviceID, providerID); err != nil {
		return nil, fmt.Errorf("update service: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit service update: %w", err)
	}
	item.Name = strings.TrimSpace(item.Name)
	return item, nil
}

func scanService(row pgx.Row) (*ServiceItem, error) {
	var item ServiceItem
	err := row.Scan(&item.ID, &item.ProviderID, &item.Name, &item.Description, &item.Category, &item.DurationMinutes, &item.PriceIRR, &item.BufferMinutes, &item.IsActive, &item.ProviderDisplayName)
	if err != nil {
		return nil, err
	}
	return &item, nil
}
