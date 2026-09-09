// Package server wires the gateway's HTTP handlers, middleware, and lifecycle.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Options configures a Server.
type Options struct {
	// Logger receives one structured line per request. Defaults to slog.Default().
	Logger *slog.Logger
}

// Server owns the gateway's HTTP routes.
type Server struct {
	logger  *slog.Logger
	handler http.Handler
}

// New builds a Server with all routes and middleware attached.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Server{logger: opts.Logger}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)

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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
