package tenant

import (
	"context"
	"errors"
	"testing"
	"time"
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
