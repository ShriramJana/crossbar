package config_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ShriramJana/crossbar/internal/config"
)

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	// Write to a temp file and rename, the way editors and deploy tools do, so
	// the watcher is exercised against an atomic replace rather than an in-place write.
	tmp := path + ".tmp"
	require.NoError(t, os.WriteFile(tmp, []byte(body), 0o600))
	require.NoError(t, os.Rename(tmp, path))
}

func newStore(t *testing.T) (*config.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, validYAML())
	s, err := config.NewStore(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	return s, path
}

func withRPM(rpm string) string {
	return strings.Replace(validYAML(), "requests_per_minute: 100", "requests_per_minute: "+rpm, 1)
}

func searchRPM(s *config.Store) int {
	team, _ := s.Current().TeamByID("search")
	return team.RequestsPerMinute
}

func TestNewStoreLoadsFile(t *testing.T) {
	s, _ := newStore(t)
	assert.Equal(t, 100, searchRPM(s))
	assert.Equal(t, uint64(1), s.Generation())
}

func TestNewStoreRejectsInvalidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, "teams: [")
	_, err := config.NewStore(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.Error(t, err)
}

func TestReloadSwapsOnValid(t *testing.T) {
	s, path := newStore(t)
	writeConfig(t, path, withRPM("250"))

	require.NoError(t, s.Reload())
	assert.Equal(t, 250, searchRPM(s))
	assert.Equal(t, uint64(2), s.Generation())
}

func TestReloadKeepsOldOnInvalid(t *testing.T) {
	s, path := newStore(t)
	before := s.Current()
	writeConfig(t, path, withRPM("0"))

	err := s.Reload()
	require.Error(t, err)
	assert.Same(t, before, s.Current(), "invalid file must leave the previous config in place")
	assert.Equal(t, 100, searchRPM(s))
	assert.Equal(t, uint64(1), s.Generation())
}

func TestReloadKeepsOldOnMissingFile(t *testing.T) {
	s, path := newStore(t)
	require.NoError(t, os.Remove(path))

	require.Error(t, s.Reload())
	assert.Equal(t, 100, searchRPM(s))
}

// waitFor polls until cond is true or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func startWatch(t *testing.T, s *config.Store) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done, err := s.Watch(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("watch loop did not exit after cancel")
		}
	})
}

func TestWatchReloadsOnFileChange(t *testing.T) {
	s, path := newStore(t)
	startWatch(t, s)

	writeConfig(t, path, withRPM("300"))
	assert.True(t, waitFor(t, 3*time.Second, func() bool { return searchRPM(s) == 300 }),
		"watcher must pick up the new file")
}

func TestWatchKeepsOldOnInvalidChange(t *testing.T) {
	s, path := newStore(t)
	startWatch(t, s)

	writeConfig(t, path, "teams: [")
	// Give the watcher ample time to observe the write and (wrongly) apply it.
	time.Sleep(500 * time.Millisecond)
	assert.Equal(t, 100, searchRPM(s))
	assert.Equal(t, uint64(1), s.Generation())

	// A subsequent valid write still gets applied: the watcher survived the bad edit.
	writeConfig(t, path, withRPM("400"))
	assert.True(t, waitFor(t, 3*time.Second, func() bool { return searchRPM(s) == 400 }))
}

func TestWatchReloadsOnSIGHUP(t *testing.T) {
	s, path := newStore(t)
	startWatch(t, s)

	// Replace the file's contents without a rename so no fsnotify event for the
	// watched name is guaranteed; SIGHUP must trigger the reload on its own.
	require.NoError(t, os.WriteFile(path, []byte(withRPM("500")), 0o600))
	// Drain any file-change reload that may have fired before sending the signal.
	waitFor(t, time.Second, func() bool { return searchRPM(s) == 500 })
	require.NoError(t, os.WriteFile(path, []byte(withRPM("600")), 0o600))
	time.Sleep(300 * time.Millisecond)

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGHUP))
	assert.True(t, waitFor(t, 3*time.Second, func() bool { return searchRPM(s) == 600 }),
		"SIGHUP must force a reload")
}

func TestWatchFailsOnMissingDirectory(t *testing.T) {
	s, path := newStore(t)
	require.NoError(t, os.RemoveAll(filepath.Dir(path)))

	_, err := s.Watch(context.Background())
	require.Error(t, err)
}

func TestStoreConcurrentReadersDuringReload(t *testing.T) {
	s, path := newStore(t)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					cfg := s.Current()
					if _, ok := cfg.TeamByKey("search-key-1"); !ok {
						t.Error("team lookup failed mid-reload")
						return
					}
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		writeConfig(t, path, withRPM("1"+strings.Repeat("0", i%3+1)))
		require.NoError(t, s.Reload())
	}
	close(stop)
	wg.Wait()
	assert.Equal(t, uint64(21), s.Generation())
}
