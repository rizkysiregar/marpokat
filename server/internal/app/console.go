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

// newConsole builds the Console API server and the job that removes expired sessions. It is its own listener: the
// ingress of each web host sends /console/v1 to it (PLAN D69), nothing else of the chat API is reachable there.
// cfg has been validated, so the origin and proxy lists parse.
func newConsole(cfg config.Console, pool *pgxpool.Pool, rdb *redisx.Client, box *secretbox.Box, log *slog.Logger, m *metrics.Registry) (*http.Server, func(context.Context)) {
	q := store.New(pool)
	var mailer console.Mailer = console.LogMailer{Log: log}
	if cfg.MailDriver == "smtp" {
		mailer = console.NewSMTPMailer(console.SMTPConfig{
			Host: cfg.MailHost, Port: cfg.MailPort, Username: cfg.MailUsername, Password: cfg.MailPassword, From: cfg.MailFrom,
		})
	}
	var captcha console.Captcha
	if cfg.TurnstileSecretKey != "" {
		captcha = console.NewTurnstile(cfg.TurnstileSecretKey, cfg.TurnstileVerifyURL, log)
	} else {
		log.Warn("console: TURNSTILE_SECRET_KEY is not set, sign up and password reset are not checked for robots")
	}
	portal, _ := cfg.PortalOrigins()
	admin, _ := cfg.AdminOriginList()
	proxies, _ := cfg.TrustedProxyPrefixes()
	if len(admin) == 0 {
		log.Info("console: CONSOLE_ADMIN_ORIGINS is not set, nobody can sign in to the admin app")
	}
	svc := console.NewService(q, tenant.NewService(q, box), mailer, ratelimit.New(rdb, log), log,
		console.Options{BaseURL: cfg.BaseURL, MaxProjects: cfg.MaxProjects, Captcha: captcha})
	handler := console.NewHandler(svc, console.HandlerOptions{PortalOrigins: portal, AdminOrigins: admin, TrustedProxies: proxies}, log).Routes()
	server := httpx.NewServer(cfg.Addr, httpx.Chain(handler,
		httpx.WithRequestID(),
		httpx.WithObservability(log, m),
		httpx.WithRecover(log),
		httpx.WithCleanInput(),
	))
	return server, func(ctx context.Context) { svc.CleanSessions(ctx, time.Hour) }
}
