//go:build integration

package console

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/platform/config"
	"github.com/rizkysiregar/marpokat/server/internal/platform/ratelimit"
	"github.com/rizkysiregar/marpokat/server/internal/platform/redisx"
	"github.com/rizkysiregar/marpokat/server/internal/platform/secretbox"
	"github.com/rizkysiregar/marpokat/server/internal/store"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
	"github.com/rizkysiregar/marpokat/server/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

const (
	portalOrigin = "https://portal.test"
	adminOrigin  = "https://admin.test"
)

// captureMailer keeps what would have been sent, so a test can follow the links.
type captureMailer struct {
	mu   sync.Mutex
	sent []Message
}

func (c *captureMailer) Send(_ context.Context, m Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, m)
	return nil
}

// to waits for the e-mail to this address (they are sent in the background) and returns the newest.
func (c *captureMailer) to(t *testing.T, addr, subject string) Message {
	t.Helper()
	for range 100 {
		c.mu.Lock()
		for i := len(c.sent) - 1; i >= 0; i-- {
			if c.sent[i].To == addr && strings.Contains(c.sent[i].Subject, subject) {
				m := c.sent[i]
				c.mu.Unlock()
				return m
			}
		}
		c.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no e-mail %q to %s", subject, addr)
	return Message{}
}

func (c *captureMailer) count(addr string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.sent {
		if m.To == addr {
			n++
		}
	}
	return n
}

var tokenInLink = regexp.MustCompile(`token=([A-Za-z0-9_%-]+)`)

func tokenOf(t *testing.T, m Message) string {
	t.Helper()
	g := tokenInLink.FindStringSubmatch(m.Text)
	if g == nil {
		t.Fatalf("no link in %q", m.Text)
	}
	tok, _ := url.QueryUnescape(g[1])
	return tok
}

type env struct {
	t      *testing.T
	srv    *httptest.Server
	svc    *Service
	mail   *captureMailer
	q      *store.Queries
	tenant *tenant.Service
}

func newEnv(t *testing.T, maxProjects int) *env {
	t.Helper()
	return newEnvWith(t, maxProjects, nil)
}

func newEnvWith(t *testing.T, maxProjects int, captcha Captcha) *env {
	t.Helper()
	pool := testdb.Pool(t)
	rc := testdb.Redis(t)
	rc.KeyPrefix = "console-" + uuid.NewString()[:8] + ":"
	rdb, err := redisx.Open(context.Background(), rc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	_ = config.Redis{}
	box, err := secretbox.New(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	q := store.New(pool)
	ts := tenant.NewService(q, box)
	mailer := &captureMailer{}
	svc := NewService(q, ts, mailer, ratelimit.New(rdb, log), log, Options{BaseURL: "https://marpokat.test", MaxProjects: maxProjects, Captcha: captcha})
	// the test client connects from loopback, which plays the ingress: its CF-Connecting-IP is believed
	srv := httptest.NewServer(NewHandler(svc, HandlerOptions{
		PortalOrigins:  []string{portalOrigin},
		AdminOrigins:   []string{adminOrigin},
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")},
	}, log).Routes())
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, svc: svc, mail: mailer, q: q, tenant: ts}
}

type reply struct {
	status int
	body   map[string]any
	raw    string
	header http.Header
}

// do calls the API the way the portal does: from its origin, JSON bodies, the session cookie, and the visitor's
// address as Cloudflare names it. token is the value of the session cookie.
func (e *env) do(method, path string, body any, token, ip string) reply {
	e.t.Helper()
	return e.doFrom(portalOrigin, method, path, body, token, ip)
}

func (e *env) doFrom(origin, method, path string, body any, token, ip string) reply {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+"/console/v1"+path, rdr)
	if method != "GET" {
		req.Header.Set("Origin", origin)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	}
	if ip != "" {
		req.Header.Set("CF-Connecting-IP", ip)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	r := reply{status: res.StatusCode, raw: string(raw), header: res.Header}
	_ = json.Unmarshal(raw, &r.body)
	return r
}

func (r reply) code() string {
	if e, ok := r.body["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

func (r reply) str(key string) string { s, _ := r.body[key].(string); return s }

// signUp registers, confirms the e-mail and signs in; it returns the session token.
func (e *env) signUp(email string, verify bool) string {
	e.t.Helper()
	if r := e.do("POST", "/auth/register", map[string]string{"email": email, "password": "a-good-password", "name": "Tester"}, "", "10.0.0."+uuid.NewString()[:2]); r.status != 201 {
		e.t.Fatalf("register %s: %d %s", email, r.status, r.raw)
	}
	if verify {
		m := e.mail.to(e.t, email, "Confirm")
		if r := e.do("POST", "/auth/verify-email", map[string]string{"token": tokenOf(e.t, m)}, "", ""); r.status != 204 {
			e.t.Fatalf("verify: %d %s", r.status, r.raw)
		}
	}
	return e.login(email, "a-good-password")
}

func (e *env) login(email, password string) string {
	e.t.Helper()
	return e.loginFrom(portalOrigin, email, password)
}

// loginFrom signs in from an app and returns the session cookie it was given.
func (e *env) loginFrom(origin, email, password string) string {
	e.t.Helper()
	r := e.doFrom(origin, "POST", "/auth/login", map[string]string{"email": email, "password": password}, "", "10.1.0."+uuid.NewString()[:2])
	if r.status != 200 {
		e.t.Fatalf("login %s: %d %s", email, r.status, r.raw)
	}
	c := r.cookie()
	if c == nil || c.Value == "" {
		e.t.Fatalf("login %s gave no session cookie: %v", email, r.header)
	}
	return c.Value
}

func (r reply) cookie() *http.Cookie {
	for _, c := range (&http.Response{Header: r.header}).Cookies() {
		if c.Name == SessionCookie {
			return c
		}
	}
	return nil
}

func uniqueEmail(prefix string) string { return prefix + "-" + uuid.NewString()[:8] + "@example.com" }

func TestTheSessionLivesInAnHttpOnlyCookie(t *testing.T) {
	e := newEnv(t, 3)
	email := uniqueEmail("cookie")
	e.signUp(email, false)

	r := e.do("POST", "/auth/login", map[string]string{"email": email, "password": "a-good-password"}, "", "10.6.0.1")
	c := r.cookie()
	if r.status != 200 || c == nil || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" {
		t.Fatalf("login: %d %v %+v", r.status, r.header, c)
	}
	if strings.Contains(r.raw, c.Value) || r.body["token"] != nil || r.str("scope") != "portal" {
		t.Fatalf("the token is readable by the page: %s", r.raw)
	}
	if c.MaxAge < 29*24*3600 { // the cookie outlives the idle limit; the server ends an idle session itself
		t.Fatalf("cookie max age: %d", c.MaxAge)
	}
	if me := e.do("GET", "/me", nil, c.Value, ""); me.status != 200 || me.str("email") != email {
		t.Fatalf("me with the cookie: %d %s", me.status, me.raw)
	}

	// a session the server does not know any more is forgotten by the browser too
	gone := e.do("GET", "/me", nil, "mpc_unknown", "")
	if gone.status != 401 || gone.cookie() == nil || gone.cookie().MaxAge >= 0 {
		t.Fatalf("unknown session: %d %v", gone.status, gone.header)
	}
	out := e.do("POST", "/auth/logout", nil, c.Value, "")
	if out.status != 204 || out.cookie() == nil || out.cookie().MaxAge >= 0 {
		t.Fatalf("logout does not clear the cookie: %d %v", out.status, out.header)
	}
	if r := e.do("GET", "/me", nil, c.Value, ""); r.status != 401 {
		t.Fatalf("session after logout: %d", r.status)
	}
	// a request that changes something from another site never reaches the session, even with the cookie
	if r := e.doFrom("https://evil.test", "PATCH", "/me", map[string]string{"name": "pwned"}, e.login(email, "a-good-password"), ""); r.status != 403 || r.code() != CodeOriginNotAllowed {
		t.Fatalf("cross-site request: %d %s", r.status, r.raw)
	}
}

// captchaStub accepts the token "human" and counts the checks.
type captchaStub struct {
	mu    sync.Mutex
	calls int
	ips   []string
}

func (c *captchaStub) Verify(_ context.Context, token, ip string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.ips = append(c.ips, ip)
	if token != "human" {
		return errCaptchaRefused
	}
	return nil
}

func TestSignUpAndPasswordResetAskForTheRobotCheck(t *testing.T) {
	stub := &captchaStub{}
	e := newEnvWith(t, 3, stub)
	email := uniqueEmail("robot")

	// invalid input is answered before the check, so that fixing a typo does not cost another one
	if r := e.do("POST", "/auth/register", map[string]string{"email": "nope", "password": "a-good-password", "name": "x"}, "", "10.10.0.1"); r.status != 400 || stub.calls != 0 {
		t.Fatalf("validation first: %d calls=%d", r.status, stub.calls)
	}
	if r := e.do("POST", "/auth/register", map[string]string{"email": email, "password": "a-good-password", "name": "Robot"}, "", "10.10.0.2"); r.status != 400 || r.code() != CodeCaptchaFailed {
		t.Fatalf("no token: %d %s", r.status, r.raw)
	}
	if e.mail.count(email) != 0 {
		t.Fatal("an account was made without the check")
	}
	if r := e.do("POST", "/auth/register", map[string]string{"email": email, "password": "a-good-password", "name": "Robot", "turnstile_token": "human"}, "", "203.0.113.77"); r.status != 201 {
		t.Fatalf("with the check: %d %s", r.status, r.raw)
	}
	e.mail.to(t, email, "Confirm")
	if last := stub.ips[len(stub.ips)-1]; last != "203.0.113.77" {
		t.Fatalf("Cloudflare was given %q, not the visitor's address", last)
	}

	// sign in is not checked (it has its own limits)
	e.login(email, "a-good-password")

	if r := e.do("POST", "/auth/forgot-password", map[string]string{"email": email, "turnstile_token": "robot"}, "", "10.10.0.3"); r.status != 400 || r.code() != CodeCaptchaFailed {
		t.Fatalf("forgot with a bad token: %d %s", r.status, r.raw)
	}
	if r := e.do("POST", "/auth/forgot-password", map[string]string{"email": email, "turnstile_token": "human"}, "", "10.10.0.4"); r.status != 204 {
		t.Fatalf("forgot: %d %s", r.status, r.raw)
	}
	e.mail.to(t, email, "Reset")
}

func TestSignUpVerifyAndFirstProject(t *testing.T) {
	e := newEnv(t, 3)
	email := uniqueEmail("ani")
	token := e.signUp(email, false)

	// signed in, but the e-mail is not confirmed: no projects yet
	if r := e.do("POST", "/projects", map[string]string{"name": "My App"}, token, ""); r.status != 403 || r.code() != CodeEmailNotVerified {
		t.Fatalf("project before verify: %d %s", r.status, r.raw)
	}
	if r := e.do("GET", "/me", nil, token, ""); r.status != 200 || r.body["email_verified"] != false || r.str("email") != email {
		t.Fatalf("me: %d %s", r.status, r.raw)
	}

	m := e.mail.to(t, email, "Confirm")
	link := tokenOf(t, m)
	if !strings.HasPrefix(m.Text, "Hi Tester") || !strings.Contains(m.Text, "https://marpokat.test/verify-email?token=") {
		t.Fatalf("verification mail: %s", m.Text)
	}
	if r := e.do("POST", "/auth/verify-email", map[string]string{"token": link}, "", ""); r.status != 204 {
		t.Fatalf("verify: %d %s", r.status, r.raw)
	}
	if r := e.do("POST", "/auth/verify-email", map[string]string{"token": link}, "", ""); r.status != 400 {
		t.Fatalf("a link works once: %d", r.status)
	}
	if r := e.do("POST", "/auth/verify-email", map[string]string{"token": "nonsense"}, "", ""); r.status != 400 {
		t.Fatalf("unknown token: %d", r.status)
	}

	r := e.do("POST", "/projects", map[string]string{"name": "My App"}, token, "")
	if r.status != 201 {
		t.Fatalf("create project: %d %s", r.status, r.raw)
	}
	secret, key := r.str("api_secret"), r.str("api_key")
	if !strings.HasPrefix(secret, "mps_") || !strings.HasPrefix(key, "mpk_") || !strings.HasPrefix(r.str("slug"), "my-app") {
		t.Fatalf("project: %s", r.raw)
	}
	// the key the website handed out really authenticates against the chat API's tenant store
	tn, got, err := e.tenant.Credentials(context.Background(), key)
	if err != nil || string(got) != secret || tn.Name != "My App" {
		t.Fatalf("credentials: %+v %q %v", tn, got, err)
	}
	// the secret is shown once: reading the project again has no secret
	id := r.str("id")
	if g := e.do("GET", "/projects/"+id, nil, token, ""); g.status != 200 || strings.Contains(g.raw, "mps_") || g.body["usage"] == nil {
		t.Fatalf("get: %d %s", g.status, g.raw)
	}
}

func TestRegisterHidesWhichAddressesHaveAnAccount(t *testing.T) {
	e := newEnv(t, 3)
	email := uniqueEmail("dup")
	e.signUp(email, false)
	before := e.mail.count(email)

	r := e.do("POST", "/auth/register", map[string]string{"email": strings.ToUpper(email), "password": "another-password", "name": "Other"}, "", "10.9.9.9")
	if r.status != 201 {
		t.Fatalf("known address: %d %s", r.status, r.raw)
	}
	m := e.mail.to(t, email, "already have")
	if e.mail.count(email) != before+1 || !strings.Contains(m.Text, "already has an account") {
		t.Fatalf("the owner was not told: %q", m.Text)
	}
	// the password of the second try does not work: no second account, the first password still does
	if r := e.do("POST", "/auth/login", map[string]string{"email": email, "password": "another-password"}, "", "10.8.8.8"); r.status != 401 {
		t.Fatalf("second password worked: %d", r.status)
	}
	e.login(email, "a-good-password")
}

func TestRegisterValidation(t *testing.T) {
	e := newEnv(t, 3)
	for name, body := range map[string]map[string]string{
		"bad email":      {"email": "nope", "password": "a-good-password", "name": "x"},
		"short password": {"email": uniqueEmail("v"), "password": "short", "name": "x"},
		"no name":        {"email": uniqueEmail("v"), "password": "a-good-password", "name": " "},
	} {
		if r := e.do("POST", "/auth/register", body, "", "10.7.7.7"); r.status != 400 || r.code() != "validation_failed" {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}
}

func TestLoginDoesNotRevealWhoHasAnAccountAndIsLimited(t *testing.T) {
	e := newEnv(t, 3)
	email := uniqueEmail("log")
	e.signUp(email, false)

	wrong := e.do("POST", "/auth/login", map[string]string{"email": email, "password": "wrong-password"}, "", "10.2.2.2")
	unknown := e.do("POST", "/auth/login", map[string]string{"email": uniqueEmail("ghost"), "password": "wrong-password"}, "", "10.2.2.3")
	if wrong.status != 401 || unknown.status != 401 || wrong.code() != CodeInvalidCredentials || wrong.raw != unknown.raw {
		t.Fatalf("answers differ:\n%s\n%s", wrong.raw, unknown.raw)
	}
	// 5 tries per e-mail, then 429 with Retry-After, even for the right password
	var last reply
	for range 6 {
		last = e.do("POST", "/auth/login", map[string]string{"email": email, "password": "wrong-password"}, "", "10.2.2.4")
	}
	if last.status != 429 || last.header.Get("Retry-After") == "" {
		t.Fatalf("not limited: %d %s", last.status, last.raw)
	}
	if r := e.do("POST", "/auth/login", map[string]string{"email": email, "password": "a-good-password"}, "", "10.2.2.5"); r.status != 429 {
		t.Fatalf("the right password gets through while limited: %d", r.status)
	}
}

func TestSessionsLogoutExpiryAndAudit(t *testing.T) {
	e := newEnv(t, 3)
	email := uniqueEmail("ses")
	a := e.signUp(email, false)
	b := e.login(email, "a-good-password")

	if r := e.do("GET", "/me/sessions", nil, a, ""); r.status != 200 || len(r.body["sessions"].([]any)) != 2 {
		t.Fatalf("sessions: %s", r.raw)
	}
	if r := e.do("GET", "/me", nil, "mpc_garbage", ""); r.status != 401 {
		t.Fatalf("garbage token: %d", r.status)
	}
	if r := e.do("GET", "/me", nil, "", ""); r.status != 401 {
		t.Fatalf("no token: %d", r.status)
	}
	if r := e.do("POST", "/auth/logout", nil, a, ""); r.status != 204 {
		t.Fatalf("logout: %d", r.status)
	}
	if r := e.do("GET", "/me", nil, a, ""); r.status != 401 {
		t.Fatalf("session after logout: %d", r.status)
	}
	// the other session ends when its idle time is over, and at its absolute end however busy it is
	if _, err := testdb.Pool(t).Exec(context.Background(), `UPDATE console_sessions SET expires_at = now() - interval '1 second' WHERE token_hash = $1`, hashToken(b)); err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/me", nil, b, ""); r.status != 401 {
		t.Fatalf("idle session: %d", r.status)
	}
	c := e.login(email, "a-good-password")
	if _, err := testdb.Pool(t).Exec(context.Background(), `UPDATE console_sessions SET absolute_expires_at = now() - interval '1 second' WHERE token_hash = $1`, hashToken(c)); err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/me", nil, c, ""); r.status != 401 {
		t.Fatalf("session past its absolute end: %d", r.status)
	}
	d := e.login(email, "a-good-password")
	r := e.do("GET", "/me/audit", nil, d, "")
	actions := map[string]bool{}
	for _, ev := range r.body["events"].([]any) {
		actions[ev.(map[string]any)["action"].(string)] = true
	}
	for _, want := range []string{"registered", "login", "logout"} {
		if !actions[want] {
			t.Errorf("audit lacks %q: %v", want, actions)
		}
	}
}

func TestChangePasswordEndsOtherSessions(t *testing.T) {
	e := newEnv(t, 3)
	email := uniqueEmail("pw")
	a := e.signUp(email, false)
	b := e.login(email, "a-good-password")

	if r := e.do("PUT", "/me/password", map[string]string{"current_password": "nope-nope-nope", "new_password": "a-new-password-1"}, a, ""); r.status != 403 || r.code() != CodeWrongPassword {
		t.Fatalf("wrong current: %d %s", r.status, r.raw)
	}
	if r := e.do("PUT", "/me/password", map[string]string{"current_password": "a-good-password", "new_password": "short"}, a, ""); r.status != 400 {
		t.Fatalf("weak new: %d", r.status)
	}
	if r := e.do("PUT", "/me/password", map[string]string{"current_password": "a-good-password", "new_password": "a-new-password-1"}, a, ""); r.status != 204 {
		t.Fatalf("change: %d", r.status)
	}
	if r := e.do("GET", "/me", nil, a, ""); r.status != 200 {
		t.Fatalf("the session that changed it was ended: %d", r.status)
	}
	if r := e.do("GET", "/me", nil, b, ""); r.status != 401 {
		t.Fatalf("another session survived: %d", r.status)
	}
	e.mail.to(t, email, "password was changed")
	if r := e.do("POST", "/auth/login", map[string]string{"email": email, "password": "a-good-password"}, "", "10.3.3.3"); r.status != 401 {
		t.Fatalf("old password works: %d", r.status)
	}
	e.login(email, "a-new-password-1")
}

func TestForgotAndResetPassword(t *testing.T) {
	e := newEnv(t, 3)
	email := uniqueEmail("rst")
	session := e.signUp(email, false)

	ghost := uniqueEmail("nobody")
	if r := e.do("POST", "/auth/forgot-password", map[string]string{"email": ghost}, "", "10.4.4.4"); r.status != 204 {
		t.Fatalf("unknown e-mail: %d", r.status)
	}
	time.Sleep(100 * time.Millisecond)
	if e.mail.count(ghost) != 0 {
		t.Fatal("mail sent to an address without an account")
	}

	if r := e.do("POST", "/auth/forgot-password", map[string]string{"email": email}, "", "10.4.4.5"); r.status != 204 {
		t.Fatalf("forgot: %d", r.status)
	}
	first := tokenOf(t, e.mail.to(t, email, "Reset"))
	if r := e.do("POST", "/auth/forgot-password", map[string]string{"email": email}, "", "10.4.4.6"); r.status != 204 {
		t.Fatalf("forgot again: %d", r.status)
	}
	var second string
	for range 100 { // the newer mail has another token
		if second = tokenOf(t, e.mail.to(t, email, "Reset")); second != first {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r := e.do("POST", "/auth/reset-password", map[string]string{"token": first, "password": "a-brand-new-pass"}, "", ""); r.status != 400 {
		t.Fatalf("the replaced link still works: %d", r.status)
	}
	// a weak password does not spend the link
	if r := e.do("POST", "/auth/reset-password", map[string]string{"token": second, "password": "short"}, "", ""); r.status != 400 {
		t.Fatalf("weak: %d", r.status)
	}
	if r := e.do("POST", "/auth/reset-password", map[string]string{"token": second, "password": "a-brand-new-pass"}, "", ""); r.status != 204 {
		t.Fatalf("reset: %d", r.status)
	}
	if r := e.do("POST", "/auth/reset-password", map[string]string{"token": second, "password": "yet-another-pass"}, "", ""); r.status != 400 {
		t.Fatalf("a link works once: %d", r.status)
	}
	if r := e.do("GET", "/me", nil, session, ""); r.status != 401 {
		t.Fatalf("a session survived the reset: %d", r.status)
	}
	e.login(email, "a-brand-new-pass")

	// an expired link
	e.do("POST", "/auth/forgot-password", map[string]string{"email": email}, "", "10.4.4.7")
	var third string
	for range 100 {
		if third = tokenOf(t, e.mail.to(t, email, "Reset")); third != second {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := testdb.Pool(t).Exec(context.Background(), `UPDATE account_tokens SET expires_at = now() - interval '1 second' WHERE token_hash = $1`, hashToken(third)); err != nil {
		t.Fatal(err)
	}
	if r := e.do("POST", "/auth/reset-password", map[string]string{"token": third, "password": "a-brand-new-pass-2"}, "", ""); r.status != 400 {
		t.Fatalf("expired link: %d", r.status)
	}
}

func TestProjectLimitSlugsAndOwnership(t *testing.T) {
	e := newEnv(t, 2)
	a := e.signUp(uniqueEmail("own"), true)
	b := e.signUp(uniqueEmail("oth"), true)

	name := "Shared Name " + uuid.NewString()[:6]
	one := e.do("POST", "/projects", map[string]string{"name": name}, a, "")
	two := e.do("POST", "/projects", map[string]string{"name": name}, b, "") // same name, another account: a free slug
	if one.status != 201 || two.status != 201 || one.str("slug") == two.str("slug") {
		t.Fatalf("slugs: %s | %s", one.raw, two.raw)
	}
	if r := e.do("POST", "/projects", map[string]string{"name": "x", "slug": one.str("slug")}, a, ""); r.status != 409 {
		t.Fatalf("taken slug: %d %s", r.status, r.raw)
	}
	if r := e.do("POST", "/projects", map[string]string{"name": "x", "slug": "Bad Slug!"}, a, ""); r.status != 400 {
		t.Fatalf("bad slug: %d", r.status)
	}
	if r := e.do("POST", "/projects", map[string]string{"name": "second", "slug": "s-" + uuid.NewString()[:8]}, a, ""); r.status != 201 {
		t.Fatalf("second: %d %s", r.status, r.raw)
	}
	if r := e.do("POST", "/projects", map[string]string{"name": "third"}, a, ""); r.status != 403 || r.code() != CodeProjectLimit {
		t.Fatalf("limit: %d %s", r.status, r.raw)
	}
	if r := e.do("GET", "/projects", nil, a, ""); len(r.body["projects"].([]any)) != 2 {
		t.Fatalf("list: %s", r.raw)
	}

	// somebody else's project is "not found", never "forbidden": its existence is not confirmed
	id := one.str("id")
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/projects/" + id, nil},
		{"PATCH", "/projects/" + id, map[string]any{"name": "mine now"}},
		{"POST", "/projects/" + id + "/rotate-secret", map[string]string{"password": "a-good-password"}},
		{"GET", "/projects/" + id + "/push", nil},
		{"PUT", "/projects/" + id + "/push", map[string]string{"service_account": "{}", "password": "a-good-password"}},
		{"POST", "/projects/" + id + "/disable", map[string]any{"disabled": true, "password": "a-good-password"}},
		{"GET", "/projects/not-a-uuid", nil},
	} {
		if r := e.do(c.method, c.path, c.body, b, ""); r.status != 404 {
			t.Errorf("%s %s by another account: %d %s", c.method, c.path, r.status, r.raw)
		}
	}
	if r := e.do("GET", "/projects/"+id, nil, a, ""); r.status != 200 || r.str("name") != name {
		t.Fatalf("the owner lost access: %s", r.raw)
	}
}

func TestUpdateProject(t *testing.T) {
	e := newEnv(t, 3)
	a := e.signUp(uniqueEmail("upd"), true)
	p := e.do("POST", "/projects", map[string]string{"name": "Old"}, a, "")
	id := p.str("id")
	r := e.do("PATCH", "/projects/"+id, map[string]any{"name": "New", "client_can_create_dm": true}, a, "")
	if r.status != 200 || r.str("name") != "New" || r.body["client_can_create_dm"] != true || r.body["client_can_create_group"] != false {
		t.Fatalf("patch: %d %s", r.status, r.raw)
	}
	tn, _, err := e.tenant.Credentials(context.Background(), p.str("api_key"))
	if err != nil || !tn.Config.ClientCanCreateDM || tn.Name != "New" {
		t.Fatalf("not stored: %+v %v", tn, err)
	}
	if r := e.do("PATCH", "/projects/"+id, map[string]any{"name": " "}, a, ""); r.status != 400 {
		t.Fatalf("empty name: %d", r.status)
	}
}

func TestSensitiveActionsAskForThePasswordAgain(t *testing.T) {
	e := newEnv(t, 3)
	email := uniqueEmail("sec")
	a := e.signUp(email, true)
	p := e.do("POST", "/projects", map[string]string{"name": "Secure"}, a, "")
	id, key, oldSecret := p.str("id"), p.str("api_key"), p.str("api_secret")

	if r := e.do("POST", "/projects/"+id+"/rotate-secret", map[string]string{"password": "wrong-password-x"}, a, ""); r.status != 403 || r.code() != CodeWrongPassword {
		t.Fatalf("wrong password: %d %s", r.status, r.raw)
	}
	if _, got, _ := e.tenant.Credentials(context.Background(), key); string(got) != oldSecret {
		t.Fatal("the secret changed without the password")
	}
	r := e.do("POST", "/projects/"+id+"/rotate-secret", map[string]string{"password": "a-good-password"}, a, "")
	newSecret := r.str("api_secret")
	if r.status != 200 || newSecret == "" || newSecret == oldSecret {
		t.Fatalf("rotate: %d %s", r.status, r.raw)
	}
	if _, got, _ := e.tenant.Credentials(context.Background(), key); string(got) != newSecret {
		t.Fatal("the new secret is not the one stored")
	}

	// push credentials: validated, stored encrypted, never shown again
	sa := `{"type":"service_account","project_id":"my-firebase","private_key":"-----BEGIN PRIVATE KEY-----\nSECRETKEYMATERIAL\n-----END PRIVATE KEY-----\n","client_email":"fcm@my-firebase.iam.gserviceaccount.com"}`
	if r := e.do("PUT", "/projects/"+id+"/push", map[string]string{"service_account": "not json", "password": "a-good-password"}, a, ""); r.status != 400 {
		t.Fatalf("not JSON: %d %s", r.status, r.raw)
	}
	if r := e.do("PUT", "/projects/"+id+"/push", map[string]string{"service_account": sa, "password": "wrong-password-x"}, a, ""); r.status != 403 {
		t.Fatalf("push without password: %d", r.status)
	}
	r = e.do("PUT", "/projects/"+id+"/push", map[string]string{"service_account": sa, "password": "a-good-password"}, a, "")
	if r.status != 200 || r.body["configured"] != true || r.str("firebase_project_id") != "my-firebase" || r.str("client_email") != "fcm@my-firebase.iam.gserviceaccount.com" || strings.Contains(r.raw, "SECRETKEYMATERIAL") {
		t.Fatalf("set push: %d %s", r.status, r.raw)
	}
	if g := e.do("GET", "/projects/"+id+"/push", nil, a, ""); g.body["configured"] != true || strings.Contains(g.raw, "SECRETKEYMATERIAL") {
		t.Fatalf("get push: %s", g.raw)
	}
	if g := e.do("GET", "/projects/"+id, nil, a, ""); g.body["push_configured"] != true || strings.Contains(g.raw, "SECRETKEYMATERIAL") {
		t.Fatalf("project: %s", g.raw)
	}
	tid, _ := uuid.Parse(id)
	if plain, ok, err := e.tenant.FCMCredentials(context.Background(), tid); err != nil || !ok || !bytes.Contains(plain, []byte("SECRETKEYMATERIAL")) {
		t.Fatalf("the worker cannot read what was stored: %v %v", ok, err)
	}
	if r := e.do("DELETE", "/projects/"+id+"/push", map[string]string{"password": "a-good-password"}, a, ""); r.status != 204 {
		t.Fatalf("remove push: %d", r.status)
	}
	if g := e.do("GET", "/projects/"+id+"/push", nil, a, ""); g.body["configured"] != false {
		t.Fatalf("still configured: %s", g.raw)
	}

	// switching a project off ends its apps' access
	if r := e.do("POST", "/projects/"+id+"/disable", map[string]any{"disabled": true, "password": "wrong-password-x"}, a, ""); r.status != 403 {
		t.Fatalf("disable without password: %d", r.status)
	}
	if r := e.do("POST", "/projects/"+id+"/disable", map[string]any{"disabled": true, "password": "a-good-password"}, a, ""); r.status != 200 || r.body["disabled"] != true {
		t.Fatalf("disable: %d %s", r.status, r.raw)
	}
	if _, _, err := e.tenant.Credentials(context.Background(), key); err == nil {
		t.Fatal("a disabled project still authenticates")
	}
	e.do("POST", "/projects/"+id+"/disable", map[string]any{"disabled": false, "password": "a-good-password"}, a, "")
	if _, _, err := e.tenant.Credentials(context.Background(), key); err != nil {
		t.Fatalf("enabling did not restore access: %v", err)
	}

	audit := e.do("GET", "/me/audit", nil, a, "")
	seen := map[string]bool{}
	for _, ev := range audit.body["events"].([]any) {
		seen[ev.(map[string]any)["action"].(string)] = true
	}
	for _, want := range []string{"secret_rotated", "push_set", "push_removed", "project_disabled", "project_enabled", "password_confirmation_failed"} {
		if !seen[want] {
			t.Errorf("audit lacks %q: %v", want, seen)
		}
	}
	if strings.Contains(audit.raw, newSecret) || strings.Contains(audit.raw, "SECRETKEYMATERIAL") {
		t.Fatal("the audit log holds a secret")
	}
}

func TestConfirmationAttemptsAreLimited(t *testing.T) {
	e := newEnv(t, 3)
	a := e.signUp(uniqueEmail("brute"), true)
	p := e.do("POST", "/projects", map[string]string{"name": "Brute"}, a, "")
	var last reply
	for range 12 {
		last = e.do("POST", "/projects/"+p.str("id")+"/rotate-secret", map[string]string{"password": "guess-guess-1"}, a, "")
	}
	if last.status != 429 {
		t.Fatalf("password guessing is not limited: %d", last.status)
	}
}

func TestPlatformAdmin(t *testing.T) {
	e := newEnv(t, 3)
	userEmail, adminEmail := uniqueEmail("usr"), uniqueEmail("adm")
	user := e.signUp(userEmail, true)
	admin := e.signUp(adminEmail, true)
	proj := e.do("POST", "/projects", map[string]string{"name": "Managed"}, user, "")

	for _, path := range []string{"/admin/accounts", "/admin/projects", "/admin/stats"} {
		if r := e.do("GET", path, nil, user, ""); r.status != 403 || r.code() != "forbidden" {
			t.Errorf("%s as a normal account: %d", path, r.status)
		}
	}
	if err := e.svc.PromoteAdmin(context.Background(), adminEmail, true); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.PromoteAdmin(context.Background(), "nobody@example.com", true); err == nil {
		t.Fatal("promoting an unknown account worked")
	}
	if r := e.do("GET", "/me", nil, admin, ""); r.body["is_platform_admin"] != true {
		t.Fatalf("me: %s", r.raw)
	}
	// an administrator signed in on the portal cannot manage the platform from there (PLAN D71)
	if r := e.do("GET", "/admin/stats", nil, admin, ""); r.status != 403 || !strings.Contains(r.raw, "admin site") {
		t.Fatalf("admin endpoints with a portal session: %d %s", r.status, r.raw)
	}
	// only an administrator can sign in on the admin app
	if r := e.doFrom(adminOrigin, "POST", "/auth/login", map[string]string{"email": userEmail, "password": "a-good-password"}, "", "10.5.5.6"); r.status != 403 || r.cookie() != nil {
		t.Fatalf("a normal account signed in on the admin app: %d %s", r.status, r.raw)
	}
	admin = e.loginFrom(adminOrigin, adminEmail, "a-good-password")
	if r := e.do("GET", "/me/sessions", nil, admin, ""); !strings.Contains(r.raw, `"scope":"admin"`) {
		t.Fatalf("sessions: %s", r.raw)
	}

	projects := e.do("GET", "/admin/projects", nil, admin, "")
	found := false
	for _, p := range projects.body["projects"].([]any) {
		if p.(map[string]any)["id"] == proj.str("id") {
			found = p.(map[string]any)["owner_email"] == userEmail
		}
	}
	if projects.status != 200 || !found {
		t.Fatalf("admin projects: %d (owner shown: %v)", projects.status, found)
	}
	if r := e.do("POST", "/admin/projects/"+proj.str("id")+"/disable", map[string]any{"disabled": true}, admin, ""); r.status != 200 || r.body["disabled"] != true {
		t.Fatalf("disable project: %d %s", r.status, r.raw)
	}
	if _, _, err := e.tenant.Credentials(context.Background(), proj.str("api_key")); err == nil {
		t.Fatal("a project disabled by an admin still authenticates")
	}

	accounts := e.do("GET", "/admin/accounts?limit=1", nil, admin, "")
	if accounts.status != 200 || len(accounts.body["accounts"].([]any)) != 1 || accounts.body["next_cursor"] == nil {
		t.Fatalf("accounts page: %s", accounts.raw)
	}
	stats := e.do("GET", "/admin/stats", nil, admin, "")
	if stats.status != 200 || stats.body["accounts"].(float64) < 2 || stats.body["projects"].(float64) < 1 {
		t.Fatalf("stats: %s", stats.raw)
	}

	// disabling an account ends its sessions and refuses new ones; an admin cannot lock themself out
	uid := e.do("GET", "/me", nil, user, "").str("id")
	if r := e.do("POST", "/admin/accounts/"+uid+"/disable", map[string]any{"disabled": true}, admin, ""); r.status != 200 || r.body["disabled"] != true {
		t.Fatalf("disable account: %d %s", r.status, r.raw)
	}
	if r := e.do("GET", "/me", nil, user, ""); r.status != 401 {
		t.Fatalf("a disabled account kept its session: %d", r.status)
	}
	if r := e.do("POST", "/auth/login", map[string]string{"email": userEmail, "password": "a-good-password"}, "", "10.5.5.5"); r.status != 403 || r.code() != CodeAccountDisabled {
		t.Fatalf("login of a disabled account: %d %s", r.status, r.raw)
	}
	self := e.do("GET", "/me", nil, admin, "").str("id")
	if r := e.do("POST", "/admin/accounts/"+self+"/disable", map[string]any{"disabled": true}, admin, ""); r.status != 400 {
		t.Fatalf("an admin disabled themself: %d", r.status)
	}
	if r := e.do("POST", "/admin/accounts/"+uid+"/disable", map[string]any{"disabled": false}, admin, ""); r.status != 200 {
		t.Fatalf("enable: %d", r.status)
	}
	e.login(userEmail, "a-good-password")
}

func TestUpdateNameAndResendVerification(t *testing.T) {
	e := newEnv(t, 3)
	email := uniqueEmail("nm")
	a := e.signUp(email, false)
	if r := e.do("PATCH", "/me", map[string]string{"name": "New Name"}, a, ""); r.status != 200 || r.str("name") != "New Name" {
		t.Fatalf("name: %d %s", r.status, r.raw)
	}
	if r := e.do("PATCH", "/me", map[string]string{"name": ""}, a, ""); r.status != 400 {
		t.Fatalf("empty name: %d", r.status)
	}
	before := e.mail.count(email)
	if r := e.do("POST", "/auth/resend-verification", nil, a, ""); r.status != 204 {
		t.Fatalf("resend: %d", r.status)
	}
	e.mail.to(t, email, "Confirm")
	for range 100 {
		if e.mail.count(email) > before {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if e.mail.count(email) != before+1 {
		t.Fatalf("no second e-mail: %d -> %d", before, e.mail.count(email))
	}
	for range 5 { // 3 per hour
		e.do("POST", "/auth/resend-verification", nil, a, "")
	}
	if r := e.do("POST", "/auth/resend-verification", nil, a, ""); r.status != 429 {
		t.Fatalf("resend is not limited: %d", r.status)
	}
}
