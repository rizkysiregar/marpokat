package push

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/platform/redisx"
)

// Dedup remembers which devices were already sent a push for an event, so that an event that arrives twice (the
// relay is at-least-once) or is retried after a partial failure sends nothing twice. It is per device: when one of a
// user's phones could not be reached, the retry must not ring the others again.
type Dedup interface {
	Done(ctx context.Context, eventID string, userID uuid.UUID, deviceToken string) (bool, error)
	MarkDone(ctx context.Context, eventID string, userID uuid.UUID, deviceToken string) error
}

// RedisDedup keeps the marks in Redis for a day, far longer than any retry lasts.
type RedisDedup struct{ rdb *redisx.Client }

func NewRedisDedup(rdb *redisx.Client) *RedisDedup { return &RedisDedup{rdb: rdb} }

const dedupTTL = 24 * time.Hour

// key names the device by a hash of its token: tokens are long and a key is not the place to keep them.
func (d *RedisDedup) key(eventID string, userID uuid.UUID, token string) string {
	sum := sha256.Sum256([]byte(token))
	return d.rdb.Key("push", "done", eventID, userID.String(), hex.EncodeToString(sum[:8]))
}

func (d *RedisDedup) Done(ctx context.Context, eventID string, userID uuid.UUID, token string) (bool, error) {
	n, err := d.rdb.Exists(ctx, d.key(eventID, userID, token)).Result()
	return n > 0, err
}

func (d *RedisDedup) MarkDone(ctx context.Context, eventID string, userID uuid.UUID, token string) error {
	return d.rdb.Set(ctx, d.key(eventID, userID, token), "1", dedupTTL).Err()
}
