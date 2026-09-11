package server_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ShriramJana/crossbar/internal/server"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := server.New(server.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestHealth(t *testing.T) {
	ts := newTestServer(t)

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{name: "GET health is ok", method: http.MethodGet, path: "/health", wantStatus: http.StatusOK},
		{name: "POST health is not allowed", method: http.MethodPost, path: "/health", wantStatus: http.StatusMethodNotAllowed},
		{name: "unknown path is not found", method: http.MethodGet, path: "/nope", wantStatus: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, ts.URL+tt.path, nil)
			require.NoError(t, err)
			resp, err := ts.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, tt.wantStatus, resp.StatusCode)
		})
	}
}

func TestHealthBody(t *testing.T) {
	ts := newTestServer(t)

	resp, err := ts.Client().Get(ts.URL + "/health")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	var body struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "ok", body.Status)
}

func TestRequestID(t *testing.T) {
	ts := newTestServer(t)

	t.Run("generated when absent", func(t *testing.T) {
		resp, err := ts.Client().Get(ts.URL + "/health")
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.NotEmpty(t, resp.Header.Get("X-Request-ID"))
	})

	t.Run("echoed when supplied", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/health", nil)
		require.NoError(t, err)
		req.Header.Set("X-Request-ID", "client-supplied-id")
		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, "client-supplied-id", resp.Header.Get("X-Request-ID"))
	})
}
