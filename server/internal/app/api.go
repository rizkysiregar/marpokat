package app

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sync/errgroup"

	"github.com/rizkysiregar/marpokat/server/internal/platform/config"
	"github.com/rizkysiregar/marpokat/server/internal/platform/db"
	"github.com/rizkysiregar/marpokat/server/internal/platform/health"
	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/platform/metrics"
	"github.com/rizkysiregar/marpokat/server/internal/platform/redisx"
	"github.com/rizkysiregar/marpokat/server/internal/realtime"
)

type apiConfig struct {
	Server   config.Server
	Postgres config.Postgres
	Redis    config.Redis
	Secrets  config.Secrets
}

// RunAPI serves the public REST + WebSocket API until ctx is cancelled.
func RunAPI(ctx context.Context, info Info) error {
	cfg, err := config.Load[apiConfig]()
	if err != nil {
		return err
	}
	log := NewLogger(cfg.Server.LogLevel, "api", info)
	box, err := openBox(cfg.Secrets)
	if err != nil {
		return err
	}

	pool, err := db.Open(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer pool.Close()

	rdb, err := redisx.Open(ctx, cfg.Redis)
	if err != nil {
		return err
	}
	defer func() { _ = rdb.Close() }()

	reg := health.NewRegistry()
	reg.Add("postgres", pool.Ping)
	reg.Add("redis", func(ctx context.Context) error { return rdb.Ping(ctx).Err() })
	m := metrics.New(info.Version)

	hub := realtime.NewHub(rdb, log, realtime.DefaultOptions(), m)
	// a pod whose event subscription is not up would accept WebSockets it cannot serve: no traffic until it is
	reg.Add("realtime", func(context.Context) error {
		if !hub.Ready() {
			return errors.New("not subscribed to Redis")
		}
		return nil
	})
	public := httpx.NewServer(cfg.Server.HTTPAddr, httpx.Chain(
		PublicRoutes(Deps{Info: info, Pool: pool, Box: box, Log: log, Redis: rdb, Hub: hub, Metrics: m}),
		httpx.WithCORS(),
		httpx.WithRequestID(),
		httpx.WithObservability(log, m),
		httpx.WithRecover(log),
	))
	// http.Server.Shutdown does not touch hijacked connections: the WebSockets are closed here, with 1012, once the
	// pod no longer receives traffic (readiness failed during the drain delay), so clients reconnect elsewhere
	public.RegisterOnShutdown(hub.Shutdown)
	mgmt := httpx.NewServer(cfg.Server.ManagementAddr, managementHandler(reg, m))

	log.Info("api starting", "http_addr", cfg.Server.HTTPAddr, "management_addr", cfg.Server.ManagementAddr)
	stopCtx, cancel := drainThenStop(ctx, reg, cfg.Server.ShutdownDelay, log)
	defer cancel()
	g, gctx := errgroup.WithContext(stopCtx)
	g.Go(func() error { hub.Run(gctx); return nil })
	g.Go(func() error { return httpx.Serve(gctx, public, cfg.Server.ShutdownTimeout, log) })
	g.Go(func() error { return httpx.Serve(gctx, mgmt, cfg.Server.ShutdownTimeout, log) })
	if err := g.Wait(); err != nil {
		return fmt.Errorf("api: %w", err)
	}
	log.Info("api stopped")
	return nil
}
