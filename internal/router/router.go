// Package router resolves a requested model to its tier's fallback chain and
// dispatches to providers with retry, circuit breaking, and failover.
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/ShriramJana/crossbar/internal/breaker"
	"github.com/ShriramJana/crossbar/internal/config"
	"github.com/ShriramJana/crossbar/internal/health"
	"github.com/ShriramJana/crossbar/internal/provider"
)

// ErrUnknownModel is returned when no tier lists the requested model.
var ErrUnknownModel = errors.New("router: no tier lists the requested model")

// Reasons a target in the chain did not serve the request.
const (
	ReasonBreakerOpen = "breaker_open"
	ReasonHealthDown  = "health_down"
	ReasonNoAdapter   = "no_adapter"
	ReasonFailed      = "failed"
)

// Attempt records what happened at one target in the chain.
type Attempt struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Reason   string `json:"reason"`
	// Tries is how many upstream calls were made; zero when skipped.
	Tries int    `json:"tries"`
	Error string `json:"error,omitempty"`
}

// ChainError is returned when every target was skipped or failed. Cause is
// set when the request's context expired partway through, so callers can
// distinguish a timeout from an exhausted chain with errors.Is.
type ChainError struct {
	Attempts []Attempt
	Cause    error
}

// Error implements error.
func (e *ChainError) Error() string {
	parts := make([]string, 0, len(e.Attempts))
	for _, a := range e.Attempts {
		s := a.Provider + "/" + a.Model + " (" + a.Reason
		if a.Tries > 0 {
			s += fmt.Sprintf(" after %d tries", a.Tries)
		}
		if a.Error != "" {
			s += ": " + a.Error
		}
		parts = append(parts, s+")")
	}
	msg := "router: all targets failed: " + strings.Join(parts, "; ")
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

// Unwrap exposes the context error, if any.
func (e *ChainError) Unwrap() error { return e.Cause }

// HealthSource is what the router needs from the health monitor.
type HealthSource interface {
	Status(provider string) health.Status
}

// Options configure a Router.
type Options struct {
	// Breakers guards each target. Nil disables circuit breaking.
	Breakers *breaker.Registry
	// Health excludes down providers and deprioritizes degraded ones. Nil treats all as healthy.
	Health HealthSource
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Sleep waits for a backoff, returning early with ctx.Err() if ctx ends. Defaults to a timer.
	Sleep func(ctx context.Context, d time.Duration) error
	// Rand supplies jitter in [0, 1). Defaults to math/rand/v2.
	Rand func() float64
}

// Router picks targets for each request and forwards it.
type Router struct {
	store     *config.Store
	providers map[string]provider.Provider
	breakers  *breaker.Registry
	health    HealthSource
	logger    *slog.Logger
	sleep     func(ctx context.Context, d time.Duration) error
	rand      func() float64
}

// New builds a Router over the live config and the given adapters, keyed by
// the provider names used in config.
func New(store *config.Store, providers map[string]provider.Provider, opts Options) *Router {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepCtx
	}
	if opts.Rand == nil {
		opts.Rand = rand.Float64
	}
	return &Router{
		store:     store,
		providers: providers,
		breakers:  opts.Breakers,
		health:    opts.Health,
		logger:    opts.Logger,
		sleep:     opts.Sleep,
		rand:      opts.Rand,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ResolveChain returns the fallback chain for a requested model.
//
// A tier name selects that tier's whole chain. A model name selects the tier
// that lists it, with the requested target first and the tier's remaining
// targets following in their configured order. When several tiers list the
// model, the one listing it earliest wins, with ties broken by tier name so
// resolution is deterministic.
func ResolveChain(cfg *config.Config, model string) ([]config.Target, bool) {
	if targets, ok := cfg.Tiers[model]; ok {
		return append([]config.Target(nil), targets...), true
	}

	names := make([]string, 0, len(cfg.Tiers))
	for name := range cfg.Tiers {
		names = append(names, name)
	}
	sort.Strings(names)

	bestTier, bestIdx := "", -1
	for _, name := range names {
		for i, t := range cfg.Tiers[name] {
			if t.Model == model && (bestIdx == -1 || i < bestIdx) {
				bestTier, bestIdx = name, i
			}
		}
	}
	if bestIdx == -1 {
		return nil, false
	}

	tier := cfg.Tiers[bestTier]
	chain := make([]config.Target, 0, len(tier))
	chain = append(chain, tier[bestIdx])
	for i, t := range tier {
		if i != bestIdx {
			chain = append(chain, t)
		}
	}
	return chain, true
}

// prioritize moves degraded providers behind healthy ones, keeping the
// relative order within each group. Down providers are left in place and
// skipped at dispatch so the attempt log can say why.
func (r *Router) prioritize(chain []config.Target) []config.Target {
	if r.health == nil {
		return chain
	}
	ordered := make([]config.Target, 0, len(chain))
	var degraded []config.Target
	for _, t := range chain {
		if r.health.Status(t.Provider) == health.Degraded {
			degraded = append(degraded, t)
		} else {
			ordered = append(ordered, t)
		}
	}
	return append(ordered, degraded...)
}

// Dispatch walks the chain for req.Model. Each target is tried up to
// 1+max_retries times on retryable failures with jittered exponential
// backoff; a non-retryable failure is returned to the caller at once. Targets
// whose breaker is open or whose provider is down are skipped. The context
// deadline bounds the whole walk.
func (r *Router) Dispatch(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	cfg := r.store.Current()
	chain, ok := ResolveChain(cfg, req.Model)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownModel, req.Model)
	}
	// Fallback means "not served by the configured primary", regardless of
	// how health reordered the chain for this request.
	primary := chain[0]
	chain = r.prioritize(chain)

	var attempts []Attempt
	for _, target := range chain {
		att := Attempt{Provider: target.Provider, Model: target.Model}

		p, ok := r.providers[target.Provider]
		if !ok {
			att.Reason = ReasonNoAdapter
			attempts = append(attempts, att)
			continue
		}
		if r.health != nil && r.health.Status(target.Provider) == health.Down {
			att.Reason = ReasonHealthDown
			attempts = append(attempts, att)
			continue
		}

		resp, err := r.tryTarget(ctx, cfg, p, target, req, &att)
		if resp != nil {
			resp.Fallback = target != primary
			return resp, nil
		}
		if err != nil {
			// Non-retryable: the caller's problem, or the deadline: nobody's.
			var ce *ChainError
			if errors.As(err, &ce) {
				ce.Attempts = append(attempts, ce.Attempts...)
			}
			return nil, err
		}
		attempts = append(attempts, att)
	}
	return nil, &ChainError{Attempts: attempts}
}

// tryTarget makes up to 1+max_retries calls to one target. It returns a
// response on success; an error when the walk must stop (non-retryable
// failure, or context expiry, wrapped in a ChainError); or (nil, nil) when
// the target is exhausted and the chain should move on, with att filled in.
func (r *Router) tryTarget(ctx context.Context, cfg *config.Config, p provider.Provider, target config.Target, req *provider.Request, att *Attempt) (*provider.Response, error) {
	var b *breaker.Breaker
	if r.breakers != nil {
		b = r.breakers.Get(target.Provider, target.Model)
	}

	fail := func(err error) {
		att.Reason = ReasonFailed
		att.Error = err.Error()
	}
	var lastErr error
	for try := 0; try <= cfg.Retry.MaxRetries; try++ {
		if try > 0 {
			if err := r.sleep(ctx, r.backoff(cfg.Retry, try-1)); err != nil {
				fail(lastErr)
				return nil, &ChainError{Attempts: []Attempt{*att}, Cause: err}
			}
		}
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				fail(lastErr)
			}
			return nil, &ChainError{Attempts: []Attempt{*att}, Cause: err}
		}

		var permit breaker.Permit
		if b != nil {
			var ok bool
			if permit, ok = b.Allow(); !ok {
				if att.Tries == 0 {
					att.Reason = ReasonBreakerOpen
				} else {
					fail(lastErr)
				}
				return nil, nil
			}
		}

		att.Tries++
		upstream := *req
		upstream.Model = target.Model
		resp, err := p.Send(ctx, &upstream)
		if err == nil {
			permit.Success()
			resp.CostUSD = r.price(cfg, target, resp)
			return resp, nil
		}
		lastErr = err

		if class, _ := provider.ClassOf(err); class == provider.ErrNonRetryable {
			// The upstream answered; the request itself was the problem.
			// That is not evidence against the provider and not worth a retry.
			permit.Success()
			return nil, err
		}
		permit.Failure()
		r.logger.Debug("upstream call failed",
			slog.String("provider", target.Provider),
			slog.String("model", target.Model),
			slog.Int("try", try+1),
			slog.Any("error", err))
	}
	fail(lastErr)
	return nil, nil
}

// backoff returns a full-jittered delay: uniform in [0, min(base*2^n, max)].
func (r *Router) backoff(rc config.RetryConfig, n int) time.Duration {
	ceiling := rc.BaseBackoff
	for i := 0; i < n && ceiling < rc.MaxBackoff; i++ {
		ceiling *= 2
	}
	ceiling = min(ceiling, rc.MaxBackoff)
	return time.Duration(r.rand() * float64(ceiling))
}

func (r *Router) price(cfg *config.Config, target config.Target, resp *provider.Response) float64 {
	price, ok := cfg.Price(target.Provider, target.Model)
	if !ok {
		// Validation guarantees a price for every tier target; a miss here is
		// a programming error worth surfacing rather than a silent zero.
		r.logger.Error("no pricing for dispatched target", slog.String("provider", target.Provider), slog.String("model", target.Model))
	}
	return price.Cost(resp.InputTokens, resp.OutputTokens)
}
