package images

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStoreIntegration(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set; skipping images store integration test")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// Create test user if needed
	var userID int64
	err = pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES ('image_test_user@example.com', 'dummy_hash')
		ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
		RETURNING id
	`).Scan(&userID)
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM user_images WHERE user_id = $1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	}()

	store := NewStore(pool)

	// 1. Initial Upsert
	key1 := uuid.NewString() + ".jpg"
	rec, prev, err := store.Upsert(ctx, userID, PurposeProfile, key1, "image/jpeg", 256, 256, 12345, "req-1")
	if err != nil {
		t.Fatalf("Upsert 1 failed: %v", err)
	}
	if prev != "" {
		t.Errorf("expected empty previous key, got %q", prev)
	}
	if rec.StorageKey != key1 {
		t.Errorf("expected storage key %s, got %s", key1, rec.StorageKey)
	}

	// 2. List
	list, err := store.List(ctx, userID)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(list) != 1 || list[0].StorageKey != key1 {
		t.Errorf("expected 1 record with key %s, got %+v", key1, list)
	}

	// 3. Replacement Upsert
	key2 := uuid.NewString() + ".jpg"
	rec2, prev2, err := store.Upsert(ctx, userID, PurposeProfile, key2, "image/jpeg", 512, 512, 23456, "req-2")
	if err != nil {
		t.Fatalf("Upsert 2 failed: %v", err)
	}
	if prev2 != key1 {
		t.Errorf("expected prev2 == %s, got %s", key1, prev2)
	}
	if rec2.StorageKey != key2 {
		t.Errorf("expected key2 %s, got %s", key2, rec2.StorageKey)
	}

	// 4. Delete
	deletedKey, err := store.Delete(ctx, userID, PurposeProfile, "req-del")
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if deletedKey != key2 {
		t.Errorf("expected deleted key %s, got %s", key2, deletedKey)
	}

	// 5. Delete absent returns ErrImageNotFound
	_, err = store.Delete(ctx, userID, PurposeProfile, "req-del2")
	if err != ErrImageNotFound {
		t.Errorf("expected ErrImageNotFound, got %v", err)
	}
}
