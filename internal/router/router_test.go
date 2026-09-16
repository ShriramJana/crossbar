package router_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ShriramJana/crossbar/internal/breaker"
	"github.com/ShriramJana/crossbar/internal/config"
	"github.com/ShriramJana/crossbar/internal/health"
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
breaker:
  min_requests: 4
  cooldown: 10s
retry:
  max_retries: 2
  base_backoff: 100ms
  max_backoff: 1s
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

// clock is a fake time source shared by the breaker registry and the router.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// stubHealth reports a fixed status per provider.
type stubHealth map[string]health.Status

func (s stubHealth) Status(name string) health.Status { return s[name] }

type env struct {
	store     *config.Store
	primary   *provider.Mock
	secondary *provider.Mock
	breakers  *breaker.Registry
	clk       *clock
	mu        sync.Mutex
	sleeps    []time.Duration
	health    stubHealth
}

func (e *env) sleep(ctx context.Context, d time.Duration) error {
	e.mu.Lock()
	e.sleeps = append(e.sleeps, d)
	e.mu.Unlock()
	return ctx.Err()
}

func (e *env) recordedSleeps() []time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]time.Duration(nil), e.sleeps...)
}

func newEnv(t *testing.T) (*env, *router.Router) {
	t.Helper()
	e := &env{
		store:     newStore(t),
		primary:   provider.NewMock("primary"),
		secondary: provider.NewMock("secondary"),
		clk:       &clock{t: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)},
		health:    stubHealth{},
	}
	e.breakers = breaker.NewRegistry(
		func() breaker.Settings { return breaker.SettingsFromConfig(e.store.Current().Breaker) },
		breaker.RegistryOptions{Now: e.clk.Now},
	)
	r := router.New(e.store, map[string]provider.Provider{"primary": e.primary, "secondary": e.secondary}, router.Options{
		Breakers: e.breakers,
		Health:   e.health,
		Sleep:    e.sleep,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return e, r
}

func bigRequest() *provider.Request {
	return &provider.Request{
		Model:     "big",
		Messages:  []provider.Message{{Role: "user", Content: "one two three"}}, // 3 input tokens
		MaxTokens: 16,
	}
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
	e, r := newEnv(t)

	resp, err := r.Dispatch(context.Background(), bigRequest())
	require.NoError(t, err)
	assert.Equal(t, "primary", resp.Provider)
	assert.Equal(t, "big", resp.Model)
	assert.False(t, resp.Fallback)
	assert.Equal(t, 1, e.primary.Calls())
	assert.Equal(t, 0, e.secondary.Calls())

	// mock output "mock response to: one two three" is 6 tokens.
	wantCost := 3*10.0/1e6 + 6*20.0/1e6
	assert.InDelta(t, wantCost, resp.CostUSD, 1e-12)
	assert.Empty(t, e.recordedSleeps())
}

func TestDispatchUnknownModel(t *testing.T) {
	_, r := newEnv(t)
	_, err := r.Dispatch(context.Background(), &provider.Request{Model: "nope", MaxTokens: 1})
	assert.True(t, errors.Is(err, router.ErrUnknownModel))
}

func TestDispatchMissingProviderAdapter(t *testing.T) {
	store := newStore(t)
	r := router.New(store, map[string]provider.Provider{"primary": provider.NewMock("primary")}, router.Options{})

	// "small-alt" lives on "secondary", which has no adapter registered; the
	// chain continues to primary/small... no: the chain for small-alt is
	// [secondary/small-alt, primary/small, primary/big], so primary serves it.
	resp, err := r.Dispatch(context.Background(), &provider.Request{Model: "small-alt", MaxTokens: 1, Messages: []provider.Message{{Role: "user", Content: "x"}}})
	require.NoError(t, err)
	assert.Equal(t, "primary", resp.Provider)
	assert.True(t, resp.Fallback)
}

func TestNonRetryableErrorReturnsImmediately(t *testing.T) {
	e, r := newEnv(t)
	e.primary.Enqueue(provider.Fail(400))

	_, err := r.Dispatch(context.Background(), bigRequest())
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, 400, pe.StatusCode)
	assert.Equal(t, 1, e.primary.Calls(), "no retry")
	assert.Equal(t, 0, e.secondary.Calls(), "no fallback")
	assert.Empty(t, e.recordedSleeps())

	snap := e.breakers.Get("primary", "big").Snapshot()
	assert.Equal(t, 0, snap.Failures, "a client error is not the provider's fault")
	assert.Equal(t, 1, snap.Requests, "but the provider did answer")
}

func TestRetryableErrorIsRetriedThenSucceeds(t *testing.T) {
	e, r := newEnv(t)
	e.primary.Enqueue(provider.Fail(500), provider.Fail(503))

	resp, err := r.Dispatch(context.Background(), bigRequest())
	require.NoError(t, err)
	assert.Equal(t, "primary", resp.Provider)
	assert.False(t, resp.Fallback, "served by the primary after retries is not a fallback")
	assert.Equal(t, 3, e.primary.Calls())
	assert.Equal(t, 0, e.secondary.Calls())

	sleeps := e.recordedSleeps()
	require.Len(t, sleeps, 2, "one backoff before each retry")
	assert.LessOrEqual(t, sleeps[0], 100*time.Millisecond, "first backoff is full-jittered within base")
	assert.LessOrEqual(t, sleeps[1], 200*time.Millisecond, "second backoff is full-jittered within 2x base")
	for _, s := range sleeps {
		assert.GreaterOrEqual(t, s, time.Duration(0))
	}

	snap := e.breakers.Get("primary", "big").Snapshot()
	assert.Equal(t, 2, snap.Failures)
	assert.Equal(t, 3, snap.Requests)
}

func TestTransportErrorIsRetried(t *testing.T) {
	e, r := newEnv(t)
	e.primary.Enqueue(provider.Transport())

	resp, err := r.Dispatch(context.Background(), bigRequest())
	require.NoError(t, err)
	assert.Equal(t, "primary", resp.Provider)
	assert.Equal(t, 2, e.primary.Calls())
}

func TestFallbackAfterRetriesExhausted(t *testing.T) {
	e, r := newEnv(t)
	e.primary.Enqueue(provider.Fail(500), provider.Fail(500), provider.Fail(500))

	resp, err := r.Dispatch(context.Background(), bigRequest())
	require.NoError(t, err)
	assert.Equal(t, "secondary", resp.Provider)
	assert.Equal(t, "big-alt", resp.Model)
	assert.True(t, resp.Fallback)
	assert.Equal(t, 3, e.primary.Calls(), "1 attempt + 2 retries")
	assert.Equal(t, 1, e.secondary.Calls())
	assert.InDelta(t, 3*1.0/1e6+6*2.0/1e6, resp.CostUSD, 1e-12, "priced at the provider that served it")
}

func TestBackoffIsCapped(t *testing.T) {
	store := newStore(t)
	// 6 retries with base 100ms would reach 3.2s; max_backoff caps at 1s.
	require.NoError(t, os.WriteFile(store.Path(), []byte(replaceOnce(testYAML, "max_retries: 2", "max_retries: 6")), 0o600))
	require.NoError(t, store.Reload())

	e := &env{clk: &clock{t: time.Now()}}
	primary := provider.NewMock("primary")
	for i := 0; i < 6; i++ {
		primary.Enqueue(provider.Fail(500))
	}
	r := router.New(store, map[string]provider.Provider{"primary": primary, "secondary": provider.NewMock("secondary")}, router.Options{
		Sleep: e.sleep,
		Rand:  func() float64 { return 1 }, // no jitter: sleep the full ceiling
	})
	_, err := r.Dispatch(context.Background(), bigRequest())
	require.NoError(t, err)
	assert.Equal(t, []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, time.Second, time.Second}, e.recordedSleeps())
}

func replaceOnce(s, old, repl string) string {
	i := indexOf(s, old)
	if i < 0 {
		return s
	}
	return s[:i] + repl + s[i+len(old):]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestChainExhausted(t *testing.T) {
	e, r := newEnv(t)
	e.primary.SetDefault(provider.Fail(503))
	e.secondary.SetDefault(provider.Transport())

	_, err := r.Dispatch(context.Background(), bigRequest())
	var ce *router.ChainError
	require.ErrorAs(t, err, &ce)
	require.Len(t, ce.Attempts, 2)
	assert.Equal(t, "primary", ce.Attempts[0].Provider)
	assert.Equal(t, "big", ce.Attempts[0].Model)
	assert.Equal(t, router.ReasonFailed, ce.Attempts[0].Reason)
	assert.Equal(t, 3, ce.Attempts[0].Tries)
	assert.Contains(t, ce.Attempts[0].Error, "503")
	assert.Equal(t, "secondary", ce.Attempts[1].Provider)
	assert.Equal(t, router.ReasonFailed, ce.Attempts[1].Reason)
	assert.Equal(t, 3, e.primary.Calls())
	assert.Equal(t, 3, e.secondary.Calls())
	assert.Contains(t, err.Error(), "primary/big")
}

func TestOpenBreakerIsSkipped(t *testing.T) {
	e, r := newEnv(t)
	b := e.breakers.Get("primary", "big")
	for i := 0; i < 4; i++ {
		p, _ := b.Allow()
		p.Failure()
	}
	require.Equal(t, breaker.Open, b.State())

	resp, err := r.Dispatch(context.Background(), bigRequest())
	require.NoError(t, err)
	assert.Equal(t, "secondary", resp.Provider)
	assert.True(t, resp.Fallback)
	assert.Equal(t, 0, e.primary.Calls(), "an open breaker rejects instantly with no upstream call")
}

func TestOpenBreakerOnEveryTargetIsReported(t *testing.T) {
	e, r := newEnv(t)
	for _, key := range [][2]string{{"primary", "big"}, {"secondary", "big-alt"}} {
		b := e.breakers.Get(key[0], key[1])
		for i := 0; i < 4; i++ {
			p, _ := b.Allow()
			p.Failure()
		}
	}
	_, err := r.Dispatch(context.Background(), bigRequest())
	var ce *router.ChainError
	require.ErrorAs(t, err, &ce)
	require.Len(t, ce.Attempts, 2)
	assert.Equal(t, router.ReasonBreakerOpen, ce.Attempts[0].Reason)
	assert.Equal(t, router.ReasonBreakerOpen, ce.Attempts[1].Reason)
	assert.Equal(t, 0, ce.Attempts[0].Tries)
}

func TestDownProviderIsSkipped(t *testing.T) {
	e, r := newEnv(t)
	e.health["primary"] = health.Down

	resp, err := r.Dispatch(context.Background(), bigRequest())
	require.NoError(t, err)
	assert.Equal(t, "secondary", resp.Provider)
	assert.True(t, resp.Fallback)
	assert.Equal(t, 0, e.primary.Calls())
}

func TestDegradedProviderIsDeprioritizedNotExcluded(t *testing.T) {
	e, r := newEnv(t)
	e.health["primary"] = health.Degraded

	resp, err := r.Dispatch(context.Background(), bigRequest())
	require.NoError(t, err)
	assert.Equal(t, "secondary", resp.Provider, "a healthy target is preferred over a degraded one")
	assert.True(t, resp.Fallback)

	e.health["secondary"] = health.Degraded
	resp, err = r.Dispatch(context.Background(), bigRequest())
	require.NoError(t, err)
	assert.Equal(t, "primary", resp.Provider, "when everything is degraded the configured order stands")
	assert.False(t, resp.Fallback)
}

func TestDeadlineIsSharedAcrossRetriesAndFallbacks(t *testing.T) {
	store := newStore(t)
	primary, secondary := provider.NewMock("primary"), provider.NewMock("secondary")
	slowFailure := provider.Behavior{Delay: 40 * time.Millisecond, Status: 503}
	primary.SetDefault(slowFailure)
	secondary.SetDefault(slowFailure)
	r := router.New(store, map[string]provider.Provider{"primary": primary, "secondary": secondary}, router.Options{
		Rand: func() float64 { return 0 }, // no backoff sleep so timing is just the calls
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Dispatch(ctx, bigRequest())
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "got %v", err)
	assert.Less(t, elapsed, 300*time.Millisecond, "3 retries + 3 fallback tries at 40ms each would be 240ms+ without a shared deadline")
	assert.LessOrEqual(t, primary.Calls()+secondary.Calls(), 3, "no attempt may start after the deadline")
}

func TestBackoffSleepHonoursContext(t *testing.T) {
	store := newStore(t)
	primary := provider.NewMock("primary")
	primary.SetDefault(provider.Fail(500))
	r := router.New(store, map[string]provider.Provider{"primary": primary, "secondary": provider.NewMock("secondary")}, router.Options{
		Rand: func() float64 { return 1 }, // full 100ms backoff
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Dispatch(ctx, bigRequest())
	assert.True(t, errors.Is(err, context.DeadlineExceeded))
	assert.Less(t, time.Since(start), 90*time.Millisecond, "backoff sleep must return when the context expires")
}

// TestBreakerOpensUnderFailureAndRecovers is the M4 acceptance path at the
// router level: a dead primary trips its breaker, traffic fails over with no
// client-visible errors, and once the primary recovers the probe closes the
// breaker and traffic returns.
func TestBreakerOpensUnderFailureAndRecovers(t *testing.T) {
	e, r := newEnv(t)
	e.primary.SetDefault(provider.Fail(503))

	for i := 0; i < 10; i++ {
		resp, err := r.Dispatch(context.Background(), bigRequest())
		require.NoError(t, err, "request %d must succeed via fallback", i)
		assert.Equal(t, "secondary", resp.Provider)
		assert.True(t, resp.Fallback)
	}
	b := e.breakers.Get("primary", "big")
	assert.Equal(t, breaker.Open, b.State())
	primaryCalls := e.primary.Calls()
	assert.LessOrEqual(t, primaryCalls, 6, "breaker trips after min_requests failures, then rejects instantly")

	// Primary recovers; after cooldown one probe goes through and closes the breaker.
	e.primary.SetDefault(provider.Succeed("back"))
	e.clk.Advance(10 * time.Second)

	resp, err := r.Dispatch(context.Background(), bigRequest())
	require.NoError(t, err)
	assert.Equal(t, "primary", resp.Provider, "the probe request is served by the recovered primary")
	assert.False(t, resp.Fallback)
	assert.Equal(t, breaker.Closed, b.State())
	assert.Equal(t, primaryCalls+1, e.primary.Calls())
}

func TestConcurrentDispatchDuringFailover(t *testing.T) {
	e, r := newEnv(t)
	e.primary.SetDefault(provider.Fail(503))

	const n = 100
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Dispatch(context.Background(), bigRequest())
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}
	assert.Equal(t, breaker.Open, e.breakers.Get("primary", "big").State())
	assert.Equal(t, n, e.secondary.Calls())
}
