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
