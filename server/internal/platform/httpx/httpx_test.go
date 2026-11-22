package httpx

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/rizkysiregar/marpokat/server/internal/platform/metrics"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRequestIDKeepsValidClientID(t *testing.T) {
	var seen string
	h := WithRequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = RequestID(r.Context()) }))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderRequestID, "abc-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen != "abc-123" || rec.Header().Get(HeaderRequestID) != "abc-123" {
		t.Fatalf("valid id not kept: ctx=%q header=%q", seen, rec.Header().Get(HeaderRequestID))
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderRequestID, "bad id with spaces\n")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen == "" || seen == "bad id with spaces\n" {
		t.Fatalf("invalid id not replaced: %q", seen)
	}
}

func TestRecoverWritesStandardError(t *testing.T) {
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }),
		WithRequestID(), WithRecover(quietLogger()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body.Error.Code != CodeInternal || body.Error.RequestID == "" {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestObservabilityUsesRoutePattern(t *testing.T) {
	m := metrics.New("test")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/channels/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := WithObservability(quietLogger(), m)(mux)

	for _, id := range []string{"a", "b", "c"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/channels/"+id, nil))
	}
	got := testutil.ToFloat64(m.HTTPRequests.WithLabelValues("GET /v1/channels/{id}", "GET", "204"))
	if got != 3 {
		t.Fatalf("counter for route pattern = %v, want 3", got)
	}
}

func TestNotFoundBody(t *testing.T) {
	rec := httptest.NewRecorder()
	NotFound(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	var body ErrorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusNotFound || body.Error.Code != CodeNotFound {
		t.Fatalf("got %d %+v", rec.Code, body)
	}
}

func TestCORS(t *testing.T) {
	var reached bool
	h := WithCORS()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true; w.WriteHeader(http.StatusOK) }))

	// preflight: answered here, before authentication, and the real handler is never called
	req := httptest.NewRequest(http.MethodOptions, "/v1/channels", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization, x-api-key, content-type")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || reached {
		t.Fatalf("preflight: status %d, handler reached %v", rec.Code, reached)
	}
	hdr := rec.Header()
	if hdr.Get("Access-Control-Allow-Origin") != "*" || hdr.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("origin policy: %v", hdr)
	}
	for _, want := range []string{"Authorization", "X-Api-Key", "Content-Type"} {
		if !strings.Contains(hdr.Get("Access-Control-Allow-Headers"), want) {
			t.Errorf("Allow-Headers %q misses %s", hdr.Get("Access-Control-Allow-Headers"), want)
		}
	}
	if !strings.Contains(hdr.Get("Access-Control-Allow-Methods"), "PATCH") {
		t.Errorf("Allow-Methods %q misses PATCH", hdr.Get("Access-Control-Allow-Methods"))
	}

	// a real cross-origin request is passed on, with the header that lets the page read the answer
	reached = false
	req = httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !reached || rec.Header().Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(rec.Header().Get("Access-Control-Expose-Headers"), "Retry-After") {
		t.Fatalf("simple request: reached=%v headers=%v", reached, rec.Header())
	}

	// no Origin, no CORS headers (server to server)
	reached = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/me", nil))
	if !reached || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("request without Origin: reached=%v headers=%v", reached, rec.Header())
	}
}

func TestWriteErrSetsRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteErr(rec, httptest.NewRequest(http.MethodPost, "/", nil), quietLogger(), ErrRateLimited(1500*time.Millisecond, "slow down"))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "2" {
		t.Fatalf("status %d, Retry-After %q (1.5s rounds up to 2)", rec.Code, rec.Header().Get("Retry-After"))
	}
	var body ErrorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error.Code != CodeRateLimited || body.Error.Details["retry_after_ms"] != float64(1500) {
		t.Fatalf("body: %+v", body)
	}
}
