package push

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fake is a stand-in for Google: the OAuth token endpoint and the FCM send endpoint.
type fake struct {
	tokens, fcm *httptest.Server

	mu         sync.Mutex
	tokenCalls int
	requests   []fcmCall
	respond    func(call fcmCall) (status int, body string)
	tokenFail  bool
}

type fcmCall struct {
	Path, Authorization string
	Body                map[string]any
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{}
	f.tokens = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.tokenCalls++
		n, fail := f.tokenCalls, f.tokenFail
		f.mu.Unlock()
		if fail {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"tok-`+string(rune('0'+n))+`","token_type":"Bearer","expires_in":3600}`)
	}))
	f.fcm = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		call := fcmCall{Path: r.URL.Path, Authorization: r.Header.Get("Authorization")}
		_ = json.Unmarshal(raw, &call.Body)
		f.mu.Lock()
		f.requests = append(f.requests, call)
		respond := f.respond
		f.mu.Unlock()
		status, body := http.StatusOK, `{"name":"projects/p/messages/1"}`
		if respond != nil {
			status, body = respond(call)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(func() { f.tokens.Close(); f.fcm.Close() })
	return f
}

// account is a service account key (a real RSA key, which the OAuth library signs with) that points at the fake.
func (f *fake) account(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	b, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "proj-1", "private_key": pemKey,
		"client_email": "svc@proj-1.iam.gserviceaccount.com", "token_uri": f.tokens.URL,
	})
	return b
}

func (f *fake) sender() *FCM {
	s := NewFCM()
	s.BaseURL = f.fcm.URL
	return s
}

func (f *fake) snapshot() (tokenCalls int, calls []fcmCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls, append([]fcmCall(nil), f.requests...)
}

func TestFCMSendsDataMessages(t *testing.T) {
	f := newFake(t)
	s, acct := f.sender(), f.account(t)
	data := map[string]string{"marpokat": "1", "text": "halo"}

	if err := s.Send(t.Context(), acct, Message{Token: "phone", Platform: "android", Data: data}); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(t.Context(), acct, Message{Token: "browser", Platform: "web", Data: data}); err != nil {
		t.Fatal(err)
	}
	tokenCalls, calls := f.snapshot()
	if len(calls) != 2 {
		t.Fatalf("%d requests, want 2", len(calls))
	}
	if tokenCalls != 1 {
		t.Errorf("the access token was requested %d times for 2 messages, want once (it is cached)", tokenCalls)
	}
	android, web := calls[0], calls[1]
	if android.Path != "/v1/projects/proj-1/messages:send" || android.Authorization != "Bearer tok-1" {
		t.Errorf("request: %s %s", android.Path, android.Authorization)
	}
	msg := android.Body["message"].(map[string]any)
	if msg["token"] != "phone" || msg["data"].(map[string]any)["text"] != "halo" || msg["notification"] != nil {
		t.Errorf("android message: %v", msg)
	}
	if a := msg["android"].(map[string]any); a["priority"] != "HIGH" || a["ttl"] != "3600s" || msg["webpush"] != nil {
		t.Errorf("android settings: %v", a)
	}
	wmsg := web.Body["message"].(map[string]any)
	if h := wmsg["webpush"].(map[string]any)["headers"].(map[string]any); h["Urgency"] != "high" || h["TTL"] != "3600" || wmsg["android"] != nil {
		t.Errorf("webpush settings: %v", wmsg["webpush"])
	}
}

func TestFCMErrors(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		invalid   bool
		retryable bool
	}{
		{"unregistered", 404, `{"error":{"code":404,"status":"NOT_FOUND","message":"Requested entity was not found.","details":[{"errorCode":"UNREGISTERED"}]}}`, true, false},
		{"unknown token", 400, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"The registration token is not a valid FCM registration token"}}`, true, false},
		{"token of another project", 403, `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"SenderId mismatch","details":[{"errorCode":"SENDER_ID_MISMATCH"}]}}`, true, false},
		{"bad request that is not the token", 400, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"Data payload is too big"}}`, false, false},
		{"quota", 429, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota"}}`, false, true},
		{"overloaded", 503, `{"error":{"code":503,"status":"UNAVAILABLE","message":"try later"}}`, false, true},
		{"server error with no body", 500, ``, false, true},
		{"authorisation", 401, `{"error":{"code":401,"status":"UNAUTHENTICATED","message":"expired"}}`, false, true},
	}
	for _, c := range cases {
		f := newFake(t)
		f.respond = func(fcmCall) (int, string) { return c.status, c.body }
		err := f.sender().Send(t.Context(), f.account(t), Message{Token: "t", Platform: "android", Data: map[string]string{}})
		var se *SendError
		switch {
		case c.invalid:
			if !errors.Is(err, ErrInvalidToken) {
				t.Errorf("%s: err = %v, want ErrInvalidToken", c.name, err)
			}
		case errors.As(err, &se):
			if se.Retryable != c.retryable || se.Status != c.status {
				t.Errorf("%s: %+v, want retryable=%v status=%d", c.name, se, c.retryable, c.status)
			}
		default:
			t.Errorf("%s: err = %v, want a SendError", c.name, err)
		}
	}
}

func TestFCMAuthorisationFailureFetchesANewAccessToken(t *testing.T) {
	f := newFake(t)
	s, acct := f.sender(), f.account(t)
	f.respond = func(fcmCall) (int, string) { return 401, `{"error":{"status":"UNAUTHENTICATED","message":"revoked"}}` }
	_ = s.Send(t.Context(), acct, Message{Token: "t", Platform: "android"})
	f.respond = nil
	if err := s.Send(t.Context(), acct, Message{Token: "t", Platform: "android"}); err != nil {
		t.Fatal(err)
	}
	tokenCalls, calls := f.snapshot()
	if tokenCalls != 2 || calls[1].Authorization != "Bearer tok-2" {
		t.Fatalf("after a 401 the next send must use a new access token: %d token calls, second request used %q", tokenCalls, calls[1].Authorization)
	}
}

func TestFCMCredentialProblems(t *testing.T) {
	f := newFake(t)
	var se *SendError
	if err := f.sender().Send(t.Context(), []byte(`{"type":"service_account"}`), Message{Token: "t"}); !errors.As(err, &se) || se.Retryable {
		t.Errorf("unusable credentials: %v, want a permanent SendError", err)
	}
	if err := f.sender().Send(t.Context(), []byte(`not json`), Message{Token: "t"}); !errors.As(err, &se) || se.Retryable {
		t.Errorf("credentials that are not JSON: %v", err)
	}
	f.tokenFail = true
	if err := f.sender().Send(t.Context(), f.account(t), Message{Token: "t"}); !errors.As(err, &se) || !se.Retryable || !strings.Contains(se.Detail, "access token") {
		t.Errorf("Google refuses to give an access token: %v, want a retryable SendError", err)
	}
	f.fcm.Close()
	f.tokenFail = false
	if err := f.sender().Send(t.Context(), f.account(t), Message{Token: "t"}); !errors.As(err, &se) || !se.Retryable {
		t.Errorf("FCM unreachable: %v, want a retryable SendError", err)
	}
}

func TestContentData(t *testing.T) {
	long := strings.Repeat("é", 250)
	d := Content{
		ChannelID: "c1", ChannelType: "group", ChannelName: "Crew", MessageID: "m1", Seq: 42, SenderID: "u_1",
		SenderName: "Budi", Text: long, UnreadCount: 3,
	}.Data()
	want := map[string]string{
		"marpokat": "1", "type": "message.new", "channel_id": "c1", "channel_type": "group", "channel_name": "Crew",
		"message_id": "m1", "seq": "42", "sender_id": "u_1", "sender_name": "Budi", "sender_avatar_url": "",
		"attachment_kind": "", "unread_count": "3",
	}
	for k, v := range want {
		if d[k] != v {
			t.Errorf("data[%q] = %q, want %q", k, d[k], v)
		}
	}
	if got := []rune(d["text"]); len(got) != MaxTextLength || string(got) != strings.Repeat("é", MaxTextLength) {
		t.Errorf("text has %d characters, want %d without cutting a character", len(got), MaxTextLength)
	}
	if got := truncateText("short", 200); got != "short" {
		t.Errorf("short text changed: %q", got)
	}
}
