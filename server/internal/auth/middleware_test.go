package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
)

type fakeTenants map[string]tenant.Tenant

func (f fakeTenants) Resolve(_ context.Context, key string) (tenant.Tenant, []byte, error) {
	if key == "mpk_down" {
		return tenant.Tenant{}, nil, errors.New("db down")
	}
	t, ok := f[key]
	if !ok {
		return tenant.Tenant{}, nil, tenant.ErrNotFound
	}
	return t, secret, nil
}

var (
	tenantA = tenant.Tenant{ID: uuid.MustParse("00000000-0000-7000-8000-00000000000a"), Slug: "a"}
	userID  = uuid.MustParse("00000000-0000-7000-8000-000000000001")
)

func newMW() *Middleware {
	users := func(_ context.Context, tid uuid.UUID, ext string) (uuid.UUID, error) {
		if tid == tenantA.ID && ext == "u_1" {
			return userID, nil
		}
		return uuid.Nil, ErrUnknownUser
	}
	m := NewMiddleware(fakeTenants{"mpk_a": tenantA}, users, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.now = func() time.Time { return now }
	return m
}

func call(h http.Handler, apiKey, token string) (int, string) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if apiKey != "" {
		req.Header.Set(HeaderAPIKey, apiKey)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	httpx.WithRequestID()(h).ServeHTTP(rec, req)
	var body httpx.ErrorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body.Error.Code
}

func TestUserMiddleware(t *testing.T) {
	m := newMW()
	var seen Principal
	h := m.User(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = FromContext(r.Context()) }))

	valid, _ := SignUserToken(secret, "u_1", time.Hour, now)
	unknown, _ := SignUserToken(secret, "u_404", time.Hour, now)
	expired, _ := SignUserToken(secret, "u_1", time.Minute, now.Add(-time.Hour))
	server, _ := SignServerToken(secret, time.Minute, now)

	cases := []struct {
		name, key, token string
		status           int
		code             string
	}{
		{"valid", "mpk_a", valid, 200, ""},
		{"no api key", "", valid, 401, httpx.CodeUnauthenticated},
		{"no token", "mpk_a", "", 401, httpx.CodeUnauthenticated},
		{"unknown api key", "mpk_x", valid, 401, httpx.CodeUnauthenticated},
		{"unknown user", "mpk_a", unknown, 401, httpx.CodeUnauthenticated},
		{"expired", "mpk_a", expired, 401, httpx.CodeTokenExpired},
		{"server token", "mpk_a", server, 401, httpx.CodeUnauthenticated},
		{"database down", "mpk_down", valid, 503, httpx.CodeUnavailable},
	}
	for _, c := range cases {
		seen = Principal{}
		status, code := call(h, c.key, c.token)
		if status != c.status || code != c.code {
			t.Errorf("%s: got %d %q, want %d %q", c.name, status, code, c.status, c.code)
		}
		if c.status == 200 && (seen.UserID != userID || seen.ExternalID != "u_1" || seen.Tenant.ID != tenantA.ID) {
			t.Errorf("%s: principal = %+v", c.name, seen)
		}
	}
}

func TestServerMiddleware(t *testing.T) {
	m := newMW()
	var seen Principal
	h := m.Server(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = FromContext(r.Context()) }))

	server, _ := SignServerToken(secret, time.Minute, now)
	user, _ := SignUserToken(secret, "u_1", time.Hour, now)

	if status, _ := call(h, "mpk_a", server); status != 200 || !seen.IsServer() || seen.Tenant.ID != tenantA.ID {
		t.Fatalf("server token: status %d, principal %+v", status, seen)
	}
	if status, code := call(h, "mpk_a", user); status != 401 || code != httpx.CodeUnauthenticated {
		t.Fatalf("user token on server route: %d %q", status, code)
	}
}

func TestBearer(t *testing.T) {
	for in, want := range map[string]string{"Bearer abc": "abc", "bearer  abc ": "abc", "Basic abc": "", "Bearer": "", "": ""} {
		got, ok := bearer(in)
		if got != want || ok != (want != "") {
			t.Errorf("bearer(%q) = %q, %v", in, got, ok)
		}
	}
}

type scriptedLimiter struct {
	allow bool
	keys  [][]string
}

func (s *scriptedLimiter) Allow(_ context.Context, capacity int, _ float64, parts ...string) (bool, time.Duration) {
	s.keys = append(s.keys, parts)
	return s.allow, 1500 * time.Millisecond
}

func TestRequestLimitAnswers429WithRetryAfterAndNeverReachesTheHandler(t *testing.T) {
	m := newMW()
	lim := &scriptedLimiter{allow: false}
	m.SetRequestLimiter(lim)
	reached := false
	h := m.User(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	valid, _ := SignUserToken(secret, "u_1", time.Hour, now)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderAPIKey, "mpk_a")
	req.Header.Set("Authorization", "Bearer "+valid)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "2" || reached {
		t.Fatalf("status %d, Retry-After %q, handler reached %v", rec.Code, rec.Header().Get("Retry-After"), reached)
	}
	// one bucket per user of a tenant: other tenants and other users never share it
	if len(lim.keys) != 1 || lim.keys[0][0] != "req" || lim.keys[0][1] != tenantA.ID.String() || lim.keys[0][2] != userID.String() {
		t.Fatalf("bucket key = %v", lim.keys)
	}
}

func TestRequestLimitLetsRequestsThroughAndServerCallsShareOneBucketPerTenant(t *testing.T) {
	m := newMW()
	lim := &scriptedLimiter{allow: true}
	m.SetRequestLimiter(lim)
	server, _ := SignServerToken(secret, time.Minute, now)
	if status, _ := call(m.Server(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})), "mpk_a", server); status != 200 {
		t.Fatalf("status %d", status)
	}
	if len(lim.keys) != 1 || lim.keys[0][2] != "server" {
		t.Fatalf("bucket key = %v", lim.keys)
	}
}

func TestWithoutALimiterNothingIsLimited(t *testing.T) {
	m := newMW()
	valid, _ := SignUserToken(secret, "u_1", time.Hour, now)
	if status, _ := call(m.User(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})), "mpk_a", valid); status != 200 {
		t.Fatalf("status %d", status)
	}
}

type scriptedGuard struct {
	blockedFor time.Duration
	failures   []string
	checked    []string
}

func (g *scriptedGuard) Blocked(_ context.Context, ip string) (bool, time.Duration) {
	g.checked = append(g.checked, ip)
	return g.blockedFor > 0, g.blockedFor
}

func (g *scriptedGuard) Failed(_ context.Context, ip string) { g.failures = append(g.failures, ip) }

func callFrom(h http.Handler, ip, apiKey, token string) int {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("CF-Connecting-IP", ip)
	if apiKey != "" {
		req.Header.Set(HeaderAPIKey, apiKey)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	httpx.WithRequestID()(h).ServeHTTP(rec, req)
	return rec.Code
}

func TestFailureGuardCountsGuessesButNotExpiredTokensOrUnknownUsers(t *testing.T) {
	m := newMW()
	g := &scriptedGuard{}
	m.SetFailureGuard(g)
	h := m.User(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	valid, _ := SignUserToken(secret, "u_1", time.Hour, now)
	expired, _ := SignUserToken(secret, "u_1", time.Minute, now.Add(-time.Hour))
	unknownUser, _ := SignUserToken(secret, "u_404", time.Hour, now)
	forged, _ := SignUserToken([]byte("another-secret-another-secret-another-secret"), "u_1", time.Hour, now)

	for name, c := range map[string]struct {
		key, token string
		counts     bool
	}{
		"no api key":      {"", valid, true},
		"no token":        {"mpk_a", "", true},
		"unknown api key": {"mpk_x", valid, true},
		"forged token":    {"mpk_a", forged, true},
		"expired token":   {"mpk_a", expired, false}, // clients refresh these all the time
		"unknown user":    {"mpk_a", unknownUser, false},
		"valid":           {"mpk_a", valid, false},
	} {
		g.failures = nil
		callFrom(h, "203.0.113.7", c.key, c.token)
		if got := len(g.failures) == 1 && g.failures[0] == "203.0.113.7"; got != c.counts {
			t.Errorf("%s: counted=%v (%v), want %v", name, got, g.failures, c.counts)
		}
	}
}

func TestAnAddressThatUsedUpItsFailuresIsRefusedBeforeAnythingIsLookedAt(t *testing.T) {
	m := newMW()
	g := &scriptedGuard{blockedFor: 7 * time.Minute}
	m.SetFailureGuard(g)
	reached := false
	h := m.User(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	valid, _ := SignUserToken(secret, "u_1", time.Hour, now)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.50")
	req.Header.Set(HeaderAPIKey, "mpk_a")
	req.Header.Set("Authorization", "Bearer "+valid)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "420" || reached {
		t.Fatalf("status %d, Retry-After %q, handler reached %v: even a right key is refused from a blocked address", rec.Code, rec.Header().Get("Retry-After"), reached)
	}
	if len(g.checked) != 1 || g.checked[0] != "203.0.113.50" || len(g.failures) != 0 {
		t.Fatalf("checked %v, failures %v: being refused is not another failure", g.checked, g.failures)
	}
}
