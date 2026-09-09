package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// ServeOptions tunes the lifecycle of a running listener.
type ServeOptions struct {
	// DrainTimeout bounds how long Serve waits for in-flight requests after ctx is cancelled.
	DrainTimeout time.Duration
	// ReadHeaderTimeout guards against slowloris clients. Defaults to 10s.
	ReadHeaderTimeout time.Duration
	// Logger records lifecycle events. Defaults to slog.Default().
	Logger *slog.Logger
}

// Serve runs h on ln until ctx is cancelled, then drains in-flight requests.
//
// It returns nil after a clean drain. If DrainTimeout elapses with requests
// still active, remaining connections are force-closed and the returned error
// wraps context.DeadlineExceeded. Listener failures are returned wrapped.
func Serve(ctx context.Context, ln net.Listener, h http.Handler, opts ServeOptions) error {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.ReadHeaderTimeout == 0 {
		opts.ReadHeaderTimeout = 10 * time.Second
	}

	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: opts.ReadHeaderTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ln)
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serving on %s: %w", ln.Addr(), err)
	case <-ctx.Done():
	}

	opts.Logger.Info("shutdown started", slog.Duration("drain_timeout", opts.DrainTimeout))
	drainCtx, cancel := context.WithTimeout(context.Background(), opts.DrainTimeout)
	defer cancel()

	if err := srv.Shutdown(drainCtx); err != nil {
		opts.Logger.Warn("drain timeout elapsed; closing remaining connections")
		_ = srv.Close()
		return fmt.Errorf("draining connections: %w", err)
	}
	<-serveErr
	opts.Logger.Info("shutdown complete")
	return nil
}
