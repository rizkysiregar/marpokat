package console

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func testHandler() *Handler {
	return NewHandler(nil, HandlerOptions{
		PortalOrigins:  []string{"https://portal.test"},
		AdminOrigins:   []string{"https://admin.test"},
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	}, quiet)
}

// The guard answers before any service is reached: these requests never get to the (nil) service.
func TestGuardRefusesRequestsFromOtherSitesAndNonJSONBodies(t *testing.T) {
	routes := testHandler().Routes()
	for name, c := range map[string]struct {
		method, origin, contentType, body string
		status                            int
		code                              string
	}{
		"no origin":             {"POST", "", "application/json", `{}`, 403, CodeOriginNotAllowed},
		"another site":          {"POST", "https://evil.test", "application/json", `{}`, 403, CodeOriginNotAllowed},
		"lookalike":             {"POST", "https://portal.test.evil.test", "application/json", `{}`, 403, CodeOriginNotAllowed},
		"null origin":           {"POST", "null", "application/json", `{}`, 403, CodeOriginNotAllowed},
		"delete from elsewhere": {"DELETE", "https://evil.test", "", "", 403, CodeOriginNotAllowed},
		"form post":             {"POST", "https://portal.test", "application/x-www-form-urlencoded", "a=b", 415, CodeUnsupportedMediaType},
		"text/plain":            {"POST", "https://admin.test", "text/plain", `{}`, 415, CodeUnsupportedMediaType},
		"body without a type":   {"POST", "https://portal.test", "", `{}`, 415, CodeUnsupportedMediaType},
		"get needs no origin":   {"GET", "", "", "", 401, httpx.CodeUnauthenticated},
		"bodiless post is fine": {"POST", "https://portal.test", "", "", 401, httpx.CodeUnauthenticated},
		"json with charset":     {"PUT", "https://Portal.test", "application/json; charset=utf-8", `{}`, 401, httpx.CodeUnauthenticated},
		"admin origin is fine":  {"PATCH", "https://admin.test", "application/json", `{}`, 401, httpx.CodeUnauthenticated},
	} {
		path := "/console/v1/me"
		if c.method == "POST" {
			path = "/console/v1/auth/logout"
		}
		if c.method == "PUT" {
			path = "/console/v1/me/password"
		}
		if c.method == "DELETE" {
			path = "/console/v1/projects/0190d2c3-0000-7000-8000-000000000000/push"
		}
		req := httptest.NewRequest(c.method, path, strings.NewReader(c.body))
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		if c.contentType != "" {
			req.Header.Set("Content-Type", c.contentType)
		}
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), `"`+c.code+`"`) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: headers %v", name, rec.Header())
		}
	}
}

func TestClientIPBelievesHeadersOnlyFromTrustedProxies(t *testing.T) {
	h := testHandler()
	for name, c := range map[string]struct {
		remote, cf, xff, want string
	}{
		"through the ingress":           {"10.42.0.7:5555", "203.0.113.9", "", "203.0.113.9"},
		"ingress, forwarded for only":   {"10.42.0.7:5555", "", "198.51.100.4, 10.0.0.1", "198.51.100.4"},
		"ingress, nothing said":         {"10.42.0.7:5555", "", "", "10.42.0.7"},
		"ingress, garbage header":       {"10.42.0.7:5555", "not-an-ip", "", "10.42.0.7"},
		"stranger writing the header":   {"192.0.2.50:4444", "203.0.113.9", "203.0.113.9", "192.0.2.50"},
		"ipv4 mapped ipv6 from ingress": {"[::ffff:10.1.1.1]:80", "203.0.113.10", "", "203.0.113.10"},
	} {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = c.remote
		if c.cf != "" {
			req.Header.Set("CF-Connecting-IP", c.cf)
		}
		if c.xff != "" {
			req.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := h.clientIP(req); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	rec := httptest.NewRecorder()
	setSessionCookie(rec, "mpc_abc", time.Now().Add(30*24*time.Hour))
	set := rec.Header().Get("Set-Cookie")
	for _, want := range []string{SessionCookie + "=mpc_abc", "Path=/", "HttpOnly", "Secure", "SameSite=Strict", "Max-Age="} {
		if !strings.Contains(set, want) {
			t.Errorf("Set-Cookie %q lacks %q", set, want)
		}
	}
	if strings.Contains(strings.ToLower(set), "domain=") {
		t.Errorf("a __Host- cookie must not have a Domain: %q", set)
	}
	rec = httptest.NewRecorder()
	clearSessionCookie(rec)
	if set := rec.Header().Get("Set-Cookie"); !strings.Contains(set, SessionCookie+"=;") || !strings.Contains(set, "Max-Age=0") {
		t.Errorf("clearing: %q", set)
	}
}

// fakeCloudflare answers siteverify like Cloudflare: success for the token "good".
func fakeCloudflare(t *testing.T, seen *url.Values) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		*seen = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		if r.PostForm.Get("response") == "good" && r.PostForm.Get("secret") == "the-secret" {
			_, _ = io.WriteString(w, `{"success":true}`)
			return
		}
		_, _ = io.WriteString(w, `{"success":false,"error-codes":["invalid-input-response"]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func codeOf(err error) string {
	var e *httpx.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestTurnstile(t *testing.T) {
	var seen url.Values
	cf := fakeCloudflare(t, &seen)
	ts := NewTurnstile("the-secret", cf.URL, quiet)
	ctx := context.Background()

	if err := ts.Verify(ctx, "good", "203.0.113.9"); err != nil {
		t.Fatalf("a good token was refused: %v", err)
	}
	if seen.Get("remoteip") != "203.0.113.9" || seen.Get("secret") != "the-secret" {
		t.Fatalf("sent to Cloudflare: %v", seen)
	}
	if err := ts.Verify(ctx, "bad", ""); codeOf(err) != CodeCaptchaFailed {
		t.Fatalf("a bad token: %v", err)
	}
	if err := ts.Verify(ctx, "  ", ""); codeOf(err) != CodeCaptchaFailed {
		t.Fatalf("no token: %v", err)
	}
	if err := ts.Verify(ctx, strings.Repeat("x", 3000), ""); codeOf(err) != CodeCaptchaFailed {
		t.Fatalf("a huge token: %v", err)
	}

	// Cloudflare unreachable or answering nonsense: closed, not open
	down := NewTurnstile("the-secret", "http://127.0.0.1:1/siteverify", quiet)
	if err := down.Verify(ctx, "good", ""); codeOf(err) != CodeCaptchaUnavailable {
		t.Fatalf("unreachable: %v", err)
	}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer broken.Close()
	if err := NewTurnstile("the-secret", broken.URL, quiet).Verify(ctx, "good", ""); codeOf(err) != CodeCaptchaUnavailable {
		t.Fatalf("502: %v", err)
	}
}
