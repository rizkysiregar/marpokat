//go:build integration

package console

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/tenant"
	"github.com/rizkysiregar/marpokat/server/internal/testdb"
)

func TestAdminSuspendsAnAccountWithItsProjects(t *testing.T) {
	e := newEnv(t, 3)
	admin := e.adminSession(uniqueEmail("boss"))
	email := uniqueEmail("abuser")
	owner := e.signUp(email, true)
	uid := e.do("GET", "/me", nil, owner, "").str("id")
	a := e.do("POST", "/projects", map[string]string{"name": "A"}, owner, "")
	b := e.do("POST", "/projects", map[string]string{"name": "B"}, owner, "")
	// A was suspended on its own before
	e.doFrom(adminOrigin, "POST", "/admin/projects/"+a.str("id")+"/suspend", map[string]string{"reason": "spam in A"}, admin, "")

	if r := e.doFrom(adminOrigin, "POST", "/admin/accounts/"+uid+"/suspend", map[string]string{"reason": ""}, admin, ""); r.status != 400 {
		t.Fatalf("no reason: %d", r.status)
	}
	r := e.doFrom(adminOrigin, "POST", "/admin/accounts/"+uid+"/suspend", map[string]string{"reason": "Abuse of the service"}, admin, "")
	if r.status != 200 || r.body["suspended"] != true || r.str("suspended_reason") != "Abuse of the service" {
		t.Fatalf("suspend: %d %s", r.status, r.raw)
	}
	if me := e.do("GET", "/me", nil, owner, ""); me.status != 401 {
		t.Fatalf("session survived: %d", me.status)
	}
	login := e.do("POST", "/auth/login", map[string]string{"email": email, "password": "a-good-password"}, "", "10.20.0.1")
	if login.status != 403 || login.code() != CodeAccountDisabled || !strings.Contains(login.raw, "Abuse of the service") {
		t.Fatalf("login of a suspended account: %d %s", login.status, login.raw)
	}
	for _, key := range []string{a.str("api_key"), b.str("api_key")} {
		if _, _, err := e.tenant.Credentials(context.Background(), key); err == nil {
			t.Fatal("a project of a suspended account authenticates")
		}
	}
	e.mail.to(t, email, "account is suspended")
	// the project stopped with its account cannot be released on its own
	if r := e.doFrom(adminOrigin, "POST", "/admin/projects/"+b.str("id")+"/unsuspend", nil, admin, ""); r.status != 409 {
		t.Fatalf("unsuspend a project stopped with its account: %d %s", r.status, r.raw)
	}

	if r := e.doFrom(adminOrigin, "POST", "/admin/accounts/"+uid+"/unsuspend", nil, admin, ""); r.status != 200 || r.body["suspended"] != false {
		t.Fatalf("unsuspend: %d %s", r.status, r.raw)
	}
	e.mail.to(t, email, "no longer suspended")
	again := e.login(email, "a-good-password")
	if _, _, err := e.tenant.Credentials(context.Background(), b.str("api_key")); err != nil {
		t.Fatalf("B did not come back with its account: %v", err)
	}
	if pa := e.do("GET", "/projects/"+a.str("id"), nil, again, ""); pa.body["suspended"] != true || pa.str("suspended_reason") != "spam in A" {
		t.Fatalf("A's own suspension was lifted with the account: %s", pa.raw)
	}
}

func TestOwnerDeletesTheirAccountWithTheGracePeriod(t *testing.T) {
	e := newEnv(t, 3)
	email := uniqueEmail("leaving")
	owner := e.signUp(email, true)
	p := e.do("POST", "/projects", map[string]string{"name": "Mine"}, owner, "")

	if r := e.do("DELETE", "/me", map[string]string{"confirm": "other@example.com", "password": "a-good-password"}, owner, ""); r.status != 400 {
		t.Fatalf("wrong e-mail: %d", r.status)
	}
	if r := e.do("DELETE", "/me", map[string]string{"confirm": email, "password": "wrong-password-1"}, owner, ""); r.status != 403 {
		t.Fatalf("wrong password: %d", r.status)
	}
	r := e.do("DELETE", "/me", map[string]string{"confirm": strings.ToUpper(email), "password": "a-good-password"}, owner, "")
	at, _ := time.Parse(time.RFC3339Nano, r.str("delete_after"))
	if r.status != 202 || time.Until(at) < 71*time.Hour {
		t.Fatalf("delete my account: %d %s", r.status, r.raw)
	}
	if _, _, err := e.tenant.Credentials(context.Background(), p.str("api_key")); err == nil {
		t.Fatal("a project of an account being deleted still authenticates")
	}
	if !strings.Contains(e.mail.to(t, email, "will be deleted").Text, "/login") {
		t.Fatal("the mail does not say how to restore")
	}
	// still signed in, can sign in again, cannot create projects
	if me := e.do("GET", "/me", nil, owner, ""); me.status != 200 || me.body["delete_after"] == nil {
		t.Fatalf("me while deleting: %s", me.raw)
	}
	e.login(email, "a-good-password")
	if r := e.do("POST", "/projects", map[string]string{"name": "New"}, owner, ""); r.status != 409 || r.code() != CodeAccountDeletionScheduled {
		t.Fatalf("create while deleting: %d %s", r.status, r.raw)
	}
	if g := e.do("GET", "/projects/"+p.str("id"), nil, owner, ""); g.body["suspended"] != true {
		t.Fatalf("project not stopped: %s", g.raw)
	}

	if r := e.do("POST", "/me/restore", nil, owner, ""); r.status != 200 || r.body["delete_after"] != nil {
		t.Fatalf("restore: %d %s", r.status, r.raw)
	}
	if _, _, err := e.tenant.Credentials(context.Background(), p.str("api_key")); err != nil {
		t.Fatalf("project did not come back: %v", err)
	}
}

func TestAdminDeletesAnAccountNowWithEverythingInIt(t *testing.T) {
	e := newEnv(t, 3)
	admin := e.adminSession(uniqueEmail("boss"))
	email := uniqueEmail("gone")
	owner := e.signUp(email, true)
	uid := e.do("GET", "/me", nil, owner, "").str("id")
	p := e.do("POST", "/projects", map[string]string{"name": "Chatty"}, owner, "")
	pid, _ := uuid.Parse(p.str("id"))
	chatData(t, pid)

	path := "/admin/accounts/" + uid + "/delete"
	if r := e.doFrom(adminOrigin, "POST", path, map[string]any{"confirm": "x@example.com", "password": "a-good-password", "now": true}, admin, ""); r.status != 400 {
		t.Fatalf("wrong e-mail: %d", r.status)
	}
	r := e.doFrom(adminOrigin, "POST", path, map[string]any{"confirm": email, "password": "a-good-password", "now": true}, admin, "")
	if r.status != 200 || r.body["delete_after"] == nil {
		t.Fatalf("delete now: %d %s", r.status, r.raw)
	}
	if me := e.do("GET", "/me", nil, owner, ""); me.status != 401 {
		t.Fatalf("session survived: %d", me.status)
	}
	if !strings.Contains(e.mail.to(t, email, "account is deleted").Text, "within a few minutes") {
		t.Fatal("owner not told")
	}
	if r := e.doFrom(adminOrigin, "POST", "/admin/accounts/"+uid+"/restore", nil, admin, ""); r.status != 409 {
		t.Fatalf("restore an account being removed: %d", r.status)
	}

	purger := &tenant.Purger{Pool: testdb.Pool(t), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if n, err := purger.Once(context.Background()); err != nil || n < 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	for table, c := range countOf(t, pid) {
		if c != 0 {
			t.Errorf("%s keeps %d rows of the deleted account's project", table, c)
		}
	}
	var n int
	if err := testdb.Pool(t).QueryRow(context.Background(), `SELECT count(*) FROM accounts WHERE id = $1`, uid).Scan(&n); err != nil || n != 0 {
		t.Fatalf("account row: %d %v", n, err)
	}
	if err := testdb.Pool(t).QueryRow(context.Background(), `SELECT count(*) FROM console_sessions WHERE account_id = $1`, uid).Scan(&n); err != nil || n != 0 {
		t.Fatalf("sessions left: %d %v", n, err)
	}
	// the address is free again
	e.signUp(email, false)
	if !strings.Contains(e.doFrom(adminOrigin, "GET", "/admin/audit?action=account_deleted_now_by_admin", nil, admin, "").raw, email) {
		t.Fatal("admin audit lacks the deletion")
	}
}

func TestAdminDeletesAnAccountAfterTheGracePeriodAndRestoresIt(t *testing.T) {
	e := newEnv(t, 3)
	admin := e.adminSession(uniqueEmail("boss"))
	email := uniqueEmail("maybe")
	owner := e.signUp(email, true)
	uid := e.do("GET", "/me", nil, owner, "").str("id")
	r := e.doFrom(adminOrigin, "POST", "/admin/accounts/"+uid+"/delete", map[string]any{"confirm": email, "password": "a-good-password"}, admin, "")
	at, _ := time.Parse(time.RFC3339Nano, r.str("delete_after"))
	if r.status != 200 || time.Until(at) < 71*time.Hour {
		t.Fatalf("delete with grace: %d %s", r.status, r.raw)
	}
	if r := e.doFrom(adminOrigin, "POST", "/admin/accounts/"+uid+"/restore", nil, admin, ""); r.status != 200 || r.body["delete_after"] != nil {
		t.Fatalf("restore: %d %s", r.status, r.raw)
	}
	e.login(email, "a-good-password")
}

func TestAdminCannotActOnAnotherAdminOrThemself(t *testing.T) {
	e := newEnv(t, 3)
	adminEmail := uniqueEmail("boss")
	admin := e.adminSession(adminEmail)
	otherEmail := uniqueEmail("boss2")
	e.adminSession(otherEmail)
	other := e.login(otherEmail, "a-good-password")
	otherID := e.do("GET", "/me", nil, other, "").str("id")
	selfID := e.doFrom(adminOrigin, "GET", "/me", nil, admin, "").str("id")
	for _, c := range []struct {
		path string
		body any
	}{
		{"/suspend", map[string]string{"reason": "x"}},
		{"/delete", map[string]any{"confirm": otherEmail, "password": "a-good-password", "now": true}},
	} {
		if r := e.doFrom(adminOrigin, "POST", "/admin/accounts/"+otherID+c.path, c.body, admin, ""); r.status != 403 || r.code() != CodeTargetIsAdmin {
			t.Errorf("%s another admin: %d %s", c.path, r.status, r.raw)
		}
		if r := e.doFrom(adminOrigin, "POST", "/admin/accounts/"+selfID+c.path, c.body, admin, ""); r.status != 400 {
			t.Errorf("%s self: %d %s", c.path, r.status, r.raw)
		}
	}
}
