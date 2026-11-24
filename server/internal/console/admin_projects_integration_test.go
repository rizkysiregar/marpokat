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

func TestProjectLabels(t *testing.T) {
	e := newEnv(t, 3)
	a := e.signUp(uniqueEmail("lbl"), true)
	p := e.do("POST", "/projects", map[string]string{"name": "Labelled", "environment": "Production"}, a, "")
	if p.status != 201 || p.str("environment") != "production" {
		t.Fatalf("create with a label: %d %s", p.status, p.raw)
	}
	if r := e.do("POST", "/projects", map[string]string{"name": "Bad", "environment": "qa"}, a, ""); r.status != 400 {
		t.Fatalf("unknown label: %d", r.status)
	}
	id := p.str("id")
	if r := e.do("PATCH", "/projects/"+id, map[string]any{"environment": "staging"}, a, ""); r.str("environment") != "staging" {
		t.Fatalf("change: %s", r.raw)
	}
	if r := e.do("PATCH", "/projects/"+id, map[string]any{"environment": ""}, a, ""); r.status != 200 || r.body["environment"] != nil {
		t.Fatalf("clear: %s", r.raw)
	}
	if r := e.do("POST", "/projects", map[string]string{"name": "Plain"}, a, ""); r.body["environment"] != nil {
		t.Fatalf("no label: %s", r.raw)
	}
}

func TestAdminSuspendsAProjectAndTheOwnerCannotLiftIt(t *testing.T) {
	e := newEnv(t, 3)
	admin := e.adminSession(uniqueEmail("boss"))
	owner := uniqueEmail("own")
	a := e.signUp(owner, true)
	p := e.do("POST", "/projects", map[string]string{"name": "Spammy"}, a, "")
	id, key := p.str("id"), p.str("api_key")

	if r := e.doFrom(adminOrigin, "POST", "/admin/projects/"+id+"/suspend", map[string]string{"reason": " "}, admin, ""); r.status != 400 {
		t.Fatalf("suspend without a reason: %d", r.status)
	}
	r := e.doFrom(adminOrigin, "POST", "/admin/projects/"+id+"/suspend", map[string]string{"reason": "Sends spam to strangers"}, admin, "")
	if r.status != 200 || r.body["suspended"] != true || r.str("suspended_reason") != "Sends spam to strangers" || r.body["disabled"] != false {
		t.Fatalf("suspend: %d %s", r.status, r.raw)
	}
	if _, _, err := e.tenant.Credentials(context.Background(), key); err == nil {
		t.Fatal("a suspended project still authenticates")
	}
	mail := e.mail.to(t, owner, "is suspended")
	if !strings.Contains(mail.Text, "Sends spam to strangers") || !strings.Contains(mail.Text, "/app/projects/"+id) {
		t.Fatalf("owner mail: %s", mail.Text)
	}
	// the owner sees why, and cannot switch it on
	if g := e.do("GET", "/projects/"+id, nil, a, ""); g.body["suspended"] != true || g.str("suspended_reason") == "" {
		t.Fatalf("owner view: %s", g.raw)
	}
	e.do("POST", "/projects/"+id+"/disable", map[string]any{"disabled": true, "password": "a-good-password"}, a, "")
	if r := e.do("POST", "/projects/"+id+"/disable", map[string]any{"disabled": false, "password": "a-good-password"}, a, ""); r.status != 409 || r.code() != CodeProjectSuspended {
		t.Fatalf("owner lifts the suspension: %d %s", r.status, r.raw)
	}
	// lifted by the admin: still off, since the owner had switched it off meanwhile
	u := e.doFrom(adminOrigin, "POST", "/admin/projects/"+id+"/unsuspend", nil, admin, "")
	if u.status != 200 || u.body["suspended"] != false || u.body["disabled"] != true {
		t.Fatalf("unsuspend: %d %s", u.status, u.raw)
	}
	e.mail.to(t, owner, "no longer suspended")
	if r := e.do("POST", "/projects/"+id+"/disable", map[string]any{"disabled": false, "password": "a-good-password"}, a, ""); r.status != 200 {
		t.Fatalf("switch on after the suspension: %d %s", r.status, r.raw)
	}
	if _, _, err := e.tenant.Credentials(context.Background(), key); err != nil {
		t.Fatalf("does not authenticate after: %v", err)
	}
	// the owner may still delete a suspended project of theirs
	e.doFrom(adminOrigin, "POST", "/admin/projects/"+id+"/suspend", map[string]string{"reason": "again"}, admin, "")
	if r := e.do("DELETE", "/projects/"+id, map[string]string{"password": "a-good-password"}, a, ""); r.status != 202 {
		t.Fatalf("owner deletes a suspended project: %d %s", r.status, r.raw)
	}
	logs := e.do("GET", "/me/audit", nil, a, "").raw
	for _, want := range []string{"project_suspended", "project_unsuspended"} {
		if !strings.Contains(logs, want) {
			t.Errorf("owner audit lacks %s", want)
		}
	}
}

func TestAdminDeletesAProjectNowOrAfterTheGracePeriod(t *testing.T) {
	e := newEnv(t, 3)
	admin := e.adminSession(uniqueEmail("boss"))
	owner := uniqueEmail("own")
	a := e.signUp(owner, true)
	later := e.do("POST", "/projects", map[string]string{"name": "Later"}, a, "")
	now := e.do("POST", "/projects", map[string]string{"name": "Now"}, a, "")
	nowID, _ := uuid.Parse(now.str("id"))
	chatData(t, nowID)

	path := "/admin/projects/" + now.str("id") + "/delete"
	if r := e.doFrom(adminOrigin, "POST", path, map[string]any{"confirm": "wrong", "password": "a-good-password", "now": true}, admin, ""); r.status != 400 {
		t.Fatalf("wrong slug: %d", r.status)
	}
	if r := e.doFrom(adminOrigin, "POST", path, map[string]any{"confirm": now.str("slug"), "password": "nope-nope-nope", "now": true}, admin, ""); r.status != 403 || r.code() != CodeWrongPassword {
		t.Fatalf("wrong admin password: %d %s", r.status, r.raw)
	}
	r := e.doFrom(adminOrigin, "POST", path, map[string]any{"confirm": now.str("slug"), "password": "a-good-password", "now": true}, admin, "")
	at, _ := time.Parse(time.RFC3339Nano, r.str("delete_after"))
	if r.status != 200 || time.Until(at) > time.Second || r.body["disabled"] != true {
		t.Fatalf("delete now: %d %s", r.status, r.raw)
	}
	if !strings.Contains(e.mail.to(t, owner, "is deleted").Text, "within a few minutes") {
		t.Fatal("owner not told the project goes now")
	}
	if r := e.doFrom(adminOrigin, "POST", "/admin/projects/"+now.str("id")+"/restore", nil, admin, ""); r.status != 409 {
		t.Fatalf("restore a project being removed: %d", r.status)
	}
	purger := &tenant.Purger{Pool: testdb.Pool(t), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if n, err := purger.Once(context.Background()); err != nil || n < 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	for table, c := range countOf(t, nowID) {
		if c != 0 {
			t.Errorf("%s keeps %d rows", table, c)
		}
	}

	// after the grace period: restorable by the admin
	lp := "/admin/projects/" + later.str("id")
	g := e.doFrom(adminOrigin, "POST", lp+"/delete", map[string]any{"confirm": later.str("slug"), "password": "a-good-password"}, admin, "")
	at, _ = time.Parse(time.RFC3339Nano, g.str("delete_after"))
	if g.status != 200 || time.Until(at) < 71*time.Hour {
		t.Fatalf("delete with grace: %d %s", g.status, g.raw)
	}
	if r := e.doFrom(adminOrigin, "POST", lp+"/restore", nil, admin, ""); r.status != 200 || r.body["delete_after"] != nil {
		t.Fatalf("restore: %d %s", r.status, r.raw)
	}
	if !strings.Contains(e.doFrom(adminOrigin, "GET", "/admin/audit?action=project_deleted_now_by_admin", nil, admin, "").raw, now.str("slug")) {
		t.Fatal("admin audit lacks the delete")
	}
}

func TestAdminCannotActOnAnotherAdminsProject(t *testing.T) {
	e := newEnv(t, 3)
	admin := e.adminSession(uniqueEmail("boss"))
	otherEmail := uniqueEmail("boss2")
	e.adminSession(otherEmail)
	other := e.login(otherEmail, "a-good-password")
	p := e.do("POST", "/projects", map[string]string{"name": "Theirs"}, other, "")
	id := p.str("id")

	if d := e.doFrom(adminOrigin, "GET", "/admin/projects/"+id, nil, admin, ""); d.body["owner_is_admin"] != true {
		t.Fatalf("detail: %s", d.raw)
	}
	for _, c := range []struct {
		path string
		body any
	}{
		{"/suspend", map[string]string{"reason": "x"}},
		{"/unsuspend", nil},
		{"/delete", map[string]any{"confirm": p.str("slug"), "password": "a-good-password", "now": true}},
		{"/restore", nil},
	} {
		if r := e.doFrom(adminOrigin, "POST", "/admin/projects/"+id+c.path, c.body, admin, ""); r.status != 403 || r.code() != CodeTargetIsAdmin {
			t.Errorf("%s on another admin's project: %d %s", c.path, r.status, r.raw)
		}
	}
	// once demoted, it is an ordinary project
	if err := e.svc.PromoteAdmin(context.Background(), otherEmail, false); err != nil {
		t.Fatal(err)
	}
	if r := e.doFrom(adminOrigin, "POST", "/admin/projects/"+id+"/suspend", map[string]string{"reason": "x"}, admin, ""); r.status != 200 {
		t.Fatalf("after demotion: %d %s", r.status, r.raw)
	}
}
