package budget_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ShriramJana/crossbar/internal/budget"
	"github.com/ShriramJana/crossbar/internal/redistest"
)

type warnRecorder struct {
	mu    sync.Mutex
	calls []budget.Warning
}

func (w *warnRecorder) warn(wn budget.Warning) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, wn)
}

func (w *warnRecorder) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.calls)
}

var fixedNow = time.Date(2026, time.March, 15, 10, 30, 0, 0, time.UTC)

func setup(t *testing.T) (*budget.Budget, string, *warnRecorder) {
	t.Helper()
	rdb := redistest.Client(t)
	rec := &warnRecorder{}
	b := budget.New(rdb, budget.Options{
		Now:    func() time.Time { return fixedNow },
		OnWarn: rec.warn,
	})
	return b, redistest.UniqueID(t, rdb), rec
}

func TestRecordAccumulates(t *testing.T) {
	b, team, _ := setup(t)
	ctx := context.Background()
	lim := budget.Limits{DailyUSD: 10, MonthlyUSD: 100}

	st, err := b.Record(ctx, team, lim, 1.25)
	require.NoError(t, err)
	assert.InDelta(t, 1.25, st.Daily.Spent, 1e-9)
	assert.InDelta(t, 1.25, st.Monthly.Spent, 1e-9)

	st, err = b.Record(ctx, team, lim, 0.75)
	require.NoError(t, err)
	assert.InDelta(t, 2.0, st.Daily.Spent, 1e-9)
	assert.InDelta(t, 2.0, st.Monthly.Spent, 1e-9)

	st, err = b.Check(ctx, team, lim)
	require.NoError(t, err)
	assert.InDelta(t, 2.0, st.Daily.Spent, 1e-9)
	assert.Equal(t, 10.0, st.Daily.Limit)
	assert.Equal(t, 100.0, st.Monthly.Limit)
	_, exceeded := st.Exceeded()
	assert.False(t, exceeded)
}

func TestCheckOnFreshTeamIsZero(t *testing.T) {
	b, team, _ := setup(t)
	st, err := b.Check(context.Background(), team, budget.Limits{DailyUSD: 1, MonthlyUSD: 2})
	require.NoError(t, err)
	assert.Zero(t, st.Daily.Spent)
	assert.Zero(t, st.Monthly.Spent)
}

func TestDailyExceeded(t *testing.T) {
	b, team, _ := setup(t)
	ctx := context.Background()
	lim := budget.Limits{DailyUSD: 5, MonthlyUSD: 100}

	_, err := b.Record(ctx, team, lim, 4.99)
	require.NoError(t, err)
	st, err := b.Check(ctx, team, lim)
	require.NoError(t, err)
	_, exceeded := st.Exceeded()
	assert.False(t, exceeded, "just under the limit is fine")

	_, err = b.Record(ctx, team, lim, 0.01)
	require.NoError(t, err)
	st, err = b.Check(ctx, team, lim)
	require.NoError(t, err)
	p, exceeded := st.Exceeded()
	require.True(t, exceeded, "reaching the limit exactly exhausts it")
	assert.Equal(t, budget.PeriodDaily, p.Name)
	assert.Equal(t, time.Date(2026, time.March, 16, 0, 0, 0, 0, time.UTC), p.ResetsAt)
}

func TestMonthlyExceeded(t *testing.T) {
	b, team, _ := setup(t)
	ctx := context.Background()
	lim := budget.Limits{DailyUSD: 1000, MonthlyUSD: 20}

	_, err := b.Record(ctx, team, lim, 25)
	require.NoError(t, err)
	st, err := b.Check(ctx, team, lim)
	require.NoError(t, err)
	p, exceeded := st.Exceeded()
	require.True(t, exceeded)
	assert.Equal(t, budget.PeriodMonthly, p.Name)
	assert.Equal(t, time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC), p.ResetsAt)
}

func TestZeroLimitIsUnlimited(t *testing.T) {
	b, team, rec := setup(t)
	ctx := context.Background()
	lim := budget.Limits{DailyUSD: 0, MonthlyUSD: 0}

	_, err := b.Record(ctx, team, lim, 1e6)
	require.NoError(t, err)
	st, err := b.Check(ctx, team, lim)
	require.NoError(t, err)
	_, exceeded := st.Exceeded()
	assert.False(t, exceeded)
	assert.Equal(t, 0, rec.count(), "no warning without a limit")
}

func TestWarningFiresOncePerPeriod(t *testing.T) {
	b, team, rec := setup(t)
	ctx := context.Background()
	lim := budget.Limits{DailyUSD: 10, MonthlyUSD: 1000}

	_, err := b.Record(ctx, team, lim, 7.9)
	require.NoError(t, err)
	assert.Equal(t, 0, rec.count(), "79% is under the threshold")

	_, err = b.Record(ctx, team, lim, 0.2)
	require.NoError(t, err)
	require.Equal(t, 1, rec.count(), "crossing 80% warns")
	assert.Equal(t, budget.PeriodDaily, rec.calls[0].Period)
	assert.Equal(t, team, rec.calls[0].Team)
	assert.InDelta(t, 8.1, rec.calls[0].Spent, 1e-9)
	assert.Equal(t, 10.0, rec.calls[0].Limit)

	_, err = b.Record(ctx, team, lim, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, rec.count(), "further spend in the same period does not warn again")
}

func TestWarningPerPeriodIndependently(t *testing.T) {
	b, team, rec := setup(t)
	ctx := context.Background()
	// Monthly crosses 80% first, then daily on a later call.
	lim := budget.Limits{DailyUSD: 100, MonthlyUSD: 10}

	_, err := b.Record(ctx, team, lim, 8.5)
	require.NoError(t, err)
	require.Equal(t, 1, rec.count())
	assert.Equal(t, budget.PeriodMonthly, rec.calls[0].Period)

	_, err = b.Record(ctx, team, lim, 75)
	require.NoError(t, err)
	require.Equal(t, 2, rec.count())
	assert.Equal(t, budget.PeriodDaily, rec.calls[1].Period)
}

func TestKeysExpire(t *testing.T) {
	rdb := redistest.Client(t)
	b := budget.New(rdb, budget.Options{Now: func() time.Time { return fixedNow }})
	team := redistest.UniqueID(t, rdb)
	ctx := context.Background()

	_, err := b.Record(ctx, team, budget.Limits{DailyUSD: 1, MonthlyUSD: 1}, 0.5)
	require.NoError(t, err)

	dailyTTL, err := rdb.TTL(ctx, budget.DailyKey(team, fixedNow)).Result()
	require.NoError(t, err)
	assert.Greater(t, dailyTTL, 47*time.Hour)
	assert.LessOrEqual(t, dailyTTL, 48*time.Hour)

	monthlyTTL, err := rdb.TTL(ctx, budget.MonthlyKey(team, fixedNow)).Result()
	require.NoError(t, err)
	assert.Greater(t, monthlyTTL, 39*24*time.Hour)
	assert.LessOrEqual(t, monthlyTTL, 40*24*time.Hour)

	// A second record must not push the expiry out.
	_, err = b.Record(ctx, team, budget.Limits{DailyUSD: 1, MonthlyUSD: 1}, 0.1)
	require.NoError(t, err)
	again, err := rdb.TTL(ctx, budget.DailyKey(team, fixedNow)).Result()
	require.NoError(t, err)
	assert.LessOrEqual(t, again, dailyTTL)
}

func TestPeriodsRollOver(t *testing.T) {
	rdb := redistest.Client(t)
	now := fixedNow
	b := budget.New(rdb, budget.Options{Now: func() time.Time { return now }})
	team := redistest.UniqueID(t, rdb)
	ctx := context.Background()
	lim := budget.Limits{DailyUSD: 10, MonthlyUSD: 100}

	_, err := b.Record(ctx, team, lim, 3)
	require.NoError(t, err)

	now = now.Add(24 * time.Hour) // next day, same month
	st, err := b.Check(ctx, team, lim)
	require.NoError(t, err)
	assert.Zero(t, st.Daily.Spent, "a new day starts at zero")
	assert.InDelta(t, 3, st.Monthly.Spent, 1e-9, "the month carries on")

	now = time.Date(2026, time.April, 1, 0, 0, 1, 0, time.UTC)
	st, err = b.Check(ctx, team, lim)
	require.NoError(t, err)
	assert.Zero(t, st.Monthly.Spent, "a new month starts at zero")
}

func TestKeyFormats(t *testing.T) {
	assert.Equal(t, "budget:search:2026-03-15", budget.DailyKey("search", fixedNow))
	assert.Equal(t, "budget:search:2026-03", budget.MonthlyKey("search", fixedNow))
}

func TestConcurrentRecordsSumExactly(t *testing.T) {
	b, team, _ := setup(t)
	ctx := context.Background()
	lim := budget.Limits{DailyUSD: 0, MonthlyUSD: 0}

	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.Record(ctx, team, lim, 0.01); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	st, err := b.Check(ctx, team, lim)
	require.NoError(t, err)
	assert.InDelta(t, 2.0, st.Daily.Spent, 1e-6)
}
