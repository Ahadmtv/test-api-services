package images

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestStorage_New(t *testing.T) {
	tempDir := t.TempDir()

	storage, err := NewStorage(tempDir)
	if err != nil {
		t.Fatalf("unexpected error creating storage: %v", err)
	}
	if storage.Dir() == "" {
		t.Errorf("expected non-empty dir")
	}

	_, err = NewStorage("")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected error for empty dir, got %v", err)
	}
}

func TestStorage_SaveAndRemove(t *testing.T) {
	tempDir := t.TempDir()
	storage, err := NewStorage(tempDir)
	if err != nil {
		t.Fatalf("NewStorage failed: %v", err)
	}

	key := uuid.NewString() + ".jpg"
	content := []byte("fake-jpeg-data")

	// Save
	if err := storage.Save(key, content); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Verify content on disk
	filePath := filepath.Join(storage.Dir(), key)
	readData, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(readData) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", readData, content)
	}

	// Collision protection: saving with same key must fail
	err = storage.Save(key, content)
	if err == nil || !errors.Is(err, ErrStorageFailure) || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected collision error, got %v", err)
	}

	// Remove
	if err := storage.Remove(key); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	// Verify removed
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Errorf("file still exists after remove")
	}

	// Remove non-existent file is safe (idempotent)
	if err := storage.Remove(key); err != nil {
		t.Errorf("expected nil error on removing absent file, got %v", err)
	}
}

func TestStorage_InvalidKey(t *testing.T) {
	tempDir := t.TempDir()
	storage, err := NewStorage(tempDir)
	if err != nil {
		t.Fatalf("NewStorage failed: %v", err)
	}

	invalidKeys := []string{
		"evil.jpg",
		"../escape.jpg",
		"../../etc/passwd",
		uuid.NewString() + ".png",
		uuid.NewString(),
		"/absolute/path.jpg",
		"",
	}

	for _, k := range invalidKeys {
		err := storage.Save(k, []byte("data"))
		if err == nil || !errors.Is(err, ErrStorageFailure) {
			t.Errorf("Save(%q): expected ErrStorageFailure, got %v", k, err)
		}

		err = storage.Remove(k)
		if err == nil || !errors.Is(err, ErrStorageFailure) {
			t.Errorf("Remove(%q): expected ErrStorageFailure, got %v", k, err)
		}
	}
}

func TestStorage_NonDirectory(t *testing.T) {
	tempFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(tempFile, []byte("hello"), 0640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := NewStorage(tempFile)
	if err == nil {
		t.Errorf("expected error when storage directory is a file, got nil")
	}
}

func TestStorage_RefuseRemoveDirectoryOrSymlink(t *testing.T) {
	tempDir := t.TempDir()
	storage, err := NewStorage(tempDir)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}

	// Try removing a subdirectory that happens to be named like a UUID key
	subDirKey := uuid.NewString() + ".jpg"
	subDirPath := filepath.Join(storage.Dir(), subDirKey)
	if err := os.Mkdir(subDirPath, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	err = storage.Remove(subDirKey)
	if err == nil || !errors.Is(err, ErrStorageFailure) {
		t.Errorf("expected ErrStorageFailure on removing directory, got %v", err)
	}
}
