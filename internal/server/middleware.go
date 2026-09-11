package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"
)

type middleware func(http.Handler) http.Handler

// chain applies middlewares so that the first listed is the outermost.
func chain(h http.Handler, ms ...middleware) http.Handler {
	for i := len(ms) - 1; i >= 0; i-- {
		h = ms[i](h)
	}
	return h
}

type ctxKey int

const (
	requestIDKey ctxKey = iota
	requestInfoKey
)

const requestIDHeader = "X-Request-ID"

// requestInfo is attached to the context by the outermost logging middleware
// and filled in by inner layers (auth, routing) so the single per-request log
// line can carry facts that are only known further down the chain.
type requestInfo struct {
	team string
}

func infoFromContext(ctx context.Context) *requestInfo {
	info, _ := ctx.Value(requestInfoKey).(*requestInfo)
	return info
}

// RequestIDFromContext returns the request ID attached by the server, or "" if none.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// requestID ensures every request carries an ID, echoing a client-supplied one
// or generating a fresh one, and returns it in the response header.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is effectively fatal on every supported platform;
		// fall back to a time-derived value rather than panic in a request path.
		return time.Now().UTC().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(b[:])
}

// statusRecorder captures the status code written by downstream handlers.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// requestLog emits one structured log line per request.
func requestLog(logger *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			info := &requestInfo{}
			ctx := context.WithValue(r.Context(), requestInfoKey, info)
			next.ServeHTTP(rec, r.WithContext(ctx))
			logger.LogAttrs(ctx, slog.LevelInfo, "request",
				slog.String("request_id", RequestIDFromContext(ctx)),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("team", info.team),
				slog.Int("status", rec.status),
				slog.Duration("duration", time.Since(start)),
			)
		})
	}
}
