package httpx

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

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
