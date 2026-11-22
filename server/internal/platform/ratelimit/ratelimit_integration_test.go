//go:build integration

package ratelimit

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/platform/config"
	"github.com/rizkysiregar/marpokat/server/internal/platform/redisx"
	"github.com/rizkysiregar/marpokat/server/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

func newLimiter(t *testing.T) *Limiter {
	t.Helper()
	rdb, err := redisx.Open(context.Background(), testdb.Redis(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return New(rdb, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestTokenBucket(t *testing.T) {
	l := newLimiter(t)
	ctx := context.Background()
	scope := uuid.NewString()

	for i := range 3 { // the burst
		if ok, _ := l.Allow(ctx, 3, 1, "t", scope); !ok {
			t.Fatalf("request %d within the burst was refused", i+1)
		}
	}
	ok, wait := l.Allow(ctx, 3, 1, "t", scope)
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("over the burst: allowed=%v retry after %v, want refused with at most 1s", ok, wait)
	}
	if ok, _ := l.Allow(ctx, 3, 1, "t", uuid.NewString()); !ok { // buckets are independent
		t.Fatal("another bucket was affected")
	}
	time.Sleep(wait + 50*time.Millisecond) // one token refills
	if ok, _ := l.Allow(ctx, 3, 1, "t", scope); !ok {
		t.Fatal("still refused after the advised wait")
	}
	if ok, _ := l.Allow(ctx, 3, 1, "t", scope); ok {
		t.Fatal("only one token should have come back")
	}
}

func TestTokenBucketIsExactUnderConcurrency(t *testing.T) {
	l := newLimiter(t)
	scope := uuid.NewString()
	var allowed atomic.Int32
	done := make(chan struct{})
	for range 60 { // from many goroutines (and, in production, pods) at once
		go func() {
			if ok, _ := l.Allow(context.Background(), 10, 0.001, "c", scope); ok {
				allowed.Add(1)
			}
			done <- struct{}{}
		}()
	}
	for range 60 {
		<-done
	}
	if got := allowed.Load(); got != 10 {
		t.Fatalf("%d of 60 simultaneous requests were allowed, want exactly the burst of 10", got)
	}
}

func TestLimiterFailsOpen(t *testing.T) {
	// a broken limiter must not stop chat
	dead := redisx.NewUnconnected(config.Redis{Host: "127.0.0.1", Port: 1, KeyPrefix: "x:"})
	l := New(dead, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if ok, _ := l.Allow(ctx, 1, 1, "x"); !ok {
		t.Fatal("a Redis failure refused the request")
	}
}
