//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/auth"
	"github.com/rizkysiregar/marpokat/server/internal/media"
	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/platform/secretbox"
	"github.com/rizkysiregar/marpokat/server/internal/store"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
	"github.com/rizkysiregar/marpokat/server/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

type env struct {
	t       *testing.T
	srv     *httptest.Server
	tenants *tenant.Service
	pods    []*pod // set by newRealtimeEnv; srv is the first
	files   *media.Storage
}

func newEnv(t *testing.T) *env {
	t.Helper()
	box, err := secretbox.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	pool := testdb.Pool(t)
	// errors of the server under test are what explains a 500, so they are shown
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	// a new router per test means a new tenant cache, so disable/rotate are visible at once
	h := httpx.Chain(PublicRoutes(Deps{Info: Info{Version: "test"}, Pool: pool, Box: box, Log: log}), httpx.WithRequestID())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, tenants: tenant.NewService(store.New(pool), box)}
}

type testTenant struct {
	tenant.Tenant
	secret []byte
}

func (e *env) createTenant(cfg tenant.Config) testTenant {
	e.t.Helper()
	slug := "t-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	tn, secret, err := e.tenants.Create(context.Background(), slug, "Test "+slug, cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	return testTenant{Tenant: tn, secret: []byte(secret)}
}

func (tt testTenant) serverToken(t *testing.T) string {
	t.Helper()
	tok, err := auth.SignServerToken(tt.secret, time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (tt testTenant) userToken(t *testing.T, ext string) string {
	t.Helper()
	tok, err := auth.SignUserToken(tt.secret, ext, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

type response struct {
	status int
	body   map[string]any
}

func (r response) errCode() string {
	e, _ := r.body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

// request is do without the *testing.T calls, so goroutines can use it.
func (e *env) request(method, path, apiKey, token string, body any) (response, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, e.srv.URL+path, rd)
	if err != nil {
		return response{}, err
	}
	if apiKey != "" {
		req.Header.Set(auth.HeaderAPIKey, apiKey)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return response{}, err
	}
	defer func() { _ = res.Body.Close() }()
	out := response{status: res.StatusCode, body: map[string]any{}}
	raw, _ := io.ReadAll(res.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out.body)
	}
	return out, nil
}

func (e *env) do(method, path, apiKey, token string, body any) response {
	e.t.Helper()
	r, err := e.request(method, path, apiKey, token, body)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func TestUserLifecycle(t *testing.T) {
	e := newEnv(t)
	a := e.createTenant(tenant.Config{})

	r := e.do("PUT", "/v1/server/users/u_1", a.APIKey, a.serverToken(t),
		map[string]any{"name": "Rizky", "avatar_url": "https://cdn.example.com/r.png", "metadata": map[string]any{"plan": "pro"}})
	if r.status != 200 || r.body["external_id"] != "u_1" || r.body["name"] != "Rizky" || r.body["deleted"] != false {
		t.Fatalf("upsert: %d %v", r.status, r.body)
	}
	firstID := r.body["id"]
	if md, _ := r.body["metadata"].(map[string]any); md["plan"] != "pro" {
		t.Fatalf("metadata did not round-trip: %v", r.body["metadata"])
	}

	r = e.do("GET", "/v1/me", a.APIKey, a.userToken(t, "u_1"), nil)
	if r.status != 200 || r.body["name"] != "Rizky" || r.body["id"] != firstID {
		t.Fatalf("me: %d %v", r.status, r.body)
	}

	r = e.do("PUT", "/v1/server/users/u_1", a.APIKey, a.serverToken(t), map[string]any{"name": "Rizky S"})
	if r.status != 200 || r.body["name"] != "Rizky S" || r.body["id"] != firstID || r.body["avatar_url"] != nil {
		t.Fatalf("update keeps id and replaces fields: %d %v", r.status, r.body)
	}

	r = e.do("PUT", "/v1/server/users/u_1", a.APIKey, a.serverToken(t), map[string]any{"avatar_url": "ftp://x", "metadata": []int{1}})
	if r.status != 400 || r.errCode() != httpx.CodeValidation {
		t.Fatalf("invalid upsert: %d %v", r.status, r.body)
	}

	for range 2 { // idempotent
		if r = e.do("DELETE", "/v1/server/users/u_1", a.APIKey, a.serverToken(t), nil); r.status != 204 {
			t.Fatalf("delete: %d %v", r.status, r.body)
		}
	}
	r = e.do("GET", "/v1/me", a.APIKey, a.userToken(t, "u_1"), nil)
	if r.status != 401 || r.errCode() != httpx.CodeUnauthenticated {
		t.Fatalf("deleted user still authenticates: %d %v", r.status, r.body)
	}

	r = e.do("PUT", "/v1/server/users/u_1", a.APIKey, a.serverToken(t), map[string]any{"name": "Back"})
	if r.status != 200 || r.body["id"] != firstID || r.body["deleted"] != false {
		t.Fatalf("re-upsert after delete: %d %v", r.status, r.body)
	}
}

func TestTenantIsolation(t *testing.T) {
	e := newEnv(t)
	a := e.createTenant(tenant.Config{})
	b := e.createTenant(tenant.Config{})

	// the same external id in two tenants is two different users
	ra := e.do("PUT", "/v1/server/users/shared", a.APIKey, a.serverToken(t), map[string]any{"name": "In A"})
	rb := e.do("PUT", "/v1/server/users/shared", b.APIKey, b.serverToken(t), map[string]any{"name": "In B"})
	if ra.status != 200 || rb.status != 200 || ra.body["id"] == rb.body["id"] {
		t.Fatalf("upserts: %v / %v", ra.body, rb.body)
	}

	if r := e.do("GET", "/v1/me", a.APIKey, a.userToken(t, "shared"), nil); r.body["name"] != "In A" {
		t.Fatalf("tenant A sees %v", r.body)
	}
	if r := e.do("GET", "/v1/me", b.APIKey, b.userToken(t, "shared"), nil); r.body["name"] != "In B" {
		t.Fatalf("tenant B sees %v", r.body)
	}

	// a token signed by B's secret does not work with A's api key, and the other way round
	if r := e.do("GET", "/v1/me", a.APIKey, b.userToken(t, "shared"), nil); r.status != 401 {
		t.Fatalf("B's user token accepted with A's key: %d", r.status)
	}
	if r := e.do("DELETE", "/v1/server/users/shared", a.APIKey, b.serverToken(t), nil); r.status != 401 {
		t.Fatalf("B's server token accepted with A's key: %d", r.status)
	}

	// B deleting its user leaves A's user alone
	if r := e.do("DELETE", "/v1/server/users/shared", b.APIKey, b.serverToken(t), nil); r.status != 204 {
		t.Fatalf("delete in B: %d", r.status)
	}
	if r := e.do("GET", "/v1/me", a.APIKey, a.userToken(t, "shared"), nil); r.status != 200 || r.body["name"] != "In A" {
		t.Fatalf("A's user affected by B's delete: %d %v", r.status, r.body)
	}
	if r := e.do("GET", "/v1/me", b.APIKey, b.userToken(t, "shared"), nil); r.status != 401 {
		t.Fatalf("B's deleted user still authenticates: %d", r.status)
	}
}

func TestAuthenticationFailures(t *testing.T) {
	e := newEnv(t)
	a := e.createTenant(tenant.Config{})
	e.do("PUT", "/v1/server/users/u_1", a.APIKey, a.serverToken(t), map[string]any{"name": "U"})

	expired, _ := auth.SignUserToken(a.secret, "u_1", time.Minute, time.Now().Add(-time.Hour))
	cases := []struct {
		name, method, path, key, token, code string
	}{
		{"user route with server token", "GET", "/v1/me", a.APIKey, a.serverToken(t), httpx.CodeUnauthenticated},
		{"server route with user token", "PUT", "/v1/server/users/u_2", a.APIKey, a.userToken(t, "u_1"), httpx.CodeUnauthenticated},
		{"expired token", "GET", "/v1/me", a.APIKey, expired, httpx.CodeTokenExpired},
		{"unknown api key", "GET", "/v1/me", "mpk_00000000000000000000000000000000", a.userToken(t, "u_1"), httpx.CodeUnauthenticated},
		{"no credentials", "GET", "/v1/me", "", "", httpx.CodeUnauthenticated},
		{"unknown user", "GET", "/v1/me", a.APIKey, a.userToken(t, "nobody"), httpx.CodeUnauthenticated},
	}
	for _, c := range cases {
		r := e.do(c.method, c.path, c.key, c.token, map[string]any{"name": "x"})
		if r.status != 401 || r.errCode() != c.code {
			t.Errorf("%s: %d %q, want 401 %q", c.name, r.status, r.errCode(), c.code)
		}
	}
}

func TestDisabledAndRotatedTenant(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	a := e.createTenant(tenant.Config{})
	e.do("PUT", "/v1/server/users/u_1", a.APIKey, a.serverToken(t), map[string]any{"name": "U"})
	oldToken := a.userToken(t, "u_1")

	newSecret, err := e.tenants.RotateSecret(ctx, a.Slug)
	if err != nil {
		t.Fatal(err)
	}
	e2 := newEnv(t) // fresh cache
	if r := e2.do("GET", "/v1/me", a.APIKey, oldToken, nil); r.status != 401 {
		t.Fatalf("token signed with the old secret accepted after rotation: %d", r.status)
	}
	rotated := testTenant{Tenant: a.Tenant, secret: []byte(newSecret)}
	if r := e2.do("GET", "/v1/me", a.APIKey, rotated.userToken(t, "u_1"), nil); r.status != 200 {
		t.Fatalf("token signed with the new secret rejected: %d %v", r.status, r.body)
	}

	if err := e.tenants.SetDisabled(ctx, a.Slug, true); err != nil {
		t.Fatal(err)
	}
	e3 := newEnv(t)
	if r := e3.do("GET", "/v1/me", a.APIKey, rotated.userToken(t, "u_1"), nil); r.status != 401 {
		t.Fatalf("disabled tenant accepted: %d", r.status)
	}
	if err := e.tenants.SetDisabled(ctx, a.Slug, false); err != nil {
		t.Fatal(err)
	}
	if r := newEnv(t).do("GET", "/v1/me", a.APIKey, rotated.userToken(t, "u_1"), nil); r.status != 200 {
		t.Fatalf("re-enabled tenant rejected: %d", r.status)
	}
}

func TestTenantCLI(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	slug := "cli-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	var out bytes.Buffer

	if err := runTenantCommand(ctx, e.tenants, []string{"create", "--slug", slug, "--client-can-create-group"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "api_secret:  mps_") || !strings.Contains(out.String(), "client_can_create_group=true") {
		t.Fatalf("create output:\n%s", out.String())
	}

	out.Reset()
	if err := runTenantCommand(ctx, e.tenants, []string{"set-config", "--slug", slug, "--client-can-create-dm=true"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "client_can_create_dm=true client_can_create_group=true") {
		t.Fatalf("set-config must keep unspecified flags:\n%s", out.String())
	}

	tn, _ := e.tenants.GetBySlug(ctx, slug)
	e.do("PUT", "/v1/server/users/u_cli", tn.APIKey, func() string {
		out.Reset()
		if err := runTenantCommand(ctx, e.tenants, []string{"token", "--slug", slug, "--server"}, nil, &out); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out.String())
	}(), map[string]any{"name": "CLI"})

	out.Reset()
	if err := runTenantCommand(ctx, e.tenants, []string{"token", "--slug", slug, "--user", "u_cli"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/v1/me", tn.APIKey, strings.TrimSpace(out.String()), nil); r.status != 200 || r.body["name"] != "CLI" {
		t.Fatalf("CLI tokens do not work: %d %v", r.status, r.body)
	}

	if err := runTenantCommand(ctx, e.tenants, []string{"create", "--slug", slug}, nil, &out); err == nil {
		t.Fatal("duplicate slug accepted")
	}
	if err := runTenantCommand(ctx, e.tenants, []string{"create", "--slug", "Bad Slug"}, nil, &out); err == nil {
		t.Fatal("invalid slug accepted")
	}
}
