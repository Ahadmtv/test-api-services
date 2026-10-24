package images

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type memoryStore struct {
	mu          sync.Mutex
	records     map[int64]map[Purpose]Record
	upsertErr   error
	deleteErr   error
	listErr     error
	auditEvents []string
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		records: make(map[int64]map[Purpose]Record),
	}
}

func (m *memoryStore) List(ctx context.Context, userID int64) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	userMap := m.records[userID]
	list := make([]Record, 0, len(userMap))
	for _, rec := range userMap {
		list = append(list, rec)
	}
	return list, nil
}

func (m *memoryStore) Upsert(ctx context.Context, userID int64, purpose Purpose, storageKey, contentType string, width, height int, byteSize int64, requestID string) (*Record, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.upsertErr != nil {
		return nil, "", m.upsertErr
	}
	userMap, ok := m.records[userID]
	if !ok {
		userMap = make(map[Purpose]Record)
		m.records[userID] = userMap
	}
	previous := userMap[purpose].StorageKey
	rec := Record{
		Purpose:     purpose,
		StorageKey:  storageKey,
		ContentType: contentType,
		Width:       width,
		Height:      height,
		ByteSize:    byteSize,
		UpdatedAt:   time.Now().UTC(),
	}
	userMap[purpose] = rec
	m.auditEvents = append(m.auditEvents, "user.image.updated")
	return &rec, previous, nil
}

func (m *memoryStore) Delete(ctx context.Context, userID int64, purpose Purpose, requestID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteErr != nil {
		return "", m.deleteErr
	}
	userMap, ok := m.records[userID]
	if !ok {
		return "", ErrImageNotFound
	}
	rec, ok := userMap[purpose]
	if !ok {
		return "", ErrImageNotFound
	}
	delete(userMap, purpose)
	m.auditEvents = append(m.auditEvents, "user.image.deleted")
	return rec.StorageKey, nil
}

func TestService_Upload_NewAndReplace(t *testing.T) {
	tempDir := t.TempDir()
	storage, err := NewStorage(tempDir)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	store := newMemoryStore()
	svc := NewService(ServiceConfig{
		Processor: NewProcessor(defaultTestProcessorConfig()),
		Storage:   storage,
		Store:     store,
	})

	userID := int64(42)
	imgData1 := createTestJPEG(t, 200, 200)

	// 1. Initial upload (Created == true)
	res1, err := svc.Upload(context.Background(), userID, PurposeProfile, bytes.NewReader(imgData1), "image/jpeg", "req-1")
	if err != nil {
		t.Fatalf("first upload failed: %v", err)
	}
	if !res1.Created {
		t.Errorf("expected res1.Created == true")
	}
	key1 := filepath.Base(res1.Image.URL)
	if _, err := os.Stat(filepath.Join(tempDir, key1)); err != nil {
		t.Errorf("expected file %s to exist on disk: %v", key1, err)
	}

	// 2. Replacement upload (Created == false)
	imgData2 := createTestJPEG(t, 250, 250)
	res2, err := svc.Upload(context.Background(), userID, PurposeProfile, bytes.NewReader(imgData2), "image/jpeg", "req-2")
	if err != nil {
		t.Fatalf("second upload failed: %v", err)
	}
	if res2.Created {
		t.Errorf("expected res2.Created == false on replacement")
	}
	key2 := filepath.Base(res2.Image.URL)
	if key1 == key2 {
		t.Errorf("expected new key on replacement")
	}

	// Verify old file is cleaned up and new file exists
	if _, err := os.Stat(filepath.Join(tempDir, key1)); !os.IsNotExist(err) {
		t.Errorf("expected old file %s to be deleted after replacement", key1)
	}
	if _, err := os.Stat(filepath.Join(tempDir, key2)); err != nil {
		t.Errorf("expected new file %s to exist: %v", key2, err)
	}
}

func TestService_Upload_RollbackOnStoreFailure(t *testing.T) {
	tempDir := t.TempDir()
	storage, err := NewStorage(tempDir)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	store := newMemoryStore()
	store.upsertErr = errors.New("db connection failure")

	svc := NewService(ServiceConfig{
		Processor: NewProcessor(defaultTestProcessorConfig()),
		Storage:   storage,
		Store:     store,
	})

	data := createTestJPEG(t, 200, 200)
	_, err = svc.Upload(context.Background(), 1, PurposeProfile, bytes.NewReader(data), "image/jpeg", "req-err")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	// Check that no orphaned files remain in tempDir
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected zero files in storage after rollback, found %d", len(entries))
	}
}

func TestService_List(t *testing.T) {
	tempDir := t.TempDir()
	storage, _ := NewStorage(tempDir)
	store := newMemoryStore()
	svc := NewService(ServiceConfig{
		Processor: NewProcessor(defaultTestProcessorConfig()),
		Storage:   storage,
		Store:     store,
	})

	// Empty list
	set, err := svc.List(context.Background(), 1)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if set.Profile != nil || set.Cover != nil {
		t.Errorf("expected nil profile and cover, got %+v", set)
	}

	// Add profile and cover
	data := createTestJPEG(t, 200, 200)
	_, _ = svc.Upload(context.Background(), 1, PurposeProfile, bytes.NewReader(data), "image/jpeg", "req-1")
	coverData := createTestJPEG(t, 640, 360)
	_, _ = svc.Upload(context.Background(), 1, PurposeCover, bytes.NewReader(coverData), "image/jpeg", "req-2")

	set, err = svc.List(context.Background(), 1)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if set.Profile == nil || set.Cover == nil {
		t.Fatalf("expected both profile and cover to be present, got %+v", set)
	}
	if set.Profile.Purpose != "profile" || set.Cover.Purpose != "cover" {
		t.Errorf("mismatched purposes in set: %+v", set)
	}
}

func TestService_Delete(t *testing.T) {
	tempDir := t.TempDir()
	storage, _ := NewStorage(tempDir)
	store := newMemoryStore()
	svc := NewService(ServiceConfig{
		Processor: NewProcessor(defaultTestProcessorConfig()),
		Storage:   storage,
		Store:     store,
	})

	data := createTestJPEG(t, 200, 200)
	res, err := svc.Upload(context.Background(), 1, PurposeProfile, bytes.NewReader(data), "image/jpeg", "req-del")
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	key := filepath.Base(res.Image.URL)

	// Delete existing
	if err := svc.Delete(context.Background(), 1, PurposeProfile, "req-del"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tempDir, key)); !os.IsNotExist(err) {
		t.Errorf("expected file %s to be deleted from disk", key)
	}

	// Delete non-existent is safe (no error)
	if err := svc.Delete(context.Background(), 1, PurposeProfile, "req-del-again"); err != nil {
		t.Errorf("expected nil error on deleting absent image, got %v", err)
	}

	// Delete with invalid purpose
	if err := svc.Delete(context.Background(), 1, Purpose("invalid"), "req"); err != ErrInvalidPurpose {
		t.Errorf("expected ErrInvalidPurpose, got %v", err)
	}
}
