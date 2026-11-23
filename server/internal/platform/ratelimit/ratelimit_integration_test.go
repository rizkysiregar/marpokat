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

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newClient(t *testing.T) *redisx.Client {
	t.Helper()
	rdb, err := redisx.Open(context.Background(), testdb.Redis(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
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

func TestGuardRefusesAnAddressAfterTooManyFailuresUntilTheWindowEnds(t *testing.T) {
	rdb := newClient(t)
	g := NewGuard(rdb, discard(), 3, 1500*time.Millisecond)
	ctx := context.Background()
	ip := "203.0.113." + uuid.NewString()[:3]

	for i := 0; i < 3; i++ {
		if blocked, _ := g.Blocked(ctx, ip); blocked {
			t.Fatalf("blocked after only %d failures", i)
		}
		g.Failed(ctx, ip)
	}
	blocked, wait := g.Blocked(ctx, ip)
	if !blocked || wait <= 0 || wait > 1500*time.Millisecond {
		t.Fatalf("blocked=%v wait=%v after 3 failures of 3", blocked, wait)
	}
	// another address is not affected
	if other, _ := g.Blocked(ctx, "198.51.100.1"); other {
		t.Fatal("another address was blocked")
	}
	// more failures do not push the end of the window back
	time.Sleep(900 * time.Millisecond)
	g.Failed(ctx, ip)
	if _, again := g.Blocked(ctx, ip); again >= wait {
		t.Fatalf("the window moved: %v then %v", wait, again)
	}
	time.Sleep(900 * time.Millisecond)
	if stillBlocked, _ := g.Blocked(ctx, ip); stillBlocked {
		t.Fatal("still blocked after the window")
	}
}

func TestGuardLetsEverythingThroughWhenRedisIsDown(t *testing.T) {
	g := NewGuard(redisx.NewUnconnected(config.Redis{Host: "127.0.0.1", Port: 1, KeyPrefix: "x:"}), discard(), 1, time.Minute)
	g.Failed(context.Background(), "203.0.113.9")
	if blocked, _ := g.Blocked(context.Background(), "203.0.113.9"); blocked {
		t.Fatal("a broken guard stopped a request")
	}
}
