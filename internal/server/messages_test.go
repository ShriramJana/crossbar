package server_test

import (
	"bytes"
	"context"
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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ShriramJana/crossbar/internal/budget"
	"github.com/ShriramJana/crossbar/internal/config"
	"github.com/ShriramJana/crossbar/internal/limiter"
	"github.com/ShriramJana/crossbar/internal/provider"
	"github.com/ShriramJana/crossbar/internal/redistest"
	"github.com/ShriramJana/crossbar/internal/router"
	"github.com/ShriramJana/crossbar/internal/server"
)

// dataEnv is a server with a mock provider behind the router. Limiter and
// budget are nil (disabled) unless the test attaches Redis-backed ones.
type dataEnv struct {
	ts    *httptest.Server
	store *config.Store
	mock  *provider.Mock
}

func dataPlaneYAML(rpm int, dailyBudget float64) string {
	return `
admin:
  key_sha256: ` + config.HashKey(adminKey) + `
providers:
  mock: {type: mock}
tiers:
  cheap:
    - {provider: mock, model: mock-model}
pricing:
  - {provider: mock, model: mock-model, input_per_million: 1000000, output_per_million: 2000000}
teams:
  - id: search
    name: Search
    keys_sha256: [` + config.HashKey(teamKey) + `]
    allowed_models: [mock-model, cheap]
    requests_per_minute: ` + strconv.Itoa(rpm) + `
    tokens_per_minute: 100000
    daily_budget_usd: ` + strconv.FormatFloat(dailyBudget, 'f', -1, 64) + `
    monthly_budget_usd: 1000
`
}

func newDataEnv(t *testing.T, yaml string, opts func(*server.Options)) *dataEnv {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := config.NewStore(path, logger)
	require.NoError(t, err)

	mock := provider.NewMock("mock")
	o := server.Options{
		Logger: logger,
		Config: store,
		Router: router.New(store, map[string]provider.Provider{"mock": mock}, logger),
	}
	if opts != nil {
		opts(&o)
	}
	ts := httptest.NewServer(server.New(o).Handler())
	t.Cleanup(ts.Close)
	return &dataEnv{ts: ts, store: store, mock: mock}
}

func (e *dataEnv) post(t *testing.T, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.ts.URL+"/v1/messages", bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+teamKey)
	resp, err := e.ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var parsed map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &parsed)
	return resp, parsed
}

const goodBody = `{"model":"mock-model","max_tokens":32,"messages":[{"role":"user","content":"hello there"}]}`

func TestMessagesSuccess(t *testing.T) {
	env := newDataEnv(t, dataPlaneYAML(100, 0), nil)

	resp, body := env.post(t, goodBody)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, "message", body["type"])
	assert.Equal(t, "assistant", body["role"])
	assert.Equal(t, "mock-model", body["model"])
	assert.Equal(t, "end_turn", body["stop_reason"])
	assert.NotEmpty(t, body["id"])
	content := body["content"].([]any)
	require.Len(t, content, 1)
	block := content[0].(map[string]any)
	assert.Equal(t, "text", block["type"])
	assert.Equal(t, "mock response to: hello there", block["text"])
	usage := body["usage"].(map[string]any)
	assert.EqualValues(t, 2, usage["input_tokens"])
	assert.EqualValues(t, 5, usage["output_tokens"])

	assert.Equal(t, "mock", resp.Header.Get("X-Crossbar-Provider"))
	assert.Equal(t, "mock-model", resp.Header.Get("X-Crossbar-Model"))
	assert.Equal(t, "false", resp.Header.Get("X-Crossbar-Fallback"))
	// 2 input tokens at $1/token + 5 output tokens at $2/token = $12.
	assert.Equal(t, "12.000000", resp.Header.Get("X-Crossbar-Cost-USD"))
	assert.Equal(t, 1, env.mock.Calls())
}

func TestMessagesForwardsFieldsToProvider(t *testing.T) {
	env := newDataEnv(t, dataPlaneYAML(100, 0), nil)
	body := `{"model":"mock-model","max_tokens":7,"system":"be terse","temperature":0.3,
	          "messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`
	resp, _ := env.post(t, body)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got := env.mock.LastRequest()
	require.NotNil(t, got)
	assert.Equal(t, "be terse", got.System)
	assert.Equal(t, 7, got.MaxTokens)
	assert.InDelta(t, 0.3, got.Temperature, 1e-9)
	assert.Equal(t, []provider.Message{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}, {Role: "user", Content: "c"}}, got.Messages)
}

func TestMessagesAcceptsContentBlocks(t *testing.T) {
	env := newDataEnv(t, dataPlaneYAML(100, 0), nil)
	body := `{"model":"mock-model","max_tokens":7,"messages":[{"role":"user","content":[{"type":"text","text":"part one "},{"type":"text","text":"part two"}]}]}`
	resp, _ := env.post(t, body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "part one part two", env.mock.LastRequest().Messages[0].Content)
}

func TestMessagesTierNameAsModel(t *testing.T) {
	env := newDataEnv(t, dataPlaneYAML(100, 0), nil)
	resp, body := env.post(t, strings.Replace(goodBody, `"mock-model"`, `"cheap"`, 1))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "mock-model", body["model"], "response names the model that served it")
}

func TestMessagesBadRequests(t *testing.T) {
	env := newDataEnv(t, dataPlaneYAML(100, 0), nil)
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "not json", body: `{`, want: "invalid_json"},
		{name: "missing model", body: `{"max_tokens":1,"messages":[{"role":"user","content":"x"}]}`, want: "model is required"},
		{name: "zero max_tokens", body: `{"model":"mock-model","max_tokens":0,"messages":[{"role":"user","content":"x"}]}`, want: "max_tokens must be positive"},
		{name: "no messages", body: `{"model":"mock-model","max_tokens":1,"messages":[]}`, want: "messages must not be empty"},
		{name: "bad role", body: `{"model":"mock-model","max_tokens":1,"messages":[{"role":"system","content":"x"}]}`, want: "role must be user or assistant"},
		{name: "unsupported content block", body: `{"model":"mock-model","max_tokens":1,"messages":[{"role":"user","content":[{"type":"image"}]}]}`, want: "unsupported content block type"},
		{name: "unknown field", body: `{"model":"mock-model","max_tokens":1,"stream":true,"messages":[{"role":"user","content":"x"}]}`, want: "unknown field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := env.post(t, tt.body)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
			assert.Contains(t, body["error"], tt.want)
		})
	}
	assert.Equal(t, 0, env.mock.Calls(), "no bad request may reach the provider")
}

func TestMessagesModelNotAllowed(t *testing.T) {
	env := newDataEnv(t, strings.Replace(dataPlaneYAML(100, 0), "allowed_models: [mock-model, cheap]", "allowed_models: [other]", 1), nil)
	resp, body := env.post(t, goodBody)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, "model_not_allowed", body["error"])
	assert.Equal(t, 0, env.mock.Calls())
}

func TestMessagesUnknownModel(t *testing.T) {
	env := newDataEnv(t, strings.Replace(dataPlaneYAML(100, 0), "allowed_models: [mock-model, cheap]", "", 1), nil)
	resp, body := env.post(t, strings.Replace(goodBody, `"mock-model"`, `"nope"`, 1))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "unknown_model", body["error"])
}

func TestMessagesUpstreamErrors(t *testing.T) {
	tests := []struct {
		name       string
		behavior   provider.Behavior
		wantStatus int
		wantError  string
	}{
		{name: "upstream 400 passes through as 400", behavior: provider.Fail(400), wantStatus: http.StatusBadRequest, wantError: "upstream_rejected"},
		{name: "upstream 401 is our misconfiguration", behavior: provider.Fail(401), wantStatus: http.StatusBadGateway, wantError: "upstream_rejected"},
		{name: "upstream 500", behavior: provider.Fail(500), wantStatus: http.StatusServiceUnavailable, wantError: "upstream_unavailable"},
		{name: "upstream 429", behavior: provider.Fail(429), wantStatus: http.StatusServiceUnavailable, wantError: "upstream_unavailable"},
		{name: "transport", behavior: provider.Transport(), wantStatus: http.StatusServiceUnavailable, wantError: "upstream_unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newDataEnv(t, dataPlaneYAML(100, 0), nil)
			env.mock.Enqueue(tt.behavior)
			resp, body := env.post(t, goodBody)
			assert.Equal(t, tt.wantStatus, resp.StatusCode)
			assert.Equal(t, tt.wantError, body["error"])
		})
	}
}

func TestMessagesRequestTimeout(t *testing.T) {
	env := newDataEnv(t, dataPlaneYAML(100, 0), func(o *server.Options) {
		o.RequestTimeout = 30 * time.Millisecond
	})
	env.mock.SetDefault(provider.Slow(time.Second))

	start := time.Now()
	resp, body := env.post(t, goodBody)
	assert.Less(t, time.Since(start), 500*time.Millisecond, "the per-request deadline must bound upstream time")
	assert.Equal(t, http.StatusGatewayTimeout, resp.StatusCode)
	assert.Equal(t, "upstream_timeout", body["error"])
}

// --- Redis-backed paths below; skipped when no Redis is reachable. ---

func withRedis(t *testing.T) func(*server.Options) {
	t.Helper()
	rdb := redistest.Client(t)
	return func(o *server.Options) {
		o.Limiter = limiter.New(rdb)
		o.Budget = budget.New(rdb, budget.Options{})
	}
}

// uniqueTeamYAML swaps the team id for one unique to this test so Redis keys do not collide.
func uniqueTeamYAML(t *testing.T, yaml string) (string, string) {
	t.Helper()
	id := redistest.UniqueID(t, redistest.Client(t))
	return strings.Replace(yaml, "id: search", "id: "+id, 1), id
}

func TestMessagesRateLimited(t *testing.T) {
	yaml, _ := uniqueTeamYAML(t, dataPlaneYAML(2, 0))
	env := newDataEnv(t, yaml, withRedis(t))

	for i := 0; i < 2; i++ {
		resp, _ := env.post(t, goodBody)
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}
	resp, body := env.post(t, goodBody)
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.Equal(t, "rate_limited", body["error"])
	assert.Equal(t, "requests", body["dimension"])
	assert.NotEmpty(t, resp.Header.Get("Retry-After"))
	assert.EqualValues(t, 2, env.mock.Calls(), "rejected requests never reach the provider")
}

func TestMessagesTokenReservationReconciled(t *testing.T) {
	yaml, id := uniqueTeamYAML(t, strings.Replace(dataPlaneYAML(100, 0), "tokens_per_minute: 100000", "tokens_per_minute: 100", 1))
	rdb := redistest.Client(t)
	env := newDataEnv(t, yaml, withRedis(t))

	// Reserve 90 of 100; the mock actually uses 2 + 5 = 7, so 83 is refunded.
	resp, _ := env.post(t, strings.Replace(goodBody, `"max_tokens":32`, `"max_tokens":90`, 1))
	require.Equal(t, http.StatusOK, resp.StatusCode)

	d, err := limiter.New(rdb).Allow(context.Background(), id, limiter.Limits{RequestsPerMinute: 100, TokensPerMinute: 100}, 1)
	require.NoError(t, err)
	assert.True(t, d.Allowed)
	assert.Equal(t, 92, d.RemainingTokens, "100 - 7 actual - 1 just reserved")
}

func TestMessagesBudgetExhausted(t *testing.T) {
	// Each call costs $12; a $20 daily budget allows one call, then rejects.
	yaml, _ := uniqueTeamYAML(t, dataPlaneYAML(100, 20))
	env := newDataEnv(t, yaml, withRedis(t))

	resp, _ := env.post(t, goodBody)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, _ = env.post(t, goodBody)
	require.Equal(t, http.StatusOK, resp.StatusCode, "spend is checked before the call, and $12 < $20")

	resp, body := env.post(t, goodBody)
	assert.Equal(t, http.StatusPaymentRequired, resp.StatusCode)
	assert.Equal(t, "budget_exhausted", body["error"])
	assert.Equal(t, "daily", body["period"])
	assert.EqualValues(t, 20, body["limit_usd"])
	assert.EqualValues(t, 24, body["spent_usd"])
	assert.NotEmpty(t, body["resets_at"])
	assert.Equal(t, 2, env.mock.Calls())
}

func TestAdminUsageShowsSpend(t *testing.T) {
	yaml, id := uniqueTeamYAML(t, dataPlaneYAML(100, 100))
	env := newDataEnv(t, yaml, withRedis(t))

	resp, _ := env.post(t, goodBody)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/admin/teams/"+id+"/usage", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	r, err := env.ts.Client().Do(req)
	require.NoError(t, err)
	defer r.Body.Close()
	var body struct {
		Spend struct {
			Daily struct {
				SpentUSD float64 `json:"spent_usd"`
				LimitUSD float64 `json:"limit_usd"`
				ResetsAt string  `json:"resets_at"`
			} `json:"daily"`
			Monthly struct {
				SpentUSD float64 `json:"spent_usd"`
			} `json:"monthly"`
		} `json:"spend"`
	}
	require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
	assert.InDelta(t, 12, body.Spend.Daily.SpentUSD, 1e-9)
	assert.InDelta(t, 100, body.Spend.Daily.LimitUSD, 1e-9)
	assert.InDelta(t, 12, body.Spend.Monthly.SpentUSD, 1e-9)
	assert.NotEmpty(t, body.Spend.Daily.ResetsAt)
}
