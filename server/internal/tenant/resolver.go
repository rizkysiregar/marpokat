package tenant

import (
	"context"
	"errors"
	"sync"
	"time"
)

// CredentialsLoader is Service.Credentials; an interface so the cache can be tested without a database.
type CredentialsLoader func(ctx context.Context, apiKey string) (Tenant, []byte, error)

// Resolver caches api key lookups so authenticating a request does not hit the database every time.
//
// Changes (rotate, disable, config) reach running pods within TTL. Unknown keys are cached too (shorter), so a
// client sending a bad key in a loop does not turn into a database query per request.
type Resolver struct {
	load        CredentialsLoader
	ttl         time.Duration
	negativeTTL time.Duration
	maxEntries  int
	now         func() time.Time

	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	tenant  Tenant
	secret  []byte
	err     error
	expires time.Time
}

func NewResolver(load CredentialsLoader) *Resolver {
	return &Resolver{
		load:        load,
		ttl:         30 * time.Second,
		negativeTTL: 10 * time.Second,
		maxEntries:  10_000,
		now:         time.Now,
		entries:     make(map[string]cacheEntry),
	}
}

// Resolve returns the tenant and api secret for apiKey, or ErrNotFound (unknown or disabled key).
// Database errors are returned as they are and never cached.
func (r *Resolver) Resolve(ctx context.Context, apiKey string) (Tenant, []byte, error) {
	now := r.now()
	r.mu.Lock()
	if e, ok := r.entries[apiKey]; ok && now.Before(e.expires) {
		r.mu.Unlock()
		return e.tenant, e.secret, e.err
	}
	r.mu.Unlock()

	t, secret, err := r.load(ctx, apiKey)
	var entry cacheEntry
	switch {
	case err == nil:
		entry = cacheEntry{tenant: t, secret: secret, expires: now.Add(r.ttl)}
	case errors.Is(err, ErrNotFound):
		entry = cacheEntry{err: ErrNotFound, expires: now.Add(r.negativeTTL)}
	default:
		return Tenant{}, nil, err
	}

	r.mu.Lock()
	if len(r.entries) >= r.maxEntries {
		r.entries = make(map[string]cacheEntry) // crude but bounded; a flood of random keys cannot grow memory
	}
	r.entries[apiKey] = entry
	r.mu.Unlock()
	return entry.tenant, entry.secret, entry.err
}
