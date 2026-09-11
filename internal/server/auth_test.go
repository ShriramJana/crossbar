package server_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ShriramJana/crossbar/internal/config"
	"github.com/ShriramJana/crossbar/internal/server"
)

const (
	teamKey  = "team-secret"
	adminKey = "admin-secret"
)

func configYAML(rpm int, withAdmin bool) string {
	var b strings.Builder
	if withAdmin {
		b.WriteString("admin:\n  key_sha256: " + config.HashKey(adminKey) + "\n")
	}
	b.WriteString(`providers:
  mock:
    type: mock
tiers:
  cheap:
    - {provider: mock, model: mock-model}
pricing:
  - {provider: mock, model: mock-model, input_per_million: 0, output_per_million: 0}
teams:
  - id: search
    name: Search
    keys_sha256: [` + config.HashKey(teamKey) + `]
    requests_per_minute: ` + strconv.Itoa(rpm) + `
    tokens_per_minute: 1000
    daily_budget_usd: 1
    monthly_budget_usd: 10
`)
	return b.String()
}

// testEnv is a server backed by a real config file on disk so reload paths are exercised.
type testEnv struct {
	ts    *httptest.Server
	store *config.Store
	path  string
}

func newEnv(t *testing.T, yaml string) *testEnv {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	store, err := config.NewStore(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)

	s := server.New(server.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: store,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{ts: ts, store: store, path: path}
}

func (e *testEnv) do(t *testing.T, method, path, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+path, strings.NewReader("{}"))
	require.NoError(t, err)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := e.ts.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (e *testEnv) rewrite(t *testing.T, yaml string) {
	t.Helper()
	require.NoError(t, os.WriteFile(e.path, []byte(yaml), 0o600))
}

func TestDataPlaneAuth(t *testing.T) {
	env := newEnv(t, configYAML(100, true))

	tests := []struct {
		name       string
		bearer     string
		rawHeader  string
		wantStatus int
	}{
		{name: "missing header", wantStatus: http.StatusUnauthorized},
		{name: "unknown key", bearer: "nope", wantStatus: http.StatusUnauthorized},
		{name: "admin key is not a team key", bearer: adminKey, wantStatus: http.StatusUnauthorized},
		{name: "hash of key is not the key", bearer: config.HashKey(teamKey), wantStatus: http.StatusUnauthorized},
		{name: "wrong scheme", rawHeader: "Basic " + teamKey, wantStatus: http.StatusUnauthorized},
		{name: "valid team key reaches handler", bearer: teamKey, wantStatus: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, env.ts.URL+"/v1/messages", strings.NewReader("{}"))
			require.NoError(t, err)
			if tt.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tt.bearer)
			}
			if tt.rawHeader != "" {
				req.Header.Set("Authorization", tt.rawHeader)
			}
			resp, err := env.ts.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, tt.wantStatus, resp.StatusCode)
			if tt.wantStatus == http.StatusUnauthorized {
				assert.Equal(t, "Bearer", resp.Header.Get("WWW-Authenticate"))
				body, _ := io.ReadAll(resp.Body)
				assert.JSONEq(t, `{"error":"unauthorized"}`, string(body), "401 must not explain why")
			}
		})
	}
}

func TestHealthNeedsNoAuth(t *testing.T) {
	env := newEnv(t, configYAML(100, true))
	resp := env.do(t, http.MethodGet, "/health", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestAdminAuth(t *testing.T) {
	env := newEnv(t, configYAML(100, true))

	tests := []struct {
		name       string
		bearer     string
		wantStatus int
	}{
		{name: "missing", wantStatus: http.StatusUnauthorized},
		{name: "team key is not admin", bearer: teamKey, wantStatus: http.StatusUnauthorized},
		{name: "admin key", bearer: adminKey, wantStatus: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := env.do(t, http.MethodGet, "/admin/teams/search/usage", tt.bearer)
			assert.Equal(t, tt.wantStatus, resp.StatusCode)
		})
	}
}

func TestAdminDisabledWithoutKey(t *testing.T) {
	env := newEnv(t, configYAML(100, false))
	resp := env.do(t, http.MethodGet, "/admin/teams/search/usage", "")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp = env.do(t, http.MethodGet, "/admin/teams/search/usage", adminKey)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAdminTeamUsage(t *testing.T) {
	env := newEnv(t, configYAML(100, true))

	resp := env.do(t, http.MethodGet, "/admin/teams/search/usage", adminKey)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body struct {
		Team   string `json:"team"`
		Name   string `json:"name"`
		Limits struct {
			RequestsPerMinute int     `json:"requests_per_minute"`
			TokensPerMinute   int     `json:"tokens_per_minute"`
			DailyBudgetUSD    float64 `json:"daily_budget_usd"`
			MonthlyBudgetUSD  float64 `json:"monthly_budget_usd"`
		} `json:"limits"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "search", body.Team)
	assert.Equal(t, "Search", body.Name)
	assert.Equal(t, 100, body.Limits.RequestsPerMinute)
	assert.Equal(t, 1000, body.Limits.TokensPerMinute)
	assert.Equal(t, 1.0, body.Limits.DailyBudgetUSD)

	resp = env.do(t, http.MethodGet, "/admin/teams/nope/usage", adminKey)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestAdminConfigReload(t *testing.T) {
	env := newEnv(t, configYAML(100, true))

	t.Run("valid edit is applied", func(t *testing.T) {
		env.rewrite(t, configYAML(250, true))
		resp := env.do(t, http.MethodPost, "/admin/config/reload", adminKey)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var body struct {
			Status     string `json:"status"`
			Generation uint64 `json:"generation"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		assert.Equal(t, "reloaded", body.Status)
		assert.Equal(t, uint64(2), body.Generation)

		team, _ := env.store.Current().TeamByID("search")
		assert.Equal(t, 250, team.RequestsPerMinute)
	})

	t.Run("invalid edit is rejected and old config kept", func(t *testing.T) {
		env.rewrite(t, "teams: [")
		resp := env.do(t, http.MethodPost, "/admin/config/reload", adminKey)
		require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
		var body struct {
			Error string `json:"error"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		assert.Contains(t, body.Error, "parsing")

		team, _ := env.store.Current().TeamByID("search")
		assert.Equal(t, 250, team.RequestsPerMinute, "previous config must survive a bad edit")

		// Traffic keeps flowing on the old config.
		resp = env.do(t, http.MethodPost, "/v1/messages", teamKey)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("requires admin", func(t *testing.T) {
		resp := env.do(t, http.MethodPost, "/admin/config/reload", teamKey)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
}

func TestKeyRevocationTakesEffectOnReload(t *testing.T) {
	env := newEnv(t, configYAML(100, true))
	resp := env.do(t, http.MethodPost, "/v1/messages", teamKey)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	env.rewrite(t, strings.Replace(configYAML(100, true), config.HashKey(teamKey), config.HashKey("rotated"), 1))
	require.NoError(t, env.store.Reload())

	resp = env.do(t, http.MethodPost, "/v1/messages", teamKey)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "old key must stop working")
	resp = env.do(t, http.MethodPost, "/v1/messages", "rotated")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "new key must work")
}
