package provider_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ShriramJana/crossbar/internal/config"
	"github.com/ShriramJana/crossbar/internal/provider"
)

func TestNewFromConfig(t *testing.T) {
	t.Setenv("TEST_UPSTREAM_KEY", "sk-test")

	tests := []struct {
		name     string
		cfg      config.ProviderConfig
		wantType any
		wantErr  string
	}{
		{name: "mock", cfg: config.ProviderConfig{Type: "mock"}, wantType: &provider.Mock{}},
		{name: "anthropic", cfg: config.ProviderConfig{Type: "anthropic", APIKeyEnv: "TEST_UPSTREAM_KEY", Timeout: time.Second}, wantType: &provider.Anthropic{}},
		{name: "anthropic with missing env var", cfg: config.ProviderConfig{Type: "anthropic", APIKeyEnv: "TEST_MISSING_KEY"}, wantErr: "TEST_MISSING_KEY is not set"},
		{name: "unsupported type", cfg: config.ProviderConfig{Type: "openai", APIKeyEnv: "TEST_UPSTREAM_KEY"}, wantErr: "no adapter for provider type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := provider.NewFromConfig("up", tt.cfg)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.IsType(t, tt.wantType, p)
			assert.Equal(t, "up", p.Name(), "the configured name wins over the adapter's default")
		})
	}
}
