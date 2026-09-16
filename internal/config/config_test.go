package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ShriramJana/crossbar/internal/config"
)

// validYAML is a complete, valid config used as the base for every table entry.
func validYAML() string {
	return `
admin:
  key_sha256: ` + config.HashKey("admin-secret") + `
providers:
  anthropic:
    type: anthropic
    api_key_env: ANTHROPIC_API_KEY
  mock:
    type: mock
tiers:
  balanced:
    - {provider: anthropic, model: claude-sonnet-4-5}
    - {provider: mock, model: mock-model}
pricing:
  - {provider: anthropic, model: claude-sonnet-4-5, input_per_million: 3.0, output_per_million: 15.0}
  - {provider: mock, model: mock-model, input_per_million: 0, output_per_million: 0}
breaker:
  failure_threshold: 0.6
teams:
  - id: search
    name: Search
    keys_sha256:
      - ` + config.HashKey("search-key-1") + `
      - ` + config.HashKey("search-key-2") + `
    allowed_models: [claude-sonnet-4-5]
    requests_per_minute: 100
    tokens_per_minute: 50000
    daily_budget_usd: 25
    monthly_budget_usd: 500
  - id: batch
    name: Batch
    keys_sha256: [` + config.HashKey("batch-key") + `]
    requests_per_minute: 10
    tokens_per_minute: 1000
    daily_budget_usd: 5
    monthly_budget_usd: 50
`
}

func TestParseValid(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader(validYAML()))
	require.NoError(t, err)

	assert.Len(t, cfg.Teams, 2)
	assert.Len(t, cfg.Providers, 2)
	assert.Equal(t, "anthropic", cfg.Providers["anthropic"].Type)
	assert.Equal(t, "ANTHROPIC_API_KEY", cfg.Providers["anthropic"].APIKeyEnv)
	require.Len(t, cfg.Tiers["balanced"], 2)
	assert.Equal(t, config.Target{Provider: "anthropic", Model: "claude-sonnet-4-5"}, cfg.Tiers["balanced"][0])

	// Explicit value kept, unset values get defaults.
	assert.Equal(t, 0.6, cfg.Breaker.FailureThreshold)
	assert.Equal(t, 20, cfg.Breaker.MinRequests)
	assert.Equal(t, 30*time.Second, cfg.Breaker.Window)
	assert.Equal(t, 15*time.Second, cfg.Breaker.Cooldown)
	assert.Equal(t, 2*time.Minute, cfg.Breaker.MaxCooldown)

	// Retry and health blocks omitted entirely: all defaults.
	assert.Equal(t, 2, cfg.Retry.MaxRetries)
	assert.Equal(t, 100*time.Millisecond, cfg.Retry.BaseBackoff)
	assert.Equal(t, 2*time.Second, cfg.Retry.MaxBackoff)
	assert.Equal(t, 30*time.Second, cfg.Health.Interval)
	assert.Equal(t, 5*time.Second, cfg.Health.Timeout)
}

func TestRetryAndHealthValidation(t *testing.T) {
	tests := []struct {
		name    string
		block   string
		wantErr string
	}{
		{name: "explicit retry values", block: "retry:\n  max_retries: 0\n  base_backoff: 50ms\n  max_backoff: 50ms\n"},
		{name: "negative retries", block: "retry:\n  max_retries: -1\n", wantErr: "max_retries must not be negative"},
		{name: "max below base", block: "retry:\n  base_backoff: 1s\n  max_backoff: 100ms\n", wantErr: "max_backoff must be at least base_backoff"},
		{name: "negative health interval", block: "health:\n  interval: -1s\n", wantErr: "health: durations must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := config.Parse(strings.NewReader(validYAML() + tt.block))
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 0, cfg.Retry.MaxRetries, "zero retries is a valid explicit choice")
			assert.Equal(t, 50*time.Millisecond, cfg.Retry.BaseBackoff)
		})
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{
			name:    "malformed yaml",
			mutate:  func(s string) string { return s + "\n  :bad" },
			wantErr: "parsing",
		},
		{
			name: "unknown field",
			mutate: func(s string) string {
				return strings.Replace(s, "requests_per_minute: 100", "requests_per_min: 100", 1)
			},
			wantErr: "field requests_per_min not found",
		},
		{
			name:    "duplicate team id",
			mutate:  func(s string) string { return strings.Replace(s, "id: batch", "id: search", 1) },
			wantErr: `duplicate team id "search"`,
		},
		{
			name: "duplicate key across teams",
			mutate: func(s string) string {
				return strings.Replace(s, config.HashKey("batch-key"), config.HashKey("search-key-1"), 1)
			},
			wantErr: "key already assigned",
		},
		{
			name: "team with no keys",
			mutate: func(s string) string {
				return strings.Replace(s, "keys_sha256: ["+config.HashKey("batch-key")+"]", "keys_sha256: []", 1)
			},
			wantErr: `team "batch": at least one key`,
		},
		{
			name:    "key that is not a sha256 hex",
			mutate:  func(s string) string { return strings.Replace(s, config.HashKey("batch-key"), "not-a-hash", 1) },
			wantErr: "not a sha256 hex digest",
		},
		{
			name: "zero rpm",
			mutate: func(s string) string {
				return strings.Replace(s, "requests_per_minute: 10\n", "requests_per_minute: 0\n", 1)
			},
			wantErr: "requests_per_minute must be positive",
		},
		{
			name:    "negative budget",
			mutate:  func(s string) string { return strings.Replace(s, "daily_budget_usd: 5\n", "daily_budget_usd: -1\n", 1) },
			wantErr: "daily_budget_usd must not be negative",
		},
		{
			name: "tier references unknown provider",
			mutate: func(s string) string {
				return strings.Replace(s, "{provider: mock, model: mock-model}", "{provider: openai, model: gpt-4o}", 1)
			},
			wantErr: `tier "balanced": unknown provider "openai"`,
		},
		{
			name: "empty tier",
			mutate: func(s string) string {
				return strings.Replace(s, "tiers:\n  balanced:\n    - {provider: anthropic, model: claude-sonnet-4-5}\n    - {provider: mock, model: mock-model}", "tiers:\n  balanced: []", 1)
			},
			wantErr: `tier "balanced": must list at least one target`,
		},
		{
			name:    "unknown provider type",
			mutate:  func(s string) string { return strings.Replace(s, "type: mock", "type: carrier-pigeon", 1) },
			wantErr: `unknown type "carrier-pigeon"`,
		},
		{
			name:    "anthropic provider without key env",
			mutate:  func(s string) string { return strings.Replace(s, "    api_key_env: ANTHROPIC_API_KEY\n", "", 1) },
			wantErr: `provider "anthropic": api_key_env is required`,
		},
		{
			name: "pricing references unknown provider",
			mutate: func(s string) string {
				return strings.Replace(s, "{provider: mock, model: mock-model, input_per_million: 0", "{provider: nope, model: mock-model, input_per_million: 0", 1)
			},
			wantErr: `pricing: unknown provider "nope"`,
		},
		{
			name: "tier target without pricing",
			mutate: func(s string) string {
				return strings.Replace(s, "  - {provider: mock, model: mock-model, input_per_million: 0, output_per_million: 0}\n", "", 1)
			},
			wantErr: `tier "balanced": no pricing for mock/mock-model`,
		},
		{
			name: "breaker threshold out of range",
			mutate: func(s string) string {
				return strings.Replace(s, "failure_threshold: 0.6", "failure_threshold: 1.5", 1)
			},
			wantErr: "failure_threshold must be in (0, 1]",
		},
		{
			name:    "admin key not a hash",
			mutate:  func(s string) string { return strings.Replace(s, config.HashKey("admin-secret"), "plaintext", 1) },
			wantErr: "admin.key_sha256",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Parse(strings.NewReader(tt.mutate(validYAML())))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestTeamByKey(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader(validYAML()))
	require.NoError(t, err)

	tests := []struct {
		name   string
		key    string
		wantID string
		ok     bool
	}{
		{name: "first key of first team", key: "search-key-1", wantID: "search", ok: true},
		{name: "second key of first team", key: "search-key-2", wantID: "search", ok: true},
		{name: "second team", key: "batch-key", wantID: "batch", ok: true},
		{name: "unknown key", key: "nope", ok: false},
		{name: "empty key", key: "", ok: false},
		{name: "hash itself is not a key", key: config.HashKey("batch-key"), ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			team, ok := cfg.TeamByKey(tt.key)
			assert.Equal(t, tt.ok, ok)
			if ok {
				assert.Equal(t, tt.wantID, team.ID)
			}
		})
	}
}

func TestTeamByID(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader(validYAML()))
	require.NoError(t, err)

	team, ok := cfg.TeamByID("batch")
	require.True(t, ok)
	assert.Equal(t, "Batch", team.Name)
	_, ok = cfg.TeamByID("nope")
	assert.False(t, ok)
}

func TestIsAdminKey(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader(validYAML()))
	require.NoError(t, err)
	assert.True(t, cfg.IsAdminKey("admin-secret"))
	assert.False(t, cfg.IsAdminKey("search-key-1"))
	assert.False(t, cfg.IsAdminKey(""))

	noAdmin, err := config.Parse(strings.NewReader(strings.Replace(validYAML(), "admin:\n  key_sha256: "+config.HashKey("admin-secret")+"\n", "", 1)))
	require.NoError(t, err)
	assert.False(t, noAdmin.IsAdminKey(""), "no admin key configured means nothing is admin")
}

func TestTeamAllowsModel(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader(validYAML()))
	require.NoError(t, err)

	search, _ := cfg.TeamByID("search")
	assert.True(t, search.AllowsModel("claude-sonnet-4-5"))
	assert.False(t, search.AllowsModel("claude-opus-4-1"))

	batch, _ := cfg.TeamByID("batch")
	assert.True(t, batch.AllowsModel("anything"), "empty allowed_models means every model")
}

func TestPrice(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader(validYAML()))
	require.NoError(t, err)

	p, ok := cfg.Price("anthropic", "claude-sonnet-4-5")
	require.True(t, ok)
	assert.InDelta(t, 3.0*1000/1e6+15.0*500/1e6, p.Cost(1000, 500), 1e-12)

	_, ok = cfg.Price("anthropic", "unpriced-model")
	assert.False(t, ok)
}

func TestHashKey(t *testing.T) {
	h := config.HashKey("abc")
	assert.Len(t, h, 64)
	assert.Equal(t, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", h)
	assert.NotEqual(t, h, config.HashKey("abd"))
}

func TestLoadMissingFile(t *testing.T) {
	_, err := config.Load(t.TempDir() + "/missing.yaml")
	require.Error(t, err)
}
