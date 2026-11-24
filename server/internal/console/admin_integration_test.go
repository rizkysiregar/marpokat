//go:build integration

package console

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/platform/secretbox"
	"github.com/rizkysiregar/marpokat/server/internal/store"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
	"github.com/rizkysiregar/marpokat/server/internal/testdb"
)

// adminSession signs up an account, makes it a platform admin and signs it in on the admin app.
func (e *env) adminSession(email string) string {
	e.t.Helper()
	e.signUp(email, true)
	if err := e.svc.PromoteAdmin(context.Background(), email, true); err != nil {
		e.t.Fatal(err)
	}
	return e.loginFrom(adminOrigin, email, "a-good-password")
}

func TestAdminAccountDetailSearchAndSignOut(t *testing.T) {
	e := newEnv(t, 3)
	admin := e.adminSession(uniqueEmail("boss"))
	who := uniqueEmail("findme")
	user := e.signUp(who, true)
	e.do("POST", "/projects", map[string]string{"name": "Theirs"}, user, "")
	uid := e.do("GET", "/me", nil, user, "").str("id")

	// search by a part of the e-mail; wildcards in the search are taken literally
	found := e.doFrom(adminOrigin, "GET", "/admin/accounts?q="+strings.Split(who, "@")[0], nil, admin, "")
	if found.status != 200 || len(found.body["accounts"].([]any)) != 1 {
		t.Fatalf("search: %d %s", found.status, found.raw)
	}
	if r := e.doFrom(adminOrigin, "GET", "/admin/accounts?q=%25", nil, admin, ""); len(r.body["accounts"].([]any)) != 0 {
		t.Fatalf("a %% in the search matched everything: %s", r.raw)
	}

	d := e.doFrom(adminOrigin, "GET", "/admin/accounts/"+uid, nil, admin, "")
	if d.status != 200 || len(d.body["projects"].([]any)) != 1 || len(d.body["sessions"].([]any)) != 1 || len(d.body["events"].([]any)) == 0 {
		t.Fatalf("account detail: %d %s", d.status, d.raw)
	}
	if r := e.doFrom(adminOrigin, "GET", "/admin/accounts/"+uuid.NewString(), nil, admin, ""); r.status != 404 {
		t.Fatalf("unknown account: %d", r.status)
	}

	if r := e.doFrom(adminOrigin, "POST", "/admin/accounts/"+uid+"/sign-out", nil, admin, ""); r.status != 204 {
		t.Fatalf("sign out: %d %s", r.status, r.raw)
	}
	if r := e.do("GET", "/me", nil, user, ""); r.status != 401 {
		t.Fatalf("the account kept its session: %d", r.status)
	}
	user = e.login(who, "a-good-password") // not disabled: they can sign in again
	self := e.doFrom(adminOrigin, "GET", "/me", nil, admin, "").str("id")
	if r := e.doFrom(adminOrigin, "POST", "/admin/accounts/"+self+"/sign-out", nil, admin, ""); r.status != 400 {
		t.Fatalf("admin signed themself out: %d", r.status)
	}
	// a normal session cannot use any of it
	for _, path := range []string{"/admin/accounts/" + uid, "/admin/audit", "/admin/stats/series"} {
		if r := e.do("GET", path, nil, user, ""); r.status != 403 {
			t.Errorf("%s as a normal account: %d", path, r.status)
		}
	}
}

func TestAdminProjectDetailAuditAndSeries(t *testing.T) {
	e := newEnv(t, 3)
	adminEmail := uniqueEmail("boss")
	admin := e.adminSession(adminEmail)
	owner := uniqueEmail("own")
	user := e.signUp(owner, true)
	p := e.do("POST", "/projects", map[string]string{"name": "Busy"}, user, "")
	pid, _ := uuid.Parse(p.str("id"))
	chatData(t, pid)

	d := e.doFrom(adminOrigin, "GET", "/admin/projects/"+p.str("id"), nil, admin, "")
	usage, _ := d.body["usage"].(map[string]any)
	proj, _ := d.body["project"].(map[string]any)
	if d.status != 200 || proj["owner_email"] != owner || usage["users"].(float64) != 2 || usage["messages"] == nil || d.body["push"] == nil {
		t.Fatalf("project detail: %d %s", d.status, d.raw)
	}

	// the promotion is in the admin's own log, and the platform log can be filtered and paged
	if !strings.Contains(e.doFrom(adminOrigin, "GET", "/me/audit", nil, admin, "").raw, "promoted_to_admin") {
		t.Fatal("promotion not audited")
	}
	all := e.doFrom(adminOrigin, "GET", "/admin/audit?limit=2", nil, admin, "")
	if all.status != 200 || len(all.body["events"].([]any)) != 2 || all.body["next_cursor"] == nil {
		t.Fatalf("audit page: %d %s", all.status, all.raw)
	}
	next := e.doFrom(adminOrigin, "GET", "/admin/audit?limit=2&cursor="+all.str("next_cursor"), nil, admin, "")
	first := all.body["events"].([]any)[1].(map[string]any)["id"]
	if next.status != 200 || len(next.body["events"].([]any)) == 0 || next.body["events"].([]any)[0].(map[string]any)["id"] == first {
		t.Fatalf("second page: %s", next.raw)
	}
	created := e.doFrom(adminOrigin, "GET", "/admin/audit?action=project_created", nil, admin, "")
	for _, ev := range created.body["events"].([]any) {
		if ev.(map[string]any)["action"] != "project_created" || ev.(map[string]any)["account_email"] == "" {
			t.Fatalf("filtered audit: %s", created.raw)
		}
	}
	if r := e.doFrom(adminOrigin, "GET", "/admin/audit?cursor=nope", nil, admin, ""); r.status != 400 {
		t.Fatalf("bad cursor: %d", r.status)
	}

	s := e.doFrom(adminOrigin, "GET", "/admin/stats/series?days=7", nil, admin, "")
	days := s.body["days"].([]any)
	if s.status != 200 || len(days) != 7 {
		t.Fatalf("series: %d %s", s.status, s.raw)
	}
	today := days[6].(map[string]any)
	if today["accounts"].(float64) < 2 || today["projects"].(float64) < 1 || today["messages"].(float64) < 1 || today["active_users"].(float64) < 1 {
		t.Fatalf("today: %v", today)
	}
	top := s.body["top_projects"].([]any)
	if len(top) == 0 || top[0].(map[string]any)["messages"].(float64) < 1 {
		t.Fatalf("top projects: %s", s.raw)
	}
	if r := e.doFrom(adminOrigin, "GET", "/admin/stats/series?days=12", nil, admin, ""); r.status != 400 {
		t.Fatalf("odd period: %d", r.status)
	}
}

func TestEndOneOfYourSessions(t *testing.T) {
	e := newEnv(t, 3)
	email := uniqueEmail("dev")
	laptop := e.signUp(email, false)
	phone := e.login(email, "a-good-password")

	var phoneID string
	for _, s := range e.do("GET", "/me/sessions", nil, laptop, "").body["sessions"].([]any) {
		if m := s.(map[string]any); m["current"] != true {
			phoneID = m["id"].(string)
		}
	}
	if r := e.do("DELETE", "/me/sessions/"+phoneID, nil, laptop, ""); r.status != 204 || r.cookie() != nil {
		t.Fatalf("end the phone: %d %v", r.status, r.header)
	}
	if r := e.do("GET", "/me", nil, phone, ""); r.status != 401 {
		t.Fatalf("the phone is still signed in: %d", r.status)
	}
	if r := e.do("GET", "/me", nil, laptop, ""); r.status != 200 {
		t.Fatalf("the laptop lost its session: %d", r.status)
	}
	// someone else's session id: not found
	other := e.signUp(uniqueEmail("x"), false)
	otherID := e.do("GET", "/me/sessions", nil, other, "").body["sessions"].([]any)[0].(map[string]any)["id"].(string)
	if r := e.do("DELETE", "/me/sessions/"+otherID, nil, laptop, ""); r.status != 404 {
		t.Fatalf("ended another account's session: %d", r.status)
	}
	// ending the current one is a sign out: the cookie goes too
	var laptopID string
	for _, s := range e.do("GET", "/me/sessions", nil, laptop, "").body["sessions"].([]any) {
		laptopID = s.(map[string]any)["id"].(string)
	}
	if r := e.do("DELETE", "/me/sessions/"+laptopID, nil, laptop, ""); r.status != 204 || r.cookie() == nil || r.cookie().MaxAge >= 0 {
		t.Fatalf("end the current session: %d %v", r.status, r.header)
	}
}

func TestClosedRegistration(t *testing.T) {
	pool := testdb.Pool(t)
	box, _ := secretbox.New(bytes.Repeat([]byte{9}, 32))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	q := store.New(pool)
	svc := NewService(q, tenant.NewService(q, box), &captureMailer{}, nil, log, Options{BaseURL: "https://marpokat.test", RegistrationClosed: true})
	srv := httptest.NewServer(NewHandler(svc, HandlerOptions{PortalOrigins: []string{portalOrigin}, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}, log).Routes())
	t.Cleanup(srv.Close)
	e := &env{t: t, srv: srv, svc: svc}

	if r := e.do("GET", "/status", nil, "", ""); r.status != 200 || r.body["registration_open"] != false {
		t.Fatalf("status: %d %s", r.status, r.raw)
	}
	if r := e.do("POST", "/auth/register", map[string]string{"email": uniqueEmail("late"), "password": "a-good-password", "name": "Late"}, "", ""); r.status != 403 || r.code() != CodeRegistrationClosed {
		t.Fatalf("register while closed: %d %s", r.status, r.raw)
	}
	open := newEnv(t, 3)
	if r := open.do("GET", "/status", nil, "", ""); r.body["registration_open"] != true {
		t.Fatalf("open status: %s", r.raw)
	}
}
