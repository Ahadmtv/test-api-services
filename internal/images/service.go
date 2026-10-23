package images

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"test-api/internal/platform"
)

// Image is the public metadata returned to clients. It deliberately contains
// no filesystem path.
type Image struct {
	Purpose     string    `json:"purpose"`
	URL         string    `json:"url"`
	ContentType string    `json:"content_type"`
	Width       int       `json:"width"`
	Height      int       `json:"height"`
	ByteSize    int64     `json:"byte_size"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Set struct {
	Profile *Image `json:"profile"`
	Cover   *Image `json:"cover"`
}

type UploadResult struct {
	Image   *Image
	Created bool
}

type ImageStore interface {
	List(ctx context.Context, userID int64) ([]Record, error)
	Upsert(ctx context.Context, userID int64, purpose Purpose, storageKey, contentType string, width, height int, byteSize int64, requestID string) (*Record, string, error)
	Delete(ctx context.Context, userID int64, purpose Purpose, requestID string) (string, error)
}

type ServiceConfig struct {
	Processor *Processor
	Storage   *Storage
	Store     ImageStore
	Logger    *slog.Logger
}

type Service struct {
	processor *Processor
	storage   *Storage
	store     ImageStore
	logger    *slog.Logger
}

func NewService(cfg ServiceConfig) *Service {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{processor: cfg.Processor, storage: cfg.Storage, store: cfg.Store, logger: logger}
}

func (s *Service) List(ctx context.Context, userID int64) (*Set, error) {
	records, err := s.store.List(ctx, userID)
	if err != nil {
		return nil, err
	}
	set := &Set{}
	for i := range records {
		image := toImage(records[i])
		switch records[i].Purpose {
		case PurposeProfile:
			set.Profile = &image
		case PurposeCover:
			set.Cover = &image
		}
	}
	return set, nil
}

func (s *Service) Upload(ctx context.Context, userID int64, purpose Purpose, src io.Reader, declaredContentType, requestID string) (*UploadResult, error) {
	if !purpose.Valid() {
		return nil, ErrInvalidPurpose
	}
	result, err := s.processor.Process(ctx, src, declaredContentType, purpose)
	if err != nil {
		return nil, err
	}

	storageKey := uuid.NewString() + ".jpg"
	if err := s.storage.Save(storageKey, result.Data); err != nil {
		return nil, err
	}
	record, previous, err := s.store.Upsert(ctx, userID, purpose, storageKey, result.ContentType, result.Width, result.Height, int64(len(result.Data)), requestID)
	if err != nil {
		if removeErr := s.storage.Remove(storageKey); removeErr != nil {
			s.logger.Error("failed to remove unpublished image", "error", removeErr)
		}
		return nil, err
	}
	if previous != "" && previous != storageKey {
		if err := s.storage.Remove(previous); err != nil {
			s.logger.Warn("failed to remove replaced image", "purpose", purpose, "error", err)
		}
	}
	image := toImage(*record)
	return &UploadResult{
		Image:   &image,
		Created: previous == "",
	}, nil
}

func (s *Service) Delete(ctx context.Context, userID int64, purpose Purpose, requestID string) error {
	if !purpose.Valid() {
		return ErrInvalidPurpose
	}
	storageKey, err := s.store.Delete(ctx, userID, purpose, requestID)
	if errors.Is(err, ErrImageNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.storage.Remove(storageKey); err != nil {
		s.logger.Warn("failed to remove deleted image file", "purpose", purpose, "error", err)
	}
	return nil
}

func toImage(record Record) Image {
	return Image{
		Purpose:     string(record.Purpose),
		URL:         platform.PublicImageURL(record.StorageKey),
		ContentType: record.ContentType,
		Width:       record.Width,
		Height:      record.Height,
		ByteSize:    record.ByteSize,
		UpdatedAt:   record.UpdatedAt,
	}
}
