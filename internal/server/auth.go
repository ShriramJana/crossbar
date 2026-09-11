package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/ShriramJana/crossbar/internal/config"
)

const teamKey ctxKey = iota + 10

// TeamFromContext returns the team authenticated for this request, if any.
func TeamFromContext(ctx context.Context) (*config.Team, bool) {
	t, ok := ctx.Value(teamKey).(*config.Team)
	return t, ok
}

// bearerToken extracts the token from "Authorization: Bearer <token>".
// It returns "" for a missing header or any other scheme.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// unauthorized writes a 401 that deliberately says nothing about why.
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
}

// teamAuth resolves the bearer key to a Team against the live config and
// rejects everything else with an uninformative 401.
func teamAuth(store *config.Store) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			team, ok := store.Current().TeamByKey(bearerToken(r))
			if !ok {
				unauthorized(w)
				return
			}
			if info := infoFromContext(r.Context()); info != nil {
				info.team = team.ID
			}
			ctx := context.WithValue(r.Context(), teamKey, team)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// adminAuth admits only the configured admin key.
func adminAuth(store *config.Store) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !store.Current().IsAdminKey(bearerToken(r)) {
				unauthorized(w)
				return
			}
			if info := infoFromContext(r.Context()); info != nil {
				info.team = "admin"
			}
			next.ServeHTTP(w, r)
		})
	}
}
