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

	"github.com/redis/go-redis/v9"

	"github.com/ShriramJana/crossbar/internal/budget"
	"github.com/ShriramJana/crossbar/internal/config"
	"github.com/ShriramJana/crossbar/internal/limiter"
	"github.com/ShriramJana/crossbar/internal/observability"
	"github.com/ShriramJana/crossbar/internal/provider"
	"github.com/ShriramJana/crossbar/internal/router"
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
		redisAddr  = flag.String("redis", envOr("REDIS_ADDR", "localhost:6379"), "redis address for limits and budgets")
		logFormat  = flag.String("log-format", envOr("CROSSBAR_LOG_FORMAT", "text"), "log format: json or text")
		logLevel   = flag.String("log-level", envOr("CROSSBAR_LOG_LEVEL", "info"), "log level: debug, info, warn, error")
		reqTimeout = flag.Duration("request-timeout", server.DefaultRequestTimeout, "end-to-end bound on one client request")
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

	providers, err := buildProviders(store.Current())
	if err != nil {
		return err
	}

	rdb := redis.NewClient(&redis.Options{Addr: *redisAddr})
	defer func() { _ = rdb.Close() }()
	pingCtx, cancelPing := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelPing()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		return fmt.Errorf("connecting to redis at %s: %w", *redisAddr, err)
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

	srv := server.New(server.Options{
		Logger:  logger,
		Config:  store,
		Router:  router.New(store, providers, logger),
		Limiter: limiter.New(rdb),
		Budget: budget.New(rdb, budget.Options{OnWarn: func(w budget.Warning) {
			logger.Warn("budget warning: 80% consumed",
				slog.String("team", w.Team),
				slog.String("period", w.Period),
				slog.Float64("spent_usd", w.Spent),
				slog.Float64("limit_usd", w.Limit))
		}}),
		RequestTimeout: *reqTimeout,
	})
	logger.Info("crossbar listening",
		slog.String("addr", ln.Addr().String()),
		slog.String("config", *configPath),
		slog.String("redis", *redisAddr),
		slog.Int("teams", len(store.Current().Teams)),
		slog.Int("providers", len(providers)))

	serveErr := server.Serve(ctx, ln, srv.Handler(), server.ServeOptions{
		DrainTimeout: *drain,
		Logger:       logger,
	})
	stop()
	<-watchDone
	return serveErr
}

// buildProviders constructs one adapter per configured provider. The set is
// fixed at startup; adding a provider requires a restart, while tiers,
// teams, and pricing reload live.
func buildProviders(cfg *config.Config) (map[string]provider.Provider, error) {
	providers := make(map[string]provider.Provider, len(cfg.Providers))
	for name, pc := range cfg.Providers {
		p, err := provider.NewFromConfig(name, pc)
		if err != nil {
			return nil, err
		}
		providers[name] = p
	}
	return providers, nil
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
