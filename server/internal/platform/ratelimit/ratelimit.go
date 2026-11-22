// Package ratelimit is a token bucket in Redis, shared by every pod: a user who sends from two devices on two
// pods is still one user (PLAN M3).
package ratelimit

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/rizkysiregar/marpokat/server/internal/platform/redisx"
)

// The bucket lives in one hash. Time comes from Redis, so pods with different clocks agree.
// KEYS[1] bucket; ARGV: capacity, tokens per second, cost. Returns {allowed (0/1), retry after in ms}.
var script = redis.NewScript(`
local t = redis.call('TIME')
local now = t[1] * 1000 + math.floor(t[2] / 1000)
local capacity = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local cost = tonumber(ARGV[3])
local data = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(data[1])
local ts = tonumber(data[2])
if tokens == nil then
  tokens = capacity
  ts = now
end
tokens = math.min(capacity, tokens + math.max(0, now - ts) * rate / 1000)
local allowed = 0
local retry = 0
if tokens >= cost then
  tokens = tokens - cost
  allowed = 1
else
  retry = math.ceil((cost - tokens) * 1000 / rate)
end
redis.call('HSET', KEYS[1], 'tokens', tostring(tokens), 'ts', now)
redis.call('PEXPIRE', KEYS[1], math.ceil(capacity / rate * 1000) * 2 + 1000)
return {allowed, retry}
`)

type Limiter struct {
	rdb *redisx.Client
	log *slog.Logger
}

func New(rdb *redisx.Client, log *slog.Logger) *Limiter {
	return &Limiter{rdb: rdb, log: log.With("subsystem", "ratelimit")}
}

// Allow takes one token from the bucket named by keyParts (prefixed with the Redis key prefix). capacity is the
// burst, perSecond the sustained rate. When it says no, retryAfter is how long until a token is available.
// If Redis fails the request is allowed: a broken limiter must not stop chat.
func (l *Limiter) Allow(ctx context.Context, capacity int, perSecond float64, keyParts ...string) (allowed bool, retryAfter time.Duration) {
	key := l.rdb.Key(append([]string{"rl"}, keyParts...)...)
	res, err := script.Run(ctx, l.rdb, []string{key}, capacity, perSecond, 1).Int64Slice()
	if err != nil || len(res) != 2 {
		l.log.Warn("rate limiter unavailable, allowing the request", "error", err, "key", key)
		return true, 0
	}
	return res[0] == 1, time.Duration(res[1]) * time.Millisecond
}
