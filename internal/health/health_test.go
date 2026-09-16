package health_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ShriramJana/crossbar/internal/health"
	"github.com/ShriramJana/crossbar/internal/provider"
)

type changes struct {
	mu   sync.Mutex
	seen []string
}

func (c *changes) on(name string, from, to health.Status) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, name+":"+from.String()+"->"+to.String())
}

func (c *changes) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

func newMonitor(t *testing.T, opts health.Options, providers ...*provider.Mock) (*health.Monitor, *changes) {
	t.Helper()
	ch := &changes{}
	probers := make([]health.Prober, 0, len(providers))
	for _, p := range providers {
		probers = append(probers, p)
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	opts.OnChange = ch.on
	return health.NewMonitor(probers, opts), ch
}

func TestStatusString(t *testing.T) {
	assert.Equal(t, "healthy", health.Healthy.String())
	assert.Equal(t, "degraded", health.Degraded.String())
	assert.Equal(t, "down", health.Down.String())
}

func TestUnknownProviderIsHealthy(t *testing.T) {
	m, _ := newMonitor(t, health.Options{})
	assert.Equal(t, health.Healthy, m.Status("nope"))
}

func TestCheckRecordsOneProbePerProvider(t *testing.T) {
	a, b := provider.NewMock("a"), provider.NewMock("b")
	m, _ := newMonitor(t, health.Options{}, a, b)

	m.Check(context.Background())

	for _, name := range []string{"a", "b"} {
		assert.Equal(t, health.Healthy, m.Status(name))
		r := m.Snapshot()[name]
		assert.Equal(t, 1, r.Probes)
		assert.Zero(t, r.ErrorRate)
		assert.False(t, r.LastChecked.IsZero())
		assert.Empty(t, r.LastError)
	}
}

func TestStatusDerivation(t *testing.T) {
	tests := []struct {
		name     string
		outcomes []bool // true = probe succeeds, oldest first
		want     health.Status
	}{
		{name: "all good", outcomes: []bool{true, true, true}, want: health.Healthy},
		{name: "one old failure is degraded", outcomes: []bool{false, true, true}, want: health.Degraded},
		{name: "one recent failure under half is degraded", outcomes: []bool{true, true, true, false}, want: health.Degraded},
		{name: "first probe failing is down", outcomes: []bool{false}, want: health.Down},
		{name: "two failures in a row is down", outcomes: []bool{true, false, false}, want: health.Down},
		{name: "recovering after down is degraded, not down", outcomes: []bool{false, false, true}, want: health.Degraded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := provider.NewMock("p")
			m, _ := newMonitor(t, health.Options{}, p)
			for _, ok := range tt.outcomes {
				if ok {
					p.SetDefault(provider.Succeed(""))
				} else {
					p.SetDefault(provider.Fail(503))
				}
				m.Check(context.Background())
			}
			assert.Equal(t, tt.want, m.Status("p"))
		})
	}
}

func TestDegradedOnLatency(t *testing.T) {
	p := provider.NewMock("p")
	p.SetDefault(provider.Slow(30 * time.Millisecond))
	m, _ := newMonitor(t, health.Options{DegradedLatency: 10 * time.Millisecond}, p)

	m.Check(context.Background())
	assert.Equal(t, health.Degraded, m.Status("p"))
	r := m.Snapshot()["p"]
	assert.GreaterOrEqual(t, r.P99, 30*time.Millisecond)
	assert.Zero(t, r.ErrorRate)
}

func TestLastErrorIsRecorded(t *testing.T) {
	p := provider.NewMock("p")
	p.SetDefault(provider.Fail(503))
	m, _ := newMonitor(t, health.Options{}, p)
	m.Check(context.Background())
	assert.Contains(t, m.Snapshot()["p"].LastError, "503")
}

func TestHistoryIsBounded(t *testing.T) {
	p := provider.NewMock("p")
	m, _ := newMonitor(t, health.Options{History: 3}, p)
	p.SetDefault(provider.Fail(503))
	for i := 0; i < 3; i++ {
		m.Check(context.Background())
	}
	p.SetDefault(provider.Succeed(""))
	for i := 0; i < 3; i++ {
		m.Check(context.Background())
	}
	r := m.Snapshot()["p"]
	assert.Equal(t, 3, r.Probes)
	assert.Zero(t, r.ErrorRate, "old failures have rolled out of the window")
	assert.Equal(t, health.Healthy, m.Status("p"))
}

func TestOnChangeFiresOnTransitionsOnly(t *testing.T) {
	p := provider.NewMock("p")
	m, ch := newMonitor(t, health.Options{}, p)

	m.Check(context.Background()) // healthy, no change from the initial healthy
	p.SetDefault(provider.Fail(503))
	m.Check(context.Background()) // down
	m.Check(context.Background()) // still down: no event
	p.SetDefault(provider.Succeed(""))
	m.Check(context.Background()) // degraded

	assert.Equal(t, []string{"p:healthy->down", "p:down->degraded"}, ch.list())
}

func TestProbeTimeoutCountsAsFailure(t *testing.T) {
	p := provider.NewMock("p")
	p.SetDefault(provider.Timeout())
	m, _ := newMonitor(t, health.Options{Timeout: 20 * time.Millisecond}, p)

	start := time.Now()
	m.Check(context.Background())
	assert.Less(t, time.Since(start), 500*time.Millisecond, "probe must be bounded by Timeout")
	assert.Equal(t, health.Down, m.Status("p"))
}

func TestStartProbesOnIntervalAndStopsOnCancel(t *testing.T) {
	p := provider.NewMock("p")
	m, _ := newMonitor(t, health.Options{Interval: 10 * time.Millisecond}, p)

	ctx, cancel := context.WithCancel(context.Background())
	done := m.Start(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && m.Snapshot()["p"].Probes < 3 {
		time.Sleep(5 * time.Millisecond)
	}
	assert.GreaterOrEqual(t, m.Snapshot()["p"].Probes, 3, "probes must repeat on the interval")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor goroutines did not exit after cancel")
	}
	probes := m.Snapshot()["p"].Probes
	time.Sleep(30 * time.Millisecond)
	assert.Equal(t, probes, m.Snapshot()["p"].Probes, "no probes after cancel")
}

func TestStartProbesImmediately(t *testing.T) {
	p := provider.NewMock("p")
	p.SetDefault(provider.Fail(503))
	m, _ := newMonitor(t, health.Options{Interval: time.Hour}, p)

	ctx, cancel := context.WithCancel(context.Background())
	done := m.Start(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && m.Status("p") != health.Down {
		time.Sleep(5 * time.Millisecond)
	}
	assert.Equal(t, health.Down, m.Status("p"), "first probe runs at start, not after the first interval")
	cancel()
	<-done
}

func TestSnapshotIsSafeDuringProbes(t *testing.T) {
	p := provider.NewMock("p")
	m, _ := newMonitor(t, health.Options{Interval: time.Millisecond}, p)
	ctx, cancel := context.WithCancel(context.Background())
	done := m.Start(ctx)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = m.Snapshot()
				_ = m.Status("p")
			}
		}()
	}
	wg.Wait()
	cancel()
	<-done
	require.Positive(t, m.Snapshot()["p"].Probes)
}
