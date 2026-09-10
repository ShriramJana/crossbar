package provider_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ShriramJana/relay/internal/provider"
)

func testRequest() *provider.Request {
	return &provider.Request{
		Model:     "mock-model",
		Messages:  []provider.Message{{Role: "user", Content: "hello"}},
		MaxTokens: 32,
	}
}

func TestMockDefaultSuccess(t *testing.T) {
	m := provider.NewMock("mock")

	resp, err := m.Send(context.Background(), testRequest())
	require.NoError(t, err)
	assert.Equal(t, "mock", resp.Provider)
	assert.Equal(t, "mock-model", resp.Model)
	assert.NotEmpty(t, resp.Text)
	assert.Positive(t, resp.InputTokens)
	assert.Positive(t, resp.OutputTokens)
	assert.Equal(t, "mock", m.Name())
	require.NoError(t, m.HealthCheck(context.Background()))
}

func TestMockScriptedFailures(t *testing.T) {
	tests := []struct {
		name       string
		behavior   provider.Behavior
		wantClass  provider.ErrorClass
		wantStatus int
	}{
		{name: "429 is retryable", behavior: provider.Fail(429), wantClass: provider.ErrRetryable, wantStatus: 429},
		{name: "500 is retryable", behavior: provider.Fail(500), wantClass: provider.ErrRetryable, wantStatus: 500},
		{name: "400 is non-retryable", behavior: provider.Fail(400), wantClass: provider.ErrNonRetryable, wantStatus: 400},
		{name: "transport failure", behavior: provider.Transport(), wantClass: provider.ErrTransport},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := provider.NewMock("mock")
			m.Enqueue(tt.behavior)

			_, err := m.Send(context.Background(), testRequest())
			var pe *provider.Error
			require.ErrorAs(t, err, &pe)
			assert.Equal(t, "mock", pe.Provider)
			assert.Equal(t, tt.wantClass, pe.Class)
			assert.Equal(t, tt.wantStatus, pe.StatusCode)

			// Queue is consumed; the next call falls back to the default.
			_, err = m.Send(context.Background(), testRequest())
			require.NoError(t, err)
		})
	}
}

func TestMockQueueOrder(t *testing.T) {
	m := provider.NewMock("mock")
	m.Enqueue(provider.Fail(500), provider.Succeed("second"), provider.Fail(429))

	_, err := m.Send(context.Background(), testRequest())
	require.Error(t, err)

	resp, err := m.Send(context.Background(), testRequest())
	require.NoError(t, err)
	assert.Equal(t, "second", resp.Text)

	_, err = m.Send(context.Background(), testRequest())
	require.Error(t, err)
	assert.Equal(t, 3, m.Calls())
}

func TestMockTimeoutHonoursContext(t *testing.T) {
	m := provider.NewMock("mock")
	m.Enqueue(provider.Timeout())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := m.Send(ctx, testRequest())
	elapsed := time.Since(start)

	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, provider.ErrRetryable, pe.Class)
	assert.True(t, errors.Is(err, context.DeadlineExceeded))
	assert.Less(t, elapsed, 500*time.Millisecond, "timeout must return as soon as ctx expires")
}

func TestMockSlowRespectsCancellation(t *testing.T) {
	m := provider.NewMock("mock")
	m.SetDefault(provider.Slow(time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := m.Send(ctx, testRequest())
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

func TestMockSlowSucceedsAfterDelay(t *testing.T) {
	m := provider.NewMock("mock")
	m.SetDefault(provider.Slow(20 * time.Millisecond))

	start := time.Now()
	resp, err := m.Send(context.Background(), testRequest())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, time.Since(start), 20*time.Millisecond)
	assert.GreaterOrEqual(t, resp.Latency, 20*time.Millisecond)
}

func TestMockHealthCheckFollowsDefault(t *testing.T) {
	m := provider.NewMock("mock")
	require.NoError(t, m.HealthCheck(context.Background()))

	m.SetDefault(provider.Fail(503))
	err := m.HealthCheck(context.Background())
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, 503, pe.StatusCode)
}

func TestMockConcurrentUse(t *testing.T) {
	m := provider.NewMock("mock")
	const n = 200
	for i := 0; i < n/2; i++ {
		m.Enqueue(provider.Fail(500))
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	failures := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Send(context.Background(), testRequest()); err != nil {
				mu.Lock()
				failures++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, n, m.Calls())
	assert.Equal(t, n/2, failures, "each queued failure must be consumed exactly once")
}
