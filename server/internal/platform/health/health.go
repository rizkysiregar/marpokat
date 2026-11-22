// Package health runs the dependency checks behind /readyz.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// CheckFunc returns nil when the dependency is usable.
type CheckFunc func(ctx context.Context) error

type check struct {
	name string
	fn   CheckFunc
}

// Registry holds the checks of one process. Liveness never runs them: a database outage must make the pod
// unready (no traffic), not restart it in a loop.
type Registry struct {
	checks   []check
	timeout  time.Duration
	draining atomic.Bool
}

func NewRegistry() *Registry { return &Registry{timeout: 2 * time.Second} }

func (r *Registry) Add(name string, fn CheckFunc) { r.checks = append(r.checks, check{name, fn}) }

// Drain makes /readyz fail from now on, so Kubernetes stops routing to this pod before it shuts down.
func (r *Registry) Drain() { r.draining.Store(true) }

type Report struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// Run executes all checks in parallel, each with its own timeout.
func (r *Registry) Run(ctx context.Context) (Report, bool) {
	rep := Report{Status: "ok", Checks: make(map[string]string, len(r.checks))}
	if r.draining.Load() {
		rep.Status = "draining"
		return rep, false
	}
	var (
		mu sync.Mutex
		wg sync.WaitGroup
		ok = true
	)
	for _, c := range r.checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, r.timeout)
			defer cancel()
			err := c.fn(cctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				ok = false
				rep.Checks[c.name] = err.Error()
				return
			}
			rep.Checks[c.name] = "ok"
		}()
	}
	wg.Wait()
	if !ok {
		rep.Status = "unavailable"
	}
	return rep, ok
}

// LiveHandler answers 200 as long as the process serves HTTP.
func LiveHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ReadyHandler answers 200 when every check passes, 503 otherwise, with the per-check result as JSON.
func (r *Registry) ReadyHandler(w http.ResponseWriter, req *http.Request) {
	rep, ok := r.Run(req.Context())
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(rep)
}
