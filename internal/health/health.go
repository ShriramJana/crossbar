// Package health runs a background prober per provider and derives a
// coarse status from the recent probe history.
package health

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Status is a provider's derived health.
type Status int

// Statuses, ordered by severity.
const (
	Healthy Status = iota
	Degraded
	Down
)

// String returns the label used in logs, metrics, and the health endpoint.
func (s Status) String() string {
	switch s {
	case Healthy:
		return "healthy"
	case Degraded:
		return "degraded"
	case Down:
		return "down"
	default:
		return "unknown"
	}
}

// Prober is what the monitor needs from a provider.
type Prober interface {
	Name() string
	HealthCheck(ctx context.Context) error
}

// Defaults for Options.
const (
	DefaultInterval        = 30 * time.Second
	DefaultTimeout         = 5 * time.Second
	DefaultDegradedLatency = 2 * time.Second
	DefaultHistory         = 10
)

// Options configure a Monitor.
type Options struct {
	// Interval between probes of one provider.
	Interval time.Duration
	// Timeout bounds a single probe.
	Timeout time.Duration
	// DegradedLatency is the p99 probe latency above which a provider is degraded.
	DegradedLatency time.Duration
	// History is how many recent probes the status is derived from.
	History int
	// Logger records status changes. Defaults to slog.Default().
	Logger *slog.Logger
	// OnChange is invoked when a provider's status changes.
	OnChange func(name string, from, to Status)
}

// Report is a provider's health as seen by the monitor.
type Report struct {
	Status      Status
	ErrorRate   float64
	P99         time.Duration
	LastError   string
	LastChecked time.Time
	// Probes is how many results are currently in the window.
	Probes int
}

type probe struct {
	ok      bool
	latency time.Duration
}

type entry struct {
	prober Prober

	mu          sync.Mutex
	history     []probe
	status      Status
	lastError   string
	lastChecked time.Time
}

// Monitor probes providers and exposes their status. It is safe for concurrent use.
type Monitor struct {
	opts    Options
	entries map[string]*entry
	names   []string
}

// NewMonitor builds a Monitor over probers. Every provider starts Healthy
// until its first probe says otherwise.
func NewMonitor(probers []Prober, opts Options) *Monitor {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.DegradedLatency <= 0 {
		opts.DegradedLatency = DefaultDegradedLatency
	}
	if opts.History <= 0 {
		opts.History = DefaultHistory
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	m := &Monitor{opts: opts, entries: make(map[string]*entry, len(probers))}
	for _, p := range probers {
		m.entries[p.Name()] = &entry{prober: p}
		m.names = append(m.names, p.Name())
	}
	sort.Strings(m.names)
	return m
}

// Start launches one goroutine per provider that probes immediately and
// then every Interval until ctx is cancelled. The returned channel closes
// once every goroutine has exited.
func (m *Monitor) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	var wg sync.WaitGroup
	for _, name := range m.names {
		e := m.entries[name]
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.probeOne(ctx, e)
			ticker := time.NewTicker(m.opts.Interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					m.probeOne(ctx, e)
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

// Check probes every provider once, concurrently, and returns when all are
// done. Start uses the same probe; Check exists for startup and tests.
func (m *Monitor) Check(ctx context.Context) {
	var wg sync.WaitGroup
	for _, e := range m.entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.probeOne(ctx, e)
		}()
	}
	wg.Wait()
}

func (m *Monitor) probeOne(ctx context.Context, e *entry) {
	if ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, m.opts.Timeout)
	defer cancel()

	start := time.Now()
	err := e.prober.HealthCheck(ctx)
	latency := time.Since(start)

	e.mu.Lock()
	e.history = append(e.history, probe{ok: err == nil, latency: latency})
	if len(e.history) > m.opts.History {
		e.history = e.history[len(e.history)-m.opts.History:]
	}
	e.lastChecked = time.Now()
	if err != nil {
		e.lastError = err.Error()
	} else {
		e.lastError = ""
	}
	from := e.status
	to := derive(e.history, m.opts.DegradedLatency)
	e.status = to
	e.mu.Unlock()

	if from != to {
		m.opts.Logger.Warn("provider health changed",
			slog.String("provider", e.prober.Name()),
			slog.String("from", from.String()),
			slog.String("to", to.String()),
			slog.Any("last_error", err))
		if m.opts.OnChange != nil {
			m.opts.OnChange(e.prober.Name(), from, to)
		}
	}
}

// derive turns the recent history into a status. Down requires both a
// failing latest probe and a failure rate of at least half, so one blip is
// degraded rather than down and one recovery is degraded rather than
// healthy. Any failure in the window, or a slow p99, is degraded.
func derive(history []probe, degradedLatency time.Duration) Status {
	if len(history) == 0 {
		return Healthy
	}
	rate, p99 := stats(history)
	latest := history[len(history)-1]
	switch {
	case !latest.ok && rate >= 0.5:
		return Down
	case rate > 0 || p99 > degradedLatency:
		return Degraded
	default:
		return Healthy
	}
}

func stats(history []probe) (errorRate float64, p99 time.Duration) {
	failures := 0
	latencies := make([]time.Duration, 0, len(history))
	for _, p := range history {
		if !p.ok {
			failures++
		}
		latencies = append(latencies, p.latency)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	idx := (len(latencies)*99+99)/100 - 1 // nearest-rank percentile
	if idx < 0 {
		idx = 0
	}
	return float64(failures) / float64(len(history)), latencies[idx]
}

// Status returns a provider's current status. Unknown providers are Healthy
// so that a name the monitor was not told about is never excluded.
func (m *Monitor) Status(name string) Status {
	e, ok := m.entries[name]
	if !ok {
		return Healthy
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status
}

// Snapshot returns every provider's report, keyed by name.
func (m *Monitor) Snapshot() map[string]Report {
	out := make(map[string]Report, len(m.entries))
	for name, e := range m.entries {
		e.mu.Lock()
		r := Report{Status: e.status, LastError: e.lastError, LastChecked: e.lastChecked, Probes: len(e.history)}
		if len(e.history) > 0 {
			r.ErrorRate, r.P99 = stats(e.history)
		}
		e.mu.Unlock()
		out[name] = r
	}
	return out
}
