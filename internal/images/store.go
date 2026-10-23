package images

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Record is the persisted metadata for one image slot.
type Record struct {
	Purpose     Purpose
	StorageKey  string
	ContentType string
	Width       int
	Height      int
	ByteSize    int64
	UpdatedAt   time.Time
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) List(ctx context.Context, userID int64) ([]Record, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT purpose, storage_key, content_type, width, height, byte_size, updated_at
		FROM user_images WHERE user_id = $1 ORDER BY purpose
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("list user images: %w", err)
	}
	defer rows.Close()

	records := make([]Record, 0, 2)
	for rows.Next() {
		var record Record
		if err := rows.Scan(&record.Purpose, &record.StorageKey, &record.ContentType, &record.Width, &record.Height, &record.ByteSize, &record.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan user image: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user images: %w", err)
	}
	return records, nil
}

// Upsert serializes concurrent replacements for one user/purpose and returns
// the new record together with the replaced storage key, if any.
func (s *Store) Upsert(ctx context.Context, userID int64, purpose Purpose, storageKey, contentType string, width, height int, byteSize int64, requestID string) (*Record, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("begin image metadata update: %w", err)
	}
	defer tx.Rollback(ctx)

	var previous sql.NullString
	err = tx.QueryRow(ctx, `
		SELECT storage_key FROM user_images WHERE user_id = $1 AND purpose = $2 FOR UPDATE
	`, userID, purpose).Scan(&previous)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, "", fmt.Errorf("lock image metadata: %w", err)
	}

	var record Record
	err = tx.QueryRow(ctx, `
		INSERT INTO user_images (user_id, purpose, storage_key, content_type, width, height, byte_size)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (user_id, purpose) DO UPDATE SET
			storage_key = EXCLUDED.storage_key,
			content_type = EXCLUDED.content_type,
			width = EXCLUDED.width,
			height = EXCLUDED.height,
			byte_size = EXCLUDED.byte_size,
			updated_at = now()
		RETURNING purpose, storage_key, content_type, width, height, byte_size, updated_at
	`, userID, purpose, storageKey, contentType, width, height, byteSize).
		Scan(&record.Purpose, &record.StorageKey, &record.ContentType, &record.Width, &record.Height, &record.ByteSize, &record.UpdatedAt)
	if err != nil {
		return nil, "", fmt.Errorf("save image metadata: %w", err)
	}

	if err := insertAudit(ctx, tx, userID, "user.image.updated", requestID, map[string]any{
		"purpose":   string(purpose),
		"width":     width,
		"height":    height,
		"byte_size": byteSize,
	}); err != nil {
		return nil, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "", fmt.Errorf("commit image metadata update: %w", err)
	}
	return &record, previous.String, nil
}

// Delete removes one slot and returns its storage key so the caller can remove
// the file after the metadata transaction commits.
func (s *Store) Delete(ctx context.Context, userID int64, purpose Purpose, requestID string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin image delete: %w", err)
	}
	defer tx.Rollback(ctx)

	var storageKey string
	err = tx.QueryRow(ctx, `
		SELECT storage_key FROM user_images WHERE user_id = $1 AND purpose = $2 FOR UPDATE
	`, userID, purpose).Scan(&storageKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrImageNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lock image for delete: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_images WHERE user_id = $1 AND purpose = $2`, userID, purpose); err != nil {
		return "", fmt.Errorf("delete image metadata: %w", err)
	}
	if err := insertAudit(ctx, tx, userID, "user.image.deleted", requestID, map[string]any{
		"purpose": string(purpose),
	}); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit image delete: %w", err)
	}
	return storageKey, nil
}

func insertAudit(ctx context.Context, tx pgx.Tx, userID int64, action, requestID string, metadata map[string]any) error {
	purpose, _ := metadata["purpose"].(string)
	if action == "user.image.updated" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO audit_events (actor_user_id, action, target_type, target_id, request_id, metadata)
			VALUES ($1, $2, 'user_image', $1, $3, jsonb_build_object(
				'purpose', $4::text,
				'width', $5::int,
				'height', $6::int,
				'byte_size', $7::bigint
			))
		`, userID, action, requestID, purpose, metadata["width"], metadata["height"], metadata["byte_size"]); err != nil {
			return fmt.Errorf("audit image update: %w", err)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_events (actor_user_id, action, target_type, target_id, request_id, metadata)
		VALUES ($1, $2, 'user_image', $1, $3, jsonb_build_object('purpose', $4::text))
	`, userID, action, requestID, purpose); err != nil {
		return fmt.Errorf("audit image delete: %w", err)
	}
	return nil
}
