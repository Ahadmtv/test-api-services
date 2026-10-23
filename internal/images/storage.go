package images

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var storageKeyPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.jpg$`)

// Storage writes immutable, server-named image files below one configured
// directory. Callers never supply a path, and every key is re-validated before
// it is joined to the root.
type Storage struct {
	dir string
}

func NewStorage(dir string) (*Storage, error) {
	if dir == "" {
		return nil, fmt.Errorf("image storage directory is empty")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve image storage directory: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o750); err != nil {
		return nil, fmt.Errorf("create image storage directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve image storage symlinks: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("stat image storage directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("image storage path is not a directory")
	}
	return &Storage{dir: filepath.Clean(resolved)}, nil
}

func (s *Storage) Dir() string {
	return s.dir
}

// Save publishes data atomically under a validated key. The final path is
// deliberately re-derived from the key instead of accepting a caller path.
func (s *Storage) Save(key string, data []byte) error {
	finalPath, err := s.path(key)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(finalPath); err == nil {
		return fmt.Errorf("%w: storage key already exists", ErrStorageFailure)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: inspect target: %v", ErrStorageFailure, err)
	}

	temp, err := os.CreateTemp(s.dir, ".upload-*")
	if err != nil {
		return fmt.Errorf("%w: create temporary file: %v", ErrStorageFailure, err)
	}
	tempName := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempName)
	}
	if err := temp.Chmod(0o640); err != nil {
		cleanup()
		return fmt.Errorf("%w: set permissions: %v", ErrStorageFailure, err)
	}
	if _, err := temp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("%w: write file: %v", ErrStorageFailure, err)
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("%w: sync file: %v", ErrStorageFailure, err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("%w: close file: %v", ErrStorageFailure, err)
	}
	if err := os.Rename(tempName, finalPath); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("%w: publish file: %v", ErrStorageFailure, err)
	}
	return nil
}

// Remove deletes exactly one validated regular file. Symlinks are never
// followed; a suspicious entry is left untouched and reported.
func (s *Storage) Remove(key string) error {
	path, err := s.path(key)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: inspect file: %v", ErrStorageFailure, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: refusing to remove a non-regular file", ErrStorageFailure)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: remove file: %v", ErrStorageFailure, err)
	}
	return nil
}

func (s *Storage) path(key string) (string, error) {
	if !storageKeyPattern.MatchString(key) {
		return "", fmt.Errorf("%w: invalid storage key", ErrStorageFailure)
	}
	path := filepath.Join(s.dir, key)
	if filepath.Dir(filepath.Clean(path)) != s.dir {
		return "", fmt.Errorf("%w: storage key escapes the image directory", ErrStorageFailure)
	}
	return path, nil
}
