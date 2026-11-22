//go:build integration

package app

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rizkysiregar/marpokat/server/internal/realtime"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
)

const thumbsUp = "👍"

func (e *env) react(tt testTenant, ext, messageID, emoji string) response {
	e.t.Helper()
	return e.asUser(tt, ext, "PUT", "/v1/messages/"+messageID+"/reactions/"+url.PathEscape(emoji), nil)
}

func (e *env) unreact(tt testTenant, ext, messageID, emoji string) response {
	e.t.Helper()
	return e.asUser(tt, ext, "DELETE", "/v1/messages/"+messageID+"/reactions/"+url.PathEscape(emoji), nil)
}

func counts(r response) map[string]float64 {
	out := map[string]float64{}
	m, _ := r.at("reactions", "counts").(map[string]any)
	for k, v := range m {
		out[k], _ = v.(float64)
	}
	return out
}

func TestReactions(t *testing.T) {
	e := newEnv(t)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob", "carol")
	ch := e.dm(tt, "alice", "bob")
	msg := e.mustSay(tt, "alice", ch, "react to me")
	id := str(msg.at("id"))

	r := e.react(tt, "bob", id, thumbsUp)
	if r.status != 200 || counts(r)[thumbsUp] != 1 || r.at("reactions", "mine", 0) != thumbsUp || num(r.at("rev_seq")) != 2 || num(r.at("seq")) != 1 {
		t.Fatalf("bob reacts: %d %v", r.status, r.body)
	}
	// each reader sees only their own reactions as theirs
	hist := func(who string) response { return e.asUser(tt, who, "GET", "/v1/channels/"+ch+"/messages", nil) }
	if h := hist("alice"); counts(response{body: map[string]any{"reactions": h.at("messages", 0, "reactions")}})[thumbsUp] != 1 || count(h.at("messages", 0, "reactions", "mine")) != 0 {
		t.Fatalf("alice sees: %v", h.at("messages", 0, "reactions"))
	}
	if h := hist("bob"); h.at("messages", 0, "reactions", "mine", 0) != thumbsUp {
		t.Fatalf("bob sees: %v", h.at("messages", 0, "reactions"))
	}

	// reacting again with the same emoji changes nothing, not even the revision
	if r := e.react(tt, "bob", id, thumbsUp); r.status != 200 || counts(r)[thumbsUp] != 1 || num(r.at("rev_seq")) != 2 {
		t.Fatalf("same reaction twice: %d %v", r.status, r.body)
	}
	e.react(tt, "alice", id, thumbsUp)
	r = e.react(tt, "alice", id, "❤️")
	if c := counts(r); c[thumbsUp] != 2 || c["❤️"] != 1 || num(r.at("rev_seq")) != 4 {
		t.Fatalf("two users, two emoji: %v rev %v", c, r.at("rev_seq"))
	}

	// taking one back; taking back what is not there changes nothing
	r = e.unreact(tt, "bob", id, thumbsUp)
	if c := counts(r); r.status != 200 || c[thumbsUp] != 1 || len(r.at("reactions", "mine").([]any)) != 0 || num(r.at("rev_seq")) != 5 {
		t.Fatalf("bob takes it back: %d %v", r.status, r.body)
	}
	if r := e.unreact(tt, "bob", id, thumbsUp); r.status != 200 || num(r.at("rev_seq")) != 5 {
		t.Fatalf("taking back twice: %d %v", r.status, r.body)
	}
	if r := e.unreact(tt, "alice", id, "❤️"); counts(r)["❤️"] != 0 {
		t.Fatalf("an emoji nobody uses is gone from the counts: %v", counts(r))
	} else if _, still := counts(r)["❤️"]; still {
		t.Fatalf("a count of zero must not stay in the summary: %v", counts(r))
	}

	// sync gives the changed message once, with its latest state
	s := e.asUser(tt, "bob", "GET", "/v1/channels/"+ch+"/sync?since_rev=1", nil)
	if count(s.at("messages")) != 1 || num(s.at("messages", 0, "rev_seq")) != 6 || num(s.at("next_rev")) != 6 {
		t.Fatalf("sync after the reactions: %v", s.body)
	}

	// who may react
	if r := e.react(tt, "carol", id, thumbsUp); r.status != 404 {
		t.Fatalf("a stranger reacted: %d", r.status)
	}
	if r := e.react(tt, "bob", "00000000-0000-0000-0000-000000000000", thumbsUp); r.status != 404 {
		t.Fatalf("unknown message: %d", r.status)
	}
	if r := e.asUser(tt, "bob", "PUT", "/v1/messages/"+id+"/reactions/a%20b", nil); r.status != 400 || r.errCode() != "validation_failed" {
		t.Fatalf("emoji with a space: %d %v", r.status, r.body)
	}
	if r := e.asUser(tt, "bob", "PUT", "/v1/messages/"+id+"/reactions/"+strings.Repeat("x", 65), nil); r.status != 400 {
		t.Fatalf("emoji too long: %d", r.status)
	}
	sys := e.asServer(tt, "POST", "/v1/server/channels/"+ch+"/messages", map[string]any{"text": "closed"})
	if r := e.react(tt, "bob", str(sys.at("id")), thumbsUp); r.status != 400 {
		t.Fatalf("reaction to a system message: %d", r.status)
	}

	// at most 10 different reactions of one user on one message
	for i := range 10 {
		if r := e.react(tt, "bob", id, fmt.Sprintf("r%d", i)); r.status != 200 {
			t.Fatalf("reaction %d: %d %v", i, r.status, r.body)
		}
	}
	if r := e.react(tt, "bob", id, "r10"); r.status != 400 {
		t.Fatalf("an 11th reaction: %d %v", r.status, r.body)
	}
	if c := counts(e.react(tt, "bob", id, "r0")); c["r10"] != 0 {
		t.Fatalf("the refused reaction was counted: %v", c)
	}

	// a frozen channel is read-only
	e.asServer(tt, "POST", "/v1/server/channels/"+ch+"/freeze", nil)
	if r := e.react(tt, "alice", id, "🎉"); r.status != 403 || r.errCode() != "channel_frozen" {
		t.Fatalf("reaction in a frozen channel: %d %v", r.status, r.body)
	}
}

func TestReactionsFromManyUsersAtOnce(t *testing.T) {
	e := newEnv(t)
	tt := e.createTenant(tenant.Config{})
	const n = 12
	members := make([]string, n)
	for i := range members {
		members[i] = fmt.Sprintf("u%02d", i)
	}
	e.seed(tt, append([]string{"owner"}, members...)...)
	ch := e.group(tt, "owner", "Crowd", members...)
	id := str(e.mustSay(tt, "owner", ch, "everybody react").at("id"))

	parallel(n, func(i int) {
		r, err := e.request("PUT", "/v1/messages/"+id+"/reactions/"+url.PathEscape(thumbsUp), tt.APIKey, tt.userToken(t, members[i]), nil)
		if err != nil || r.status != 200 {
			t.Errorf("reaction of %s: %v %d %v", members[i], err, r.status, r.body)
		}
	})
	last := e.asUser(tt, "owner", "GET", "/v1/channels/"+ch+"/sync?since_rev=0&limit=100", nil)
	var found response
	for _, m := range last.at("messages").([]any) {
		if str(at(m, "id")) == id {
			found = response{body: m.(map[string]any)}
		}
	}
	if counts(found)[thumbsUp] != n {
		t.Fatalf("%d simultaneous reactions counted as %v", n, counts(found)[thumbsUp])
	}
	// every change took its own value of the channel counter (a response shows the message as it is when it is
	// read, which may already include the change of another request)
	taken := countWhere(t, `SELECT count(DISTINCT payload->>'rev_seq') FROM outbox WHERE tenant_id = $1 AND topic = 'message.reaction' AND payload->>'message_id' = $2`, tt.ID, id)
	if taken != n {
		t.Fatalf("%d reactions took %d different revisions", n, taken)
	}
	if got, want := num(found.at("rev_seq")), num(found.at("seq"))+n; got != want {
		t.Fatalf("the message ends at revision %d, want %d (its own seq plus one per reaction)", got, want)
	}
}

func TestEditAndDelete(t *testing.T) {
	e := newEnv(t)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob", "carol")
	ch := e.dm(tt, "alice", "bob")
	msg := e.mustSay(tt, "alice", ch, "first draft")
	id := str(msg.at("id"))
	edit := func(who, text string) response {
		return e.asUser(tt, who, "PATCH", "/v1/messages/"+id, map[string]any{"text": text})
	}

	r := edit("alice", "  final text  ")
	if r.status != 200 || r.at("text") != "final text" || r.at("edited_at") == nil || num(r.at("rev_seq")) != 2 || num(r.at("seq")) != 1 {
		t.Fatalf("edit: %d %v", r.status, r.body)
	}
	if r := edit("alice", "final text"); r.status != 200 || num(r.at("rev_seq")) != 2 {
		t.Fatalf("editing to the same text must change nothing: %v", r.body)
	}
	for name, got := range map[string]response{
		"bob edits alice's message": edit("bob", "hacked"),
		"a stranger edits":          edit("carol", "hacked"),
		"empty text":                edit("alice", "   "),
		"text too long":             edit("alice", strings.Repeat("x", 4001)),
	} {
		want := map[string]int{"bob edits alice's message": 403, "a stranger edits": 404, "empty text": 400, "text too long": 400}[name]
		if got.status != want {
			t.Errorf("%s: %d %v, want %d", name, got.status, got.body, want)
		}
	}
	sys := e.asServer(tt, "POST", "/v1/server/channels/"+ch+"/messages", map[string]any{"text": "notice"})
	if r := e.asUser(tt, "alice", "PATCH", "/v1/messages/"+str(sys.at("id")), map[string]any{"text": "x"}); r.status != 403 {
		t.Fatalf("editing a system message: %d", r.status)
	}
	// the edit shows in history and in sync
	if h := e.asUser(tt, "bob", "GET", "/v1/channels/"+ch+"/messages", nil); h.at("messages", 1, "text") != "final text" {
		t.Fatalf("history after the edit: %v", h.body)
	}
	if s := e.asUser(tt, "bob", "GET", "/v1/channels/"+ch+"/sync?since_rev=1", nil); s.at("messages", 0, "text") != "final text" || num(s.at("next_rev")) < 2 {
		t.Fatalf("sync after the edit: %v", s.body)
	}

	// deleting: only the sender in a DM
	del := func(who string) response { return e.asUser(tt, who, "DELETE", "/v1/messages/"+id, nil) }
	if r := del("bob"); r.status != 403 {
		t.Fatalf("bob deleted alice's message in a DM: %d", r.status)
	}
	if r := del("carol"); r.status != 404 {
		t.Fatalf("a stranger deleted: %d", r.status)
	}
	e.react(tt, "bob", id, thumbsUp)
	if r := del("alice"); r.status != 204 {
		t.Fatalf("alice deletes: %d %v", r.status, r.body)
	}
	if r := del("alice"); r.status != 204 {
		t.Fatalf("deleting twice: %d", r.status)
	}
	tomb := e.asUser(tt, "bob", "GET", "/v1/channels/"+ch+"/sync?since_rev=0", nil)
	var gone map[string]any
	for _, m := range tomb.at("messages").([]any) {
		if str(at(m, "id")) == id {
			gone = m.(map[string]any)
		}
	}
	if gone == nil || gone["text"] != nil || gone["deleted_at"] == nil || count(gone["attachments"]) != 0 || len(at(gone, "reactions", "counts").(map[string]any)) != 0 ||
		at(gone, "sender", "external_id") != "alice" || num(gone["seq"]) != 1 {
		t.Fatalf("tombstone: %v", gone)
	}
	if r := edit("alice", "back from the dead"); r.status != 404 {
		t.Fatalf("editing a deleted message: %d", r.status)
	}
	if r := e.react(tt, "bob", id, "🎉"); r.status != 404 {
		t.Fatalf("reacting to a deleted message: %d", r.status)
	}
	last := str(e.mustSay(tt, "alice", ch, "the last one").at("id"))
	e.asUser(tt, "alice", "DELETE", "/v1/messages/"+last, nil)
	if c := e.asUser(tt, "bob", "GET", "/v1/channels/"+ch, nil); c.at("last_message", "deleted_at") == nil || c.at("last_message", "text") != nil || c.at("last_message", "id") != last {
		t.Fatalf("the channel's last message shows its tombstone: %v", c.at("last_message"))
	}
	if got := e.unread(tt, "bob", ch); got != 0 {
		t.Fatalf("a deleted message counts as unread: %d", got)
	}
}

func TestOwnersDeleteOtherPeoplesMessagesInAGroup(t *testing.T) {
	e := newEnv(t)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "owner", "a", "b")
	ch := e.group(tt, "owner", "Rules", "a", "b")
	byA := str(e.mustSay(tt, "a", ch, "from a").at("id"))
	byB := str(e.mustSay(tt, "b", ch, "from b").at("id"))
	byOwner := str(e.mustSay(tt, "owner", ch, "from the owner").at("id"))
	del := func(who, id string) int { return e.asUser(tt, who, "DELETE", "/v1/messages/"+id, nil).status }

	if got := del("b", byA); got != 403 {
		t.Fatalf("a member deleted another member's message: %d", got)
	}
	if got := del("a", byOwner); got != 403 {
		t.Fatalf("a member deleted the owner's message: %d", got)
	}
	if got := del("owner", byA); got != 204 {
		t.Fatalf("the owner could not delete a member's message: %d", got)
	}
	if got := del("b", byB); got != 204 {
		t.Fatalf("a member could not delete their own message: %d", got)
	}
	// a member who left cannot touch anything any more
	e.asUser(tt, "a", "DELETE", "/v1/channels/"+ch+"/members/me", nil)
	if got := del("a", byOwner); got != 404 {
		t.Fatalf("a member who left deleted a message: %d", got)
	}
	e.asServer(tt, "POST", "/v1/server/channels/"+ch+"/freeze", nil)
	if got := del("owner", byOwner); got != 403 {
		t.Fatalf("deleting in a frozen channel: %d", got)
	}
	if r := e.asUser(tt, "owner", "PATCH", "/v1/messages/"+byOwner, map[string]any{"text": "x"}); r.status != 403 || r.errCode() != "channel_frozen" {
		t.Fatalf("editing in a frozen channel: %d %v", r.status, r.body)
	}
}

func TestReplies(t *testing.T) {
	e := newEnv(t)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob", "carol")
	ch := e.dm(tt, "alice", "bob")
	other := e.dm(tt, "alice", "carol")
	original := e.mustSay(tt, "alice", ch, strings.Repeat("é", 250))
	id := str(original.at("id"))
	reply := func(who, channel string, replyTo any) response {
		return e.asUser(tt, who, "POST", "/v1/channels/"+channel+"/messages",
			map[string]any{"client_msg_id": fmt.Sprintf("r-%d", time.Now().UnixNano()), "text": "answer", "reply_to_id": replyTo})
	}

	r := reply("bob", ch, id)
	if r.status != 201 || r.at("reply_to", "id") != id || num(r.at("reply_to", "seq")) != 1 || r.at("reply_to", "sender_id") != "alice" ||
		r.at("reply_to", "has_attachments") != false || r.at("reply_to", "deleted") != false {
		t.Fatalf("reply: %d %v", r.status, r.body)
	}
	if got := []rune(str(r.at("reply_to", "text"))); len(got) != 200 {
		t.Fatalf("the quoted text has %d characters, want it cut at 200", len(got))
	}
	// the quote follows the original: an edit shows, a deletion shows
	e.asUser(tt, "alice", "PATCH", "/v1/messages/"+id, map[string]any{"text": "edited original"})
	if h := e.asUser(tt, "bob", "GET", "/v1/channels/"+ch+"/messages", nil); h.at("messages", 0, "reply_to", "text") != "edited original" {
		t.Fatalf("the quote did not follow the edit: %v", h.at("messages", 0, "reply_to"))
	}
	e.asUser(tt, "alice", "DELETE", "/v1/messages/"+id, nil)
	if h := e.asUser(tt, "bob", "GET", "/v1/channels/"+ch+"/messages", nil); h.at("messages", 0, "reply_to", "deleted") != true || h.at("messages", 0, "reply_to", "text") != nil {
		t.Fatalf("the quote of a deleted message: %v", h.at("messages", 0, "reply_to"))
	}
	// replying to a deleted message is still allowed: it may have been deleted while the answer was being typed
	if r := reply("bob", ch, id); r.status != 201 || r.at("reply_to", "deleted") != true {
		t.Fatalf("replying to a deleted message: %d %v", r.status, r.body)
	}

	// only to a message of the same channel
	inOther := str(e.mustSay(tt, "carol", other, "elsewhere").at("id"))
	for name, target := range map[string]any{
		"message of another channel": inOther,
		"unknown message":            "00000000-0000-0000-0000-000000000000",
		"not an id":                  "nope",
	} {
		if r := reply("bob", ch, target); r.status != 400 || r.errCode() != "validation_failed" {
			t.Errorf("%s: %d %v", name, r.status, r.body)
		}
	}
	// the channel list quotes too
	if c := e.asUser(tt, "bob", "GET", "/v1/channels/"+ch, nil); c.at("last_message", "reply_to", "deleted") != true {
		t.Fatalf("last message in the channel list: %v", c.at("last_message"))
	}
}

func TestWebSocketAnnouncesChangesToMessages(t *testing.T) {
	e := newRealtimeEnv(t, 2, nil)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob", "carol")
	ch := e.dm(tt, "alice", "bob")
	alice := e.connect(0, tt, "alice")
	bob := e.connect(1, tt, "bob")
	carol := e.connect(0, tt, "carol")

	// a reply sent over the WebSocket carries its quote
	alice.send("message.send", "s1", map[string]any{"channel_id": ch, "client_msg_id": "w1", "text": "original"})
	first := alice.next("ack")
	id := str(at(first, "data", "message", "id"))
	bob.next("message.new")
	bob.send("message.send", "s2", map[string]any{"channel_id": ch, "client_msg_id": "w2", "text": "answer", "reply_to_id": id})
	if ack := bob.next("ack"); at(ack, "data", "message", "reply_to", "text") != "original" {
		t.Fatalf("ack of a reply: %v", ack)
	}
	if ev := alice.next("message.new"); at(ev, "data", "message", "reply_to", "sender_id") != "alice" {
		t.Fatalf("message.new of a reply: %v", ev)
	}

	// a reaction: everybody hears it, each with their own reactions as theirs
	if r := e.react(tt, "bob", id, thumbsUp); r.status != 200 {
		t.Fatal(r.body)
	}
	evA, evB := alice.next("reaction.added"), bob.next("reaction.added")
	for name, ev := range map[string]map[string]any{"alice": evA, "bob": evB} {
		if at(ev, "data", "emoji") != thumbsUp || at(ev, "data", "user_id") != "bob" || at(ev, "data", "message_id") != id ||
			at(ev, "data", "channel_id") != ch || at(ev, "data", "reactions", "counts", thumbsUp) != float64(1) {
			t.Errorf("%s: reaction.added %v", name, ev)
		}
	}
	if count(at(evA, "data", "reactions", "mine")) != 0 || at(evB, "data", "reactions", "mine", 0) != thumbsUp {
		t.Errorf("mine: alice %v, bob %v", at(evA, "data", "reactions", "mine"), at(evB, "data", "reactions", "mine"))
	}
	revAdded := at(evA, "data", "rev_seq")
	carol.none("reaction.added", 0)
	e.unreact(tt, "bob", id, thumbsUp)
	if ev := alice.next("reaction.removed"); at(ev, "data", "reactions", "counts", thumbsUp) != nil || at(ev, "data", "rev_seq").(float64) <= revAdded.(float64) {
		t.Fatalf("reaction.removed: %v", ev)
	}
	bob.next("reaction.removed")

	// an edit
	e.react(tt, "alice", id, "🎉")
	alice.next("reaction.added")
	bob.next("reaction.added")
	if r := e.asUser(tt, "alice", "PATCH", "/v1/messages/"+id, map[string]any{"text": "edited"}); r.status != 200 {
		t.Fatal(r.body)
	}
	evA, evB = alice.next("message.updated"), bob.next("message.updated")
	if at(evA, "data", "message", "text") != "edited" || at(evA, "data", "message", "edited_at") == nil || at(evA, "data", "message", "reactions", "mine", 0) != "🎉" ||
		count(at(evB, "data", "message", "reactions", "mine")) != 0 {
		t.Fatalf("message.updated: alice %v / bob %v", at(evA, "data", "message", "reactions"), at(evB, "data", "message", "reactions"))
	}
	carol.none("message.updated", 0)

	// a deletion
	if r := e.asUser(tt, "alice", "DELETE", "/v1/messages/"+id, nil); r.status != 204 {
		t.Fatal(r.status)
	}
	for name, w := range map[string]*wsConn{"alice": alice, "bob": bob} {
		ev := w.next("message.deleted")
		if at(ev, "data", "message", "id") != id || at(ev, "data", "message", "deleted_at") == nil || at(ev, "data", "message", "text") != nil {
			t.Errorf("%s: message.deleted %v", name, ev)
		}
	}
	// the quote in the answer now says the original is gone, which a client learns when it loads the answer again
	if h := e.asUser(tt, "bob", "GET", "/v1/channels/"+ch+"/messages", nil); h.at("messages", 0, "reply_to", "deleted") != true {
		t.Fatalf("answer after the deletion: %v", h.at("messages", 0))
	}
	_ = realtime.ProtocolVersion
}
