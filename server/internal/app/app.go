// Package app wires the subcommands: it opens dependencies, builds the servers and runs them until shutdown.
package app

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rizkysiregar/marpokat/server/internal/platform/config"
	"github.com/rizkysiregar/marpokat/server/internal/platform/health"
	"github.com/rizkysiregar/marpokat/server/internal/platform/metrics"
	"github.com/rizkysiregar/marpokat/server/internal/platform/secretbox"
)

// Info identifies the running binary.
type Info struct {
	Version string
	Commit  string
}

// NewLogger returns the JSON logger every subcommand uses.
func NewLogger(level, component string, info Info) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(level))); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l})).
		With("service", "marpokat", "component", component, "version", info.Version)
}

// managementHandler serves the probes and metrics on the internal port, never exposed through the ingress.
func managementHandler(reg *health.Registry, m *metrics.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", health.LiveHandler)
	mux.HandleFunc("GET /readyz", reg.ReadyHandler)
	mux.Handle("GET /metrics", m.Handler())
	return mux
}

// drainThenStop returns a context that is cancelled `delay` after ctx: in between, reg reports not ready while
// the servers keep serving, so in-flight traffic finishes and the ingress stops sending new requests.
func drainThenStop(ctx context.Context, reg *health.Registry, delay time.Duration, log *slog.Logger) (context.Context, context.CancelFunc) {
	stopCtx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-ctx.Done():
		case <-stopCtx.Done():
			return
		}
		reg.Drain()
		if delay > 0 {
			log.Info("shutdown requested, draining", "delay", delay.String())
			select {
			case <-time.After(delay):
			case <-stopCtx.Done():
			}
		}
		cancel()
	}()
	return stopCtx, cancel
}

// openBox builds the encryption box for tenant secrets from MARPOKAT_MASTER_KEY.
func openBox(c config.Secrets) (*secretbox.Box, error) {
	key, err := c.MasterKeyBytes()
	if err != nil {
		return nil, err
	}
	return secretbox.New(key)
}
