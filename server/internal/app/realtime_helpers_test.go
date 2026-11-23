//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/rizkysiregar/marpokat/server/internal/auth"
	"github.com/rizkysiregar/marpokat/server/internal/media"
	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/platform/redisx"
	"github.com/rizkysiregar/marpokat/server/internal/platform/secretbox"
	"github.com/rizkysiregar/marpokat/server/internal/platform/storage"
	"github.com/rizkysiregar/marpokat/server/internal/realtime"
	"github.com/rizkysiregar/marpokat/server/internal/store"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
	"github.com/rizkysiregar/marpokat/server/internal/testdb"
)

// pod is one API replica: its own HTTP server, hub and Redis connections, sharing Postgres and Redis with the others.
type pod struct {
	srv  *httptest.Server
	hub  *realtime.Hub
	rdb  *redisx.Client
	stop context.CancelFunc
}

// envOption changes how newRealtimeEnv builds its pods.
type envOption func(*envConfig)

type envConfig struct {
	media bool
	// authFailures is how many wrong credentials one address may present; high, because every test comes from 127.0.0.1
	authFailures int
}

// withMedia gives the pods object storage (a MinIO container), which turns on attachments.
func withMedia() envOption { return func(c *envConfig) { c.media = true } }

// withAuthFailureLimit sets how many wrong credentials one address may present before it is refused.
func withAuthFailureLimit(n int) envOption { return func(c *envConfig) { c.authFailures = n } }

// newRealtimeEnv starts n pods. tweak shortens timings for the test.
func newRealtimeEnv(t *testing.T, n int, tweak func(*realtime.Options), options ...envOption) *env {
	t.Helper()
	box, err := secretbox.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	pool := testdb.Pool(t)
	opts := realtime.DefaultOptions()
	if tweak != nil {
		tweak(&opts)
	}
	cfg := envConfig{authFailures: 1_000_000}
	for _, o := range options {
		o(&cfg)
	}
	var files *media.Storage
	if cfg.media {
		files = testStorage(t)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	e := &env{t: t, tenants: tenant.NewService(store.New(pool), box), files: files}
	for range n {
		rdb, err := redisx.Open(context.Background(), testdb.Redis(t))
		if err != nil {
			t.Fatal(err)
		}
		hub := realtime.NewHub(rdb, log, opts, nil)
		hubCtx, stop := context.WithCancel(context.Background())
		go hub.Run(hubCtx)
		h := httpx.Chain(PublicRoutes(Deps{Info: Info{Version: "test"}, Pool: pool, Box: box, Log: log, Redis: rdb, Hub: hub, Realtime: opts, Storage: files, AuthFailuresPerAddress: cfg.authFailures}),
			httpx.WithCORS(), httpx.WithRequestID())
		srv := httptest.NewServer(h)
		srv.Config.RegisterOnShutdown(hub.Shutdown)
		p := &pod{srv: srv, hub: hub, rdb: rdb, stop: stop}
		e.pods = append(e.pods, p)
		t.Cleanup(func() {
			hub.Shutdown()
			stop()
			srv.Close()
			_ = rdb.Close()
		})
	}
	e.srv = e.pods[0].srv
	deadline := time.Now().Add(5 * time.Second)
	for _, p := range e.pods {
		for !p.hub.Ready() {
			if time.Now().After(deadline) {
				t.Fatal("hub did not subscribe to Redis")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	return e
}

// on returns the env as seen through one pod, for REST calls.
func (e *env) on(i int) *env {
	c := *e
	c.srv = e.pods[i].srv
	return &c
}

func (e *env) wsURL(pod int) string {
	return "ws" + strings.TrimPrefix(e.pods[pod].srv.URL, "http") + "/v1/ws"
}

// wsConn is a test client of the WebSocket: it collects every frame, tests ask for the ones they expect.
type wsConn struct {
	t      *testing.T
	c      *websocket.Conn
	mu     sync.Mutex
	frames []map[string]any
	closed bool
	code   websocket.StatusCode
}

func dialWS(t *testing.T, url string) *wsConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, res, err := websocket.Dial(ctx, url, nil)
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	w := &wsConn{t: t, c: c}
	t.Cleanup(func() { _ = c.CloseNow() })
	go func() {
		for {
			_, data, err := c.Read(context.Background())
			if err != nil {
				w.mu.Lock()
				w.closed, w.code = true, websocket.CloseStatus(err)
				w.mu.Unlock()
				return
			}
			var f map[string]any
			if json.Unmarshal(data, &f) == nil {
				w.mu.Lock()
				w.frames = append(w.frames, f)
				w.mu.Unlock()
			}
		}
	}()
	return w
}

func (w *wsConn) sendText(s string) {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.c.Write(ctx, websocket.MessageText, []byte(s)); err != nil {
		w.t.Logf("write failed (the server may have closed already): %v", err)
	}
}

func (w *wsConn) send(typ, id string, data any) {
	w.t.Helper()
	b, _ := json.Marshal(map[string]any{"type": typ, "id": id, "data": data})
	w.sendText(string(b))
}

// next removes and returns the first received frame of the type, waiting for it if needed.
func (w *wsConn) next(typ string) map[string]any {
	w.t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for {
		w.mu.Lock()
		for i, f := range w.frames {
			if f["type"] == typ {
				w.frames = append(w.frames[:i:i], w.frames[i+1:]...)
				w.mu.Unlock()
				return f
			}
		}
		closed, code, have := w.closed, w.code, w.summary()
		w.mu.Unlock()
		if closed {
			w.t.Fatalf("connection closed (%d) while waiting for %q; received %s", code, typ, have)
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("no %q frame within 4s; received %s", typ, have)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// none asserts that no frame of the type shows up within d (and none is waiting already).
func (w *wsConn) none(typ string, d time.Duration) {
	w.t.Helper()
	time.Sleep(d)
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, f := range w.frames {
		if f["type"] == typ {
			w.t.Fatalf("unexpected %q frame: %v", typ, f)
		}
	}
}

// waitClose returns the close code once the connection has ended.
func (w *wsConn) waitClose(timeout time.Duration) websocket.StatusCode {
	w.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		w.mu.Lock()
		closed, code := w.closed, w.code
		w.mu.Unlock()
		if closed {
			return code
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("connection still open after %s; received %s", timeout, w.summaryLocked())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (w *wsConn) summaryLocked() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.summary()
}

func (w *wsConn) summary() string {
	types := make([]string, 0, len(w.frames))
	for _, f := range w.frames {
		types = append(types, fmt.Sprint(f["type"]))
	}
	return "[" + strings.Join(types, " ") + "]"
}

func authFrame(apiKey, token string, version int) map[string]any {
	return map[string]any{"type": "auth", "id": "a1", "data": map[string]any{"api_key": apiKey, "token": token, "protocol_version": version}}
}

// connect opens an authenticated WebSocket of a user on a pod.
func (e *env) connect(pod int, tt testTenant, ext string) *wsConn {
	e.t.Helper()
	w := dialWS(e.t, e.wsURL(pod))
	b, _ := json.Marshal(authFrame(tt.APIKey, tt.userToken(e.t, ext), realtime.ProtocolVersion))
	w.sendText(string(b))
	ok := w.next("auth.ok")
	if at(ok, "data", "user", "external_id") != ext || at(ok, "data", "connection_id") == "" {
		e.t.Fatalf("auth.ok for %s: %v", ext, ok)
	}
	return w
}

func (tt testTenant) tokenFor(t *testing.T, ext string, ttl time.Duration, issuedAt time.Time) string {
	t.Helper()
	tok, err := auth.SignUserToken(tt.secret, ext, ttl, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// nextOf removes and returns the first received frame that is one of the types.
func (w *wsConn) nextOf(types ...string) map[string]any {
	w.t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for {
		w.mu.Lock()
		for i, f := range w.frames {
			if slices.Contains(types, fmt.Sprint(f["type"])) {
				w.frames = append(w.frames[:i:i], w.frames[i+1:]...)
				w.mu.Unlock()
				return f
			}
		}
		closed, code, have := w.closed, w.code, w.summary()
		w.mu.Unlock()
		if closed {
			w.t.Fatalf("connection closed (%d) while waiting for %v; received %s", code, types, have)
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("none of %v within 4s; received %s", types, have)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// testStorage is the object storage of the shared MinIO container.
func testStorage(t *testing.T) *media.Storage {
	t.Helper()
	st, err := storage.Open(context.Background(), testdb.MinIO(t))
	if err != nil {
		t.Fatal(err)
	}
	return media.NewStorage(st)
}
