//go:build integration

package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/rizkysiregar/marpokat/server/internal/broker"
	"github.com/rizkysiregar/marpokat/server/internal/outbox"
	"github.com/rizkysiregar/marpokat/server/internal/platform/rabbitmq"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
	"github.com/rizkysiregar/marpokat/server/internal/testdb"
)

func TestPushGoesToMembersWhoAreOffline(t *testing.T) {
	drainOutbox(t)
	e := newRealtimeEnv(t, 1, nil)
	g := newFakeGoogle(t)
	startWorker(t, g, nil)
	tt := e.createTenant(tenant.Config{})
	e.withFCM(tt, g)
	e.seed(tt, "alice", "bob", "carol", "dave", "eve")
	ch := e.group(tt, "alice", "Crew", "bob", "carol", "dave")
	e.registerDevice(tt, "alice", "android", "alice-phone")
	e.registerDevice(tt, "bob", "android", "bob-phone")
	e.registerDevice(tt, "bob", "web", "bob-browser")
	e.registerDevice(tt, "carol", "android", "carol-phone")
	e.registerDevice(tt, "dave", "android", "dave-phone")
	e.registerDevice(tt, "eve", "android", "eve-phone")
	e.mute(mustUUID(t, ch), e.userID(tt, "carol"))
	e.connect(0, tt, "dave") // dave has the app open

	e.mustSay(tt, "alice", ch, "hello crew")
	phone := g.waitDelivered(t, "bob-phone", 1, 10*time.Second)[0]
	browser := g.waitDelivered(t, "bob-browser", 1, 10*time.Second)[0]

	want := map[string]string{
		"marpokat": "1", "type": "message.new", "channel_id": ch, "channel_type": "group", "channel_name": "Crew",
		"sender_id": "alice", "sender_name": "User alice", "text": "hello crew", "unread_count": "1", "seq": "2",
		"attachment_kind": "", "sender_avatar_url": "",
	}
	for k, v := range want {
		if phone.Data[k] != v {
			t.Errorf("push data[%q] = %q, want %q", k, phone.Data[k], v)
		}
	}
	if phone.Data["message_id"] == "" || phone.Auth != "Bearer fake-access-token" || phone.Platform != "android" || browser.Platform != "web" {
		t.Errorf("push: %+v / %+v", phone, browser)
	}

	// everyone else is left alone: the sender, someone looking at the chat, someone who muted it, someone outside it
	stays(t, 700*time.Millisecond, "no other push", func() bool {
		return g.total() == 2 && g.attempts("alice-phone")+g.attempts("carol-phone")+g.attempts("dave-phone")+g.attempts("eve-phone") == 0
	})

	// an announcement of the channel never rings a phone
	if r := e.asUser(tt, "alice", "PATCH", "/v1/channels/"+ch, map[string]any{"name": "Crew 2"}); r.status != 200 {
		t.Fatal(r.body)
	}
	stays(t, 700*time.Millisecond, "no push for an announcement", func() bool { return g.total() == 2 })

	// the unread count grows with the messages, and shrinks when bob has read
	e.mustSay(tt, "alice", ch, "second")
	if got := g.waitDelivered(t, "bob-phone", 2, 10*time.Second); got[1].Data["unread_count"] != "2" || got[1].Data["channel_name"] != "Crew 2" {
		t.Errorf("second push: %v", got[1].Data)
	}
	if r := e.asUser(tt, "bob", "POST", "/v1/channels/"+ch+"/read", map[string]any{"seq": 100}); r.status != 204 {
		t.Fatal(r.status)
	}
	e.mustSay(tt, "alice", ch, "third")
	if got := g.waitDelivered(t, "bob-phone", 3, 10*time.Second); got[2].Data["unread_count"] != "1" {
		t.Errorf("third push: %v", got[2].Data)
	}
}

func TestPushWithoutCredentialsIsSkipped(t *testing.T) {
	drainOutbox(t)
	e := newRealtimeEnv(t, 1, nil)
	g := newFakeGoogle(t)
	w := startWorker(t, g, nil)
	tt := e.createTenant(tenant.Config{}) // never asked for push
	e.seed(tt, "alice", "bob")
	ch := e.dm(tt, "alice", "bob")
	e.registerDevice(tt, "bob", "android", "bob-phone")

	e.mustSay(tt, "alice", ch, "anyone there?")
	waitUntil(t, 10*time.Second, "the event to be handled", func() bool {
		return testutil.ToFloat64(w.metrics.PushSkipped.WithLabelValues("no_credentials")) >= 1
	})
	if g.total() != 0 {
		t.Fatalf("FCM was called %d times for a tenant without credentials", g.total())
	}
}

func TestPushRemovesTokensFCMDoesNotKnow(t *testing.T) {
	drainOutbox(t)
	e := newRealtimeEnv(t, 1, nil)
	g := newFakeGoogle(t)
	g.answer("bad-phone", func(int) (int, string) {
		return 404, `{"error":{"code":404,"status":"NOT_FOUND","message":"Requested entity was not found.","details":[{"errorCode":"UNREGISTERED"}]}}`
	})
	w := startWorker(t, g, nil)
	tt := e.createTenant(tenant.Config{})
	e.withFCM(tt, g)
	e.seed(tt, "alice", "bob")
	ch := e.dm(tt, "alice", "bob")
	e.registerDevice(tt, "bob", "android", "good-phone")
	e.registerDevice(tt, "bob", "android", "bad-phone")

	e.mustSay(tt, "alice", ch, "one")
	g.waitDelivered(t, "good-phone", 1, 10*time.Second)
	waitUntil(t, 10*time.Second, "the unknown token to be removed", func() bool {
		return countWhere(t, `SELECT count(*) FROM devices WHERE tenant_id = $1 AND push_token = 'bad-phone'`, tt.ID) == 0
	})
	if countWhere(t, `SELECT count(*) FROM devices WHERE tenant_id = $1 AND push_token = 'good-phone'`, tt.ID) != 1 {
		t.Fatal("the good device was removed too")
	}
	if got := testutil.ToFloat64(w.metrics.PushFailed.WithLabelValues("invalid_token")); got != 1 {
		t.Errorf("invalid_token metric = %v, want 1", got)
	}

	// FCM is not asked about it again, and the good one keeps working
	e.mustSay(tt, "alice", ch, "two")
	g.waitDelivered(t, "good-phone", 2, 10*time.Second)
	if g.attempts("bad-phone") != 1 {
		t.Errorf("FCM was asked about the removed token %d times, want once", g.attempts("bad-phone"))
	}
}

func TestPushRetriesWhatFailedAndOnlyThat(t *testing.T) {
	drainOutbox(t)
	e := newRealtimeEnv(t, 1, nil)
	g := newFakeGoogle(t)
	g.answer("flaky-phone", func(n int) (int, string) { // two failures, then it works
		if n <= 2 {
			return 503, `{"error":{"code":503,"status":"UNAVAILABLE","message":"try later"}}`
		}
		return 200, `{}`
	})
	w := startWorker(t, g, nil)
	tt := e.createTenant(tenant.Config{})
	e.withFCM(tt, g)
	e.seed(tt, "alice", "bob")
	ch := e.dm(tt, "alice", "bob")
	e.registerDevice(tt, "bob", "android", "steady-phone")
	e.registerDevice(tt, "bob", "android", "flaky-phone")

	e.mustSay(tt, "alice", ch, "persistent")
	g.waitDelivered(t, "flaky-phone", 1, 15*time.Second)
	stays(t, time.Second, "no extra pushes after the success", func() bool {
		return g.attempts("flaky-phone") == 3 && len(g.delivered("steady-phone")) == 1
	})
	if got := testutil.ToFloat64(w.metrics.ConsumerHandled.WithLabelValues("push", "retry")); got != 2 {
		t.Errorf("retries = %v, want 2", got)
	}
	if got := testutil.ToFloat64(w.metrics.ConsumerHandled.WithLabelValues("push", "dead")); got != 0 {
		t.Errorf("dead letters = %v, want 0", got)
	}
}

func TestPushThatKeepsFailingEndsInTheDeadLetterQueue(t *testing.T) {
	drainOutbox(t)
	e := newRealtimeEnv(t, 1, nil)
	g := newFakeGoogle(t)
	g.answer("down-phone", func(int) (int, string) { return 503, `{"error":{"status":"UNAVAILABLE","message":"down"}}` })
	w := startWorker(t, g, nil)
	tt := e.createTenant(tenant.Config{})
	e.withFCM(tt, g)
	e.seed(tt, "alice", "bob")
	ch := e.dm(tt, "alice", "bob")
	e.registerDevice(tt, "bob", "android", "down-phone")

	e.mustSay(tt, "alice", ch, "never arrives")
	dlq := w.names.DeadQueue("push")
	waitUntil(t, 15*time.Second, "the message in the dead letter queue", func() bool {
		n, ok := broker.QueueLength(w.conn, dlq)
		return ok && n == 1
	})
	// first try plus three retries, then it is left alone
	stays(t, time.Second, "no more attempts", func() bool { return g.attempts("down-phone") == 4 })
	for _, q := range []string{w.names.Queue("push"), w.names.RetryQueue("push", 1), w.names.RetryQueue("push", 2), w.names.RetryQueue("push", 3)} {
		if n, _ := broker.QueueLength(w.conn, q); n != 0 {
			t.Errorf("queue %s still holds %d messages", q, n)
		}
	}
	if got := testutil.ToFloat64(w.metrics.ConsumerHandled.WithLabelValues("push", "dead")); got != 1 {
		t.Errorf("dead letters = %v, want 1", got)
	}
	// the length of the dead letter queue is what an alert watches
	waitUntil(t, 5*time.Second, "the queue gauge", func() bool {
		return testutil.ToFloat64(w.metrics.QueueMessages.WithLabelValues(dlq)) == 1
	})
}

// The scenario of the milestone: RabbitMQ goes away while chat goes on, comes back, and every push is sent once.
func TestPushSurvivesTheBrokerBeingDown(t *testing.T) {
	drainOutbox(t)
	e := newRealtimeEnv(t, 1, nil)
	g := newFakeGoogle(t)
	w := startWorker(t, g, nil)
	tt := e.createTenant(tenant.Config{})
	e.withFCM(tt, g)
	e.seed(tt, "alice", "bob")
	ch := e.dm(tt, "alice", "bob")
	e.registerDevice(tt, "bob", "android", "bob-phone")

	e.mustSay(tt, "alice", ch, "before the outage")
	g.waitDelivered(t, "bob-phone", 1, 10*time.Second)

	testdb.RabbitCtl(t, "stop_app")
	waitUntil(t, 20*time.Second, "the worker to notice the broker is gone", func() bool { return !w.conn.Ready() })

	// chat does not care: the messages are stored, and their events wait in the outbox
	for _, text := range []string{"during the outage 1", "during the outage 2"} {
		e.mustSay(tt, "alice", ch, text)
	}
	pending := func() int {
		return countWhere(t, `SELECT count(*) FROM outbox WHERE tenant_id = $1 AND topic = 'message.created' AND published_at IS NULL`, tt.ID)
	}
	if got := pending(); got != 2 {
		t.Fatalf("%d events wait in the outbox, want 2", got)
	}
	stays(t, time.Second, "no push while the broker is down", func() bool { return len(g.delivered("bob-phone")) == 1 })

	testdb.RabbitCtl(t, "start_app")
	pushes := g.waitDelivered(t, "bob-phone", 3, 60*time.Second)
	waitUntil(t, 10*time.Second, "the outbox to be emptied", func() bool { return pending() == 0 })

	// each message exactly once, nothing extra
	stays(t, time.Second, "no duplicate push", func() bool { return len(g.delivered("bob-phone")) == 3 })
	var texts []string
	for _, p := range pushes {
		texts = append(texts, p.Data["text"])
	}
	if got := strings.Join(texts, "|"); !strings.Contains(got, "before the outage") || !strings.Contains(got, "during the outage 1") || !strings.Contains(got, "during the outage 2") {
		t.Fatalf("pushes: %v", texts)
	}
}

func TestPushIsSentOnceWhateverTheRelayDoes(t *testing.T) {
	drainOutbox(t)
	e := newRealtimeEnv(t, 1, nil)
	g := newFakeGoogle(t)
	startWorker(t, g, nil)
	tt := e.createTenant(tenant.Config{})
	e.withFCM(tt, g)
	e.seed(tt, "alice", "bob")
	ch := e.dm(tt, "alice", "bob")
	e.registerDevice(tt, "bob", "android", "bob-phone")

	e.mustSay(tt, "alice", ch, "just once")
	g.waitDelivered(t, "bob-phone", 1, 10*time.Second)

	// the relay is at-least-once: if it crashed after publishing and before marking, the same event goes out again
	if _, err := testdb.Pool(t).Exec(context.Background(),
		`UPDATE outbox SET published_at = NULL WHERE tenant_id = $1 AND topic = 'message.created'`, tt.ID); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, "the event to be relayed again", func() bool {
		return countWhere(t, `SELECT count(*) FROM outbox WHERE tenant_id = $1 AND topic = 'message.created' AND published_at IS NULL`, tt.ID) == 0
	})
	stays(t, 1500*time.Millisecond, "no second push", func() bool { return len(g.delivered("bob-phone")) == 1 })
}

func TestPushSkipsStaleEvents(t *testing.T) {
	drainOutbox(t)
	e := newRealtimeEnv(t, 1, nil)
	g := newFakeGoogle(t)
	tt := e.createTenant(tenant.Config{})
	e.withFCM(tt, g)
	e.seed(tt, "alice", "bob")
	ch := e.dm(tt, "alice", "bob")
	e.registerDevice(tt, "bob", "android", "bob-phone")
	e.mustSay(tt, "alice", ch, "from long ago") // sent while no worker was running: a backlog

	time.Sleep(700 * time.Millisecond)
	w := startWorker(t, g, func(o *WorkerOptions) { o.PushMaxAge = 400 * time.Millisecond })
	waitUntil(t, 10*time.Second, "the backlog to be handled", func() bool {
		return testutil.ToFloat64(w.metrics.PushSkipped.WithLabelValues("stale")) >= 1
	})
	if g.total() != 0 {
		t.Fatal("a push was sent for a message from before the worker started")
	}
	e.mustSay(tt, "alice", ch, "fresh") // new messages are pushed as usual
	g.waitDelivered(t, "bob-phone", 1, 10*time.Second)
}

func TestOutboxRelayKeepsOrderAndMarksWhatItPublished(t *testing.T) {
	drainOutbox(t)
	e := newRealtimeEnv(t, 1, nil)
	g := newFakeGoogle(t)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob")
	ch := e.dm(tt, "alice", "bob")
	for i := range 5 {
		e.mustSay(tt, "alice", ch, fmt.Sprintf("message %d", i))
	}

	// a relay on its own, with a recording publisher in place of RabbitMQ
	rec := &recordingPublisher{}
	r := outbox.NewRelay(testdb.Pool(t), rec, discardLogger(), nil)
	r.Batch = 3
	total := 0
	for {
		n, err := r.Once(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		total += n
	}
	var seqs []string
	for _, env := range rec.all() {
		if env.TenantID == tt.ID.String() && env.Topic == outbox.TopicMessageCreated {
			seqs = append(seqs, string(env.Payload))
		}
	}
	if len(seqs) != 5 {
		t.Fatalf("relayed %d message events of the tenant, want 5 (%d events in all)", len(seqs), total)
	}
	for i := 1; i < len(seqs); i++ { // payloads carry the seq: in order
		if seqs[i-1] >= seqs[i] && !strings.Contains(seqs[i], fmt.Sprintf(`"seq":%d`, i+2)) {
			t.Errorf("events out of order: %v", seqs)
			break
		}
	}
	if left := countWhere(t, `SELECT count(*) FROM outbox WHERE tenant_id = $1 AND published_at IS NULL`, tt.ID); left != 0 {
		t.Fatalf("%d events are still unpublished", left)
	}

	// a failing broker leaves the events where they are, and counts the attempt
	e.mustSay(tt, "alice", ch, "one more")
	rec.fail(fmt.Errorf("broker down"))
	if _, err := r.Once(context.Background()); err == nil {
		t.Fatal("a publish failure was not reported")
	}
	if got := countWhere(t, `SELECT count(*) FROM outbox WHERE tenant_id = $1 AND published_at IS NULL AND attempts = 1 AND last_error LIKE '%broker down%'`, tt.ID); got != 1 {
		t.Fatalf("the failed event is not marked with its error: %d", got)
	}
	rec.fail(nil)
	if n, err := r.Once(context.Background()); err != nil || n < 1 {
		t.Fatalf("after the broker is back: %d, %v", n, err)
	}
	_ = g
	_ = rabbitmq.ErrNotConnected
}

func TestOutboxCleanerDeletesOnlyOldPublishedEvents(t *testing.T) {
	drainOutbox(t)
	e := newRealtimeEnv(t, 1, nil)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob")
	ch := e.dm(tt, "alice", "bob")
	e.mustSay(tt, "alice", ch, "old")
	pool := testdb.Pool(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE outbox SET published_at = now() - interval '8 days' WHERE tenant_id = $1`, tt.ID) // all of them: old and published
	e.mustSay(tt, "alice", ch, "recent")
	exec(`UPDATE outbox SET published_at = now() - interval '1 day' WHERE tenant_id = $1 AND published_at IS NULL AND topic = 'message.created'`, tt.ID)
	e.mustSay(tt, "alice", ch, "waiting") // unpublished

	before := countWhere(t, `SELECT count(*) FROM outbox WHERE tenant_id = $1`, tt.ID)
	n, err := outbox.NewCleaner(pool, discardLogger()).Once(context.Background())
	if err != nil || n < 1 {
		t.Fatalf("cleaner deleted %d rows, err %v", n, err)
	}
	after := countWhere(t, `SELECT count(*) FROM outbox WHERE tenant_id = $1`, tt.ID)
	if countWhere(t, `SELECT count(*) FROM outbox WHERE tenant_id = $1 AND published_at < now() - interval '7 days'`, tt.ID) != 0 {
		t.Fatal("events published more than 7 days ago are still there")
	}
	if got := countWhere(t, `SELECT count(*) FROM outbox WHERE tenant_id = $1 AND published_at IS NOT NULL`, tt.ID); got != 1 {
		t.Fatalf("%d recent published events left, want exactly the one of yesterday", got)
	}
	if got := countWhere(t, `SELECT count(*) FROM outbox WHERE tenant_id = $1 AND published_at IS NULL`, tt.ID); got != 1 {
		t.Fatalf("%d unpublished events left, want the one still waiting", got)
	}
	if before-after < 1 {
		t.Fatalf("nothing was deleted: %d -> %d", before, after)
	}
}

func TestDevices(t *testing.T) {
	e := newEnv(t)
	tt := e.createTenant(tenant.Config{})
	e.seed(tt, "alice", "bob")
	owner := func(token string) string {
		var u string
		_ = testdb.Pool(t).QueryRow(context.Background(), `SELECT user_id::text FROM devices WHERE tenant_id = $1 AND push_token = $2`, tt.ID, token).Scan(&u)
		return u
	}
	alice, bob := e.userID(tt, "alice").String(), e.userID(tt, "bob").String()

	e.registerDevice(tt, "alice", "android", "shared-phone")
	if owner("shared-phone") != alice {
		t.Fatal("the token is not alice's")
	}
	// the same phone, another person signed in: it moves
	e.registerDevice(tt, "bob", "android", "shared-phone")
	if owner("shared-phone") != bob {
		t.Fatal("the token did not move to bob")
	}
	// alice signing out cannot take bob's token away, and it is not an error
	if r := e.asUser(tt, "alice", "DELETE", "/v1/devices/shared-phone", nil); r.status != 204 || owner("shared-phone") != bob {
		t.Fatalf("alice removed bob's token: %d", r.status)
	}
	if r := e.asUser(tt, "bob", "DELETE", "/v1/devices/shared-phone", nil); r.status != 204 || owner("shared-phone") != "" {
		t.Fatalf("bob could not remove his own token: %d", r.status)
	}
	if r := e.asUser(tt, "bob", "DELETE", "/v1/devices/never-registered", nil); r.status != 204 {
		t.Fatalf("removing an unknown token: %d", r.status)
	}

	for name, body := range map[string]map[string]any{
		"unknown platform": {"platform": "ios", "push_token": "t"},
		"no token":         {"platform": "android", "push_token": ""},
		"token with space": {"platform": "android", "push_token": "a b"},
		"token too long":   {"platform": "android", "push_token": strings.Repeat("x", 4097)},
	} {
		if r := e.asUser(tt, "alice", "POST", "/v1/devices", body); r.status != 400 || r.errCode() != "validation_failed" {
			t.Errorf("%s: %d %v", name, r.status, r.body)
		}
	}
	if r := e.do("POST", "/v1/devices", tt.APIKey, "", map[string]any{"platform": "android", "push_token": "t"}); r.status != 401 {
		t.Errorf("without a token: %d", r.status)
	}

	// only the most recent 10 tokens of a user are kept
	for i := range 12 {
		e.registerDevice(tt, "alice", "android", fmt.Sprintf("phone-%02d", i))
		time.Sleep(3 * time.Millisecond)
	}
	if got := countWhere(t, `SELECT count(*) FROM devices WHERE tenant_id = $1 AND user_id = $2`, tt.ID, e.userID(tt, "alice")); got != 10 {
		t.Fatalf("alice has %d devices, want 10", got)
	}
	if owner("phone-00") != "" || owner("phone-11") != alice {
		t.Fatal("the oldest tokens must be the ones that go")
	}

	// another tenant does not see them
	other := e.createTenant(tenant.Config{})
	e.seed(other, "alice")
	e.registerDevice(other, "alice", "web", "phone-11")
	if owner("phone-11") != alice {
		t.Fatal("registering the same token in another app took it from this one")
	}
}

// On a fresh broker the relay may start before the consumer has declared its queue. An event published then must wait
// in the queue, not vanish because nothing was bound yet.
func TestEventsPublishedBeforeTheConsumerExistsAreKept(t *testing.T) {
	log := discardLogger()
	conn := rabbitmq.New(testdb.RabbitMQ(t).URL(), log)
	conn.SetBackoff(100*time.Millisecond, 500*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go conn.Run(ctx)
	waitUntil(t, 30*time.Second, "the connection", conn.Ready)

	opts := DefaultWorkerOptions()
	opts.Names = broker.Names{Prefix: "fresh" + uuid.NewString()[:8]}
	pub := broker.NewPublisher(conn, opts.Names, pushQueue(opts))
	defer pub.Close()
	err := pub.Publish(ctx, []broker.Envelope{{
		EventID: uuid.NewString(), TenantID: uuid.NewString(), Topic: outbox.TopicMessageCreated,
		Payload: []byte(`{}`), CreatedAt: time.Now(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := broker.QueueLength(conn, opts.Names.Queue("push")); !ok || n != 1 {
		t.Fatalf("the push queue holds %d messages (exists: %v), want the 1 that was published", n, ok)
	}
	// events of topics nobody works on are dropped on purpose: no queue, no pile-up
	err = pub.Publish(ctx, []broker.Envelope{{EventID: uuid.NewString(), TenantID: uuid.NewString(), Topic: outbox.TopicChannelCreated, Payload: []byte(`{}`), CreatedAt: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := broker.QueueLength(conn, opts.Names.Queue("push")); n != 1 {
		t.Fatalf("an event of another topic ended up in the push queue: %d messages", n)
	}
}
