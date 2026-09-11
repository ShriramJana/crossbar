package server_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ShriramJana/crossbar/internal/server"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	return ln
}

func TestServeDrainsInFlightRequests(t *testing.T) {
	ln := listen(t)
	started := make(chan struct{})
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	})

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var serveErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		serveErr = server.Serve(ctx, ln, h, server.ServeOptions{
			DrainTimeout: 5 * time.Second,
			Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()

	var respErr error
	var status int
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			respErr = err
			return
		}
		defer resp.Body.Close()
		status = resp.StatusCode
	}()

	<-started
	cancel()
	// Give the server a moment to begin shutting down before releasing the handler.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	require.NoError(t, serveErr)
	require.NoError(t, respErr)
	assert.Equal(t, http.StatusOK, status, "in-flight request must complete during drain")
}

func TestServeDrainTimeoutFires(t *testing.T) {
	ln := listen(t)
	started := make(chan struct{})
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(ctx, ln, h, server.ServeOptions{
			DrainTimeout: 50 * time.Millisecond,
			Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()

	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err == nil {
			resp.Body.Close()
		}
	}()

	<-started
	cancel()

	select {
	case err := <-errCh:
		assert.True(t, errors.Is(err, context.DeadlineExceeded), "expected drain timeout, got %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after drain timeout")
	}
	close(release)
}

func TestServeReturnsListenerError(t *testing.T) {
	ln := listen(t)
	require.NoError(t, ln.Close())

	err := server.Serve(context.Background(), ln, http.NotFoundHandler(), server.ServeOptions{
		DrainTimeout: time.Second,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.Error(t, err)
}
