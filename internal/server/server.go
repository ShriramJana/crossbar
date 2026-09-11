// Package server wires the gateway's HTTP handlers, middleware, and lifecycle.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/ShriramJana/crossbar/internal/config"
)

// Options configures a Server.
type Options struct {
	// Logger receives one structured line per request. Defaults to slog.Default().
	Logger *slog.Logger
	// Config is the live configuration; auth and limits read it per request.
	Config *config.Store
}

// Server owns the gateway's HTTP routes.
type Server struct {
	logger  *slog.Logger
	config  *config.Store
	handler http.Handler
}

// New builds a Server with all routes and middleware attached.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Server{logger: opts.Logger, config: opts.Config}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)

	// Data plane: authenticated as a team.
	mux.Handle("POST /v1/messages", chain(http.HandlerFunc(s.handleMessages), teamAuth(s.config)))

	// Control plane: authenticated with the admin key.
	admin := adminAuth(s.config)
	mux.Handle("GET /admin/teams/{id}/usage", chain(http.HandlerFunc(s.handleTeamUsage), admin))
	mux.Handle("POST /admin/config/reload", chain(http.HandlerFunc(s.handleConfigReload), admin))

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

// handleMessages is the data-plane entry point. Routing lands in a later
// milestone; until then an authenticated caller gets an explicit 501.
func (s *Server) handleMessages(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented"})
}

func (s *Server) handleTeamUsage(w http.ResponseWriter, r *http.Request) {
	team, ok := s.config.Current().TeamByID(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown team"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"team": team.ID,
		"name": team.Name,
		"limits": map[string]any{
			"requests_per_minute": team.RequestsPerMinute,
			"tokens_per_minute":   team.TokensPerMinute,
			"daily_budget_usd":    team.DailyBudgetUSD,
			"monthly_budget_usd":  team.MonthlyBudgetUSD,
			"allowed_models":      team.AllowedModels,
		},
	})
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
