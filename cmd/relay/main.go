// Command relay runs the LLM gateway.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ShriramJana/relay/internal/observability"
	"github.com/ShriramJana/relay/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "relay:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr      = flag.String("addr", envOr("RELAY_ADDR", ":8080"), "listen address")
		logFormat = flag.String("log-format", envOr("RELAY_LOG_FORMAT", "text"), "log format: json or text")
		logLevel  = flag.String("log-level", envOr("RELAY_LOG_LEVEL", "info"), "log level: debug, info, warn, error")
		drain     = flag.Duration("drain-timeout", 10*time.Second, "how long to wait for in-flight requests on shutdown")
	)
	flag.Parse()

	logger, err := observability.NewLogger(os.Stdout, *logFormat, *logLevel)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", *addr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := server.New(server.Options{Logger: logger})
	logger.Info("relay listening", slog.String("addr", ln.Addr().String()))

	return server.Serve(ctx, ln, srv.Handler(), server.ServeOptions{
		DrainTimeout: *drain,
		Logger:       logger,
	})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
