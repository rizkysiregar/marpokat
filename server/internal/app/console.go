package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rizkysiregar/marpokat/server/internal/console"
	"github.com/rizkysiregar/marpokat/server/internal/platform/config"
	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/platform/metrics"
	"github.com/rizkysiregar/marpokat/server/internal/platform/ratelimit"
	"github.com/rizkysiregar/marpokat/server/internal/platform/redisx"
	"github.com/rizkysiregar/marpokat/server/internal/platform/secretbox"
	"github.com/rizkysiregar/marpokat/server/internal/store"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
)

// newConsole builds the Console API server and the job that removes expired sessions. It is its own listener so that
// the account endpoints are never reachable through the public ingress.
func newConsole(cfg config.Console, pool *pgxpool.Pool, rdb *redisx.Client, box *secretbox.Box, log *slog.Logger, m *metrics.Registry) (*http.Server, func(context.Context)) {
	q := store.New(pool)
	var mailer console.Mailer = console.LogMailer{Log: log}
	if cfg.MailDriver == "smtp" {
		mailer = console.NewSMTPMailer(console.SMTPConfig{
			Host: cfg.MailHost, Port: cfg.MailPort, Username: cfg.MailUsername, Password: cfg.MailPassword, From: cfg.MailFrom,
		})
	}
	svc := console.NewService(q, tenant.NewService(q, box), mailer, ratelimit.New(rdb, log), log,
		console.Options{BaseURL: cfg.BaseURL, MaxProjects: cfg.MaxProjects})
	handler := console.NewHandler(svc, cfg.ServiceKey, log).Routes()
	server := httpx.NewServer(cfg.Addr, httpx.Chain(handler,
		httpx.WithRequestID(),
		httpx.WithObservability(log, m),
		httpx.WithRecover(log),
		httpx.WithCleanInput(),
	))
	return server, func(ctx context.Context) { svc.CleanSessions(ctx, time.Hour) }
}
