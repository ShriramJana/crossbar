// Command crossbar runs the LLM gateway.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ShriramJana/crossbar/internal/config"
	"github.com/ShriramJana/crossbar/internal/observability"
	"github.com/ShriramJana/crossbar/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "crossbar:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr       = flag.String("addr", envOr("CROSSBAR_ADDR", ":8080"), "listen address")
		configPath = flag.String("config", envOr("CROSSBAR_CONFIG", "config.yaml"), "path to config file")
		logFormat  = flag.String("log-format", envOr("CROSSBAR_LOG_FORMAT", "text"), "log format: json or text")
		logLevel   = flag.String("log-level", envOr("CROSSBAR_LOG_LEVEL", "info"), "log level: debug, info, warn, error")
		drain      = flag.Duration("drain-timeout", 10*time.Second, "how long to wait for in-flight requests on shutdown")
		hashKey    = flag.Bool("hash-key", false, "read a key from stdin, print its SHA-256 for config, and exit")
	)
	flag.Parse()

	if *hashKey {
		return printKeyHash(os.Stdin, os.Stdout)
	}

	logger, err := observability.NewLogger(os.Stdout, *logFormat, *logLevel)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	store, err := config.NewStore(*configPath, logger)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", *addr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	watchDone, err := store.Watch(ctx)
	if err != nil {
		return fmt.Errorf("watching config: %w", err)
	}

	srv := server.New(server.Options{Logger: logger, Config: store})
	logger.Info("crossbar listening",
		slog.String("addr", ln.Addr().String()),
		slog.String("config", *configPath),
		slog.Int("teams", len(store.Current().Teams)))

	serveErr := server.Serve(ctx, ln, srv.Handler(), server.ServeOptions{
		DrainTimeout: *drain,
		Logger:       logger,
	})
	stop()
	<-watchDone
	return serveErr
}

// printKeyHash reads one line from in and writes its config-ready hash to out.
func printKeyHash(in io.Reader, out io.Writer) error {
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("reading key: %w", err)
	}
	key := strings.TrimRight(line, "\r\n")
	if key == "" {
		return errors.New("no key on stdin")
	}
	_, err = fmt.Fprintln(out, config.HashKey(key))
	return err
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
