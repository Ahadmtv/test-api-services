package images

import "errors"

// Domain errors are mapped to stable problem codes by the HTTP handler.
var (
	ErrEmptyFile       = errors.New("image file is empty")
	ErrFileTooLarge    = errors.New("image file is too large")
	ErrUnsupportedType = errors.New("unsupported image type")
	ErrInvalidImage    = errors.New("invalid image")
	ErrImageTooSmall   = errors.New("image dimensions are too small")
	ErrImageNotFound   = errors.New("image not found")
	ErrInvalidPurpose  = errors.New("invalid image purpose")
	ErrStorageFailure  = errors.New("image storage failure")
)

// Purpose identifies the single image slot owned by a user.
type Purpose string

const (
	PurposeProfile Purpose = "profile"
	PurposeCover   Purpose = "cover"
)

func (p Purpose) Valid() bool {
	return p == PurposeProfile || p == PurposeCover
}
