package breaker_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ShriramJana/crossbar/internal/breaker"
)

// clock is a fake time source so window and cooldown behaviour can be tested
// without sleeping.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var defaultSettings = breaker.Settings{
	FailureThreshold: 0.5,
	MinRequests:      4,
	Window:           30 * time.Second,
	Cooldown:         10 * time.Second,
	MaxCooldown:      40 * time.Second,
}

type transition struct{ from, to breaker.State }

type harness struct {
	b           *breaker.Breaker
	clk         *clock
	mu          sync.Mutex
	transitions []transition
}

func newHarness(t *testing.T, s breaker.Settings) *harness {
	t.Helper()
	h := &harness{clk: newClock()}
	h.b = breaker.New(func() breaker.Settings { return s }, breaker.Options{
		Now: h.clk.Now,
		OnTransition: func(from, to breaker.State) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.transitions = append(h.transitions, transition{from, to})
		},
	})
	return h
}

// record takes a permit and immediately records the outcome.
func (h *harness) record(t *testing.T, success bool) {
	t.Helper()
	p, ok := h.b.Allow()
	require.True(t, ok, "expected a permit in state %s", h.b.State())
	if success {
		p.Success()
	} else {
		p.Failure()
	}
}

// trip drives a closed breaker open with MinRequests failures.
func (h *harness) trip(t *testing.T) {
	t.Helper()
	for i := 0; i < defaultSettings.MinRequests; i++ {
		h.record(t, false)
	}
	require.Equal(t, breaker.Open, h.b.State())
}

func TestStateString(t *testing.T) {
	assert.Equal(t, "closed", breaker.Closed.String())
	assert.Equal(t, "half_open", breaker.HalfOpen.String())
	assert.Equal(t, "open", breaker.Open.String())
}

func TestClosedToOpenThreshold(t *testing.T) {
	tests := []struct {
		name      string
		failures  int
		successes int
		want      breaker.State
	}{
		{name: "no traffic stays closed", want: breaker.Closed},
		{name: "all failures but under min_requests", failures: 3, want: breaker.Closed},
		{name: "exactly threshold at min_requests opens", failures: 2, successes: 2, want: breaker.Open},
		{name: "under threshold at min_requests stays closed", failures: 1, successes: 3, want: breaker.Closed},
		{name: "well over threshold opens", failures: 9, successes: 1, want: breaker.Open},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, defaultSettings)
			// Interleave so ordering is not what trips it.
			for i := 0; i < tt.failures || i < tt.successes; i++ {
				if i < tt.successes {
					h.record(t, true)
				}
				if i < tt.failures && h.b.State() == breaker.Closed {
					h.record(t, false)
				}
			}
			assert.Equal(t, tt.want, h.b.State())
		})
	}
}

func TestOpenRejectsUntilCooldown(t *testing.T) {
	h := newHarness(t, defaultSettings)
	h.trip(t)

	_, ok := h.b.Allow()
	assert.False(t, ok, "open breaker must reject")

	h.clk.Advance(defaultSettings.Cooldown - time.Millisecond)
	_, ok = h.b.Allow()
	assert.False(t, ok, "still within cooldown")

	h.clk.Advance(time.Millisecond)
	p, ok := h.b.Allow()
	require.True(t, ok, "cooldown elapsed: one probe is allowed")
	assert.Equal(t, breaker.HalfOpen, h.b.State())

	_, ok = h.b.Allow()
	assert.False(t, ok, "only one probe may be in flight")
	p.Success()
}

func TestProbeSuccessCloses(t *testing.T) {
	h := newHarness(t, defaultSettings)
	h.trip(t)
	h.clk.Advance(defaultSettings.Cooldown)

	p, ok := h.b.Allow()
	require.True(t, ok)
	p.Success()

	assert.Equal(t, breaker.Closed, h.b.State())
	snap := h.b.Snapshot()
	assert.Zero(t, snap.Requests, "window is cleared on recovery")
	assert.Zero(t, snap.Failures)
	assert.Equal(t, defaultSettings.Cooldown, snap.Cooldown, "cooldown resets to base on recovery")

	_, ok = h.b.Allow()
	assert.True(t, ok)
}

func TestProbeFailureReopensWithDoubledCooldown(t *testing.T) {
	h := newHarness(t, defaultSettings)
	h.trip(t)
	assert.Equal(t, defaultSettings.Cooldown, h.b.Snapshot().Cooldown)

	wantCooldowns := []time.Duration{20 * time.Second, 40 * time.Second, 40 * time.Second} // doubles, then capped
	for i, want := range wantCooldowns {
		h.clk.Advance(h.b.Snapshot().Cooldown)
		p, ok := h.b.Allow()
		require.True(t, ok, "probe %d", i)
		p.Failure()
		assert.Equal(t, breaker.Open, h.b.State())
		assert.Equal(t, want, h.b.Snapshot().Cooldown, "after failed probe %d", i+1)

		h.clk.Advance(want - time.Second)
		_, ok = h.b.Allow()
		assert.False(t, ok, "must wait the full doubled cooldown")
		h.clk.Advance(time.Second)
	}
}

func TestCooldownResetsAfterRecovery(t *testing.T) {
	h := newHarness(t, defaultSettings)
	h.trip(t)
	h.clk.Advance(defaultSettings.Cooldown)
	p, _ := h.b.Allow()
	p.Failure() // cooldown now 20s
	h.clk.Advance(20 * time.Second)
	p, _ = h.b.Allow()
	p.Success() // recovered

	h.trip(t)
	assert.Equal(t, defaultSettings.Cooldown, h.b.Snapshot().Cooldown, "a fresh outage starts from the base cooldown")
}

func TestStalePermitsDoNotAffectLaterStates(t *testing.T) {
	h := newHarness(t, defaultSettings)

	// Hand out permits while closed, then trip the breaker with four of them.
	var permits []breaker.Permit
	for i := 0; i < 7; i++ {
		p, ok := h.b.Allow()
		require.True(t, ok)
		permits = append(permits, p)
	}
	for i := 0; i < 4; i++ {
		permits[i].Failure()
	}
	require.Equal(t, breaker.Open, h.b.State())

	// Results from pre-open permits must not touch the open state.
	permits[4].Success()
	permits[5].Failure()
	assert.Equal(t, breaker.Open, h.b.State())

	h.clk.Advance(defaultSettings.Cooldown)
	probe, ok := h.b.Allow()
	require.True(t, ok)
	require.Equal(t, breaker.HalfOpen, h.b.State())

	// A stale success while half-open must not close the breaker: only the probe decides.
	permits[6].Success()
	assert.Equal(t, breaker.HalfOpen, h.b.State())

	probe.Failure()
	assert.Equal(t, breaker.Open, h.b.State())
}

func TestWindowExpiresOldResults(t *testing.T) {
	h := newHarness(t, defaultSettings)
	h.record(t, false)
	h.record(t, false)

	h.clk.Advance(defaultSettings.Window + time.Second)
	h.record(t, true)
	h.record(t, true)
	assert.Equal(t, breaker.Closed, h.b.State(), "failures outside the window must not count")
	snap := h.b.Snapshot()
	assert.Equal(t, 2, snap.Requests)
	assert.Equal(t, 0, snap.Failures)

	h.record(t, false)
	h.record(t, false)
	assert.Equal(t, breaker.Open, h.b.State(), "fresh failures within the window do count")
}

func TestReset(t *testing.T) {
	h := newHarness(t, defaultSettings)
	h.trip(t)
	h.b.Reset()
	assert.Equal(t, breaker.Closed, h.b.State())
	assert.Zero(t, h.b.Snapshot().Requests)
	_, ok := h.b.Allow()
	assert.True(t, ok)
}

func TestTransitionsAreReported(t *testing.T) {
	h := newHarness(t, defaultSettings)
	h.trip(t)
	h.clk.Advance(defaultSettings.Cooldown)
	p, _ := h.b.Allow()
	p.Failure()
	h.clk.Advance(20 * time.Second)
	p, _ = h.b.Allow()
	p.Success()

	assert.Equal(t, []transition{
		{breaker.Closed, breaker.Open},
		{breaker.Open, breaker.HalfOpen},
		{breaker.HalfOpen, breaker.Open},
		{breaker.Open, breaker.HalfOpen},
		{breaker.HalfOpen, breaker.Closed},
	}, h.transitions)
}

func TestSnapshotWhileOpenReportsRetryAt(t *testing.T) {
	h := newHarness(t, defaultSettings)
	h.trip(t)
	snap := h.b.Snapshot()
	assert.Equal(t, breaker.Open, snap.State)
	assert.Equal(t, h.clk.Now().Add(defaultSettings.Cooldown), snap.RetryAt)
	assert.Equal(t, 4, snap.Requests)
	assert.Equal(t, 4, snap.Failures)
}

func TestSettingsAreReadLive(t *testing.T) {
	var mu sync.Mutex
	s := defaultSettings
	h := &harness{clk: newClock()}
	h.b = breaker.New(func() breaker.Settings { mu.Lock(); defer mu.Unlock(); return s }, breaker.Options{Now: h.clk.Now})

	h.record(t, false)
	h.record(t, true)
	h.record(t, true)
	h.record(t, true) // 4 requests, 25% failures: under 0.5
	require.Equal(t, breaker.Closed, h.b.State())

	mu.Lock()
	s.FailureThreshold = 0.2
	mu.Unlock()
	h.record(t, true) // re-evaluated at 5 requests, 20% failures: at the new threshold
	assert.Equal(t, breaker.Open, h.b.State())
}

func TestHalfOpenAdmitsExactlyOneProbeUnderContention(t *testing.T) {
	h := newHarness(t, defaultSettings)
	h.trip(t)
	h.clk.Advance(defaultSettings.Cooldown)

	const n = 200
	var wg sync.WaitGroup
	var mu sync.Mutex
	var admitted []breaker.Permit
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if p, ok := h.b.Allow(); ok {
				mu.Lock()
				admitted = append(admitted, p)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	require.Len(t, admitted, 1, "exactly one goroutine may probe")
	admitted[0].Success()
	assert.Equal(t, breaker.Closed, h.b.State())
}

func TestConcurrentRecordingIsConsistent(t *testing.T) {
	s := defaultSettings
	s.MinRequests = 1_000_000 // never trips; we are checking the counters
	h := newHarness(t, s)

	const n = 1000
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, ok := h.b.Allow()
			if !ok {
				t.Error("closed breaker rejected a request")
				return
			}
			if i%4 == 0 {
				p.Failure()
			} else {
				p.Success()
			}
		}(i)
	}
	wg.Wait()

	snap := h.b.Snapshot()
	assert.Equal(t, n, snap.Requests)
	assert.Equal(t, n/4, snap.Failures)
	assert.Equal(t, breaker.Closed, snap.State)
}

func TestRegistry(t *testing.T) {
	clk := newClock()
	var mu sync.Mutex
	var seen []string
	reg := breaker.NewRegistry(func() breaker.Settings { return defaultSettings }, breaker.RegistryOptions{
		Now: clk.Now,
		OnTransition: func(provider, model string, from, to breaker.State) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, provider+"/"+model+":"+from.String()+"->"+to.String())
		},
	})

	a := reg.Get("anthropic", "big")
	assert.Same(t, a, reg.Get("anthropic", "big"), "same key returns the same breaker")
	b := reg.Get("openai", "big")
	assert.NotSame(t, a, b)

	for i := 0; i < 4; i++ {
		p, _ := a.Allow()
		p.Failure()
	}
	assert.Equal(t, breaker.Open, a.State())
	assert.Equal(t, breaker.Closed, b.State())
	assert.Equal(t, []string{"anthropic/big:closed->open"}, seen)

	snaps := reg.Snapshot()
	require.Len(t, snaps, 2)
	assert.Equal(t, "anthropic", snaps[0].Provider, "snapshot is sorted by provider then model")
	assert.Equal(t, breaker.Open, snaps[0].State)
	assert.Equal(t, "openai", snaps[1].Provider)

	assert.True(t, reg.Reset("anthropic", "big"))
	assert.Equal(t, breaker.Closed, a.State())
	assert.False(t, reg.Reset("nope", "nope"))
}

func TestRegistryConcurrentGet(t *testing.T) {
	reg := breaker.NewRegistry(func() breaker.Settings { return defaultSettings }, breaker.RegistryOptions{})
	var wg sync.WaitGroup
	results := make([]*breaker.Breaker, 100)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = reg.Get("p", "m")
		}(i)
	}
	wg.Wait()
	for _, r := range results[1:] {
		assert.Same(t, results[0], r)
	}
}
