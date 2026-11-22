package app

import (
	"context"
	"fmt"
	"net/http"

	"golang.org/x/sync/errgroup"

	"github.com/rizkysiregar/marpokat/server/internal/platform/config"
	"github.com/rizkysiregar/marpokat/server/internal/platform/db"
	"github.com/rizkysiregar/marpokat/server/internal/platform/health"
	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/platform/metrics"
	"github.com/rizkysiregar/marpokat/server/internal/platform/redisx"
)

type apiConfig struct {
	Server   config.Server
	Postgres config.Postgres
	Redis    config.Redis
}

// RunAPI serves the public REST + WebSocket API until ctx is cancelled.
func RunAPI(ctx context.Context, info Info) error {
	cfg, err := config.Load[apiConfig]()
	if err != nil {
		return err
	}
	log := NewLogger(cfg.Server.LogLevel, "api", info)

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

	public := httpx.NewServer(cfg.Server.HTTPAddr, httpx.Chain(publicRoutes(info),
		httpx.WithRequestID(),
		httpx.WithObservability(log, m),
		httpx.WithRecover(log),
	))
	mgmt := httpx.NewServer(cfg.Server.ManagementAddr, managementHandler(reg, m))

	log.Info("api starting", "http_addr", cfg.Server.HTTPAddr, "management_addr", cfg.Server.ManagementAddr)
	stopCtx, cancel := drainThenStop(ctx, reg, cfg.Server.ShutdownDelay, log)
	defer cancel()
	g, gctx := errgroup.WithContext(stopCtx)
	g.Go(func() error { return httpx.Serve(gctx, public, cfg.Server.ShutdownTimeout, log) })
	g.Go(func() error { return httpx.Serve(gctx, mgmt, cfg.Server.ShutdownTimeout, log) })
	if err := g.Wait(); err != nil {
		return fmt.Errorf("api: %w", err)
	}
	log.Info("api stopped")
	return nil
}

// publicRoutes is the public router. Feature modules register their routes here from M1 on.
func publicRoutes(info Info) http.Handler {
	mux := http.NewServeMux()
	// reachable through Cloudflare, used to check the tunnel and ingress; no dependency checks
	mux.HandleFunc("GET /healthz", health.LiveHandler)
	mux.HandleFunc("GET /v1", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"name": "marpokat", "version": info.Version})
	})
	mux.HandleFunc("/", httpx.NotFound)
	return mux
}
