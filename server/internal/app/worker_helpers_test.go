//go:build integration

package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/broker"
	"github.com/rizkysiregar/marpokat/server/internal/platform/metrics"
	"github.com/rizkysiregar/marpokat/server/internal/platform/rabbitmq"
	"github.com/rizkysiregar/marpokat/server/internal/platform/redisx"
	"github.com/rizkysiregar/marpokat/server/internal/platform/secretbox"
	"github.com/rizkysiregar/marpokat/server/internal/testdb"
)

var (
	keyOnce sync.Once
	keyPEM  string
)

// serviceAccountKey is one RSA key for every fake service account: generating keys is slow.
func serviceAccountKey(t *testing.T) string {
	t.Helper()
	keyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		der, _ := x509.MarshalPKCS8PrivateKey(key)
		keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	})
	return keyPEM
}

type googleCall struct {
	Token    string
	Platform string
	Data     map[string]string
	Auth     string
	Status   int
}

// fakeGoogle is Google as far as the worker is concerned: the OAuth token endpoint and the FCM send endpoint.
// Tests script what FCM answers for a device token.
type fakeGoogle struct {
	tokens, fcm *httptest.Server

	mu      sync.Mutex
	calls   []googleCall
	perTok  map[string]int
	behaves map[string]func(n int) (int, string)
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	g := &fakeGoogle{perTok: map[string]int{}, behaves: map[string]func(int) (int, string){}}
	g.tokens = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"fake-access-token","token_type":"Bearer","expires_in":3600}`)
	}))
	g.fcm = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Message struct {
				Token   string            `json:"token"`
				Data    map[string]string `json:"data"`
				Android json.RawMessage   `json:"android"`
				Webpush json.RawMessage   `json:"webpush"`
			} `json:"message"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		g.mu.Lock()
		g.perTok[body.Message.Token]++
		n, behave := g.perTok[body.Message.Token], g.behaves[body.Message.Token]
		g.mu.Unlock()
		status, resp := http.StatusOK, `{"name":"ok"}`
		if behave != nil {
			status, resp = behave(n)
		}
		platform := "android"
		if body.Message.Webpush != nil {
			platform = "web"
		}
		g.mu.Lock()
		g.calls = append(g.calls, googleCall{Token: body.Message.Token, Platform: platform, Data: body.Message.Data, Auth: r.Header.Get("Authorization"), Status: status})
		g.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(func() { g.tokens.Close(); g.fcm.Close() })
	return g
}

// account is the service account JSON a tenant would store, pointing at this fake.
func (g *fakeGoogle) account(t *testing.T) []byte {
	b, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "test-project", "private_key": serviceAccountKey(t),
		"client_email": "svc@test-project.iam.gserviceaccount.com", "token_uri": g.tokens.URL,
	})
	return b
}

// answer scripts FCM: for requests with this device token, fn gets the number of the request (1, 2, ...).
func (g *fakeGoogle) answer(token string, fn func(n int) (int, string)) {
	g.mu.Lock()
	g.behaves[token] = fn
	g.mu.Unlock()
}

func (g *fakeGoogle) delivered(token string) []googleCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []googleCall
	for _, c := range g.calls {
		if c.Token == token && c.Status == http.StatusOK {
			out = append(out, c)
		}
	}
	return out
}

func (g *fakeGoogle) attempts(token string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.perTok[token]
}

func (g *fakeGoogle) total() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}

// waitDelivered waits until FCM has accepted n pushes for the token and returns them.
func (g *fakeGoogle) waitDelivered(t *testing.T, token string, n int, timeout time.Duration) []googleCall {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if got := g.delivered(token); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("FCM accepted %d pushes for %q within %s, want %d", len(g.delivered(token)), token, timeout, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type testWorker struct {
	names   broker.Names
	conn    *rabbitmq.Conn
	metrics *metrics.Registry
	opts    WorkerOptions
}

// startWorker runs the relay and the push consumer against the shared RabbitMQ, Postgres and Redis, with queues of
// its own (a prefix per test) and short retry delays.
func startWorker(t *testing.T, g *fakeGoogle, tweak func(*WorkerOptions)) *testWorker {
	t.Helper()
	box, err := secretbox.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	rdb, err := redisx.Open(context.Background(), testdb.Redis(t))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	conn := rabbitmq.New(testdb.RabbitMQ(t).URL(), log)
	conn.SetBackoff(100*time.Millisecond, 500*time.Millisecond)

	opts := DefaultWorkerOptions()
	opts.Names = broker.Names{Prefix: "t" + uuid.NewString()[:8]}
	opts.RetryDelays = []time.Duration{150 * time.Millisecond, 150 * time.Millisecond, 150 * time.Millisecond}
	opts.RelayInterval = 30 * time.Millisecond
	opts.StatsEvery = 200 * time.Millisecond
	opts.FCMBaseURL = g.fcm.URL
	if tweak != nil {
		tweak(&opts)
	}
	m := metrics.New("test")
	w := NewWorker(WorkerDeps{Pool: testdb.Pool(t), Redis: rdb, Rabbit: conn, Box: box, Log: log, Metrics: m, Options: opts})

	ctx, cancel := context.WithCancel(context.Background())
	connCtx, stopConn := context.WithCancel(context.Background())
	done := make(chan struct{})
	go conn.Run(connCtx)
	go func() { defer close(done); w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("the worker did not stop within 15s")
		}
		stopConn()
		_ = rdb.Close()
	})
	waitUntil(t, 30*time.Second, "the worker to connect to RabbitMQ", conn.Ready)
	return &testWorker{names: opts.Names, conn: conn, metrics: m, opts: opts}
}

func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stays asserts that cond holds for the whole duration: for "nothing else happens".
func stays(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !cond() {
			t.Fatalf("%s did not hold", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// drainOutbox marks everything in the outbox as published, so that events of earlier tests are not relayed into this one.
func drainOutbox(t *testing.T) {
	t.Helper()
	if _, err := testdb.Pool(t).Exec(context.Background(), `UPDATE outbox SET published_at = now() WHERE published_at IS NULL`); err != nil {
		t.Fatal(err)
	}
}

func countWhere(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := testdb.Pool(t).QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *env) registerDevice(tt testTenant, ext, platform, token string) {
	e.t.Helper()
	if r := e.asUser(tt, ext, "POST", "/v1/devices", map[string]any{"platform": platform, "push_token": token}); r.status != 204 {
		e.t.Fatalf("register device %s for %s: %d %v", token, ext, r.status, r.body)
	}
}

func (e *env) userID(tt testTenant, ext string) uuid.UUID {
	e.t.Helper()
	r := e.asUser(tt, ext, "GET", "/v1/me", nil)
	id, err := uuid.Parse(str(r.at("id")))
	if err != nil {
		e.t.Fatalf("user %s: %v", ext, r.body)
	}
	return id
}

// group creates a group with the server API and returns its id.
func (e *env) group(tt testTenant, creator, name string, members ...string) string {
	e.t.Helper()
	r := e.asServer(tt, "POST", "/v1/server/channels", map[string]any{"created_by": creator, "name": name, "member_ids": members})
	if r.status != 201 {
		e.t.Fatalf("create group: %d %v", r.status, r.body)
	}
	return str(r.at("id"))
}

func (e *env) mute(channelID, userID uuid.UUID) {
	e.t.Helper()
	if _, err := testdb.Pool(e.t).Exec(context.Background(),
		`UPDATE channel_members SET muted_until = now() + interval '1 hour' WHERE channel_id = $1 AND user_id = $2`, channelID, userID); err != nil {
		e.t.Fatal(err)
	}
}

// withFCM gives the tenant the service account of the fake Google.
func (e *env) withFCM(tt testTenant, g *fakeGoogle) {
	e.t.Helper()
	if err := e.tenants.SetFCMCredentials(context.Background(), tt.Slug, g.account(e.t)); err != nil {
		e.t.Fatal(err)
	}
}

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// recordingPublisher stands in for RabbitMQ: it keeps what it is given, or fails on request.
type recordingPublisher struct {
	mu   sync.Mutex
	envs []broker.Envelope
	err  error
}

func (r *recordingPublisher) Publish(_ context.Context, envs []broker.Envelope) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.envs = append(r.envs, envs...)
	return nil
}

func (r *recordingPublisher) fail(err error) {
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
}

func (r *recordingPublisher) all() []broker.Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]broker.Envelope(nil), r.envs...)
}
