package images

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func createTestJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 100, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}
	return buf.Bytes()
}

func createTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: uint8(x % 256), B: uint8(y % 256), A: 128})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

func defaultTestProcessorConfig() ProcessorConfig {
	return ProcessorConfig{
		MaxUploadBytes:      5 << 20,
		MaxInputDimension:   4000,
		MaxInputPixels:      16_000_000,
		ProfileMaxDimension: 256,
		CoverMaxDimension:   800,
	}
}

func TestProcessor_ValidProfileJPEG(t *testing.T) {
	p := NewProcessor(defaultTestProcessorConfig())
	data := createTestJPEG(t, 400, 300)

	result, err := p.Process(context.Background(), bytes.NewReader(data), "image/jpeg", PurposeProfile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ContentType != "image/jpeg" {
		t.Errorf("expected image/jpeg, got %s", result.ContentType)
	}
	if result.Width != 256 || result.Height != 256 {
		t.Errorf("expected 256x256, got %dx%d", result.Width, result.Height)
	}
	if len(result.Data) == 0 {
		t.Errorf("expected non-empty data")
	}
}

func TestProcessor_ValidCoverPNG(t *testing.T) {
	p := NewProcessor(defaultTestProcessorConfig())
	data := createTestPNG(t, 800, 600)

	result, err := p.Process(context.Background(), bytes.NewReader(data), "image/png", PurposeCover)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ContentType != "image/jpeg" {
		t.Errorf("expected output normalized to image/jpeg, got %s", result.ContentType)
	}
	if result.Width != 800 {
		t.Errorf("expected width 800, got %d", result.Width)
	}
	expectedHeight := int(800.0 / (16.0 / 9.0))
	if result.Height != expectedHeight {
		t.Errorf("expected height %d, got %d", expectedHeight, result.Height)
	}
}

func TestProcessor_InvalidPurpose(t *testing.T) {
	p := NewProcessor(defaultTestProcessorConfig())
	data := createTestJPEG(t, 200, 200)

	_, err := p.Process(context.Background(), bytes.NewReader(data), "image/jpeg", Purpose("avatar"))
	if err != ErrInvalidPurpose {
		t.Errorf("expected ErrInvalidPurpose, got %v", err)
	}
}

func TestProcessor_InvalidConfig(t *testing.T) {
	badConfig := ProcessorConfig{
		MaxUploadBytes: 0,
	}
	p := NewProcessor(badConfig)
	data := createTestJPEG(t, 200, 200)

	_, err := p.Process(context.Background(), bytes.NewReader(data), "image/jpeg", PurposeProfile)
	if err == nil || !strings.Contains(err.Error(), "invalid image processor configuration") {
		t.Errorf("expected invalid config error, got %v", err)
	}
}

func TestProcessor_ContextCancelled(t *testing.T) {
	p := NewProcessor(defaultTestProcessorConfig())
	data := createTestJPEG(t, 200, 200)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.Process(ctx, bytes.NewReader(data), "image/jpeg", PurposeProfile)
	if err == nil {
		t.Fatalf("expected context error, got nil")
	}
}

func TestProcessor_EmptyFile(t *testing.T) {
	p := NewProcessor(defaultTestProcessorConfig())
	_, err := p.Process(context.Background(), bytes.NewReader([]byte{}), "image/jpeg", PurposeProfile)
	if err != ErrEmptyFile {
		t.Errorf("expected ErrEmptyFile, got %v", err)
	}
}

func TestProcessor_OversizedFile(t *testing.T) {
	cfg := defaultTestProcessorConfig()
	cfg.MaxUploadBytes = 100
	p := NewProcessor(cfg)

	data := createTestJPEG(t, 200, 200)
	_, err := p.Process(context.Background(), bytes.NewReader(data), "image/jpeg", PurposeProfile)
	if err != ErrFileTooLarge {
		t.Errorf("expected ErrFileTooLarge, got %v", err)
	}
}

func TestProcessor_UnsupportedType(t *testing.T) {
	p := NewProcessor(defaultTestProcessorConfig())
	textData := []byte("plain text content not an image")
	_, err := p.Process(context.Background(), bytes.NewReader(textData), "text/plain", PurposeProfile)
	if err != ErrUnsupportedType {
		t.Errorf("expected ErrUnsupportedType, got %v", err)
	}
}

func TestProcessor_MismatchedContentType(t *testing.T) {
	p := NewProcessor(defaultTestProcessorConfig())
	data := createTestJPEG(t, 200, 200)
	_, err := p.Process(context.Background(), bytes.NewReader(data), "image/png", PurposeProfile)
	if err != ErrUnsupportedType {
		t.Errorf("expected ErrUnsupportedType on mime mismatch, got %v", err)
	}
}

func TestProcessor_CorruptImage(t *testing.T) {
	p := NewProcessor(defaultTestProcessorConfig())
	// JPEG magic bytes followed by garbage
	corrupt := append([]byte("\xFF\xD8\xFF\xE0\x00\x10JFIF\x00"), bytes.Repeat([]byte{0x00}, 100)...)
	_, err := p.Process(context.Background(), bytes.NewReader(corrupt), "image/jpeg", PurposeProfile)
	if err != ErrInvalidImage {
		t.Errorf("expected ErrInvalidImage, got %v", err)
	}
}

func TestProcessor_ExceedsMaxDimension(t *testing.T) {
	cfg := defaultTestProcessorConfig()
	cfg.MaxInputDimension = 150
	p := NewProcessor(cfg)

	data := createTestJPEG(t, 200, 200)
	_, err := p.Process(context.Background(), bytes.NewReader(data), "image/jpeg", PurposeProfile)
	if err != ErrInvalidImage {
		t.Errorf("expected ErrInvalidImage for exceeding max dimension, got %v", err)
	}
}

func TestProcessor_ExceedsMaxPixels(t *testing.T) {
	cfg := defaultTestProcessorConfig()
	cfg.MaxInputPixels = 30_000
	p := NewProcessor(cfg)

	data := createTestJPEG(t, 200, 200) // 40,000 pixels > 30,000
	_, err := p.Process(context.Background(), bytes.NewReader(data), "image/jpeg", PurposeProfile)
	if err != ErrInvalidImage {
		t.Errorf("expected ErrInvalidImage for exceeding max pixels, got %v", err)
	}
}

func TestProcessor_TooSmallForProfile(t *testing.T) {
	p := NewProcessor(defaultTestProcessorConfig())
	data := createTestJPEG(t, 100, 100) // minProfileSide is 128
	_, err := p.Process(context.Background(), bytes.NewReader(data), "image/jpeg", PurposeProfile)
	if err != ErrImageTooSmall {
		t.Errorf("expected ErrImageTooSmall, got %v", err)
	}
}

func TestProcessor_TooSmallForCover(t *testing.T) {
	p := NewProcessor(defaultTestProcessorConfig())
	data := createTestJPEG(t, 500, 300) // minCoverHeight is 360
	_, err := p.Process(context.Background(), bytes.NewReader(data), "image/jpeg", PurposeCover)
	if err != ErrImageTooSmall {
		t.Errorf("expected ErrImageTooSmall, got %v", err)
	}
}

func TestProcessor_ExifOrientation(t *testing.T) {
	// Construct minimal EXIF headers with orientation tags
	buildExifJPEG := func(orientation uint16) []byte {
		var buf bytes.Buffer
		// SOI
		buf.Write([]byte{0xFF, 0xD8})
		// APP1 Marker
		buf.Write([]byte{0xFF, 0xE1})
		// APP1 length: 2 (length) + 6 (Exif\0\0) + 8 (TIFF header) + 2 (count) + 12 (entry) + 4 (offset)
		length := uint16(2 + 6 + 8 + 2 + 12 + 4)
		buf.WriteByte(byte(length >> 8))
		buf.WriteByte(byte(length & 0xFF))
		// "Exif\0\0"
		buf.Write([]byte("Exif\x00\x00"))
		// TIFF Header (Little Endian "II")
		buf.Write([]byte("II\x2A\x00\x08\x00\x00\x00"))
		// Number of entries = 1
		buf.Write([]byte{0x01, 0x00})
		// Tag 0x0112 (Orientation), type 3 (SHORT), count 1, value (orientation)
		buf.Write([]byte{0x12, 0x01, 0x03, 0x00, 0x01, 0x00, 0x00, 0x00})
		buf.WriteByte(byte(orientation & 0xFF))
		buf.WriteByte(byte(orientation >> 8))
		buf.Write([]byte{0x00, 0x00}) // pad
		// Next IFD offset = 0
		buf.Write([]byte{0x00, 0x00, 0x00, 0x00})

		// Append real image data (without SOI)
		raw := createTestJPEG(t, 200, 200)
		buf.Write(raw[2:])
		return buf.Bytes()
	}

	p := NewProcessor(defaultTestProcessorConfig())
	for orient := uint16(1); orient <= 8; orient++ {
		data := buildExifJPEG(orient)
		result, err := p.Process(context.Background(), bytes.NewReader(data), "image/jpeg", PurposeProfile)
		if err != nil {
			t.Fatalf("orientation %d processing failed: %v", orient, err)
		}
		if result.Width != 200 || result.Height != 200 {
			t.Errorf("orientation %d: expected 200x200, got %dx%d", orient, result.Width, result.Height)
		}
	}
}

func TestNormalizeContentType(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"image/jpeg", "image/jpeg"},
		{"IMAGE/JPEG", "image/jpeg"},
		{"image/jpg", "image/jpeg"},
		{"image/png", "image/png"},
		{"image/x-png", "image/png"},
		{"image/webp", "image/webp"},
		{"application/octet-stream", ""},
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		got := normalizeContentType(c.input)
		if got != c.expected {
			t.Errorf("normalizeContentType(%q) = %q, want %q", c.input, got, c.expected)
		}
	}
}

func TestDetectImageType_WebP(t *testing.T) {
	// RIFF....WEBP header
	data := []byte("RIFF1234WEBPVP8 ...")
	got := detectImageType(data)
	if got != "image/webp" {
		t.Errorf("detectImageType webp header: got %s, want image/webp", got)
	}
}
