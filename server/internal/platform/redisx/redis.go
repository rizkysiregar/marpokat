// Package redisx opens the Redis client and owns the key prefix.
package redisx

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/rizkysiregar/marpokat/server/internal/platform/config"
)

// Client wraps go-redis with the configured prefix. Code must build every key and Pub/Sub channel with Key,
// never with a literal string: Redis is shared with other apps (PLAN D20).
type Client struct {
	*redis.Client
	prefix string
}

func Open(ctx context.Context, c config.Redis) (*Client, error) {
	rc := redis.NewClient(&redis.Options{
		Addr:         c.Addr(),
		Password:     c.Password,
		DB:           c.DB,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rc.Ping(pingCtx).Err(); err != nil {
		_ = rc.Close()
		return nil, fmt.Errorf("redis: ping %s: %w", c.Addr(), err)
	}
	return &Client{Client: rc, prefix: c.KeyPrefix}, nil
}

// Key joins the prefix and the given parts with ":", e.g. Key("ch", tenantID, channelID).
func (c *Client) Key(parts ...string) string { return BuildKey(c.prefix, parts...) }

// BuildKey is Key without a client, for tests and for code that only holds the prefix.
func BuildKey(prefix string, parts ...string) string {
	n := len(prefix)
	for _, p := range parts {
		n += len(p) + 1
	}
	b := make([]byte, 0, n)
	b = append(b, prefix...)
	for i, p := range parts {
		if i > 0 {
			b = append(b, ':')
		}
		b = append(b, p...)
	}
	return string(b)
}
