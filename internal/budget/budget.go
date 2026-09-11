// Package budget tracks per-team spend in Redis and enforces daily and
// monthly USD limits.
package budget

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Period names.
const (
	PeriodDaily   = "daily"
	PeriodMonthly = "monthly"
)

// Key lifetimes. Long enough to survive a late reconciliation or a report
// run the morning after, short enough that Redis is not a ledger.
const (
	dailyTTL   = 48 * time.Hour
	monthlyTTL = 40 * 24 * time.Hour
)

// warnFraction is the share of a budget at which a one-time warning fires.
const warnFraction = 0.8

// Limits are a team's budgets in USD. Zero means unlimited.
type Limits struct {
	DailyUSD   float64
	MonthlyUSD float64
}

// Period is the state of one budget window.
type Period struct {
	Name     string
	Spent    float64
	Limit    float64
	ResetsAt time.Time
}

// exhausted reports whether this period's limit is set and reached.
func (p Period) exhausted() bool {
	return p.Limit > 0 && p.Spent >= p.Limit
}

// Status is a team's spend across both windows.
type Status struct {
	Daily   Period
	Monthly Period
}

// Exceeded returns the first exhausted period, daily before monthly.
func (s Status) Exceeded() (Period, bool) {
	if s.Daily.exhausted() {
		return s.Daily, true
	}
	if s.Monthly.exhausted() {
		return s.Monthly, true
	}
	return Period{}, false
}

// Warning is emitted once per period when spend crosses warnFraction of the limit.
type Warning struct {
	Team   string
	Period string
	Spent  float64
	Limit  float64
}

// Options tunes a Budget.
type Options struct {
	// Now supplies the clock; defaults to time.Now. Periods are computed in UTC.
	Now func() time.Time
	// OnWarn receives each once-per-period warning. Nil disables warnings.
	OnWarn func(Warning)
}

// Budget is safe for concurrent use.
type Budget struct {
	rdb    redis.UniversalClient
	now    func() time.Time
	onWarn func(Warning)
}

// New returns a Budget backed by rdb.
func New(rdb redis.UniversalClient, opts Options) *Budget {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Budget{rdb: rdb, now: opts.Now, onWarn: opts.OnWarn}
}

// DailyKey is the counter for team on the UTC day containing t.
func DailyKey(team string, t time.Time) string {
	return "budget:" + team + ":" + t.UTC().Format("2006-01-02")
}

// MonthlyKey is the counter for team in the UTC month containing t.
func MonthlyKey(team string, t time.Time) string {
	return "budget:" + team + ":" + t.UTC().Format("2006-01")
}

func nextDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
}

func nextMonth(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}

// Check reads current spend without changing it.
func (b *Budget) Check(ctx context.Context, team string, lim Limits) (Status, error) {
	now := b.now()
	vals, err := b.rdb.MGet(ctx, DailyKey(team, now), MonthlyKey(team, now)).Result()
	if err != nil {
		return Status{}, fmt.Errorf("budget: reading counters: %w", err)
	}
	daily, err := parseSpend(vals[0])
	if err != nil {
		return Status{}, err
	}
	monthly, err := parseSpend(vals[1])
	if err != nil {
		return Status{}, err
	}
	return b.status(now, lim, daily, monthly), nil
}

// Record adds cost to both counters and returns the resulting status. The
// first call in a period sets the key's expiry; later calls leave it alone.
func (b *Budget) Record(ctx context.Context, team string, lim Limits, cost float64) (Status, error) {
	now := b.now()
	dk, mk := DailyKey(team, now), MonthlyKey(team, now)

	pipe := b.rdb.TxPipeline()
	dailyCmd := pipe.IncrByFloat(ctx, dk, cost)
	pipe.ExpireNX(ctx, dk, dailyTTL)
	monthlyCmd := pipe.IncrByFloat(ctx, mk, cost)
	pipe.ExpireNX(ctx, mk, monthlyTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return Status{}, fmt.Errorf("budget: recording spend: %w", err)
	}

	st := b.status(now, lim, dailyCmd.Val(), monthlyCmd.Val())
	b.maybeWarn(ctx, team, dk, dailyTTL, st.Daily)
	b.maybeWarn(ctx, team, mk, monthlyTTL, st.Monthly)
	return st, nil
}

func (b *Budget) status(now time.Time, lim Limits, daily, monthly float64) Status {
	return Status{
		Daily:   Period{Name: PeriodDaily, Spent: daily, Limit: lim.DailyUSD, ResetsAt: nextDay(now)},
		Monthly: Period{Name: PeriodMonthly, Spent: monthly, Limit: lim.MonthlyUSD, ResetsAt: nextMonth(now)},
	}
}

// maybeWarn fires the once-per-period warning. SETNX on a marker key makes
// "once" hold across every gateway instance, not just this process.
func (b *Budget) maybeWarn(ctx context.Context, team, key string, ttl time.Duration, p Period) {
	if b.onWarn == nil || p.Limit <= 0 || p.Spent < warnFraction*p.Limit {
		return
	}
	first, err := b.rdb.SetNX(ctx, key+":warned", "1", ttl).Result()
	if err != nil || !first {
		return
	}
	b.onWarn(Warning{Team: team, Period: p.Name, Spent: p.Spent, Limit: p.Limit})
}

func parseSpend(v any) (float64, error) {
	if v == nil {
		return 0, nil
	}
	s, ok := v.(string)
	if !ok {
		return 0, errors.New("budget: counter is not a string")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("budget: parsing counter %q: %w", s, err)
	}
	return f, nil
}
