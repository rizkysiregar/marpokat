package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadyHandler(t *testing.T) {
	r := NewRegistry()
	r.Add("postgres", func(context.Context) error { return nil })

	rec := httptest.NewRecorder()
	r.ReadyHandler(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("all checks pass: status %d, want 200", rec.Code)
	}

	r.Add("redis", func(context.Context) error { return errors.New("connection refused") })
	rec = httptest.NewRecorder()
	r.ReadyHandler(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("one check fails: status %d, want 503", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"redis":"connection refused"`) || !strings.Contains(body, `"postgres":"ok"`) {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestDrainFailsReadiness(t *testing.T) {
	r := NewRegistry()
	r.Drain()
	if _, ok := r.Run(context.Background()); ok {
		t.Fatal("draining registry reported ready")
	}
}
