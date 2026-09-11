package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// debounce absorbs the burst of events an editor or deploy tool emits for one
// logical save (write, chmod, rename) into a single reload.
const debounce = 100 * time.Millisecond

// Store holds the live configuration behind an atomic pointer. Readers call
// Current on every request and never block; Reload swaps in a new Config only
// after it has parsed and validated completely.
type Store struct {
	path   string
	logger *slog.Logger
	cur    atomic.Pointer[Config]
	gen    atomic.Uint64
}

// NewStore loads the file at path and returns a Store serving it.
func NewStore(path string, logger *slog.Logger) (*Store, error) {
	if logger == nil {
		logger = slog.Default()
	}
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, logger: logger}
	s.cur.Store(cfg)
	s.gen.Store(1)
	return s, nil
}

// Current returns the active configuration. The returned value is immutable.
func (s *Store) Current() *Config {
	return s.cur.Load()
}

// Generation counts successful loads, starting at 1 for the initial load.
func (s *Store) Generation() uint64 {
	return s.gen.Load()
}

// Reload re-reads the file. On any error the previous configuration stays
// active and the error is returned; a bad edit never takes the gateway down.
func (s *Store) Reload() error {
	cfg, err := Load(s.path)
	if err != nil {
		s.logger.Error("config reload failed; keeping previous config",
			slog.String("path", s.path), slog.Any("error", err))
		return err
	}
	s.cur.Store(cfg)
	gen := s.gen.Add(1)
	s.logger.Info("config reloaded",
		slog.String("path", s.path),
		slog.Uint64("generation", gen),
		slog.Int("teams", len(cfg.Teams)),
		slog.Int("providers", len(cfg.Providers)),
		slog.Int("tiers", len(cfg.Tiers)))
	return nil
}

// Watch reloads on file changes and on SIGHUP until ctx is cancelled.
//
// The watch is registered before Watch returns, so any change made after it
// returns is observed. The loop runs in a goroutine; the returned channel is
// closed when it exits. It watches the file's directory rather than the file
// itself so atomic rename-replace saves are seen. Reload failures are logged,
// not returned; only a watcher setup failure is returned.
func (s *Store) Watch(ctx context.Context) (<-chan struct{}, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("creating watcher: %w", err)
	}

	abs, err := filepath.Abs(s.path)
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("resolving config path: %w", err)
	}
	if err := w.Add(filepath.Dir(abs)); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("watching %s: %w", filepath.Dir(abs), err)
	}

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer signal.Stop(hup)
		defer func() { _ = w.Close() }()
		s.watchLoop(ctx, w, filepath.Base(abs), hup)
	}()
	return done, nil
}

func (s *Store) watchLoop(ctx context.Context, w *fsnotify.Watcher, base string, hup <-chan os.Signal) {
	var pending <-chan time.Time // nil until a relevant event arrives
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			// Any operation on the config's name schedules a reload. Backends
			// disagree on how a rename-replace surfaces (inotify: Create;
			// kqueue: Remove then Create), and a reload that finds the file
			// unchanged or momentarily absent is harmless.
			if filepath.Base(ev.Name) == base {
				pending = time.After(debounce)
			}
		case <-pending:
			pending = nil
			_ = s.Reload() // already logged
		case <-hup:
			s.logger.Info("SIGHUP received; reloading config")
			_ = s.Reload()
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			s.logger.Warn("config watcher error", slog.Any("error", err))
		}
	}
}
