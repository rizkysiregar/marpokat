package ratelimit

import (
	"context"
	"log/slog"
	"time"

	"github.com/rizkysiregar/marpokat/server/internal/platform/redisx"
)

// Guard slows down someone who keeps presenting credentials that do not work (guessing api keys or forging tokens).
// Each failure from an address counts in a window that starts at its first failure; at max failures the address is
// refused until the window ends. Only failures count: a person whose calls work is never limited by it.
//
// If Redis fails the guard lets everything through: a broken guard must not stop chat.
type Guard struct {
	rdb    *redisx.Client
	log    *slog.Logger
	max    int64
	window time.Duration
}

func NewGuard(rdb *redisx.Client, log *slog.Logger, max int, window time.Duration) *Guard {
	return &Guard{rdb: rdb, log: log.With("subsystem", "authguard"), max: int64(max), window: window}
}

// Blocked says whether the address has used up its failures, and for how much longer it is refused.
func (g *Guard) Blocked(ctx context.Context, ip string) (bool, time.Duration) {
	key := g.rdb.Key("authfail", ip)
	n, err := g.rdb.Get(ctx, key).Int64()
	if err != nil { // no failures yet (redis.Nil) or Redis down: not blocked
		return false, 0
	}
	if n < g.max {
		return false, 0
	}
	ttl, err := g.rdb.PTTL(ctx, key).Result()
	if err != nil || ttl <= 0 {
		return false, 0
	}
	return true, ttl
}

// Failed records one failure of the address.
func (g *Guard) Failed(ctx context.Context, ip string) {
	key := g.rdb.Key("authfail", ip)
	pipe := g.rdb.Pipeline()
	pipe.Incr(ctx, key)
	pipe.ExpireNX(ctx, key, g.window) // the window starts at the first failure and is not pushed back by the next ones
	if _, err := pipe.Exec(ctx); err != nil {
		g.log.Warn("auth failure not recorded", "error", err)
	}
}
