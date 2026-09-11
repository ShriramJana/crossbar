package provider_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ShriramJana/crossbar/internal/provider"
)

// TestAnthropicLive makes one real, billable call. It is skipped unless both
// CROSSBAR_LIVE_ANTHROPIC=1 and ANTHROPIC_API_KEY are set, so the default
// `go test ./...` never touches the network or needs a secret.
func TestAnthropicLive(t *testing.T) {
	if os.Getenv("CROSSBAR_LIVE_ANTHROPIC") != "1" {
		t.Skip("set CROSSBAR_LIVE_ANTHROPIC=1 and ANTHROPIC_API_KEY to run against the real API")
	}
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}

	a := provider.NewAnthropic(provider.AnthropicConfig{APIKey: key})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	require.NoError(t, a.HealthCheck(ctx))

	resp, err := a.Send(ctx, &provider.Request{
		Model:     "claude-haiku-4-5",
		Messages:  []provider.Message{{Role: "user", Content: "Reply with the single word: pong"}},
		MaxTokens: 8,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, resp.Text)
	assert.Positive(t, resp.InputTokens)
	assert.Positive(t, resp.OutputTokens)
	assert.Equal(t, "anthropic", resp.Provider)
	t.Logf("model=%s text=%q in=%d out=%d latency=%s", resp.Model, resp.Text, resp.InputTokens, resp.OutputTokens, resp.Latency)
}
