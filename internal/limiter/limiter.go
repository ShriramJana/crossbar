// Package limiter enforces per-team request and token rate limits with a
// Redis-backed token bucket. Check-and-decrement runs in a Lua script so the
// decision is atomic across every gateway instance sharing the Redis.
package limiter

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
)

// Dimensions a request can be limited on.
const (
	DimensionRequests = "requests"
	DimensionTokens   = "tokens"
)

// bucketTTL is how long an idle bucket lives. A full refill takes one minute,
// so anything older than two is indistinguishable from a fresh bucket.
const bucketTTL = 2 * time.Minute

// Limits are a team's per-minute capacities.
type Limits struct {
	RequestsPerMinute int
	TokensPerMinute   int
}

// Decision is the outcome of an Allow call.
type Decision struct {
	Allowed bool
	// Dimension names the limit that rejected the request; empty when allowed.
	Dimension string
	// RetryAfter is how long until the rejecting bucket can admit the request,
	// rounded up to whole seconds and never less than one second.
	RetryAfter time.Duration
	// ExceedsCapacity is set when the request asks for more tokens than the
	// per-minute cap: it can never be admitted, so waiting is pointless.
	ExceedsCapacity   bool
	RemainingRequests int
	RemainingTokens   int
}

// Limiter is safe for concurrent use.
type Limiter struct {
	rdb       redis.UniversalClient
	allow     *redis.Script
	reconcile *redis.Script
}

// New returns a Limiter backed by rdb.
func New(rdb redis.UniversalClient) *Limiter {
	return &Limiter{
		rdb:       rdb,
		allow:     redis.NewScript(allowScript),
		reconcile: redis.NewScript(reconcileScript),
	}
}

// Client exposes the underlying client, for tests that inspect bucket state.
func (l *Limiter) Client() redis.UniversalClient { return l.rdb }

// BucketKey returns the Redis key of a team's bucket in one dimension. The
// team is wrapped in a hash tag so both dimensions share a cluster slot and
// the script can touch them atomically.
func BucketKey(team, dimension string) string {
	return "ratelimit:{" + team + "}:" + dimension
}

// Allow reserves one request and tokens tokens for team. Either both
// reservations succeed or neither does. A rejected call consumes nothing.
func (l *Limiter) Allow(ctx context.Context, team string, lim Limits, tokens int) (Decision, error) {
	if tokens > lim.TokensPerMinute {
		return Decision{
			Dimension:       DimensionTokens,
			ExceedsCapacity: true,
			RemainingTokens: lim.TokensPerMinute,
		}, nil
	}

	keys := []string{BucketKey(team, DimensionRequests), BucketKey(team, DimensionTokens)}
	res, err := l.allow.Run(ctx, l.rdb, keys, lim.RequestsPerMinute, lim.TokensPerMinute, tokens, bucketTTL.Milliseconds()).Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("limiter: running allow script: %w", err)
	}
	if len(res) != 5 {
		return Decision{}, fmt.Errorf("limiter: unexpected script reply of length %d", len(res))
	}

	allowed, _ := res[0].(int64)
	dimension, _ := res[1].(string)
	waitMS, _ := res[2].(int64)
	remReq, _ := res[3].(int64)
	remTok, _ := res[4].(int64)

	d := Decision{
		Allowed:           allowed == 1,
		Dimension:         dimension,
		RemainingRequests: int(max(0, remReq)),
		RemainingTokens:   int(max(0, remTok)),
	}
	if !d.Allowed {
		secs := math.Ceil(float64(waitMS) / 1000)
		d.RetryAfter = time.Duration(max(1, int64(secs))) * time.Second
	}
	return d, nil
}

// Reconcile settles a reservation against actual usage once the upstream
// call has returned: unused tokens are refunded and overspend is charged.
func (l *Limiter) Reconcile(ctx context.Context, team string, lim Limits, reserved, actual int) error {
	delta := reserved - actual
	if delta == 0 {
		return nil
	}
	err := l.reconcile.Run(ctx, l.rdb, []string{BucketKey(team, DimensionTokens)}, lim.TokensPerMinute, delta, bucketTTL.Milliseconds()).Err()
	if err != nil {
		return fmt.Errorf("limiter: running reconcile script: %w", err)
	}
	return nil
}

// Both scripts store a bucket as a hash {level, ts} where level is the
// fractional number of units available and ts is the Redis server time (in
// microseconds) at which that level was true. Refill is computed lazily from
// the elapsed time, so no background process is needed and the clock is the
// server's, not any one gateway's.
const bucketLib = `
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000000 + tonumber(t[2])

local function current(key, cap)
  local v = redis.call('HMGET', key, 'level', 'ts')
  if not v[1] then return cap end
  local lvl = tonumber(v[1])
  local elapsed = now - tonumber(v[2])
  if elapsed < 0 then elapsed = 0 end
  lvl = lvl + elapsed / 1000000 * cap / 60
  if lvl > cap then lvl = cap end
  return lvl
end

-- tostring() would render the microsecond timestamp as "1.7576e+15",
-- losing precision and breaking integer parsing, so both fields are
-- formatted explicitly.
local function save(key, lvl, ttl)
  redis.call('HSET', key, 'level', string.format('%.9f', lvl), 'ts', string.format('%d', now))
  redis.call('PEXPIRE', key, ttl)
end
`

// allowScript: KEYS = {requests bucket, tokens bucket};
// ARGV = {requests/min, tokens/min, tokens needed, ttl ms}.
// Returns {allowed, dimension, retry_after_ms, remaining_requests, remaining_tokens}.
const allowScript = bucketLib + `
local rcap = tonumber(ARGV[1])
local tcap = tonumber(ARGV[2])
local need = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])

local r = current(KEYS[1], rcap)
local tk = current(KEYS[2], tcap)

if r < 1 then
  return {0, 'requests', math.ceil((1 - r) * 60000 / rcap), math.floor(r), math.floor(tk)}
end
if tk < need then
  return {0, 'tokens', math.ceil((need - tk) * 60000 / tcap), math.floor(r), math.floor(tk)}
end

r = r - 1
tk = tk - need
save(KEYS[1], r, ttl)
save(KEYS[2], tk, ttl)
return {1, '', 0, math.floor(r), math.floor(tk)}
`

// reconcileScript: KEYS = {tokens bucket}; ARGV = {tokens/min, delta, ttl ms}.
// Adds delta (positive refund, negative charge) and clamps to [-cap, cap] so
// a single wildly wrong reservation cannot lock a team out for more than a
// minute or bank more than a minute of credit.
const reconcileScript = bucketLib + `
local cap = tonumber(ARGV[1])
local delta = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])

local lvl = current(KEYS[1], cap) + delta
if lvl > cap then lvl = cap end
if lvl < -cap then lvl = -cap end
save(KEYS[1], lvl, ttl)
return math.floor(lvl)
`
