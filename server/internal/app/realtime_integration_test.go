//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/rizkysiregar/marpokat/server/internal/platform/redisx"
	"github.com/rizkysiregar/marpokat/server/internal/realtime"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
	"github.com/rizkysiregar/marpokat/server/internal/testdb"
)

const quiet = 300 * time.Millisecond

func TestWebSocketAuthentication(t *testing.T) {
	e := newRealtimeEnv(t, 1, func(o *realtime.Options) { o.AuthTimeout = 400 * time.Millisecond })
	tt := e.createTenant(tenant.Config{})
	other := e.createTenant(tenant.Config{})
	e.seed(tt, "alice")
	now := time.Now()

	cases := []struct {
		name     string
		frame    string
		wantCode websocket.StatusCode
		wantErr  string
	}{
		{"unsupported protocol version", jsonOf(authFrame(tt.APIKey, tt.userToken(t, "alice"), 2)), realtime.CloseUnsupportedVersion, "bad_request"},
		{"unknown api key", jsonOf(authFrame("mpk_00000000000000000000000000000000", tt.userToken(t, "alice"), 1)), realtime.CloseAuthFailed, "unauthenticated"},
		{"token signed by another tenant", jsonOf(authFrame(tt.APIKey, other.userToken(t, "alice"), 1)), realtime.CloseAuthFailed, "unauthenticated"},
		{"expired token", jsonOf(authFrame(tt.APIKey, tt.tokenFor(t, "alice", time.Minute, now.Add(-time.Hour)), 1)), realtime.CloseTokenExpired, "token_expired"},
		{"server token", jsonOf(authFrame(tt.APIKey, tt.serverToken(t), 1)), realtime.CloseAuthFailed, "unauthenticated"},
		{"user that does not exist", jsonOf(authFrame(tt.APIKey, tt.userToken(t, "ghost"), 1)), realtime.CloseAuthFailed, "unauthenticated"},
		{"first frame is not auth", `{"type":"ping","data":{}}`, realtime.CloseProtocolError, "bad_request"},
		{"first frame is not JSON", `hello`, realtime.CloseProtocolError, "bad_request"},
	}
	for _, c := range cases {
		w := dialWS(t, e.wsURL(0))
		w.sendText(c.frame)
		if got := at(w.next("error"), "data", "code"); got != c.wantErr {
			t.Errorf("%s: error code %v, want %s", c.name, got, c.wantErr)
		}
		if code := w.waitClose(3 * time.Second); code != c.wantCode {
			t.Errorf("%s: closed with %d, want %d", c.name, code, c.wantCode)
		}
	}

	// silence: closed after the auth timeout
	if code := dialWS(t, e.wsURL(0)).waitClose(3 * time.Second); code != realtime.CloseAuthTimeout {
		t.Errorf("no auth frame: closed with %d, want %d", code, realtime.CloseAuthTimeout)
	}

	// a disabled tenant cannot connect (a pod that has not cached the tenant yet: the cache lasts 30 seconds)
	off := e.createTenant(tenant.Config{})
	e.seed(off, "bob")
	if err := e.tenants.SetDisabled(context.Background(), off.Slug, true); err != nil {
		t.Fatal(err)
	}
	fresh := newRealtimeEnv(t, 1, nil)
	w := dialWS(t, fresh.wsURL(0))
	w.sendText(jsonOf(authFrame(off.APIKey, off.userToken(t, "bob"), 1)))
	if code := w.waitClose(3 * time.Second); code != realtime.CloseAuthFailed {
		t.Errorf("disabled tenant: closed with %d", code)
	}

	// a frame over the limit ends the connection with 1009
	big := e.connect(0, tt, "alice")
	big.sendText(strings.Repeat("x", 70<<10))
	if code := big.waitClose(3 * time.Second); code != websocket.StatusMessageTooBig {
		t.Errorf("oversized frame: closed with %d, want 1009", code)
	}
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestWebSocketDeliveryAcrossPods(t *testing.T) {
	e := newRealtimeEnv(t, 2, nil)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob", "carol")
	ch := e.dm(tt, "alice", "bob")

	alice0 := e.connect(0, tt, "alice") // alice on two devices, on two pods
	alice1 := e.connect(1, tt, "alice")
	bob1 := e.connect(1, tt, "bob")
	carol := e.connect(0, tt, "carol") // not in the channel

	alice0.send("message.send", "s1", map[string]any{"channel_id": ch, "client_msg_id": "c1", "text": "hi bob"})
	ack := alice0.next("ack")
	if ack["id"] != "s1" || at(ack, "data", "message", "seq") != float64(1) || at(ack, "data", "message", "text") != "hi bob" ||
		at(ack, "data", "message", "sender", "external_id") != "alice" {
		t.Fatalf("ack: %v", ack)
	}
	msgID := at(ack, "data", "message", "id")
	for name, w := range map[string]*wsConn{"bob (other pod)": bob1, "alice's other device": alice1} {
		ev := w.next("message.new")
		if at(ev, "data", "channel_id") != ch || at(ev, "data", "rev_seq") != float64(1) || at(ev, "data", "message", "id") != msgID || ev["ts"] == nil {
			t.Errorf("%s got %v", name, ev)
		}
	}
	alice0.none("message.new", quiet) // the connection that sent it is answered by the ack only
	carol.none("message.new", 0)      // strangers hear nothing

	// the same client_msg_id again: same message, acked again, announced once
	alice0.send("message.send", "s2", map[string]any{"channel_id": ch, "client_msg_id": "c1", "text": "hi bob"})
	if again := alice0.next("ack"); again["id"] != "s2" || at(again, "data", "message", "id") != msgID {
		t.Fatalf("resend ack: %v", again)
	}
	bob1.none("message.new", quiet)

	// a REST send has no connection to skip: every device of the sender hears it too
	if r := e.mustSay(tt, "bob", ch, "hi alice"); num(r.at("seq")) != 2 {
		t.Fatalf("rest send: %v", r.body)
	}
	for name, w := range map[string]*wsConn{"alice0": alice0, "alice1": alice1, "bob": bob1} {
		if ev := w.next("message.new"); at(ev, "data", "message", "text") != "hi alice" {
			t.Errorf("%s got %v", name, ev)
		}
	}

	// failures answer the request, with its id, and keep the connection
	alice0.send("message.send", "bad1", map[string]any{"channel_id": ch, "client_msg_id": "c9", "text": "  "})
	if er := alice0.next("error"); er["id"] != "bad1" || at(er, "data", "code") != "validation_failed" {
		t.Errorf("validation error: %v", er)
	}
	alice0.send("message.send", "bad2", map[string]any{"channel_id": "00000000-0000-0000-0000-000000000000", "client_msg_id": "c9", "text": "x"})
	if er := alice0.next("error"); er["id"] != "bad2" || at(er, "data", "code") != "not_found" {
		t.Errorf("unknown channel: %v", er)
	}
	carol.send("message.send", "bad3", map[string]any{"channel_id": ch, "client_msg_id": "c1", "text": "let me in"})
	if er := carol.next("error"); er["id"] != "bad3" || at(er, "data", "code") != "not_found" {
		t.Errorf("non-member send: %v", er)
	}
	alice0.send("message.send", "", map[string]any{"channel_id": ch, "client_msg_id": "c2", "text": "x"})
	if er := alice0.next("error"); at(er, "data", "code") != "validation_failed" {
		t.Errorf("send without id: %v", er)
	}
	alice0.send("nonsense", "n1", nil)
	if er := alice0.next("error"); er["id"] != "n1" || at(er, "data", "code") != "bad_request" {
		t.Errorf("unknown type: %v", er)
	}

	// several senders on different pods: every event arrives, rev_seq values form 3..N without gaps. They may arrive
	// out of order (two pods publish independently), which is what the client's gap rule is for.
	const each = 4
	for i := range each {
		alice0.send("message.send", fmt.Sprintf("p%d", i), map[string]any{"channel_id": ch, "client_msg_id": fmt.Sprintf("pa%d", i), "text": "a"})
		bob1.send("message.send", fmt.Sprintf("q%d", i), map[string]any{"channel_id": ch, "client_msg_id": fmt.Sprintf("pb%d", i), "text": "b"})
	}
	var got []int64
	for range each { // bob hears alice's messages, alice (pod 0) hears bob's, and alice's other device hears both
		alice0.next("ack")
		bob1.next("ack")
	}
	for range each * 2 {
		got = append(got, int64(at(alice1.next("message.new"), "data", "rev_seq").(float64)))
	}
	slices.Sort(got)
	for i, v := range got {
		if v != int64(i+3) {
			t.Fatalf("alice's second device got rev_seqs %v, want 3..%d", got, each*2+2)
		}
	}
}

func TestWebSocketTypingAndRead(t *testing.T) {
	e := newRealtimeEnv(t, 2, func(o *realtime.Options) { o.TypingExpiry = 500 * time.Millisecond })
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob", "carol")
	ch := e.dm(tt, "alice", "bob")
	alice := e.connect(0, tt, "alice")
	alice2 := e.connect(1, tt, "alice")
	bob := e.connect(1, tt, "bob")
	carol := e.connect(0, tt, "carol")

	// typing: others hear it, the typist (any device) and strangers do not
	alice.send("typing.start", "", map[string]any{"channel_id": ch})
	ev := bob.next("typing")
	if at(ev, "data", "user_id") != "alice" || at(ev, "data", "typing") != true || at(ev, "data", "channel_id") != ch {
		t.Fatalf("typing event: %v", ev)
	}
	alice.none("typing", 0)
	alice2.none("typing", 0)
	carol.none("typing", 0)

	// too soon after the last one: dropped
	for range 4 {
		alice.send("typing.start", "", map[string]any{"channel_id": ch})
	}
	bob.none("typing", quiet)

	alice.send("typing.stop", "", map[string]any{"channel_id": ch})
	if ev := bob.next("typing"); at(ev, "data", "typing") != false {
		t.Fatalf("typing stop: %v", ev)
	}
	alice.send("typing.stop", "", map[string]any{"channel_id": ch}) // nothing to stop
	bob.none("typing", quiet)

	// not refreshed: ends by itself
	alice.send("typing.start", "", map[string]any{"channel_id": ch})
	bob.next("typing")
	if ev := bob.next("typing"); at(ev, "data", "typing") != false {
		t.Fatalf("typing expiry: %v", ev)
	}

	// disconnecting in the middle of a sentence ends it
	bob.send("typing.start", "", map[string]any{"channel_id": ch})
	alice.next("typing")
	_ = bob.c.Close(websocket.StatusNormalClosure, "bye")
	if ev := alice.next("typing"); at(ev, "data", "typing") != false || at(ev, "data", "user_id") != "bob" {
		t.Fatalf("typing after disconnect: %v", ev)
	}
	bob = e.connect(1, tt, "bob")

	// typing in a channel you are not in is an error, and nobody hears it
	carol.send("typing.start", "", map[string]any{"channel_id": ch})
	if er := carol.next("error"); at(er, "data", "code") != "not_found" {
		t.Fatalf("typing as a stranger: %v", er)
	}
	alice.none("typing", 0)

	// reading: the others see how far, the reader's own connection gets an ack
	e.mustSay(tt, "alice", ch, "one")
	e.mustSay(tt, "alice", ch, "two")
	bob.send("read.mark", "r1", map[string]any{"channel_id": ch, "seq": 2})
	if ack := bob.next("ack"); ack["id"] != "r1" {
		t.Fatalf("read ack: %v", ack)
	}
	for name, w := range map[string]*wsConn{"alice": alice, "alice's other device": alice2} {
		ev := w.next("read.updated")
		if at(ev, "data", "user_id") != "bob" || at(ev, "data", "last_read_seq") != float64(2) || at(ev, "data", "channel_id") != ch {
			t.Errorf("%s: read.updated %v", name, ev)
		}
	}
	bob.none("read.updated", quiet)

	// REST read marks are announced too
	e.say(tt, "alice", ch, "three")
	if r := e.asUser(tt, "bob", "POST", "/v1/channels/"+ch+"/read", map[string]any{"seq": 3}); r.status != 204 {
		t.Fatal(r.body)
	}
	if ev := alice.next("read.updated"); at(ev, "data", "last_read_seq") != float64(3) {
		t.Fatalf("read.updated after REST: %v", ev)
	}
	bob.send("read.mark", "r2", map[string]any{"channel_id": ch})
	if er := bob.next("error"); er["id"] != "r2" || at(er, "data", "code") != "validation_failed" {
		t.Fatalf("read.mark without seq: %v", er)
	}
}

func TestWebSocketPresence(t *testing.T) {
	e := newRealtimeEnv(t, 2, nil)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob", "stranger")
	e.dm(tt, "alice", "bob")
	bob := e.connect(0, tt, "bob")

	presence := func(asUser string, ids ...string) response {
		return e.asUser(tt, asUser, "GET", "/v1/presence?user_ids="+strings.Join(ids, ","), nil)
	}
	if r := presence("bob", "alice"); r.status != 200 || r.at("users", 0, "online") != false || r.at("users", 0, "last_seen_at") != nil {
		t.Fatalf("alice before ever connecting: %v", r.body)
	}

	alice1 := e.connect(1, tt, "alice")
	if ev := bob.next("presence"); at(ev, "data", "user_id") != "alice" || at(ev, "data", "online") != true {
		t.Fatalf("presence online: %v", ev)
	}
	if r := presence("bob", "alice", "bob"); r.at("users", 0, "user_id") != "alice" || r.at("users", 0, "online") != true || num(float64(count(r.at("users")))) != 2 {
		t.Fatalf("alice online: %v", r.body)
	}
	alice2 := e.connect(0, tt, "alice") // a second device is not news
	bob.none("presence", quiet)

	// only people you share a channel with are visible
	if r := presence("bob", "stranger"); r.status != 200 || count(r.at("users")) != 0 {
		t.Fatalf("presence of a stranger: %v", r.body)
	}
	if r := presence("stranger", "alice"); r.status != 200 || count(r.at("users")) != 0 {
		t.Fatalf("a stranger asking for presence: %v", r.body)
	}
	for _, bad := range []string{"", ",,", strings.Repeat("a,", 101)} {
		if r := e.asUser(tt, "bob", "GET", "/v1/presence?user_ids="+bad, nil); r.status != 400 {
			t.Errorf("presence with %q: %d", bad, r.status)
		}
	}

	// still online with one device left; offline, with last_seen_at, when the last one goes
	_ = alice1.c.Close(websocket.StatusNormalClosure, "bye")
	bob.none("presence", quiet)
	_ = alice2.c.Close(websocket.StatusNormalClosure, "bye")
	ev := bob.next("presence")
	if at(ev, "data", "user_id") != "alice" || at(ev, "data", "online") != false || at(ev, "data", "last_seen_at") == nil {
		t.Fatalf("presence offline: %v", ev)
	}
	if r := presence("bob", "alice"); r.at("users", 0, "online") != false || r.at("users", 0, "last_seen_at") == nil {
		t.Fatalf("alice offline: %v", r.body)
	}
}

func TestPresenceExpiresWithoutDisconnect(t *testing.T) {
	// a pod that crashes never says goodbye: its users must go offline by themselves
	rdb, err := redisx.Open(context.Background(), testdb.Redis(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rdb.Close() }()
	p := realtime.NewPresence(rdb, 600*time.Millisecond)
	ctx := context.Background()
	tn, us := uuid.New(), uuid.New()
	online := func() bool {
		st, err := p.Of(ctx, tn, []uuid.UUID{us})
		if err != nil {
			t.Fatal(err)
		}
		return st[us].Online
	}

	first, ok, err := p.Connect(ctx, tn, us, "c1", 2)
	if err != nil || !first || !ok || !online() {
		t.Fatalf("connect: first=%v ok=%v err=%v online=%v", first, ok, err, online())
	}
	time.Sleep(350 * time.Millisecond)
	if err := p.Touch(ctx, tn, us, "c1"); err != nil { // a ping keeps it alive
		t.Fatal(err)
	}
	time.Sleep(350 * time.Millisecond)
	if !online() {
		t.Fatal("a connection that pinged went offline")
	}
	time.Sleep(700 * time.Millisecond)
	if online() {
		t.Fatal("a connection that stopped pinging is still online")
	}
	if first, _, _ := p.Connect(ctx, tn, us, "c2", 2); !first {
		t.Fatal("after the expiry the next connection is the first again")
	}

	// the limit counts live connections; reconnecting with the same id is not a new one
	if _, ok, _ := p.Connect(ctx, tn, us, "c3", 2); !ok {
		t.Fatal("second connection refused")
	}
	if _, ok, _ := p.Connect(ctx, tn, us, "c4", 2); ok {
		t.Fatal("third connection accepted beyond the limit of 2")
	}
	if _, ok, _ := p.Connect(ctx, tn, us, "c3", 2); !ok {
		t.Fatal("an existing connection id was refused")
	}
	if last, _, _ := p.Disconnect(ctx, tn, us, "c2"); last {
		t.Fatal("one connection left, not the last")
	}
	last, seen, _ := p.Disconnect(ctx, tn, us, "c3")
	if !last || time.Since(seen) > 5*time.Second {
		t.Fatalf("last disconnect: last=%v seen=%v", last, seen)
	}
	st, _ := p.Of(ctx, tn, []uuid.UUID{us})
	if st[us].Online || st[us].LastSeen == nil {
		t.Fatalf("after leaving: %+v", st[us])
	}
}

func TestWebSocketChannelEvents(t *testing.T) {
	e := newRealtimeEnv(t, 2, nil)
	tt := e.createTenant(tenant.Config{ClientCanCreateGroup: true})
	e.seed(tt, "alice", "bob", "carol", "dave")
	alice := e.connect(0, tt, "alice")
	bob := e.connect(1, tt, "bob")
	carol := e.connect(0, tt, "carol")
	dave := e.connect(1, tt, "dave")

	// a DM made by the backend: both members are told, each sees the other as the peer
	dm := e.dm(tt, "alice", "bob")
	evA, evB := alice.next("channel.added"), bob.next("channel.added")
	if at(evA, "data", "channel", "id") != dm || at(evA, "data", "channel", "dm_peer", "external_id") != "bob" ||
		at(evB, "data", "channel", "dm_peer", "external_id") != "alice" || at(evB, "data", "channel", "type") != "dm" {
		t.Fatalf("dm channel.added: %v / %v", evA, evB)
	}
	carol.none("channel.added", 0)

	// a group made by a client: the creator and the members are told
	g := e.asUser(tt, "alice", "POST", "/v1/channels", map[string]any{"type": "group", "name": "Crew", "member_ids": []string{"bob"}})
	if g.status != 201 {
		t.Fatal(g.body)
	}
	gid := str(g.at("id"))
	for name, w := range map[string]*wsConn{"alice": alice, "bob": bob} {
		ev := w.next("channel.added")
		if at(ev, "data", "channel", "id") != gid || at(ev, "data", "channel", "name") != "Crew" || at(ev, "data", "channel", "last_message", "metadata", "event") != "channel.created" {
			t.Errorf("%s: group channel.added %v", name, ev)
		}
	}

	// a new member: told of the channel; the others are told of the member and of the announcement
	if r := e.asUser(tt, "alice", "POST", "/v1/channels/"+gid+"/members", map[string]any{"user_ids": []string{"carol"}}); r.status != 200 {
		t.Fatal(r.body)
	}
	if ev := carol.next("channel.added"); at(ev, "data", "channel", "id") != gid || at(ev, "data", "channel", "membership", "role") != "member" {
		t.Fatalf("carol channel.added: %v", ev)
	}
	for name, w := range map[string]*wsConn{"alice": alice, "bob": bob} {
		ev := w.next("member.added")
		if at(ev, "data", "channel_id") != gid || at(ev, "data", "member", "user", "external_id") != "carol" || at(ev, "data", "member", "role") != "member" {
			t.Errorf("%s: member.added %v", name, ev)
		}
	}
	carol.none("member.added", 0)
	for name, w := range map[string]*wsConn{"alice": alice, "bob": bob, "carol": carol} {
		if ev := w.next("message.new"); at(ev, "data", "message", "metadata", "event") != "member.added" || at(ev, "data", "message", "type") != "system" {
			t.Errorf("%s: announcement %v", name, ev)
		}
	}
	dave.none("member.added", 0)

	// rename and freeze reach every member
	if r := e.asUser(tt, "alice", "PATCH", "/v1/channels/"+gid, map[string]any{"name": "Crew 2"}); r.status != 200 {
		t.Fatal(r.body)
	}
	for name, w := range map[string]*wsConn{"alice": alice, "bob": bob, "carol": carol} {
		if ev := w.next("channel.updated"); at(ev, "data", "channel", "name") != "Crew 2" {
			t.Errorf("%s: channel.updated %v", name, ev)
		}
		if ev := w.next("message.new"); at(ev, "data", "message", "metadata", "event") != "channel.renamed" {
			t.Errorf("%s: rename announcement %v", name, ev)
		}
	}
	if r := e.asServer(tt, "POST", "/v1/server/channels/"+gid+"/freeze", nil); r.status != 204 {
		t.Fatal(r.status)
	}
	for name, w := range map[string]*wsConn{"alice": alice, "bob": bob, "carol": carol} {
		if ev := w.next("channel.updated"); at(ev, "data", "channel", "frozen") != true {
			t.Errorf("%s: frozen %v", name, ev)
		}
	}
	e.asServer(tt, "DELETE", "/v1/server/channels/"+gid+"/freeze", nil)
	for _, w := range []*wsConn{alice, bob, carol} {
		w.next("channel.updated")
	}

	// removal: the removed member loses the channel, the rest hear who left and the announcement
	if r := e.asUser(tt, "alice", "DELETE", "/v1/channels/"+gid+"/members/carol", nil); r.status != 204 {
		t.Fatal(r.status)
	}
	if ev := carol.next("channel.removed"); at(ev, "data", "channel_id") != gid || at(ev, "data", "reason") != "removed" {
		t.Fatalf("channel.removed: %v", ev)
	}
	for name, w := range map[string]*wsConn{"alice": alice, "bob": bob} {
		if ev := w.next("member.removed"); at(ev, "data", "user_id") != "carol" || at(ev, "data", "channel_id") != gid {
			t.Errorf("%s: member.removed %v", name, ev)
		}
		if ev := w.next("message.new"); at(ev, "data", "message", "metadata", "event") != "member.removed" {
			t.Errorf("%s: removal announcement %v", name, ev)
		}
	}
	carol.none("message.new", quiet) // no longer a member, so no longer told anything about it
	bob.send("typing.start", "", map[string]any{"channel_id": gid})
	alice.next("typing")
	carol.none("typing", quiet)

	// leaving
	if r := e.asUser(tt, "bob", "DELETE", "/v1/channels/"+gid+"/members/me", nil); r.status != 204 {
		t.Fatal(r.status)
	}
	if ev := bob.next("channel.removed"); at(ev, "data", "reason") != "left" {
		t.Fatalf("leaving: %v", ev)
	}
}

func TestWebSocketRateLimits(t *testing.T) {
	e := newRealtimeEnv(t, 1, func(o *realtime.Options) { o.RequestsPerSecond = 5 })
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob", "carol")
	ch := e.dm(tt, "alice", "bob")
	ch2 := e.dm(tt, "alice", "carol")
	alice := e.connect(0, tt, "alice")

	// one connection may send only so many frames per second
	for i := range 20 {
		alice.send("read.mark", fmt.Sprintf("r%d", i), map[string]any{"channel_id": ch, "seq": 0})
	}
	acks, limited := 0, 0
	for range 20 {
		select {
		case <-time.After(3 * time.Second):
			t.Fatal("missing answers to read.mark")
		default:
		}
		for {
			alice.mu.Lock()
			n := len(alice.frames)
			alice.mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		alice.mu.Lock()
		f := alice.frames[0]
		alice.frames = alice.frames[1:]
		alice.mu.Unlock()
		switch {
		case f["type"] == "ack":
			acks++
		case f["type"] == "error" && at(f, "data", "code") == "rate_limited":
			limited++
		}
	}
	if acks < 5 || limited < 1 {
		t.Fatalf("20 quick frames at 5 per second: %d acked, %d limited", acks, limited)
	}
	time.Sleep(time.Second) // the connection bucket refills

	// a user may send a burst of 10 messages, then must wait
	sent, refused := 0, 0
	var retryMs float64
	for i := range 14 {
		alice.send("message.send", fmt.Sprintf("m%d", i), map[string]any{"channel_id": ch2, "client_msg_id": fmt.Sprintf("burst-%d", i), "text": "x"})
		time.Sleep(210 * time.Millisecond) // under the 5 frames per second of the connection
	}
	for range 14 {
		f := alice.nextOf("ack", "error")
		if f["type"] == "ack" {
			sent++
		} else if at(f, "data", "code") == "rate_limited" {
			refused++
			retryMs, _ = at(f, "data", "details", "retry_after_ms").(float64)
		}
	}
	if sent < 10 || sent > 13 || refused < 1 || retryMs <= 0 {
		t.Fatalf("burst of 14 messages: %d sent, %d refused (retry after %vms)", sent, refused, retryMs)
	}

	// the same limit holds for REST: 429 with Retry-After
	req, _ := http.NewRequestWithContext(context.Background(), "POST", e.srv.URL+"/v1/channels/"+ch2+"/messages", strings.NewReader(`{"client_msg_id":"over-limit","text":"x"}`))
	req.Header.Set("X-Api-Key", tt.APIKey)
	req.Header.Set("Authorization", "Bearer "+tt.userToken(t, "alice"))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != 429 || res.Header.Get("Retry-After") == "" || !strings.Contains(string(body), "rate_limited") {
		t.Fatalf("rest over the limit: %d retry-after=%q %s", res.StatusCode, res.Header.Get("Retry-After"), body)
	}
	time.Sleep(time.Duration(retryMs+100) * time.Millisecond)
	if r := e.say(tt, "alice", ch2, "after waiting"); r.status != 201 {
		t.Fatalf("after waiting: %d %v", r.status, r.body)
	}

	// another user is not affected by alice's limit
	if r := e.say(tt, "carol", ch2, "carol is fine"); r.status != 201 {
		t.Fatalf("other user: %d %v", r.status, r.body)
	}
}

func TestWebSocketHeartbeat(t *testing.T) {
	e := newRealtimeEnv(t, 1, func(o *realtime.Options) { o.IdleTimeout = 600 * time.Millisecond })
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice")

	// pinging keeps the connection alive well past the idle timeout
	w := e.connect(0, tt, "alice")
	for range 8 {
		w.send("ping", "", nil)
		w.next("pong")
		time.Sleep(200 * time.Millisecond)
	}
	// silence ends it
	if code := w.waitClose(3 * time.Second); code != realtime.CloseHeartbeatTimeout {
		t.Fatalf("idle connection closed with %d, want %d", code, realtime.CloseHeartbeatTimeout)
	}

	// auth.ok tells how often to ping
	e2 := newRealtimeEnv(t, 1, nil)
	tt2 := e2.createTenant(tenant.Config{})
	e2.seed(tt2, "bob")
	c := dialWS(t, e2.wsURL(0))
	c.sendText(jsonOf(authFrame(tt2.APIKey, tt2.userToken(t, "bob"), 1)))
	if ms := at(c.next("auth.ok"), "data", "heartbeat_interval_ms"); ms != float64(25000) {
		t.Fatalf("heartbeat_interval_ms = %v", ms)
	}
}

func TestWebSocketTokenExpiry(t *testing.T) {
	e := newRealtimeEnv(t, 1, nil)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice")
	w := dialWS(t, e.wsURL(0))
	w.sendText(jsonOf(authFrame(tt.APIKey, tt.tokenFor(t, "alice", time.Second, time.Now()), 1)))
	w.next("auth.ok")
	if er := w.next("error"); at(er, "data", "code") != "token_expired" {
		t.Fatalf("expiry error: %v", er)
	}
	if code := w.waitClose(3 * time.Second); code != realtime.CloseTokenExpired {
		t.Fatalf("closed with %d, want %d", code, realtime.CloseTokenExpired)
	}
}

func TestWebSocketConnectionLimit(t *testing.T) {
	e := newRealtimeEnv(t, 2, func(o *realtime.Options) { o.MaxSessionsPerUser = 2 })
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob")
	e.connect(0, tt, "alice")
	e.connect(1, tt, "alice") // counted over all pods
	w := dialWS(t, e.wsURL(0))
	w.sendText(jsonOf(authFrame(tt.APIKey, tt.userToken(t, "alice"), 1)))
	if er := w.next("error"); at(er, "data", "code") != "rate_limited" {
		t.Fatalf("third connection: %v", er)
	}
	if code := w.waitClose(3 * time.Second); code != realtime.CloseRateLimited {
		t.Fatalf("third connection closed with %d, want %d", code, realtime.CloseRateLimited)
	}
	e.connect(0, tt, "bob") // other users are not affected
}

// The scenario of the milestone: chat between two browsers on two pods, one pod goes away, the client reconnects to
// the other one and catches up with sync.
func TestWebSocketSurvivesAPodGoingAway(t *testing.T) {
	e := newRealtimeEnv(t, 2, nil)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob")
	ch := e.dm(tt, "alice", "bob")
	alice := e.connect(0, tt, "alice")
	bob := e.connect(1, tt, "bob")

	// chat both ways across the pods; alice keeps a cursor like a real client
	alice.send("message.send", "1", map[string]any{"channel_id": ch, "client_msg_id": "a1", "text": "hello"})
	alice.next("ack")
	bob.next("message.new")
	bob.send("message.send", "2", map[string]any{"channel_id": ch, "client_msg_id": "b1", "text": "hello back"})
	bob.next("ack")
	cursor := int64(at(alice.next("message.new"), "data", "rev_seq").(float64))
	if cursor != 2 {
		t.Fatalf("cursor = %d", cursor)
	}

	// pod 0 goes away: it shuts down its sockets with 1012, "reconnect at once"
	e.pods[0].hub.Shutdown()
	if code := alice.waitClose(3 * time.Second); code != realtime.CloseServerRestart {
		t.Fatalf("alice closed with %d, want %d", code, realtime.CloseServerRestart)
	}
	e.pods[0].srv.Close()

	// while alice is away, bob keeps talking
	for i := range 3 {
		bob.send("message.send", fmt.Sprintf("m%d", i), map[string]any{"channel_id": ch, "client_msg_id": fmt.Sprintf("b%d", i+2), "text": fmt.Sprintf("while you were gone %d", i)})
		bob.next("ack")
	}

	// alice reconnects to the surviving pod and syncs from her cursor: exactly what she missed, in order
	alice = e.connect(1, tt, "alice")
	r := e.on(1).asUser(tt, "alice", "GET", fmt.Sprintf("/v1/channels/%s/sync?since_rev=%d", ch, cursor), nil)
	if r.status != 200 || count(r.at("messages")) != 3 || num(r.at("next_rev")) != 5 || r.at("has_more") != false {
		t.Fatalf("sync after reconnect: %d %v", r.status, r.body)
	}
	for i := range 3 {
		if r.at("messages", i, "text") != fmt.Sprintf("while you were gone %d", i) || num(r.at("messages", i, "seq")) != int64(3+i) {
			t.Fatalf("missed message %d: %v", i, r.at("messages", i))
		}
	}

	// and live events flow again, in both directions
	bob.send("message.send", "live", map[string]any{"channel_id": ch, "client_msg_id": "b9", "text": "live again"})
	bob.next("ack")
	if ev := alice.next("message.new"); at(ev, "data", "message", "text") != "live again" || at(ev, "data", "rev_seq") != float64(6) {
		t.Fatalf("live event after reconnect: %v", ev)
	}
	alice.send("message.send", "back", map[string]any{"channel_id": ch, "client_msg_id": "a9", "text": "I am back"})
	alice.next("ack")
	if ev := bob.next("message.new"); at(ev, "data", "message", "text") != "I am back" {
		t.Fatalf("bob after alice's return: %v", ev)
	}
}

func TestWebSocketShutdownRefusesNewConnections(t *testing.T) {
	e := newRealtimeEnv(t, 1, nil)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice")
	e.pods[0].hub.Shutdown()
	w := dialWS(t, e.wsURL(0))
	w.sendText(jsonOf(authFrame(tt.APIKey, tt.userToken(t, "alice"), 1)))
	if code := w.waitClose(3 * time.Second); code != realtime.CloseServerRestart {
		t.Fatalf("a connection to a pod that is shutting down closed with %d, want %d", code, realtime.CloseServerRestart)
	}
}

func TestWebSocketRedisConnectionLoss(t *testing.T) {
	e := newRealtimeEnv(t, 2, nil)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob")
	ch := e.dm(tt, "alice", "bob")
	alice := e.connect(0, tt, "alice")
	bob := e.connect(1, tt, "bob")

	// Redis drops every subscriber connection. The client library reconnects by itself, but events published in the
	// gap are gone, so the hub must not pretend nothing happened: it closes its sessions and clients resync.
	admin := redis.NewClient(&redis.Options{Addr: testdb.Redis(t).Addr()})
	defer func() { _ = admin.Close() }()
	if err := admin.Do(context.Background(), "CLIENT", "KILL", "TYPE", "pubsub").Err(); err != nil {
		t.Fatal(err)
	}
	for name, w := range map[string]*wsConn{"alice": alice, "bob": bob} {
		if code := w.waitClose(10 * time.Second); code != realtime.CloseServerRestart {
			t.Errorf("%s closed with %d, want %d", name, code, realtime.CloseServerRestart)
		}
	}

	// after reconnecting, delivery across pods works again
	alice = e.connect(0, tt, "alice")
	bob = e.connect(1, tt, "bob")
	alice.send("message.send", "1", map[string]any{"channel_id": ch, "client_msg_id": "after", "text": "after the outage"})
	alice.next("ack")
	if ev := bob.next("message.new"); at(ev, "data", "message", "text") != "after the outage" {
		t.Fatalf("delivery after the outage: %v", ev)
	}
}
