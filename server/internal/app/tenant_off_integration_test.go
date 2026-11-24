//go:build integration

package app

import (
	"context"
	"testing"
	"time"

	"github.com/rizkysiregar/marpokat/server/internal/realtime"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
)

// Switching a tenant off (the website's Switch off and Delete) ends its open WebSockets on every pod at once, and
// every pod refuses its keys at once instead of after the 30 second cache. Other tenants are not touched.
func TestSwitchingATenantOffClosesItsSocketsEverywhere(t *testing.T) {
	e := newRealtimeEnv(t, 2, nil)
	doomed := e.createTenant(tenant.Config{})
	other := e.createTenant(tenant.Config{})
	e.seed(doomed, "alice", "bob")
	e.seed(other, "carol")
	alice := e.connect(0, doomed, "alice")
	bob := e.connect(1, doomed, "bob")
	carol := e.connect(0, other, "carol")
	// both pods have the doomed tenant's key in their cache now
	for i := range e.pods {
		if r := e.on(i).asUser(doomed, "alice", "GET", "/v1/me", nil); r.status != 200 {
			t.Fatalf("pod %d before: %d", i, r.status)
		}
	}

	tenants := e.tenants.WithNotifier(realtime.NewTenantNotifier(e.pods[0].rdb))
	start := time.Now()
	if err := tenants.SetDisabled(context.Background(), doomed.Slug, true); err != nil {
		t.Fatal(err)
	}
	for name, w := range map[string]*wsConn{"alice (pod 0)": alice, "bob (pod 1)": bob} {
		if code := w.waitClose(2 * time.Second); code != realtime.CloseAuthFailed {
			t.Fatalf("%s closed with %d, want %d", name, code, realtime.CloseAuthFailed)
		}
	}
	t.Logf("sockets of the switched off tenant closed in %s", time.Since(start).Round(time.Millisecond))

	// keys refused on both pods right away, not after the cache TTL
	for i := range e.pods {
		if r := e.on(i).asUser(doomed, "alice", "GET", "/v1/me", nil); r.status != 401 {
			t.Fatalf("pod %d still accepts the key: %d", i, r.status)
		}
	}
	// the other tenant's socket and API keep working
	carol.none("error", 300*time.Millisecond)
	carol.send("ping", "p1", nil)
	carol.next("pong")
	if r := e.on(1).asUser(other, "carol", "GET", "/v1/me", nil); r.status != 200 {
		t.Fatalf("the other tenant was refused: %d", r.status)
	}

	// switched on again: accepted at once (the refusal was not kept for its TTL either)
	if err := tenants.SetDisabled(context.Background(), doomed.Slug, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for i := range e.pods {
		for {
			r := e.on(i).asUser(doomed, "alice", "GET", "/v1/me", nil)
			if r.status == 200 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("pod %d still refuses after switching on: %d", i, r.status)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	e.connect(1, doomed, "alice")
}

// A suspension by the platform admin closes the sockets the same way, and its keys are refused at once.
func TestSuspendingATenantClosesItsSockets(t *testing.T) {
	e := newRealtimeEnv(t, 2, nil)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice")
	alice := e.connect(1, tt, "alice")
	tenants := e.tenants.WithNotifier(realtime.NewTenantNotifier(e.pods[0].rdb))
	if err := tenants.SetSuspended(context.Background(), tt.ID, true, "spam"); err != nil {
		t.Fatal(err)
	}
	if code := alice.waitClose(2 * time.Second); code != realtime.CloseAuthFailed {
		t.Fatalf("closed with %d", code)
	}
	if r := e.on(1).asUser(tt, "alice", "GET", "/v1/me", nil); r.status != 401 {
		t.Fatalf("key accepted while suspended: %d", r.status)
	}
	if err := tenants.SetSuspended(context.Background(), tt.ID, false, ""); err != nil {
		t.Fatal(err)
	}
	e.connect(0, tt, "alice")
}
