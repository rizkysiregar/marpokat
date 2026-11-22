package realtime

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/rizkysiregar/marpokat/server/internal/platform/redisx"
)

// Presence keeps who is online in Redis, one sorted set per user: member = connection id, score = the moment the
// connection stops counting unless it pings again. Whoever has a live score is online, on whichever pod.
type Presence struct {
	rdb *redisx.Client
	ttl time.Duration
}

func NewPresence(rdb *redisx.Client, ttl time.Duration) *Presence {
	return &Presence{rdb: rdb, ttl: ttl}
}

func (p *Presence) key(tenantID, userID uuid.UUID) string {
	return p.rdb.Key("presence", tenantID.String(), userID.String())
}

func (p *Presence) lastSeenKey(tenantID, userID uuid.UUID) string {
	return p.rdb.Key("lastseen", tenantID.String(), userID.String())
}

// Redis time is used for every score, so pods with different clocks agree.
const nowMillis = `local t = redis.call('TIME') local now = t[1] * 1000 + math.floor(t[2] / 1000)`

// KEYS[1] set; ARGV: connection id, ttl ms, max live connections. Returns {-1, live} when the user is at the
// maximum, else {live before, live after}.
var connectScript = redis.NewScript(nowMillis + `
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
local live = redis.call('ZCARD', KEYS[1])
if live >= tonumber(ARGV[3]) and redis.call('ZSCORE', KEYS[1], ARGV[1]) == false then
  return {-1, live}
end
redis.call('ZADD', KEYS[1], now + tonumber(ARGV[2]), ARGV[1])
redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[2]) * 2)
return {live, redis.call('ZCARD', KEYS[1])}
`)

// KEYS[1] set, KEYS[2] last seen; ARGV: connection id, last seen retention ms. Returns {live left, now ms}.
var disconnectScript = redis.NewScript(nowMillis + `
redis.call('ZREM', KEYS[1], ARGV[1])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
local live = redis.call('ZCARD', KEYS[1])
if live == 0 then
  redis.call('SET', KEYS[2], now, 'PX', tonumber(ARGV[2]))
end
return {live, now}
`)

// KEYS[1] set; ARGV: connection id, ttl ms.
var touchScript = redis.NewScript(nowMillis + `
redis.call('ZADD', KEYS[1], now + tonumber(ARGV[2]), ARGV[1])
redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[2]) * 2)
return 1
`)

const lastSeenRetention = 30 * 24 * time.Hour

// Connect registers a connection. first says the user was offline before it. ok is false when the user already has
// max live connections.
func (p *Presence) Connect(ctx context.Context, tenantID, userID uuid.UUID, conn string, max int) (first, ok bool, err error) {
	res, err := connectScript.Run(ctx, p.rdb, []string{p.key(tenantID, userID)}, conn, p.ttl.Milliseconds(), max).Int64Slice()
	if err != nil || len(res) != 2 {
		return false, false, fmt.Errorf("presence: connect: %w", err)
	}
	if res[0] < 0 {
		return false, false, nil
	}
	return res[0] == 0, true, nil
}

// Touch keeps a connection online; called on every ping.
func (p *Presence) Touch(ctx context.Context, tenantID, userID uuid.UUID, conn string) error {
	if err := touchScript.Run(ctx, p.rdb, []string{p.key(tenantID, userID)}, conn, p.ttl.Milliseconds()).Err(); err != nil {
		return fmt.Errorf("presence: touch: %w", err)
	}
	return nil
}

// Disconnect removes a connection. last says it was the user's last one, and lastSeen is then the moment they left.
func (p *Presence) Disconnect(ctx context.Context, tenantID, userID uuid.UUID, conn string) (last bool, lastSeen time.Time, err error) {
	res, err := disconnectScript.Run(ctx, p.rdb, []string{p.key(tenantID, userID), p.lastSeenKey(tenantID, userID)},
		conn, lastSeenRetention.Milliseconds()).Int64Slice()
	if err != nil || len(res) != 2 {
		return false, time.Time{}, fmt.Errorf("presence: disconnect: %w", err)
	}
	return res[0] == 0, time.UnixMilli(res[1]).UTC(), nil
}

// Status is whether a user is online, and when they were last seen if not.
type Status struct {
	Online   bool
	LastSeen *time.Time
}

// Of returns the status of the given users.
func (p *Presence) Of(ctx context.Context, tenantID uuid.UUID, userIDs []uuid.UUID) (map[uuid.UUID]Status, error) {
	out := make(map[uuid.UUID]Status, len(userIDs))
	if len(userIDs) == 0 {
		return out, nil
	}
	now := strconv.FormatInt(time.Now().UnixMilli(), 10)
	pipe := p.rdb.Pipeline()
	live := make([]*redis.IntCmd, len(userIDs))
	seen := make([]*redis.StringCmd, len(userIDs))
	for i, id := range userIDs {
		live[i] = pipe.ZCount(ctx, p.key(tenantID, id), now, "+inf")
		seen[i] = pipe.Get(ctx, p.lastSeenKey(tenantID, id))
	}
	// Exec reports the first failing command, and a user who never left has no last seen key (redis.Nil), which is
	// normal. A failure of the counts is what matters.
	_, _ = pipe.Exec(ctx)
	for i := range userIDs {
		if err := live[i].Err(); err != nil {
			return nil, fmt.Errorf("presence: status: %w", err)
		}
	}
	for i, id := range userIDs {
		st := Status{Online: live[i].Val() > 0}
		if !st.Online {
			if ms, err := strconv.ParseInt(seen[i].Val(), 10, 64); err == nil {
				t := time.UnixMilli(ms).UTC()
				st.LastSeen = &t
			}
		}
		out[id] = st
	}
	return out, nil
}
