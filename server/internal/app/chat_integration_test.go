//go:build integration

package app

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rizkysiregar/marpokat/server/internal/tenant"
)

func TestDirectMessages(t *testing.T) {
	e := newEnv(t)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob", "carol")

	// the server API makes DMs: once per pair, in either order
	r := e.asServer(tt, "POST", "/v1/server/channels/dm", map[string]any{"user_a": "alice", "user_b": "bob"})
	if r.status != 201 || r.at("type") != "dm" || r.at("name") != nil || num(r.at("member_count")) != 2 {
		t.Fatalf("create dm: %d %v", r.status, r.body)
	}
	ch := str(r.at("id"))
	if r = e.asServer(tt, "POST", "/v1/server/channels/dm", map[string]any{"user_a": "bob", "user_b": "alice"}); r.status != 200 || str(r.at("id")) != ch {
		t.Fatalf("same pair again: %d %v", r.status, r.body)
	}
	if r = e.asServer(tt, "POST", "/v1/server/channels/dm", map[string]any{"user_a": "alice", "user_b": "alice"}); r.status != 400 {
		t.Fatalf("dm with yourself: %d %v", r.status, r.body)
	}
	if r = e.asServer(tt, "POST", "/v1/server/channels/dm", map[string]any{"user_a": "alice", "user_b": "nobody"}); r.status != 404 {
		t.Fatalf("dm with an unknown user: %d %v", r.status, r.body)
	}
	// this tenant does not let clients create DMs (PLAN D11)
	if r = e.asUser(tt, "alice", "POST", "/v1/channels", map[string]any{"type": "dm", "user_id": "carol"}); r.status != 403 {
		t.Fatalf("client created a dm: %d %v", r.status, r.body)
	}

	// sending: seq counts up, the same client_msg_id is the same message
	msg := e.asUser(tt, "alice", "POST", "/v1/channels/"+ch+"/messages", map[string]any{"client_msg_id": "c-1", "text": "  hello  "})
	if msg.status != 201 || num(msg.at("seq")) != 1 || num(msg.at("rev_seq")) != 1 || msg.at("text") != "hello" ||
		msg.at("type") != "text" || msg.at("sender", "external_id") != "alice" || msg.at("client_msg_id") != "c-1" {
		t.Fatalf("send: %d %v", msg.status, msg.body)
	}
	again := e.asUser(tt, "alice", "POST", "/v1/channels/"+ch+"/messages", map[string]any{"client_msg_id": "c-1", "text": "hello"})
	if again.status != 200 || again.at("id") != msg.at("id") || num(again.at("seq")) != 1 {
		t.Fatalf("resend: %d %v", again.status, again.body)
	}
	if r = e.say(tt, "bob", ch, "hi alice"); r.status != 201 || num(r.at("seq")) != 2 {
		t.Fatalf("second message: %d %v", r.status, r.body)
	}

	// strangers see nothing
	if r = e.asUser(tt, "carol", "GET", "/v1/channels/"+ch+"/messages", nil); r.status != 404 {
		t.Fatalf("history for a non-member: %d", r.status)
	}
	if r = e.say(tt, "carol", ch, "let me in"); r.status != 404 {
		t.Fatalf("send by a non-member: %d", r.status)
	}
	if r = e.asUser(tt, "carol", "GET", "/v1/channels/"+ch, nil); r.status != 404 {
		t.Fatalf("channel for a non-member: %d", r.status)
	}
	if r = e.asUser(tt, "alice", "GET", "/v1/channels/not-a-uuid", nil); r.status != 404 {
		t.Fatalf("bad id: %d", r.status)
	}

	// validation
	for name, body := range map[string]map[string]any{
		"empty text":      {"client_msg_id": "v1", "text": "   "},
		"no client id":    {"text": "x"},
		"long client id":  {"client_msg_id": strings.Repeat("x", 65), "text": "x"},
		"text too long":   {"client_msg_id": "v2", "text": strings.Repeat("é", 4001)},
		"attachments yet": {"client_msg_id": "v3", "text": "x", "attachment_ids": []string{"00000000-0000-0000-0000-000000000000"}},
	} {
		if r := e.asUser(tt, "alice", "POST", "/v1/channels/"+ch+"/messages", body); r.status != 400 || r.errCode() != "validation_failed" {
			t.Errorf("%s: %d %v", name, r.status, r.body)
		}
	}
	if r = e.say(tt, "alice", ch, strings.Repeat("é", 4000)); r.status != 201 { // exactly the limit, in characters
		t.Fatalf("text of the maximum length: %d %v", r.status, r.body)
	}

	// history: newest first, scroll back with before_seq
	r = e.asUser(tt, "bob", "GET", "/v1/channels/"+ch+"/messages?limit=2", nil)
	if num(r.at("messages", 0, "seq")) != 3 || num(r.at("messages", 1, "seq")) != 2 || r.at("has_more") != true {
		t.Fatalf("history page 1: %v", r.body)
	}
	r = e.asUser(tt, "bob", "GET", "/v1/channels/"+ch+"/messages?limit=2&before_seq=2", nil)
	if count(r.at("messages")) != 1 || num(r.at("messages", 0, "seq")) != 1 || r.at("has_more") != false {
		t.Fatalf("history page 2: %v", r.body)
	}
	if r = e.asUser(tt, "bob", "GET", "/v1/channels/"+ch+"/messages?limit=0", nil); r.status != 400 {
		t.Fatalf("limit 0: %d", r.status)
	}

	// sync: everything after the cursor, oldest first, and the cursor to continue from
	r = e.asUser(tt, "bob", "GET", "/v1/channels/"+ch+"/sync?since_rev=0", nil)
	if count(r.at("messages")) != 3 || num(r.at("messages", 0, "seq")) != 1 || num(r.at("next_rev")) != 3 || r.at("has_more") != false || num(r.at("channel_last_seq")) != 3 {
		t.Fatalf("sync from 0: %v", r.body)
	}
	r = e.asUser(tt, "bob", "GET", "/v1/channels/"+ch+"/sync?since_rev=0&limit=2", nil)
	if count(r.at("messages")) != 2 || num(r.at("next_rev")) != 2 || r.at("has_more") != true {
		t.Fatalf("sync in pages: %v", r.body)
	}
	r = e.asUser(tt, "bob", "GET", "/v1/channels/"+ch+"/sync?since_rev=3", nil)
	if count(r.at("messages")) != 0 || num(r.at("next_rev")) != 3 || r.at("has_more") != false {
		t.Fatalf("sync with nothing new: %v", r.body)
	}
	if r = e.asUser(tt, "bob", "GET", "/v1/channels/"+ch+"/sync", nil); r.status != 400 {
		t.Fatalf("sync without since_rev: %d", r.status)
	}

	// each side sees the other as the peer, and the DM has no name
	r = e.asUser(tt, "alice", "GET", "/v1/channels/"+ch, nil)
	if r.at("dm_peer", "external_id") != "bob" || r.at("name") != nil || r.at("membership", "role") != "member" || num(r.at("last_message_seq")) != 3 {
		t.Fatalf("channel as alice: %v", r.body)
	}
	if peer := e.asUser(tt, "bob", "GET", "/v1/channels/"+ch, nil).at("dm_peer", "external_id"); peer != "alice" {
		t.Fatalf("peer of bob: %v", peer)
	}

	// frozen: still readable, no longer writable
	if r = e.asServer(tt, "POST", "/v1/server/channels/"+ch+"/freeze", nil); r.status != 204 {
		t.Fatalf("freeze: %d", r.status)
	}
	if r = e.say(tt, "alice", ch, "still there?"); r.status != 403 || r.errCode() != "channel_frozen" {
		t.Fatalf("send to a frozen channel: %d %v", r.status, r.body)
	}
	if r = e.asUser(tt, "alice", "GET", "/v1/channels/"+ch, nil); r.status != 200 || r.at("frozen") != true {
		t.Fatalf("read a frozen channel: %d %v", r.status, r.body)
	}
	if r = e.asServer(tt, "POST", "/v1/server/channels/"+ch+"/messages", map[string]any{"text": "This chat was closed"}); r.status != 201 || r.at("type") != "system" || r.at("sender") != nil {
		t.Fatalf("system message into a frozen channel: %d %v", r.status, r.body)
	}
	if r = e.asServer(tt, "DELETE", "/v1/server/channels/"+ch+"/freeze", nil); r.status != 204 {
		t.Fatalf("unfreeze: %d", r.status)
	}
	e.mustSay(tt, "alice", ch, "back")
}

func TestUnreadAndRead(t *testing.T) {
	e := newEnv(t)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob")
	ch := e.dm(tt, "alice", "bob")

	for _, text := range []string{"one", "two", "three"} {
		e.mustSay(tt, "alice", ch, text)
	}
	if got := e.unread(tt, "bob", ch); got != 3 {
		t.Fatalf("bob unread = %d, want 3", got)
	}
	if got := e.unread(tt, "alice", ch); got != 0 {
		t.Fatalf("alice unread her own messages = %d, want 0", got)
	}

	mark := func(ext string, seq any) response {
		return e.asUser(tt, ext, "POST", "/v1/channels/"+ch+"/read", map[string]any{"seq": seq})
	}
	if r := mark("bob", 2); r.status != 204 {
		t.Fatalf("mark read: %d %v", r.status, r.body)
	}
	if got := e.unread(tt, "bob", ch); got != 1 {
		t.Fatalf("after reading 2: unread = %d, want 1", got)
	}
	mark("bob", 3)
	mark("bob", 1) // backwards: ignored
	if r := e.asUser(tt, "bob", "GET", "/v1/channels/"+ch, nil); num(r.at("membership", "last_read_seq")) != 3 || num(r.at("membership", "unread_count")) != 0 {
		t.Fatalf("read marker after reading everything: %v", r.body)
	}
	mark("bob", 999) // beyond the end: clamped, or new messages would count as read
	e.mustSay(tt, "alice", ch, "four")
	if got := e.unread(tt, "bob", ch); got != 1 {
		t.Fatalf("a message after an over-eager read = %d unread, want 1", got)
	}
	if r := mark("bob", -1); r.status != 400 {
		t.Fatalf("negative seq: %d", r.status)
	}
	if r := e.asUser(tt, "bob", "POST", "/v1/channels/"+ch+"/read", map[string]any{}); r.status != 400 {
		t.Fatalf("no seq: %d", r.status)
	}

	// replying marks the channel read up to your own message
	e.mustSay(tt, "bob", ch, "reply")
	if got := e.unread(tt, "bob", ch); got != 0 {
		t.Fatalf("unread after replying = %d, want 0", got)
	}
}

func TestGroups(t *testing.T) {
	e := newEnv(t)
	tt := e.createTenant(tenant.Config{ClientCanCreateGroup: true})
	e.seed(tt, "owner", "a", "b", "c", "d")

	create := e.asUser(tt, "owner", "POST", "/v1/channels", map[string]any{"type": "group", "name": "  Team  ", "member_ids": []string{"a", "b", "a"}})
	if create.status != 201 || create.at("type") != "group" || create.at("name") != "Team" || num(create.at("member_count")) != 3 ||
		create.at("membership", "role") != "owner" || create.at("last_message", "type") != "system" ||
		create.at("last_message", "metadata", "event") != "channel.created" {
		t.Fatalf("create group: %d %v", create.status, create.body)
	}
	ch := str(create.at("id"))
	if got := e.unread(tt, "a", ch); got != 0 {
		t.Fatalf("a system message counts as unread: %d", got)
	}
	for name, body := range map[string]map[string]any{
		"no name":      {"type": "group", "member_ids": []string{"a"}},
		"unknown user": {"type": "group", "name": "X", "member_ids": []string{"ghost"}},
		"no type":      {"name": "X"},
	} {
		if r := e.asUser(tt, "owner", "POST", "/v1/channels", body); r.status < 400 || r.status > 404 {
			t.Errorf("create group (%s): %d %v", name, r.status, r.body)
		}
	}
	if r := e.asUser(tt, "owner", "POST", "/v1/channels", map[string]any{"type": "dm", "user_id": "a"}); r.status != 403 {
		t.Fatalf("this tenant does not allow client DMs: %d", r.status)
	}

	// who may rename, who may add
	if r := e.asUser(tt, "a", "PATCH", "/v1/channels/"+ch, map[string]any{"name": "Mine now"}); r.status != 403 {
		t.Fatalf("member renamed the group: %d", r.status)
	}
	if r := e.asUser(tt, "owner", "PATCH", "/v1/channels/"+ch, map[string]any{}); r.status != 400 {
		t.Fatalf("empty update: %d", r.status)
	}
	if r := e.asUser(tt, "owner", "PATCH", "/v1/channels/"+ch, map[string]any{"name": "Crew"}); r.status != 200 || r.at("name") != "Crew" || r.at("last_message", "metadata", "event") != "channel.renamed" {
		t.Fatalf("owner renamed the group: %d %v", r.status, r.body)
	}
	if r := e.asUser(tt, "owner", "PATCH", "/v1/channels/"+ch, map[string]any{"avatar_url": "https://cdn.example.com/g.png"}); r.status != 200 || r.at("avatar_url") != "https://cdn.example.com/g.png" || r.at("name") != "Crew" {
		t.Fatalf("avatar: %d %v", r.status, r.body)
	}
	if r := e.asUser(tt, "owner", "PATCH", "/v1/channels/"+ch, map[string]any{"avatar_url": nil}); r.status != 200 || r.at("avatar_url") != nil {
		t.Fatalf("remove avatar: %d %v", r.status, r.body)
	}
	if r := e.asUser(tt, "a", "POST", "/v1/channels/"+ch+"/members", map[string]any{"user_ids": []string{"c"}}); r.status != 403 {
		t.Fatalf("member added a member: %d", r.status)
	}
	e.mustSay(tt, "a", ch, "before c joined")
	if r := e.asUser(tt, "owner", "POST", "/v1/channels/"+ch+"/members", map[string]any{"user_ids": []string{"c"}}); r.status != 200 || num(r.at("member_count")) != 4 {
		t.Fatalf("owner added c: %d %v", r.status, r.body)
	}
	history := func(ext string) []any {
		r := e.asUser(tt, ext, "GET", "/v1/channels/"+ch+"/messages?limit=100", nil)
		msgs, _ := r.at("messages").([]any)
		return msgs
	}
	before := len(history("owner"))
	e.asUser(tt, "owner", "POST", "/v1/channels/"+ch+"/members", map[string]any{"user_ids": []string{"c"}}) // already in: nothing to announce
	if got := len(history("owner")); got != before {
		t.Fatalf("adding an existing member announced something: %d -> %d messages", before, got)
	}
	// a new member starts caught up: what was said before they joined is not unread
	if got := e.unread(tt, "c", ch); got != 0 {
		t.Fatalf("new member has %d unread messages from before joining", got)
	}
	e.mustSay(tt, "a", ch, "after c joined")
	if got := e.unread(tt, "c", ch); got != 1 {
		t.Fatalf("new member unread after one new message = %d, want 1", got)
	}
	var r response
	if r := e.asUser(tt, "owner", "POST", "/v1/channels/"+ch+"/members", map[string]any{"user_ids": []string{"ghost"}}); r.status != 404 {
		t.Fatalf("add an unknown user: %d", r.status)
	}

	r = e.asUser(tt, "a", "GET", "/v1/channels/"+ch+"/members", nil)
	var exts []string
	for _, m := range r.at("members").([]any) {
		exts = append(exts, str(at(m, "user", "external_id")))
	}
	slices.Sort(exts)
	if strings.Join(exts, ",") != "a,b,c,owner" {
		t.Fatalf("members = %v", exts)
	}

	// leaving: gone for you, still announced by updated_since so a client can drop it
	since := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	if r := e.asUser(tt, "b", "DELETE", "/v1/channels/"+ch+"/members/me", nil); r.status != 204 {
		t.Fatalf("b leaves: %d %v", r.status, r.body)
	}
	for name, path := range map[string]string{"channel": "/v1/channels/" + ch, "history": "/v1/channels/" + ch + "/messages", "members": "/v1/channels/" + ch + "/members"} {
		if r := e.asUser(tt, "b", "GET", path, nil); r.status != 404 {
			t.Errorf("%s after leaving: %d", name, r.status)
		}
	}
	if r := e.say(tt, "b", ch, "wait"); r.status != 404 {
		t.Fatalf("send after leaving: %d", r.status)
	}
	if r := e.asUser(tt, "b", "GET", "/v1/channels", nil); count(r.at("channels")) != 0 {
		t.Fatalf("a left channel is in the list: %v", r.body)
	}
	r = e.asUser(tt, "b", "GET", "/v1/channels?updated_since="+url.QueryEscape(since), nil)
	if count(r.at("channels")) != 1 || r.at("channels", 0, "membership", "left_at") == nil {
		t.Fatalf("updated_since does not announce the left channel: %v", r.body)
	}
	if got := len(history("owner")); got < before+1 {
		t.Fatalf("leaving was not announced")
	}

	// the owner leaves: the oldest member takes over
	if r := e.asUser(tt, "owner", "DELETE", "/v1/channels/"+ch+"/members/me", nil); r.status != 204 {
		t.Fatalf("owner leaves: %d", r.status)
	}
	if r := e.asUser(tt, "a", "GET", "/v1/channels/"+ch, nil); r.at("membership", "role") != "owner" || num(r.at("member_count")) != 2 {
		t.Fatalf("a did not become the owner: %v", r.body)
	}
	if r := e.asUser(tt, "c", "DELETE", "/v1/channels/"+ch+"/members/a", nil); r.status != 403 {
		t.Fatalf("member removed the owner: %d", r.status)
	}
	if r := e.asUser(tt, "a", "DELETE", "/v1/channels/"+ch+"/members/c", nil); r.status != 204 {
		t.Fatalf("owner removed c: %d", r.status)
	}
	if r := e.asUser(tt, "a", "DELETE", "/v1/channels/"+ch+"/members/c", nil); r.status != 404 {
		t.Fatalf("remove c twice: %d", r.status)
	}

	// the tenant backend can do the same without being a member
	if r := e.asServer(tt, "POST", "/v1/server/channels/"+ch+"/members", map[string]any{"user_ids": []string{"d", "c"}}); r.status != 200 || num(r.at("member_count")) != 3 {
		t.Fatalf("server added members: %d %v", r.status, r.body)
	}
	if r := e.asUser(tt, "d", "DELETE", "/v1/channels/"+ch+"/members/c", nil); r.status != 403 {
		t.Fatalf("member removed another member: %d", r.status)
	}
	if r := e.asServer(tt, "DELETE", "/v1/server/channels/"+ch+"/members/d", nil); r.status != 204 {
		t.Fatalf("server removed d: %d", r.status)
	}
	if r := e.asServer(tt, "DELETE", "/v1/server/channels/"+ch+"/members/ghost", nil); r.status != 404 {
		t.Fatalf("server removed an unknown user: %d", r.status)
	}

	// the last member leaving ends the group
	e.asUser(tt, "a", "DELETE", "/v1/channels/"+ch+"/members/c", nil)
	if r := e.asUser(tt, "a", "DELETE", "/v1/channels/"+ch+"/members/me", nil); r.status != 204 {
		t.Fatalf("last member leaves: %d", r.status)
	}
	if r := e.asServer(tt, "POST", "/v1/server/channels/"+ch+"/freeze", nil); r.status != 404 {
		t.Fatalf("the ended group still exists: %d", r.status)
	}

	// the announcements, in order, as clients will word them
	hist := e.asUser(tt, "owner", "GET", "/v1/channels/"+ch+"/messages", nil)
	if hist.status != 404 {
		t.Fatalf("history of a group the owner left: %d", hist.status)
	}
}

func TestGroupHistoryAnnouncesChanges(t *testing.T) {
	e := newEnv(t)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "o", "a", "b")

	r := e.asServer(tt, "POST", "/v1/server/channels", map[string]any{"created_by": "o", "name": "Club", "member_ids": []string{"a"}})
	if r.status != 201 || num(r.at("member_count")) != 2 || r.at("type") != "group" {
		t.Fatalf("server created group: %d %v", r.status, r.body)
	}
	ch := str(r.at("id"))
	e.asServer(tt, "POST", "/v1/server/channels/"+ch+"/members", map[string]any{"user_ids": []string{"b"}})
	e.mustSay(tt, "a", ch, "hello club")
	e.asUser(tt, "b", "DELETE", "/v1/channels/"+ch+"/members/me", nil)
	e.asUser(tt, "o", "DELETE", "/v1/channels/"+ch+"/members/a", nil)

	var events []string
	hist := e.asUser(tt, "o", "GET", "/v1/channels/"+ch+"/sync?since_rev=0&limit=100", nil)
	for _, m := range hist.at("messages").([]any) {
		if str(at(m, "type")) == "system" {
			events = append(events, str(at(m, "metadata", "event")))
		} else {
			events = append(events, "text:"+str(at(m, "text")))
		}
	}
	want := "channel.created,member.added,text:hello club,member.left,member.removed"
	if strings.Join(events, ",") != want {
		t.Fatalf("history = %v, want %s", events, want)
	}
	removed := hist.at("messages", 4)
	if at(removed, "metadata", "user_id") != "a" || at(removed, "metadata", "actor_id") != "o" {
		t.Fatalf("who removed whom: %v", at(removed, "metadata"))
	}
}

func TestChannelListPagingAndSince(t *testing.T) {
	e := newEnv(t)
	tt := e.createTenant(tenant.Config{})
	peers := []string{"p1", "p2", "p3", "p4", "p5"}
	e.seed(tt, append([]string{"me"}, peers...)...)

	ids := map[string]string{}
	for _, p := range peers {
		ids[p] = e.dm(tt, "me", p)
	}
	// activity order, oldest first: p3, p1, p5 (p2 and p4 never get a message)
	for _, p := range []string{"p3", "p1", "p5"} {
		e.mustSay(tt, p, ids[p], "ping from "+p)
		time.Sleep(5 * time.Millisecond)
	}

	var seen []string
	cursor := ""
	for pageNo := 0; ; pageNo++ {
		path := "/v1/channels?limit=2"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		r := e.asUser(tt, "me", "GET", path, nil)
		if r.status != 200 {
			t.Fatalf("list page %d: %d %v", pageNo, r.status, r.body)
		}
		for _, c := range r.at("channels").([]any) {
			seen = append(seen, str(at(c, "dm_peer", "external_id")))
		}
		if r.at("next_cursor") == nil {
			break
		}
		cursor = str(r.at("next_cursor"))
		if pageNo > 5 {
			t.Fatal("paging does not end")
		}
	}
	if got := strings.Join(seen[:3], ","); got != "p5,p1,p3" {
		t.Fatalf("channels with messages come first, newest first: %v", seen)
	}
	if len(seen) != 5 || len(slices.Compact(slices.Sorted(slices.Values(seen)))) != 5 {
		t.Fatalf("every channel exactly once: %v", seen)
	}
	if r := e.asUser(tt, "me", "GET", "/v1/channels?cursor=garbage", nil); r.status != 400 {
		t.Fatalf("bad cursor: %d", r.status)
	}

	// updated_since: only what changed after the given moment
	all := e.asUser(tt, "me", "GET", "/v1/channels?limit=100", nil)
	var newest time.Time
	for _, c := range all.at("channels").([]any) {
		if ts, _ := time.Parse(time.RFC3339Nano, str(at(c, "updated_at"))); ts.After(newest) {
			newest = ts
		}
	}
	e.mustSay(tt, "p2", ids["p2"], "late news")
	r := e.asUser(tt, "me", "GET", "/v1/channels?updated_since="+url.QueryEscape(newest.Format(time.RFC3339Nano)), nil)
	if count(r.at("channels")) != 1 || r.at("channels", 0, "dm_peer", "external_id") != "p2" || num(r.at("channels", 0, "membership", "unread_count")) != 1 ||
		r.at("channels", 0, "last_message", "text") != "late news" {
		t.Fatalf("updated_since: %v", r.body)
	}
	if r := e.asUser(tt, "me", "GET", "/v1/channels?updated_since=yesterday", nil); r.status != 400 {
		t.Fatalf("bad updated_since: %d", r.status)
	}
}

func TestChatTenantIsolation(t *testing.T) {
	e := newEnv(t)
	a := e.createTenant(tenant.Config{ClientCanCreateGroup: true})
	b := e.createTenant(tenant.Config{ClientCanCreateGroup: true})
	e.seed(a, "alice", "bob")
	e.seed(b, "alice", "bob") // the same external ids, other people

	dmA := e.dm(a, "alice", "bob")
	e.mustSay(a, "alice", dmA, "secret of A")
	group := e.asUser(a, "alice", "POST", "/v1/channels", map[string]any{"type": "group", "name": "A only", "member_ids": []string{"bob"}})
	groupA := str(group.at("id"))

	// tenant B's users, using tenant A's ids, find nothing, and nothing of theirs changes
	for _, id := range []string{dmA, groupA} {
		for name, r := range map[string]response{
			"get":      e.asUser(b, "alice", "GET", "/v1/channels/"+id, nil),
			"history":  e.asUser(b, "alice", "GET", "/v1/channels/"+id+"/messages", nil),
			"sync":     e.asUser(b, "alice", "GET", "/v1/channels/"+id+"/sync?since_rev=0", nil),
			"send":     e.say(b, "alice", id, "intruder"),
			"read":     e.asUser(b, "alice", "POST", "/v1/channels/"+id+"/read", map[string]any{"seq": 1}),
			"members":  e.asUser(b, "alice", "GET", "/v1/channels/"+id+"/members", nil),
			"rename":   e.asUser(b, "alice", "PATCH", "/v1/channels/"+id, map[string]any{"name": "mine"}),
			"add":      e.asUser(b, "alice", "POST", "/v1/channels/"+id+"/members", map[string]any{"user_ids": []string{"bob"}}),
			"leave":    e.asUser(b, "alice", "DELETE", "/v1/channels/"+id+"/members/me", nil),
			"s-freeze": e.asServer(b, "POST", "/v1/server/channels/"+id+"/freeze", nil),
			"s-system": e.asServer(b, "POST", "/v1/server/channels/"+id+"/messages", map[string]any{"text": "x"}),
			"s-add":    e.asServer(b, "POST", "/v1/server/channels/"+id+"/members", map[string]any{"user_ids": []string{"bob"}}),
			"s-remove": e.asServer(b, "DELETE", "/v1/server/channels/"+id+"/members/alice", nil),
		} {
			if r.status != 404 {
				t.Errorf("tenant B %s on a channel of tenant A: %d %v", name, r.status, r.body)
			}
		}
	}
	if r := e.asUser(b, "alice", "GET", "/v1/channels", nil); count(r.at("channels")) != 0 {
		t.Fatalf("tenant B lists tenant A's channels: %v", r.body)
	}

	// the DM of B's alice and bob is another channel, and A is untouched
	dmB := e.dm(b, "alice", "bob")
	if dmB == dmA {
		t.Fatal("two tenants share a DM")
	}
	if r := e.asUser(a, "bob", "GET", "/v1/channels/"+dmA+"/messages", nil); count(r.at("messages")) != 1 || r.at("messages", 0, "text") != "secret of A" {
		t.Fatalf("tenant A's history changed: %v", r.body)
	}
}

func TestChatConcurrency(t *testing.T) {
	e := newEnv(t)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob")
	ch := e.dm(tt, "alice", "bob")

	// many sends at once: every one gets its own seq, with no gaps and no duplicates
	const n = 30
	seqs := make([]int64, n)
	parallel(n, func(i int) {
		who := "alice"
		if i%2 == 1 {
			who = "bob"
		}
		r, err := e.request("POST", "/v1/channels/"+ch+"/messages", tt.APIKey, tt.userToken(t, who),
			map[string]any{"client_msg_id": fmt.Sprintf("p-%d", i), "text": fmt.Sprintf("message %d", i)})
		if err != nil || r.status != 201 {
			t.Errorf("parallel send %d: %v %d %v", i, err, r.status, r.body)
			return
		}
		seqs[i] = num(r.at("seq"))
	})
	got := slices.Clone(seqs)
	slices.Sort(got)
	for i, s := range got {
		if s != int64(i+1) {
			t.Fatalf("seqs are not 1..%d without gaps: %v", n, got)
		}
	}
	if r := e.asUser(tt, "alice", "GET", "/v1/channels/"+ch+"/sync?since_rev=0&limit=100", nil); count(r.at("messages")) != n || num(r.at("next_rev")) != n {
		t.Fatalf("sync after parallel sends: %d messages, next_rev %d", count(r.at("messages")), num(r.at("next_rev")))
	}

	// the same client_msg_id sent at once creates one message
	var created, ids = 0, map[string]bool{}
	results := make([]response, 10)
	parallel(10, func(i int) {
		r, err := e.request("POST", "/v1/channels/"+ch+"/messages", tt.APIKey, tt.userToken(t, "alice"),
			map[string]any{"client_msg_id": "same-id", "text": "only once"})
		if err != nil {
			t.Errorf("duplicate send: %v", err)
			return
		}
		results[i] = r
	})
	for _, r := range results {
		if r.status == 201 {
			created++
		} else if r.status != 200 {
			t.Errorf("duplicate send answered %d %v", r.status, r.body)
		}
		ids[str(r.at("id"))] = true
	}
	if created != 1 || len(ids) != 1 {
		t.Fatalf("duplicate sends: %d created, %d distinct messages", created, len(ids))
	}
	if r := e.asUser(tt, "alice", "GET", "/v1/channels/"+ch+"/sync?since_rev=0&limit=100", nil); count(r.at("messages")) != n+1 {
		t.Fatalf("history has %d messages, want %d", count(r.at("messages")), n+1)
	}

	// the DM of a pair, asked for at once from both sides, is one channel
	e.seed(tt, "x", "y")
	var dmIDs = map[string]bool{}
	var made int
	dmResults := make([]response, 10)
	parallel(10, func(i int) {
		a, b := "x", "y"
		if i%2 == 1 {
			a, b = b, a
		}
		r, err := e.request("POST", "/v1/server/channels/dm", tt.APIKey, tt.serverToken(t), map[string]any{"user_a": a, "user_b": b})
		if err != nil {
			t.Errorf("parallel dm: %v", err)
			return
		}
		dmResults[i] = r
	})
	for _, r := range dmResults {
		if r.status == 201 {
			made++
		} else if r.status != 200 {
			t.Errorf("parallel dm answered %d %v", r.status, r.body)
		}
		dmIDs[str(r.at("id"))] = true
	}
	if made != 1 || len(dmIDs) != 1 {
		t.Fatalf("parallel DMs: %d created, %d distinct channels", made, len(dmIDs))
	}
}
