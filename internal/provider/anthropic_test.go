package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ShriramJana/crossbar/internal/provider"
)

// anthropicMessage mirrors the subset of the Messages API response the adapter reads.
const anthropicOK = `{
  "id": "msg_01",
  "type": "message",
  "role": "assistant",
  "model": "claude-sonnet-4-5",
  "content": [{"type": "text", "text": "Hi "}, {"type": "text", "text": "there"}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 12, "output_tokens": 5}
}`

func newAnthropic(t *testing.T, h http.HandlerFunc) (*provider.Anthropic, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	a := provider.NewAnthropic(provider.AnthropicConfig{
		BaseURL: ts.URL,
		APIKey:  "test-key",
		Client:  ts.Client(),
	})
	return a, ts
}

func TestAnthropicSendTranslatesRequest(t *testing.T) {
	var gotPath, gotKey, gotVersion, gotCT string
	var gotBody map[string]any
	a, _ := newAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotCT = r.Header.Get("Content-Type")
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicOK))
	})

	resp, err := a.Send(context.Background(), &provider.Request{
		Model:       "claude-sonnet-4-5",
		System:      "be brief",
		Messages:    []provider.Message{{Role: "user", Content: "hello"}},
		MaxTokens:   64,
		Temperature: 0.2,
	})
	require.NoError(t, err)

	assert.Equal(t, "/v1/messages", gotPath)
	assert.Equal(t, "test-key", gotKey)
	assert.Equal(t, "2023-06-01", gotVersion)
	assert.Equal(t, "application/json", gotCT)
	assert.Equal(t, "claude-sonnet-4-5", gotBody["model"])
	assert.Equal(t, "be brief", gotBody["system"])
	assert.EqualValues(t, 64, gotBody["max_tokens"])
	assert.EqualValues(t, 0.2, gotBody["temperature"])
	msgs, ok := gotBody["messages"].([]any)
	require.True(t, ok)
	require.Len(t, msgs, 1)
	assert.Equal(t, map[string]any{"role": "user", "content": "hello"}, msgs[0])

	assert.Equal(t, "Hi there", resp.Text, "text blocks are concatenated")
	assert.Equal(t, 12, resp.InputTokens)
	assert.Equal(t, 5, resp.OutputTokens)
	assert.Equal(t, "claude-sonnet-4-5", resp.Model)
	assert.Equal(t, "anthropic", resp.Provider)
	assert.Positive(t, resp.Latency)
	assert.False(t, resp.Fallback)
}

func TestAnthropicOmitsEmptyOptionalFields(t *testing.T) {
	var gotBody map[string]any
	a, _ := newAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		_, _ = w.Write([]byte(anthropicOK))
	})

	_, err := a.Send(context.Background(), &provider.Request{
		Model:     "claude-sonnet-4-5",
		Messages:  []provider.Message{{Role: "user", Content: "hi"}},
		MaxTokens: 8,
	})
	require.NoError(t, err)
	_, hasSystem := gotBody["system"]
	_, hasTemp := gotBody["temperature"]
	assert.False(t, hasSystem, "empty system must not be sent")
	assert.False(t, hasTemp, "zero temperature must not be sent so the provider default applies")
}

func TestAnthropicErrorClassification(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantClass provider.ErrorClass
	}{
		{name: "400 invalid request", status: 400, body: `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`, wantClass: provider.ErrNonRetryable},
		{name: "401 auth", status: 401, body: `{"type":"error","error":{"type":"authentication_error","message":"nope"}}`, wantClass: provider.ErrNonRetryable},
		{name: "429 rate limited", status: 429, body: `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, wantClass: provider.ErrRetryable},
		{name: "500 internal", status: 500, body: `{"type":"error","error":{"type":"api_error","message":"boom"}}`, wantClass: provider.ErrRetryable},
		{name: "529 overloaded", status: 529, body: `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`, wantClass: provider.ErrRetryable},
		{name: "non-JSON error body", status: 502, body: `<html>bad gateway</html>`, wantClass: provider.ErrRetryable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _ := newAnthropic(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})

			_, err := a.Send(context.Background(), testRequest())
			var pe *provider.Error
			require.ErrorAs(t, err, &pe)
			assert.Equal(t, "anthropic", pe.Provider)
			assert.Equal(t, tt.wantClass, pe.Class)
			assert.Equal(t, tt.status, pe.StatusCode)
		})
	}
}

func TestAnthropicErrorMessageSurfaced(t *testing.T) {
	a, _ := newAnthropic(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens is required"}}`))
	})
	_, err := a.Send(context.Background(), testRequest())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_tokens is required")
}

func TestAnthropicTransportError(t *testing.T) {
	a, ts := newAnthropic(t, func(http.ResponseWriter, *http.Request) {})
	ts.Close()

	_, err := a.Send(context.Background(), testRequest())
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, provider.ErrTransport, pe.Class)
	assert.Equal(t, 0, pe.StatusCode)
}

func TestAnthropicTimeoutIsRetryable(t *testing.T) {
	a, _ := newAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := a.Send(ctx, testRequest())

	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, provider.ErrRetryable, pe.Class)
	assert.True(t, errors.Is(err, context.DeadlineExceeded))
}

func TestAnthropicMalformedSuccessBody(t *testing.T) {
	a, _ := newAnthropic(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content": [`))
	})
	_, err := a.Send(context.Background(), testRequest())
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, provider.ErrRetryable, pe.Class)
}

func TestAnthropicHealthCheck(t *testing.T) {
	var hits atomic.Int32
	a, _ := newAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		w.WriteHeader(500)
	})
	require.NoError(t, a.HealthCheck(context.Background()))
	assert.Equal(t, int32(1), hits.Load())
	assert.Equal(t, "anthropic", a.Name())
}

func TestAnthropicHealthCheckFailure(t *testing.T) {
	a, _ := newAnthropic(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
	})
	err := a.HealthCheck(context.Background())
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, provider.ErrRetryable, pe.Class)
}
