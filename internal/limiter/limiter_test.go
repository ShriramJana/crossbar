package limiter_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ShriramJana/crossbar/internal/limiter"
	"github.com/ShriramJana/crossbar/internal/redistest"
)

func setup(t *testing.T) (*limiter.Limiter, string) {
	t.Helper()
	rdb := redistest.Client(t)
	return limiter.New(rdb), redistest.UniqueID(t, rdb)
}

// rewind moves a bucket's last-refill timestamp into the past so refill can
// be tested without sleeping.
func rewind(t *testing.T, l *limiter.Limiter, team, dimension string, by time.Duration) {
	t.Helper()
	ctx := context.Background()
	key := limiter.BucketKey(team, dimension)
	ts, err := l.Client().HGet(ctx, key, "ts").Int64()
	require.NoError(t, err)
	require.NoError(t, l.Client().HSet(ctx, key, "ts", strconv.FormatInt(ts-by.Microseconds(), 10)).Err())
}

func TestAllowUpToRequestLimit(t *testing.T) {
	l, team := setup(t)
	ctx := context.Background()
	lim := limiter.Limits{RequestsPerMinute: 3, TokensPerMinute: 1000}

	for i := 0; i < 3; i++ {
		d, err := l.Allow(ctx, team, lim, 10)
		require.NoError(t, err)
		assert.True(t, d.Allowed, "request %d should be admitted", i+1)
		assert.Equal(t, 2-i, d.RemainingRequests)
	}

	d, err := l.Allow(ctx, team, lim, 10)
	require.NoError(t, err)
	assert.False(t, d.Allowed)
	assert.Equal(t, limiter.DimensionRequests, d.Dimension)
	assert.GreaterOrEqual(t, d.RetryAfter, time.Second, "Retry-After must round up to at least one second")
	assert.LessOrEqual(t, d.RetryAfter, 21*time.Second)
}

func TestAllowUpToTokenLimit(t *testing.T) {
	l, team := setup(t)
	ctx := context.Background()
	lim := limiter.Limits{RequestsPerMinute: 100, TokensPerMinute: 100}

	d, err := l.Allow(ctx, team, lim, 60)
	require.NoError(t, err)
	assert.True(t, d.Allowed)
	assert.Equal(t, 40, d.RemainingTokens)

	d, err = l.Allow(ctx, team, lim, 60)
	require.NoError(t, err)
	assert.False(t, d.Allowed)
	assert.Equal(t, limiter.DimensionTokens, d.Dimension)
	assert.GreaterOrEqual(t, d.RetryAfter, time.Second)
}

func TestDeniedRequestConsumesNothing(t *testing.T) {
	l, team := setup(t)
	ctx := context.Background()
	lim := limiter.Limits{RequestsPerMinute: 1, TokensPerMinute: 10}

	d, err := l.Allow(ctx, team, lim, 20)
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, limiter.DimensionTokens, d.Dimension)

	// The single request slot must still be available.
	d, err = l.Allow(ctx, team, lim, 5)
	require.NoError(t, err)
	assert.True(t, d.Allowed)
}

func TestRequestExceedingCapacityIsRejectedOutright(t *testing.T) {
	l, team := setup(t)
	lim := limiter.Limits{RequestsPerMinute: 10, TokensPerMinute: 100}

	d, err := l.Allow(context.Background(), team, lim, 101)
	require.NoError(t, err)
	assert.False(t, d.Allowed)
	assert.Equal(t, limiter.DimensionTokens, d.Dimension)
	assert.True(t, d.ExceedsCapacity, "a request larger than the per-minute cap can never succeed")
}

func TestRefillOverTime(t *testing.T) {
	l, team := setup(t)
	ctx := context.Background()
	lim := limiter.Limits{RequestsPerMinute: 60, TokensPerMinute: 6000} // 1 req/s, 100 tok/s

	for i := 0; i < 60; i++ {
		d, err := l.Allow(ctx, team, lim, 100)
		require.NoError(t, err)
		require.True(t, d.Allowed)
	}
	d, err := l.Allow(ctx, team, lim, 100)
	require.NoError(t, err)
	require.False(t, d.Allowed)

	rewind(t, l, team, limiter.DimensionRequests, 2*time.Second)
	rewind(t, l, team, limiter.DimensionTokens, 2*time.Second)

	for i := 0; i < 2; i++ {
		d, err := l.Allow(ctx, team, lim, 100)
		require.NoError(t, err)
		assert.True(t, d.Allowed, "two seconds of refill should admit two requests")
	}
	d, err = l.Allow(ctx, team, lim, 100)
	require.NoError(t, err)
	assert.False(t, d.Allowed)
}

func TestRefillNeverExceedsCapacity(t *testing.T) {
	l, team := setup(t)
	ctx := context.Background()
	lim := limiter.Limits{RequestsPerMinute: 2, TokensPerMinute: 100}

	d, err := l.Allow(ctx, team, lim, 1)
	require.NoError(t, err)
	require.True(t, d.Allowed)

	rewind(t, l, team, limiter.DimensionRequests, time.Hour)
	d, err = l.Allow(ctx, team, lim, 1)
	require.NoError(t, err)
	assert.True(t, d.Allowed)
	assert.Equal(t, 1, d.RemainingRequests, "an idle hour refills to capacity, not beyond")
}

func TestReconcileRefundsUnusedTokens(t *testing.T) {
	l, team := setup(t)
	ctx := context.Background()
	lim := limiter.Limits{RequestsPerMinute: 100, TokensPerMinute: 100}

	d, err := l.Allow(ctx, team, lim, 80)
	require.NoError(t, err)
	require.True(t, d.Allowed)

	require.NoError(t, l.Reconcile(ctx, team, lim, 80, 30))

	d, err = l.Allow(ctx, team, lim, 60)
	require.NoError(t, err)
	assert.True(t, d.Allowed, "50 refunded + 20 remaining covers 60")
	assert.Equal(t, 10, d.RemainingTokens)
}

func TestReconcileChargesOverspend(t *testing.T) {
	l, team := setup(t)
	ctx := context.Background()
	lim := limiter.Limits{RequestsPerMinute: 100, TokensPerMinute: 100}

	d, err := l.Allow(ctx, team, lim, 10)
	require.NoError(t, err)
	require.True(t, d.Allowed)

	require.NoError(t, l.Reconcile(ctx, team, lim, 10, 95))

	d, err = l.Allow(ctx, team, lim, 10)
	require.NoError(t, err)
	assert.False(t, d.Allowed, "actual usage beyond the reservation is charged")
	assert.Equal(t, limiter.DimensionTokens, d.Dimension)
}

func TestReconcileNoopWhenExact(t *testing.T) {
	l, team := setup(t)
	ctx := context.Background()
	lim := limiter.Limits{RequestsPerMinute: 100, TokensPerMinute: 100}

	_, err := l.Allow(ctx, team, lim, 40)
	require.NoError(t, err)
	require.NoError(t, l.Reconcile(ctx, team, lim, 40, 40))

	d, err := l.Allow(ctx, team, lim, 1)
	require.NoError(t, err)
	assert.Equal(t, 59, d.RemainingTokens)
}

func TestTeamsAreIsolated(t *testing.T) {
	l, team := setup(t)
	ctx := context.Background()
	lim := limiter.Limits{RequestsPerMinute: 1, TokensPerMinute: 100}

	d, err := l.Allow(ctx, team+"-a", lim, 1)
	require.NoError(t, err)
	require.True(t, d.Allowed)
	d, err = l.Allow(ctx, team+"-a", lim, 1)
	require.NoError(t, err)
	require.False(t, d.Allowed)

	d, err = l.Allow(ctx, team+"-b", lim, 1)
	require.NoError(t, err)
	assert.True(t, d.Allowed, "another team's bucket is untouched")
}

func TestBucketsExpire(t *testing.T) {
	l, team := setup(t)
	ctx := context.Background()
	_, err := l.Allow(ctx, team, limiter.Limits{RequestsPerMinute: 10, TokensPerMinute: 100}, 1)
	require.NoError(t, err)

	for _, dim := range []string{limiter.DimensionRequests, limiter.DimensionTokens} {
		ttl, err := l.Client().TTL(ctx, limiter.BucketKey(team, dim)).Result()
		require.NoError(t, err)
		assert.Greater(t, ttl, time.Minute, "idle buckets must expire on their own")
		assert.LessOrEqual(t, ttl, 2*time.Minute)
	}
}

func TestAllowHonoursContext(t *testing.T) {
	l, team := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := l.Allow(ctx, team, limiter.Limits{RequestsPerMinute: 10, TokensPerMinute: 100}, 1)
	require.Error(t, err)
}

// TestConcurrentAccuracy is the M3 acceptance check: 1000 concurrent requests
// against a 100 rpm limit must admit exactly 100. Anything else means the
// check-and-decrement is not atomic.
func TestConcurrentAccuracy(t *testing.T) {
	l, team := setup(t)
	ctx := context.Background()
	lim := limiter.Limits{RequestsPerMinute: 100, TokensPerMinute: 1_000_000}

	const n = 1000
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			d, err := l.Allow(ctx, team, lim, 10)
			if err != nil {
				t.Error(err)
				return
			}
			if d.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	// Refill during the burst is bounded by elapsed time: at 100/min, even a
	// full second of wall-clock adds under 2 slots. The spec allows ±2.
	t.Logf("admitted %d of %d concurrent requests against a 100 rpm limit", allowed, n)
	assert.InDelta(t, 100, allowed, 2, "admitted %d of %d against a 100 rpm limit", allowed, n)
}
