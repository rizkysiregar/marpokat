//go:build integration

package console

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/tenant"
	"github.com/rizkysiregar/marpokat/server/internal/testdb"
)

func TestDeleteAndRestoreAProject(t *testing.T) {
	e := newEnv(t, 2)
	a := e.signUp(uniqueEmail("del"), true)
	p := e.do("POST", "/projects", map[string]string{"name": "Doomed"}, a, "")
	id, key := p.str("id"), p.str("api_key")

	if r := e.do("DELETE", "/projects/"+id, map[string]string{"password": "wrong-password-x"}, a, ""); r.status != 403 || r.code() != CodeWrongPassword {
		t.Fatalf("delete without the password: %d %s", r.status, r.raw)
	}
	r := e.do("DELETE", "/projects/"+id, map[string]string{"password": "a-good-password"}, a, "")
	if r.status != 202 || r.body["disabled"] != true || r.body["delete_after"] == nil {
		t.Fatalf("delete: %d %s", r.status, r.raw)
	}
	after, _ := time.Parse(time.RFC3339Nano, r.str("delete_after"))
	if d := time.Until(after); d < 71*time.Hour || d > 73*time.Hour {
		t.Fatalf("grace period: %s", d)
	}
	if _, _, err := e.tenant.Credentials(context.Background(), key); err == nil {
		t.Fatal("a project waiting to be deleted still authenticates")
	}
	// asking again keeps the date and needs no password
	if again := e.do("DELETE", "/projects/"+id, map[string]string{"password": ""}, a, ""); again.status != 202 || again.str("delete_after") != r.str("delete_after") {
		t.Fatalf("delete twice: %d %s", again.status, again.raw)
	}
	// it cannot be switched on, by its owner or an admin, until it is restored
	if r := e.do("POST", "/projects/"+id+"/disable", map[string]any{"disabled": false, "password": "a-good-password"}, a, ""); r.status != 409 || r.code() != CodeDeletionScheduled {
		t.Fatalf("switch on while scheduled: %d %s", r.status, r.raw)
	}
	// it does not count against the limit (2): two more projects fit
	for _, n := range []string{"One", "Two"} {
		if r := e.do("POST", "/projects", map[string]string{"name": n}, a, ""); r.status != 201 {
			t.Fatalf("project %s: %d %s", n, r.status, r.raw)
		}
	}
	// the list says when it goes
	found := false
	for _, x := range e.do("GET", "/projects", nil, a, "").body["projects"].([]any) {
		if m := x.(map[string]any); m["id"] == id {
			found = m["delete_after"] != nil
		}
	}
	if !found {
		t.Fatal("the list does not show the scheduled deletion")
	}

	// restore: off until switched on
	res := e.do("POST", "/projects/"+id+"/restore", nil, a, "")
	if res.status != 200 || res.body["delete_after"] != nil || res.body["disabled"] != true {
		t.Fatalf("restore: %d %s", res.status, res.raw)
	}
	if r := e.do("POST", "/projects/"+id+"/disable", map[string]any{"disabled": false, "password": "a-good-password"}, a, ""); r.status != 200 {
		t.Fatalf("switch on after restore: %d %s", r.status, r.raw)
	}
	if _, _, err := e.tenant.Credentials(context.Background(), key); err != nil {
		t.Fatalf("restored project does not authenticate: %v", err)
	}

	// somebody else's project: not found
	b := e.signUp(uniqueEmail("oth"), true)
	if r := e.do("DELETE", "/projects/"+id, map[string]string{"password": "a-good-password"}, b, ""); r.status != 404 {
		t.Fatalf("delete another's project: %d", r.status)
	}
	audit := e.do("GET", "/me/audit", nil, a, "").raw
	for _, want := range []string{"project_deletion_scheduled", "project_restored"} {
		if !strings.Contains(audit, want) {
			t.Errorf("audit lacks %s", want)
		}
	}
}

type fakeObjects struct {
	mu       sync.Mutex
	prefixes []string
}

func (f *fakeObjects) RemovePrefix(_ context.Context, prefix string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prefixes = append(f.prefixes, prefix)
	return 3, nil
}

// chatData writes one of everything a tenant can own, the way the chat API would.
func chatData(t *testing.T, tenantID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	pool := testdb.Pool(t)
	userA, userB, ch, msg, att := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, tenant_id, external_id, name) VALUES ($1, $2, $3, 'A'), ($4, $2, $5, 'B')`, []any{userA, tenantID, "a-" + userA.String(), userB, "b-" + userB.String()}},
		{`INSERT INTO channels (id, tenant_id, type, name, created_by) VALUES ($1, $2, 'group', 'g', $3)`, []any{ch, tenantID, userA}},
		{`INSERT INTO channel_members (channel_id, user_id, tenant_id, role) VALUES ($1, $2, $3, 'owner'), ($1, $4, $3, 'member')`, []any{ch, userA, tenantID, userB}},
		{`INSERT INTO messages (channel_id, seq, id, tenant_id, sender_id, type, text, rev_seq) VALUES ($1, 1, $2, $3, $4, 'text', 'hi', 1)`, []any{ch, msg, tenantID, userA}},
		{`INSERT INTO message_reactions (message_id, user_id, emoji) VALUES ($1, $2, '🥑')`, []any{msg, userB}},
		{`INSERT INTO attachments (id, tenant_id, uploader_id, object_key, mime, size, status) VALUES ($1, $2, $3, $4, 'image/png', 10, 'ready')`, []any{att, tenantID, userA, tenantID.String() + "/" + att.String()}},
		{`INSERT INTO message_attachments (message_id, attachment_id, position) VALUES ($1, $2, 0)`, []any{msg, att}},
		{`INSERT INTO devices (tenant_id, push_token, user_id, platform) VALUES ($1, $2, $3, 'android')`, []any{tenantID, "tok-" + userA.String(), userA}},
		{`INSERT INTO outbox (tenant_id, topic, event_id, payload) VALUES ($1, 'message.created', $2, '{}')`, []any{tenantID, uuid.New()}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
}

func countOf(t *testing.T, tenantID uuid.UUID) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"tenants", "users", "channels", "channel_members", "messages", "attachments", "devices", "outbox"} {
		col := "tenant_id"
		if table == "tenants" {
			col = "id"
		}
		var n int
		if err := testdb.Pool(t).QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE "+col+" = $1", tenantID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[table] = n
	}
	return out
}

func TestThePurgeRemovesADeletedProjectAndNothingElse(t *testing.T) {
	e := newEnv(t, 3)
	a := e.signUp(uniqueEmail("pur"), true)
	gone := e.do("POST", "/projects", map[string]string{"name": "Gone"}, a, "")
	kept := e.do("POST", "/projects", map[string]string{"name": "Kept"}, a, "")
	goneID, _ := uuid.Parse(gone.str("id"))
	keptID, _ := uuid.Parse(kept.str("id"))
	chatData(t, goneID)
	chatData(t, keptID)

	if r := e.do("DELETE", "/projects/"+gone.str("id"), map[string]string{"password": "a-good-password"}, a, ""); r.status != 202 {
		t.Fatalf("delete: %d %s", r.status, r.raw)
	}
	objects := &fakeObjects{}
	purger := &tenant.Purger{Pool: testdb.Pool(t), Objects: objects, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// within the grace period nothing happens
	if n, err := purger.Once(context.Background()); err != nil || countOf(t, goneID)["tenants"] != 1 {
		t.Fatalf("purged too early: %d %v", n, err)
	}
	if _, err := testdb.Pool(t).Exec(context.Background(), `UPDATE tenants SET delete_after = now() - interval '1 second' WHERE id = $1`, goneID); err != nil {
		t.Fatal(err)
	}
	n, err := purger.Once(context.Background())
	if err != nil || n < 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	for table, c := range countOf(t, goneID) {
		if c != 0 {
			t.Errorf("%s still has %d rows of the deleted project", table, c)
		}
	}
	if len(objects.prefixes) != 1 || objects.prefixes[0] != goneID.String()+"/" {
		t.Errorf("files removed: %v", objects.prefixes)
	}
	// the other project of the same account is untouched
	for table, c := range countOf(t, keptID) {
		if c == 0 {
			t.Errorf("%s of the kept project was emptied", table)
		}
	}
	if r := e.do("GET", "/projects/"+gone.str("id"), nil, a, ""); r.status != 404 {
		t.Fatalf("purged project still there: %d", r.status)
	}
	if !strings.Contains(e.do("GET", "/me/audit", nil, a, "").raw, "project_deleted") {
		t.Fatal("the owner's log does not say the project was deleted")
	}
	// nothing more to do
	if n, err := purger.Once(context.Background()); err != nil || n != 0 {
		t.Fatalf("second round: %d %v", n, err)
	}
}
