// Package observability owns logging setup and, later, metrics registration.
package observability

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// NewLogger builds a slog.Logger writing to w.
//
// format is "json" or "text"; level is one of debug, info, warn, error
// (case-insensitive). Unknown values are an error rather than a silent default
// so a typo in deployment config is caught at startup.
func NewLogger(w io.Writer, format, level string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.ToLower(level))); err != nil {
		return nil, fmt.Errorf("parsing log level %q: %w", level, err)
	}
	opts := &slog.HandlerOptions{Level: lvl}

	var h slog.Handler
	switch strings.ToLower(format) {
	case "json":
		h = slog.NewJSONHandler(w, opts)
	case "text":
		h = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("unknown log format %q (want json or text)", format)
	}
	return slog.New(h), nil
}
