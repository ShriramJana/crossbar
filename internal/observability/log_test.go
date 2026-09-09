package observability_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ShriramJana/relay/internal/observability"
)

func TestNewLogger(t *testing.T) {
	tests := []struct {
		name      string
		format    string
		level     string
		logAt     slog.Level
		wantLine  bool
		wantJSON  bool
		wantError bool
	}{
		{name: "json at info emits info", format: "json", level: "info", logAt: slog.LevelInfo, wantLine: true, wantJSON: true},
		{name: "json at warn suppresses info", format: "json", level: "warn", logAt: slog.LevelInfo, wantLine: false},
		{name: "text at debug emits debug", format: "text", level: "debug", logAt: slog.LevelDebug, wantLine: true},
		{name: "unknown format is rejected", format: "xml", level: "info", wantError: true},
		{name: "unknown level is rejected", format: "json", level: "loud", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger, err := observability.NewLogger(&buf, tt.format, tt.level)
			if tt.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			logger.Log(context.Background(), tt.logAt, "hello", slog.String("k", "v"))
			if !tt.wantLine {
				assert.Empty(t, buf.String())
				return
			}
			require.NotEmpty(t, buf.String())
			if tt.wantJSON {
				var m map[string]any
				require.NoError(t, json.Unmarshal(buf.Bytes(), &m))
				assert.Equal(t, "hello", m["msg"])
				assert.Equal(t, "v", m["k"])
			} else {
				assert.Contains(t, buf.String(), "msg=hello")
			}
		})
	}
}
