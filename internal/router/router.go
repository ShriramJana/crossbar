// Package router resolves a requested model to its tier's fallback chain and
// dispatches to providers.
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/ShriramJana/crossbar/internal/config"
	"github.com/ShriramJana/crossbar/internal/provider"
)

// Sentinel errors callers inspect with errors.Is.
var (
	ErrUnknownModel = errors.New("router: no tier lists the requested model")
	ErrNoProvider   = errors.New("router: no adapter registered for provider")
)

// Router picks a target for each request and forwards it.
type Router struct {
	store     *config.Store
	providers map[string]provider.Provider
	logger    *slog.Logger
}

// New builds a Router over the live config and the given adapters, keyed by
// the provider names used in config.
func New(store *config.Store, providers map[string]provider.Provider, logger *slog.Logger) *Router {
	if logger == nil {
		logger = slog.Default()
	}
	return &Router{store: store, providers: providers, logger: logger}
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

// Dispatch sends req to the first target in its chain and prices the response.
func (r *Router) Dispatch(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	cfg := r.store.Current()
	chain, ok := ResolveChain(cfg, req.Model)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownModel, req.Model)
	}

	target := chain[0]
	p, ok := r.providers[target.Provider]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoProvider, target.Provider)
	}

	upstream := *req
	upstream.Model = target.Model
	resp, err := p.Send(ctx, &upstream)
	if err != nil {
		return nil, err
	}

	// Validation guarantees a price for every tier target, so a miss here is a
	// programming error worth surfacing rather than a silent zero.
	price, ok := cfg.Price(target.Provider, target.Model)
	if !ok {
		r.logger.Error("no pricing for dispatched target", slog.String("provider", target.Provider), slog.String("model", target.Model))
	}
	resp.CostUSD = price.Cost(resp.InputTokens, resp.OutputTokens)
	return resp, nil
}
