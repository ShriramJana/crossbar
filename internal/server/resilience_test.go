package server_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ShriramJana/crossbar/internal/breaker"
	"github.com/ShriramJana/crossbar/internal/config"
	"github.com/ShriramJana/crossbar/internal/health"
	"github.com/ShriramJana/crossbar/internal/provider"
	"github.com/ShriramJana/crossbar/internal/router"
	"github.com/ShriramJana/crossbar/internal/server"
)

const twoProviderYAML = `
admin:
  key_sha256: ` + "%ADMIN%" + `
providers:
  primary: {type: mock}
  secondary: {type: mock}
tiers:
  cheap:
    - {provider: primary, model: mock-model}
    - {provider: secondary, model: alt-model}
pricing:
  - {provider: primary, model: mock-model, input_per_million: 0, output_per_million: 0}
  - {provider: secondary, model: alt-model, input_per_million: 0, output_per_million: 0}
breaker:
  min_requests: 4
retry:
  max_retries: 1
  base_backoff: 1ms
  max_backoff: 1ms
teams:
  - id: search
    name: Search
    keys_sha256: [` + "%TEAM%" + `]
    requests_per_minute: 1000
    tokens_per_minute: 100000
    daily_budget_usd: 0
    monthly_budget_usd: 0
`

type resilienceEnv struct {
	ts        *httptest.Server
	primary   *provider.Mock
	secondary *provider.Mock
	breakers  *breaker.Registry
	monitor   *health.Monitor
}

func newResilienceEnv(t *testing.T) *resilienceEnv {
	t.Helper()
	yaml := strings.NewReplacer("%ADMIN%", config.HashKey(adminKey), "%TEAM%", config.HashKey(teamKey)).Replace(twoProviderYAML)
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := config.NewStore(path, logger)
	require.NoError(t, err)

	e := &resilienceEnv{primary: provider.NewMock("primary"), secondary: provider.NewMock("secondary")}
	e.breakers = breaker.NewRegistry(func() breaker.Settings { return breaker.SettingsFromConfig(store.Current().Breaker) }, breaker.RegistryOptions{})
	e.monitor = health.NewMonitor([]health.Prober{e.primary, e.secondary}, health.Options{Logger: logger})
	providers := map[string]provider.Provider{"primary": e.primary, "secondary": e.secondary}
	s := server.New(server.Options{
		Logger:   logger,
		Config:   store,
		Router:   router.New(store, providers, router.Options{Breakers: e.breakers, Health: e.monitor, Logger: logger}),
		Breakers: e.breakers,
		Health:   e.monitor,
	})
	e.ts = httptest.NewServer(s.Handler())
	t.Cleanup(e.ts.Close)
	return e
}

func (e *resilienceEnv) request(t *testing.T, method, path, bearer, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	require.NoError(t, err)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := e.ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var parsed map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &parsed)
	return resp, parsed
}

func TestMessagesFallbackIsTransparent(t *testing.T) {
	e := newResilienceEnv(t)
	e.primary.SetDefault(provider.Fail(503))

	resp, body := e.request(t, http.MethodPost, "/v1/messages", teamKey, goodBody)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "secondary", resp.Header.Get("X-Crossbar-Provider"))
	assert.Equal(t, "alt-model", resp.Header.Get("X-Crossbar-Model"))
	assert.Equal(t, "true", resp.Header.Get("X-Crossbar-Fallback"))
	assert.Equal(t, "alt-model", body["model"])
}

func TestMessagesChainExhaustedLists503Attempts(t *testing.T) {
	e := newResilienceEnv(t)
	e.primary.SetDefault(provider.Fail(503))
	e.secondary.SetDefault(provider.Transport())

	resp, body := e.request(t, http.MethodPost, "/v1/messages", teamKey, goodBody)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, "upstream_unavailable", body["error"])
	attempts, ok := body["attempted"].([]any)
	require.True(t, ok)
	require.Len(t, attempts, 2)
	first := attempts[0].(map[string]any)
	assert.Equal(t, "primary", first["provider"])
	assert.Equal(t, "mock-model", first["model"])
	assert.Equal(t, "failed", first["reason"])
	assert.EqualValues(t, 2, first["tries"])
}

func TestHealthProvidersEndpoint(t *testing.T) {
	e := newResilienceEnv(t)
	e.secondary.SetDefault(provider.Fail(503))
	e.monitor.Check(context.Background())
	// Trip the primary breaker so the endpoint has something non-trivial to show.
	b := e.breakers.Get("primary", "mock-model")
	for i := 0; i < 4; i++ {
		p, _ := b.Allow()
		p.Failure()
	}

	resp, body := e.request(t, http.MethodGet, "/health/providers", "", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, "no auth required")

	providers := body["providers"].(map[string]any)
	assert.Equal(t, "healthy", providers["primary"].(map[string]any)["status"])
	sec := providers["secondary"].(map[string]any)
	assert.Equal(t, "down", sec["status"])
	assert.EqualValues(t, 1, sec["error_rate"])
	assert.Contains(t, sec["last_error"], "503")

	breakers := body["breakers"].([]any)
	require.Len(t, breakers, 1)
	br := breakers[0].(map[string]any)
	assert.Equal(t, "primary", br["provider"])
	assert.Equal(t, "mock-model", br["model"])
	assert.Equal(t, "open", br["state"])
	assert.EqualValues(t, 4, br["failures"])
	assert.NotEmpty(t, br["retry_at"])
}

func TestAdminBreakerReset(t *testing.T) {
	e := newResilienceEnv(t)
	b := e.breakers.Get("primary", "mock-model")
	for i := 0; i < 4; i++ {
		p, _ := b.Allow()
		p.Failure()
	}
	require.Equal(t, breaker.Open, b.State())

	resp, _ := e.request(t, http.MethodPost, "/admin/breakers/primary/mock-model/reset", "", "")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	resp, body := e.request(t, http.MethodPost, "/admin/breakers/primary/mock-model/reset", adminKey, "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "closed", body["state"])
	assert.Equal(t, breaker.Closed, b.State())

	resp, _ = e.request(t, http.MethodPost, "/admin/breakers/primary/nope/reset", adminKey, "")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
