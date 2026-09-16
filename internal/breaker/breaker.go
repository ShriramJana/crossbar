// Package breaker implements a circuit breaker state machine.
//
// A breaker guards one upstream target. While closed it admits every request
// and tracks outcomes over a rolling window; once the failure ratio crosses
// the threshold with enough traffic behind it, the breaker opens and rejects
// instantly. After a cooldown it admits exactly one probe. A successful probe
// closes the breaker; a failed one reopens it with a doubled cooldown.
package breaker

import (
	"sync"
	"time"

	"github.com/ShriramJana/crossbar/internal/config"
)

// State is the breaker's position in the state machine.
type State int

// Breaker states, ordered so the numeric value reads as severity.
const (
	Closed State = iota
	HalfOpen
	Open
)

// String returns the label used in logs, metrics, and the health endpoint.
func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case HalfOpen:
		return "half_open"
	case Open:
		return "open"
	default:
		return "unknown"
	}
}

// Settings tune one breaker. They are read on every decision so a config
// reload takes effect without recreating breakers.
type Settings struct {
	// FailureThreshold is the failure ratio in (0, 1] at which a closed breaker opens.
	FailureThreshold float64
	// MinRequests is the minimum traffic in the window before the ratio is judged.
	MinRequests int
	// Window is the rolling period over which outcomes are counted.
	Window time.Duration
	// Cooldown is how long an opened breaker waits before allowing a probe.
	Cooldown time.Duration
	// MaxCooldown caps the doubling that follows each failed probe.
	MaxCooldown time.Duration
}

// SettingsFromConfig converts the validated config block.
func SettingsFromConfig(c config.BreakerConfig) Settings {
	return Settings{
		FailureThreshold: c.FailureThreshold,
		MinRequests:      c.MinRequests,
		Window:           c.Window,
		Cooldown:         c.Cooldown,
		MaxCooldown:      c.MaxCooldown,
	}
}

// Options configure a Breaker.
type Options struct {
	// Now supplies the clock; defaults to time.Now.
	Now func() time.Time
	// OnTransition is invoked, outside the breaker's lock, on every state change.
	OnTransition func(from, to State)
}

// numBuckets is the resolution of the rolling window. Each bucket spans
// Window/numBuckets; a result ages out once its bucket leaves the window.
const numBuckets = 10

type bucket struct {
	stamp     int64
	successes int
	failures  int
}

// Breaker is safe for concurrent use.
type Breaker struct {
	settings     func() Settings
	now          func() time.Time
	onTransition func(from, to State)

	mu      sync.Mutex
	state   State
	buckets [numBuckets]bucket
	// gen increments each time a probe is issued so a stale probe permit
	// cannot decide a later half-open period.
	gen         uint64
	probing     bool
	probeIssued time.Time
	cooldown    time.Duration
	openedAt    time.Time
}

// New builds a closed Breaker.
func New(settings func() Settings, opts Options) *Breaker {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Breaker{
		settings:     settings,
		now:          opts.Now,
		onTransition: opts.OnTransition,
		cooldown:     settings().Cooldown,
	}
}

// Permit is the right to make one call, handed out by Allow. Exactly one of
// Success or Failure should be called once the outcome is known. A permit
// issued in an earlier state is ignored when reported, so an in-flight call
// from before the breaker opened cannot disturb the half-open probe.
type Permit struct {
	b     *Breaker
	probe bool
	gen   uint64
}

// Success reports that the call completed and the upstream is healthy.
func (p Permit) Success() { p.report(true) }

// Failure reports that the call failed in a way that is the upstream's fault.
func (p Permit) Failure() { p.report(false) }

func (p Permit) report(success bool) {
	if p.b == nil {
		return
	}
	p.b.mu.Lock()
	tr := p.b.recordLocked(p, success)
	p.b.mu.Unlock()
	p.b.fire(tr)
}

type transition struct {
	from, to State
	fired    bool
}

// Allow reports whether a call may proceed and returns the permit to report
// its outcome with. Closed admits everything; open admits nothing until the
// cooldown elapses; half-open admits one probe at a time.
func (b *Breaker) Allow() (Permit, bool) {
	b.mu.Lock()
	p, ok, tr := b.allowLocked()
	b.mu.Unlock()
	b.fire(tr)
	return p, ok
}

func (b *Breaker) allowLocked() (Permit, bool, transition) {
	now := b.now()
	switch b.state {
	case Closed:
		return Permit{b: b}, true, transition{}
	case Open:
		if now.Before(b.openedAt.Add(b.cooldown)) {
			return Permit{}, false, transition{}
		}
		tr := b.setState(HalfOpen)
		return b.issueProbe(now), true, tr
	default: // HalfOpen
		// A probe whose holder never reported (crashed, hung past every
		// timeout) would otherwise wedge the breaker half-open forever.
		if b.probing && now.Before(b.probeIssued.Add(b.cooldown)) {
			return Permit{}, false, transition{}
		}
		return b.issueProbe(now), true, transition{}
	}
}

func (b *Breaker) issueProbe(now time.Time) Permit {
	b.gen++
	b.probing = true
	b.probeIssued = now
	return Permit{b: b, probe: true, gen: b.gen}
}

func (b *Breaker) recordLocked(p Permit, success bool) transition {
	s := b.settings()
	now := b.now()

	if p.probe {
		if b.state != HalfOpen || p.gen != b.gen {
			return transition{}
		}
		b.probing = false
		if success {
			b.buckets = [numBuckets]bucket{}
			b.cooldown = s.Cooldown
			return b.setState(Closed)
		}
		b.cooldown = min(b.cooldown*2, s.MaxCooldown)
		b.openedAt = now
		return b.setState(Open)
	}

	if b.state != Closed {
		return transition{}
	}
	b.add(s, now, success)
	requests, failures := b.totals(s, now)
	if requests >= s.MinRequests && float64(failures)/float64(requests) >= s.FailureThreshold {
		b.cooldown = s.Cooldown
		b.openedAt = now
		return b.setState(Open)
	}
	return transition{}
}

func (b *Breaker) setState(to State) transition {
	from := b.state
	b.state = to
	return transition{from: from, to: to, fired: from != to}
}

func (b *Breaker) fire(tr transition) {
	if tr.fired && b.onTransition != nil {
		b.onTransition(tr.from, tr.to)
	}
}

func bucketWidth(s Settings) time.Duration {
	return max(s.Window/numBuckets, time.Millisecond)
}

func (b *Breaker) add(s Settings, now time.Time, success bool) {
	stamp := now.UnixNano() / int64(bucketWidth(s))
	bk := &b.buckets[stamp%numBuckets]
	if bk.stamp != stamp {
		*bk = bucket{stamp: stamp}
	}
	if success {
		bk.successes++
	} else {
		bk.failures++
	}
}

func (b *Breaker) totals(s Settings, now time.Time) (requests, failures int) {
	current := now.UnixNano() / int64(bucketWidth(s))
	for _, bk := range b.buckets {
		if bk.stamp > current-numBuckets && bk.stamp <= current {
			requests += bk.successes + bk.failures
			failures += bk.failures
		}
	}
	return requests, failures
}

// State returns the current state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Reset forces the breaker closed and clears its window. Used by the admin API.
func (b *Breaker) Reset() {
	b.mu.Lock()
	b.buckets = [numBuckets]bucket{}
	b.probing = false
	b.cooldown = b.settings().Cooldown
	tr := b.setState(Closed)
	b.mu.Unlock()
	b.fire(tr)
}

// Snapshot is a point-in-time view for the health endpoint and metrics.
type Snapshot struct {
	State State
	// Requests and Failures are the counts inside the current window.
	Requests int
	Failures int
	// Cooldown is the wait currently in force before the next probe.
	Cooldown time.Duration
	// RetryAt is when an open breaker will next allow a probe; zero otherwise.
	RetryAt time.Time
}

// Snapshot returns the breaker's current view.
func (b *Breaker) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	requests, failures := b.totals(b.settings(), b.now())
	snap := Snapshot{State: b.state, Requests: requests, Failures: failures, Cooldown: b.cooldown}
	if b.state == Open {
		snap.RetryAt = b.openedAt.Add(b.cooldown)
	}
	return snap
}
