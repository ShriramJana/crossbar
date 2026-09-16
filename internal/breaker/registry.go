package breaker

import (
	"sort"
	"sync"
	"time"
)

// RegistryOptions configure a Registry and the breakers it creates.
type RegistryOptions struct {
	// Now supplies the clock to every breaker; defaults to time.Now.
	Now func() time.Time
	// OnTransition is invoked on every state change of any breaker.
	OnTransition func(provider, model string, from, to State)
}

type key struct{ provider, model string }

// Registry holds one Breaker per (provider, model) pair, created on first use.
type Registry struct {
	settings func() Settings
	opts     RegistryOptions

	mu       sync.Mutex
	breakers map[key]*Breaker
}

// NewRegistry returns an empty Registry. settings is consulted by every
// breaker on every decision, so it should read live configuration.
func NewRegistry(settings func() Settings, opts RegistryOptions) *Registry {
	return &Registry{settings: settings, opts: opts, breakers: make(map[key]*Breaker)}
}

// Get returns the breaker for a target, creating it closed if needed.
func (r *Registry) Get(provider, model string) *Breaker {
	k := key{provider, model}
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.breakers[k]; ok {
		return b
	}
	var onTransition func(from, to State)
	if r.opts.OnTransition != nil {
		onTransition = func(from, to State) { r.opts.OnTransition(provider, model, from, to) }
	}
	b := New(r.settings, Options{Now: r.opts.Now, OnTransition: onTransition})
	r.breakers[k] = b
	return b
}

// Reset closes the named breaker and reports whether it existed.
func (r *Registry) Reset(provider, model string) bool {
	r.mu.Lock()
	b, ok := r.breakers[key{provider, model}]
	r.mu.Unlock()
	if !ok {
		return false
	}
	b.Reset()
	return true
}

// Entry is one breaker's snapshot together with its identity.
type Entry struct {
	Provider string
	Model    string
	Snapshot
}

// Snapshot returns every breaker's state, sorted by provider then model.
func (r *Registry) Snapshot() []Entry {
	r.mu.Lock()
	entries := make([]Entry, 0, len(r.breakers))
	for k, b := range r.breakers {
		entries = append(entries, Entry{Provider: k.provider, Model: k.model, Snapshot: b.Snapshot()})
	}
	r.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Provider != entries[j].Provider {
			return entries[i].Provider < entries[j].Provider
		}
		return entries[i].Model < entries[j].Model
	})
	return entries
}
