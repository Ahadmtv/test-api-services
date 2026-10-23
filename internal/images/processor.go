package images

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"mime"
	"net/http"
	"runtime"
	"strings"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	coverAspect      = 16.0 / 9.0
	minProfileSide   = 128
	minCoverHeight   = 360
	jpegQuality      = 85
	maxConcurrent    = 4
	decodedImageMime = "image/jpeg"
)

// ProcessorConfig bounds decoding work and output dimensions.
type ProcessorConfig struct {
	MaxUploadBytes      int64
	MaxInputDimension   int
	MaxInputPixels      int64
	ProfileMaxDimension int
	CoverMaxDimension   int
}

// ProcessResult is the normalized JPEG that will be stored.
type ProcessResult struct {
	Data        []byte
	Width       int
	Height      int
	ContentType string
}

// Processor validates, re-orients, crops, resizes and re-encodes images. Every
// output is a fresh JPEG, so source metadata (including EXIF/GPS) is discarded.
type Processor struct {
	cfg       ProcessorConfig
	semaphore chan struct{}
}

func NewProcessor(cfg ProcessorConfig) *Processor {
	workers := runtime.NumCPU()
	if workers > maxConcurrent {
		workers = maxConcurrent
	}
	if workers < 1 {
		workers = 1
	}
	return &Processor{cfg: cfg, semaphore: make(chan struct{}, workers)}
}

func (p *Processor) Process(ctx context.Context, src io.Reader, declaredContentType string, purpose Purpose) (*ProcessResult, error) {
	if !purpose.Valid() {
		return nil, ErrInvalidPurpose
	}
	if p.cfg.MaxUploadBytes < 1 || p.cfg.MaxInputDimension < 1 || p.cfg.MaxInputPixels < 1 {
		return nil, fmt.Errorf("invalid image processor configuration")
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	select {
	case p.semaphore <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-p.semaphore }()

	data, err := readLimited(src, p.cfg.MaxUploadBytes)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, ErrEmptyFile
	}

	actualType := detectImageType(data)
	if actualType == "" {
		return nil, ErrUnsupportedType
	}
	if declared := normalizeContentType(declaredContentType); declared != "" && declared != actualType {
		return nil, ErrUnsupportedType
	}

	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 {
		return nil, ErrInvalidImage
	}
	if config.Width > p.cfg.MaxInputDimension || config.Height > p.cfg.MaxInputDimension {
		return nil, ErrInvalidImage
	}
	if int64(config.Width)*int64(config.Height) > p.cfg.MaxInputPixels {
		return nil, ErrInvalidImage
	}

	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, ErrInvalidImage
	}
	if actualType == "image/jpeg" {
		decoded = applyOrientation(decoded, jpegOrientation(data))
	}

	bounds := decoded.Bounds()
	if bounds.Dx() < 1 || bounds.Dy() < 1 {
		return nil, ErrInvalidImage
	}
	switch purpose {
	case PurposeProfile:
		if min(bounds.Dx(), bounds.Dy()) < minProfileSide {
			return nil, ErrImageTooSmall
		}
	case PurposeCover:
		if min(bounds.Dx(), bounds.Dy()) < minCoverHeight {
			return nil, ErrImageTooSmall
		}
	}

	source, outputWidth, outputHeight := cropPlan(bounds, purpose, p.cfg)
	canvas := image.NewRGBA(image.Rect(0, 0, outputWidth, outputHeight))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.CatmullRom.Scale(canvas, canvas.Bounds(), decoded, source, draw.Over, nil)

	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, canvas, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, fmt.Errorf("encode normalized image: %w", err)
	}
	return &ProcessResult{
		Data:        encoded.Bytes(),
		Width:       outputWidth,
		Height:      outputHeight,
		ContentType: decodedImageMime,
	}, nil
}

func readLimited(src io.Reader, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(io.LimitReader(src, limit+1)); err != nil {
		return nil, fmt.Errorf("read image: %w", err)
	}
	if int64(buf.Len()) > limit {
		return nil, ErrFileTooLarge
	}
	return buf.Bytes(), nil
}

func detectImageType(data []byte) string {
	contentType := http.DetectContentType(data)
	switch contentType {
	case "image/jpeg", "image/png", "image/webp":
		return contentType
	}
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp"
	}
	return ""
}

func normalizeContentType(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return strings.ToLower(value)
	}
	mediaType = strings.ToLower(mediaType)
	switch mediaType {
	case "application/octet-stream":
		return ""
	case "image/jpg":
		return "image/jpeg"
	case "image/x-png":
		return "image/png"
	default:
		return mediaType
	}
}

func cropPlan(bounds image.Rectangle, purpose Purpose, cfg ProcessorConfig) (image.Rectangle, int, int) {
	width, height := bounds.Dx(), bounds.Dy()
	aspect := 1.0
	maxDimension := cfg.ProfileMaxDimension
	if purpose == PurposeCover {
		aspect = coverAspect
		maxDimension = cfg.CoverMaxDimension
	}

	sourceWidth, sourceHeight := width, height
	if float64(width)/float64(height) > aspect {
		sourceWidth = int(float64(height) * aspect)
	} else {
		sourceHeight = int(float64(width) / aspect)
	}
	if sourceWidth < 1 {
		sourceWidth = 1
	}
	if sourceHeight < 1 {
		sourceHeight = 1
	}
	source := image.Rect(
		bounds.Min.X+(width-sourceWidth)/2,
		bounds.Min.Y+(height-sourceHeight)/2,
		bounds.Min.X+(width-sourceWidth)/2+sourceWidth,
		bounds.Min.Y+(height-sourceHeight)/2+sourceHeight,
	)

	outputWidth, outputHeight := sourceWidth, sourceHeight
	if purpose == PurposeProfile {
		side := min(sourceWidth, sourceHeight)
		if side > maxDimension {
			side = maxDimension
		}
		outputWidth, outputHeight = side, side
	} else if outputWidth > maxDimension {
		outputWidth = maxDimension
		outputHeight = int(float64(outputWidth) / aspect)
	}
	if outputWidth < 1 {
		outputWidth = 1
	}
	if outputHeight < 1 {
		outputHeight = 1
	}
	return source, outputWidth, outputHeight
}

func applyOrientation(src image.Image, orientation int) image.Image {
	if orientation < 2 || orientation > 8 {
		return src
	}
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	outputWidth, outputHeight := width, height
	if orientation >= 5 {
		outputWidth, outputHeight = height, width
	}
	dst := image.NewNRGBA(image.Rect(0, 0, outputWidth, outputHeight))
	for y := 0; y < outputHeight; y++ {
		for x := 0; x < outputWidth; x++ {
			var sourceX, sourceY int
			switch orientation {
			case 2:
				sourceX, sourceY = width-1-x, y
			case 3:
				sourceX, sourceY = width-1-x, height-1-y
			case 4:
				sourceX, sourceY = x, height-1-y
			case 5:
				sourceX, sourceY = y, x
			case 6:
				sourceX, sourceY = y, height-1-x
			case 7:
				sourceX, sourceY = width-1-y, height-1-x
			case 8:
				sourceX, sourceY = width-1-y, x
			}
			dst.Set(x, y, src.At(bounds.Min.X+sourceX, bounds.Min.Y+sourceY))
		}
	}
	return dst
}

// jpegOrientation reads only the EXIF orientation tag. Malformed metadata is
// ignored and treated as the normal orientation.
func jpegOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	offset := 2
	for offset+4 <= len(data) {
		if data[offset] != 0xFF {
			offset++
			continue
		}
		marker := data[offset+1]
		offset += 2
		if marker == 0xDA || marker == 0xD9 {
			break
		}
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			continue
		}
		if offset+2 > len(data) {
			break
		}
		size := int(binary.BigEndian.Uint16(data[offset : offset+2]))
		if size < 2 || offset+size > len(data) {
			break
		}
		if marker == 0xE1 && size >= 8 && string(data[offset+2:offset+8]) == "Exif\x00\x00" {
			return tiffOrientation(data[offset+8 : offset+size])
		}
		offset += size
	}
	return 1
}

func tiffOrientation(data []byte) int {
	if len(data) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(data[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(data[2:4]) != 42 {
		return 1
	}
	offset := int(order.Uint32(data[4:8]))
	if offset < 0 || offset+2 > len(data) {
		return 1
	}
	count := int(order.Uint16(data[offset : offset+2]))
	offset += 2
	for i := 0; i < count; i++ {
		entry := offset + i*12
		if entry+12 > len(data) {
			return 1
		}
		if order.Uint16(data[entry:entry+2]) != 0x0112 {
			continue
		}
		if order.Uint16(data[entry+2:entry+4]) != 3 || order.Uint32(data[entry+4:entry+8]) != 1 {
			return 1
		}
		orientation := int(order.Uint16(data[entry+8 : entry+10]))
		if orientation >= 1 && orientation <= 8 {
			return orientation
		}
		return 1
	}
	return 1
}
