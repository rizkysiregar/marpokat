package tenant

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestResolverCaches(t *testing.T) {
	calls := 0
	now := time.Unix(1_000_000, 0)
	r := NewResolver(func(_ context.Context, key string) (Tenant, []byte, error) {
		calls++
		if key == "mpk_known" {
			return Tenant{Slug: "narrativ"}, []byte("s"), nil
		}
		return Tenant{}, nil, ErrNotFound
	})
	r.now = func() time.Time { return now }
	ctx := context.Background()

	for range 3 {
		if tn, _, err := r.Resolve(ctx, "mpk_known"); err != nil || tn.Slug != "narrativ" {
			t.Fatalf("Resolve = %+v, %v", tn, err)
		}
	}
	if calls != 1 {
		t.Fatalf("known key loaded %d times, want 1", calls)
	}

	for range 3 {
		if _, _, err := r.Resolve(ctx, "mpk_unknown"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown key: err = %v", err)
		}
	}
	if calls != 2 {
		t.Fatalf("unknown key not negatively cached: %d loads", calls)
	}

	now = now.Add(31 * time.Second)
	_, _, _ = r.Resolve(ctx, "mpk_known")
	_, _, _ = r.Resolve(ctx, "mpk_unknown")
	if calls != 4 {
		t.Fatalf("entries did not expire: %d loads, want 4", calls)
	}
}

func TestResolverDoesNotCacheFailures(t *testing.T) {
	calls := 0
	r := NewResolver(func(context.Context, string) (Tenant, []byte, error) {
		calls++
		return Tenant{}, nil, errors.New("connection refused")
	})
	for range 2 {
		if _, _, err := r.Resolve(context.Background(), "mpk_x"); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want the database error", err)
		}
	}
	if calls != 2 {
		t.Fatalf("database error was cached: %d loads", calls)
	}
}

func TestResolverForgetsATenantAndEveryRefusal(t *testing.T) {
	calls := map[string]int{}
	a, b := uuid.New(), uuid.New()
	off := false
	r := NewResolver(func(_ context.Context, key string) (Tenant, []byte, error) {
		calls[key]++
		switch {
		case key == "mpk_a" && !off:
			return Tenant{ID: a}, []byte("s"), nil
		case key == "mpk_b":
			return Tenant{ID: b}, []byte("s"), nil
		}
		return Tenant{}, nil, ErrNotFound
	})
	ctx := context.Background()
	_, _, _ = r.Resolve(ctx, "mpk_a")
	_, _, _ = r.Resolve(ctx, "mpk_b")

	// a is switched off: forgetting it means the next request asks the database and is refused at once
	off = true
	r.ForgetTenant(a)
	if _, _, err := r.Resolve(ctx, "mpk_a"); !errors.Is(err, ErrNotFound) || calls["mpk_a"] != 2 {
		t.Fatalf("after forget: %v, %d loads", err, calls["mpk_a"])
	}
	_, _, _ = r.Resolve(ctx, "mpk_b")
	if calls["mpk_b"] != 1 {
		t.Fatalf("another tenant was forgotten too: %d loads", calls["mpk_b"])
	}

	// switched on again: the cached refusal goes with the next change, it does not wait for its TTL
	off = false
	r.ForgetTenant(a)
	if tn, _, err := r.Resolve(ctx, "mpk_a"); err != nil || tn.ID != a {
		t.Fatalf("after switching on: %+v %v", tn, err)
	}
}
