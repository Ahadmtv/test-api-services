package platform

import (
	"fmt"
	"log/slog"
	"os"
)

// NewLogger creates a structured logger. Supported formats are json and text.
func NewLogger(level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q: %w", level, err)
	}

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: &lvl}
	switch format {
	case "text":
		handler = slog.NewTextHandler(os.Stdout, opts)
	default:
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler), nil
}
