package router_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ShriramJana/crossbar/internal/config"
	"github.com/ShriramJana/crossbar/internal/provider"
	"github.com/ShriramJana/crossbar/internal/router"
)

const testYAML = `
providers:
  primary: {type: mock}
  secondary: {type: mock}
tiers:
  frontier:
    - {provider: primary, model: big}
    - {provider: secondary, model: big-alt}
  cheap:
    - {provider: primary, model: small}
    - {provider: secondary, model: small-alt}
    - {provider: primary, model: big}
pricing:
  - {provider: primary, model: big, input_per_million: 10, output_per_million: 20}
  - {provider: secondary, model: big-alt, input_per_million: 1, output_per_million: 2}
  - {provider: primary, model: small, input_per_million: 0.5, output_per_million: 1}
  - {provider: secondary, model: small-alt, input_per_million: 0.1, output_per_million: 0.2}
teams: []
`

func newStore(t *testing.T) *config.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(testYAML), 0o600))
	s, err := config.NewStore(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	return s
}

func TestResolveChain(t *testing.T) {
	cfg := newStore(t).Current()
	tests := []struct {
		name  string
		model string
		want  []config.Target
	}{
		{
			name:  "model listed first in its tier",
			model: "small",
			want: []config.Target{
				{Provider: "primary", Model: "small"},
				{Provider: "secondary", Model: "small-alt"},
				{Provider: "primary", Model: "big"},
			},
		},
		{
			name:  "requested model leads, rest of tier follows in order",
			model: "small-alt",
			want: []config.Target{
				{Provider: "secondary", Model: "small-alt"},
				{Provider: "primary", Model: "small"},
				{Provider: "primary", Model: "big"},
			},
		},
		{
			name:  "model in two tiers resolves to the tier listing it earliest",
			model: "big",
			want: []config.Target{
				{Provider: "primary", Model: "big"},
				{Provider: "secondary", Model: "big-alt"},
			},
		},
		{
			name:  "tier name selects the whole chain",
			model: "cheap",
			want: []config.Target{
				{Provider: "primary", Model: "small"},
				{Provider: "secondary", Model: "small-alt"},
				{Provider: "primary", Model: "big"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := router.ResolveChain(cfg, tt.model)
			require.True(t, ok)
			assert.Equal(t, tt.want, got)
		})
	}

	_, ok := router.ResolveChain(cfg, "nope")
	assert.False(t, ok)
}

func TestDispatchUsesPrimaryAndPricesResponse(t *testing.T) {
	store := newStore(t)
	primary := provider.NewMock("primary")
	secondary := provider.NewMock("secondary")
	r := router.New(store, map[string]provider.Provider{"primary": primary, "secondary": secondary}, nil)

	resp, err := r.Dispatch(context.Background(), &provider.Request{
		Model:     "big",
		Messages:  []provider.Message{{Role: "user", Content: "one two three"}}, // 3 input tokens
		MaxTokens: 16,
	})
	require.NoError(t, err)
	assert.Equal(t, "primary", resp.Provider)
	assert.Equal(t, "big", resp.Model)
	assert.False(t, resp.Fallback)
	assert.Equal(t, 1, primary.Calls())
	assert.Equal(t, 0, secondary.Calls())

	// mock output "mock response to: one two three" is 6 tokens.
	wantCost := 3*10.0/1e6 + 6*20.0/1e6
	assert.InDelta(t, wantCost, resp.CostUSD, 1e-12)
}

func TestDispatchUnknownModel(t *testing.T) {
	store := newStore(t)
	r := router.New(store, map[string]provider.Provider{"primary": provider.NewMock("primary")}, nil)

	_, err := r.Dispatch(context.Background(), &provider.Request{Model: "nope", MaxTokens: 1})
	assert.True(t, errors.Is(err, router.ErrUnknownModel))
}

func TestDispatchReturnsProviderError(t *testing.T) {
	store := newStore(t)
	primary := provider.NewMock("primary")
	primary.Enqueue(provider.Fail(400))
	r := router.New(store, map[string]provider.Provider{"primary": primary, "secondary": provider.NewMock("secondary")}, nil)

	_, err := r.Dispatch(context.Background(), &provider.Request{Model: "big", MaxTokens: 1})
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, 400, pe.StatusCode)
}

func TestDispatchMissingProviderAdapter(t *testing.T) {
	store := newStore(t)
	r := router.New(store, map[string]provider.Provider{"primary": provider.NewMock("primary")}, nil)

	// "small-alt" lives on "secondary", which has no adapter registered.
	_, err := r.Dispatch(context.Background(), &provider.Request{Model: "small-alt", MaxTokens: 1})
	assert.True(t, errors.Is(err, router.ErrNoProvider))
}
