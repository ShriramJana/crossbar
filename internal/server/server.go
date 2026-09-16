// Package server wires the gateway's HTTP handlers, middleware, and lifecycle.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/ShriramJana/crossbar/internal/breaker"
	"github.com/ShriramJana/crossbar/internal/budget"
	"github.com/ShriramJana/crossbar/internal/config"
	"github.com/ShriramJana/crossbar/internal/health"
	"github.com/ShriramJana/crossbar/internal/limiter"
	"github.com/ShriramJana/crossbar/internal/provider"
)

// Dispatcher is what the server needs from the router.
type Dispatcher interface {
	Dispatch(ctx context.Context, req *provider.Request) (*provider.Response, error)
}

// RateLimiter is what the server needs from the limiter.
type RateLimiter interface {
	Allow(ctx context.Context, team string, lim limiter.Limits, tokens int) (limiter.Decision, error)
	Reconcile(ctx context.Context, team string, lim limiter.Limits, reserved, actual int) error
}

// Budgeter is what the server needs from the budget store.
type Budgeter interface {
	Check(ctx context.Context, team string, lim budget.Limits) (budget.Status, error)
	Record(ctx context.Context, team string, lim budget.Limits, cost float64) (budget.Status, error)
}

// HealthReporter is what the server needs from the health monitor.
type HealthReporter interface {
	Snapshot() map[string]health.Report
}

// DefaultRequestTimeout bounds one client request end to end, including
// every retry and fallback the router attempts.
const DefaultRequestTimeout = 60 * time.Second

// Options configures a Server.
type Options struct {
	// Logger receives one structured line per request. Defaults to slog.Default().
	Logger *slog.Logger
	// Config is the live configuration; auth and limits read it per request.
	Config *config.Store
	// Router dispatches data-plane requests to providers.
	Router Dispatcher
	// Limiter enforces per-team rate limits. Nil disables rate limiting.
	Limiter RateLimiter
	// Budget enforces per-team spend limits. Nil disables budgets.
	Budget Budgeter
	// Breakers is exposed on the health endpoint and the admin reset route. Optional.
	Breakers *breaker.Registry
	// Health is exposed on the health endpoint. Optional.
	Health HealthReporter
	// RequestTimeout bounds one request end to end. Defaults to DefaultRequestTimeout.
	RequestTimeout time.Duration
}

// Server owns the gateway's HTTP routes.
type Server struct {
	logger         *slog.Logger
	config         *config.Store
	router         Dispatcher
	limiter        RateLimiter
	budget         Budgeter
	breakers       *breaker.Registry
	health         HealthReporter
	requestTimeout time.Duration
	handler        http.Handler
}

// New builds a Server with all routes and middleware attached.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = DefaultRequestTimeout
	}
	s := &Server{
		logger:         opts.Logger,
		config:         opts.Config,
		router:         opts.Router,
		limiter:        opts.Limiter,
		budget:         opts.Budget,
		breakers:       opts.Breakers,
		health:         opts.Health,
		requestTimeout: opts.RequestTimeout,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /health/providers", s.handleHealthProviders)

	// Data plane: authenticated as a team.
	mux.Handle("POST /v1/messages", chain(http.HandlerFunc(s.handleMessages), teamAuth(s.config)))

	// Control plane: authenticated with the admin key.
	admin := adminAuth(s.config)
	mux.Handle("GET /admin/teams/{id}/usage", chain(http.HandlerFunc(s.handleTeamUsage), admin))
	mux.Handle("POST /admin/config/reload", chain(http.HandlerFunc(s.handleConfigReload), admin))
	mux.Handle("POST /admin/breakers/{provider}/{model}/reset", chain(http.HandlerFunc(s.handleBreakerReset), admin))

	s.handler = chain(mux,
		requestID,
		requestLog(s.logger),
	)
	return s
}

// Handler returns the fully wrapped root handler.
func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleHealthProviders(w http.ResponseWriter, _ *http.Request) {
	providers := map[string]any{}
	if s.health != nil {
		for name, r := range s.health.Snapshot() {
			entry := map[string]any{
				"status":     r.Status.String(),
				"error_rate": r.ErrorRate,
				"p99_ms":     float64(r.P99) / float64(time.Millisecond),
				"probes":     r.Probes,
			}
			if r.LastError != "" {
				entry["last_error"] = r.LastError
			}
			if !r.LastChecked.IsZero() {
				entry["last_checked"] = r.LastChecked.UTC().Format(time.RFC3339)
			}
			providers[name] = entry
		}
	}

	breakers := []any{}
	if s.breakers != nil {
		for _, e := range s.breakers.Snapshot() {
			entry := map[string]any{
				"provider":    e.Provider,
				"model":       e.Model,
				"state":       e.State.String(),
				"requests":    e.Requests,
				"failures":    e.Failures,
				"cooldown_ms": float64(e.Cooldown) / float64(time.Millisecond),
			}
			if !e.RetryAt.IsZero() {
				entry["retry_at"] = e.RetryAt.UTC().Format(time.RFC3339)
			}
			breakers = append(breakers, entry)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers, "breakers": breakers})
}

func (s *Server) handleBreakerReset(w http.ResponseWriter, r *http.Request) {
	prov, model := r.PathValue("provider"), r.PathValue("model")
	if s.breakers == nil || !s.breakers.Reset(prov, model) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown breaker"})
		return
	}
	s.logger.Warn("breaker reset by admin", slog.String("provider", prov), slog.String("model", model))
	writeJSON(w, http.StatusOK, map[string]string{"provider": prov, "model": model, "state": breaker.Closed.String()})
}

func (s *Server) handleTeamUsage(w http.ResponseWriter, r *http.Request) {
	team, ok := s.config.Current().TeamByID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown team"})
		return
	}
	body := map[string]any{
		"team": team.ID,
		"name": team.Name,
		"limits": map[string]any{
			"requests_per_minute": team.RequestsPerMinute,
			"tokens_per_minute":   team.TokensPerMinute,
			"daily_budget_usd":    team.DailyBudgetUSD,
			"monthly_budget_usd":  team.MonthlyBudgetUSD,
			"allowed_models":      team.AllowedModels,
		},
	}
	if s.budget != nil {
		lim := budget.Limits{DailyUSD: team.DailyBudgetUSD, MonthlyUSD: team.MonthlyBudgetUSD}
		st, err := s.budget.Check(r.Context(), team.ID, lim)
		if err != nil {
			body["spend_error"] = err.Error()
		} else {
			body["spend"] = map[string]any{
				"daily":   periodJSON(st.Daily),
				"monthly": periodJSON(st.Monthly),
			}
		}
	}
	writeJSON(w, http.StatusOK, body)
}

func periodJSON(p budget.Period) map[string]any {
	return map[string]any{
		"spent_usd": p.Spent,
		"limit_usd": p.Limit,
		"resets_at": p.ResetsAt.Format(time.RFC3339),
	}
}

func (s *Server) handleConfigReload(w http.ResponseWriter, _ *http.Request) {
	if err := s.config.Reload(); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "reloaded",
		"generation": s.config.Generation(),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
