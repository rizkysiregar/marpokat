package app

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"

	"github.com/rizkysiregar/marpokat/server/internal/media"
	"github.com/rizkysiregar/marpokat/server/internal/platform/config"
	"github.com/rizkysiregar/marpokat/server/internal/platform/db"
	"github.com/rizkysiregar/marpokat/server/internal/platform/health"
	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/platform/metrics"
	"github.com/rizkysiregar/marpokat/server/internal/platform/rabbitmq"
	"github.com/rizkysiregar/marpokat/server/internal/platform/redisx"
	"github.com/rizkysiregar/marpokat/server/internal/platform/storage"
)

type workerConfig struct {
	Server   config.Server
	Postgres config.Postgres
	Redis    config.Redis
	RabbitMQ config.RabbitMQ
	MinIO    config.MinIO
	Secrets  config.Secrets
	// The binary that makes the poster of a video; unset, videos are kept without one.
	FFmpeg string `env:"FFMPEG_PATH"`
}

// RunWorker runs the background jobs (outbox relay, push) until ctx is cancelled.
func RunWorker(ctx context.Context, info Info) error {
	cfg, err := config.Load[workerConfig]()
	if err != nil {
		return err
	}
	log := NewLogger(cfg.Server.LogLevel, "worker", info)
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

	// RabbitMQ may be down at start: the connection retries in the background and readiness reports it.
	mq := rabbitmq.New(cfg.RabbitMQ.URL(), log)

	reg := health.NewRegistry()
	reg.Add("postgres", pool.Ping)
	reg.Add("redis", func(ctx context.Context) error { return rdb.Ping(ctx).Err() })
	reg.Add("rabbitmq", mq.Check)
	files, err := storage.Open(ctx, cfg.MinIO)
	if err != nil {
		return err
	}
	reg.Add("storage", files.Check)
	m := metrics.New(info.Version)
	mgmt := httpx.NewServer(cfg.Server.ManagementAddr, managementHandler(reg, m))
	opts := DefaultWorkerOptions()
	opts.FFmpeg = cfg.FFmpeg
	worker := NewWorker(WorkerDeps{Pool: pool, Redis: rdb, Rabbit: mq, Storage: media.NewStorage(files), Box: box, Log: log, Metrics: m, Options: opts})

	log.Info("worker starting", "management_addr", cfg.Server.ManagementAddr)
	stopCtx, cancel := drainThenStop(ctx, reg, cfg.Server.ShutdownDelay, log)
	defer cancel()
	g, gctx := errgroup.WithContext(stopCtx)
	// the broker connection outlives the jobs: a message being handled when the pod stops must still be acknowledged
	mqCtx, stopMQ := context.WithCancel(context.WithoutCancel(ctx))
	defer stopMQ()
	g.Go(func() error { mq.Run(mqCtx); return nil })
	g.Go(func() error { worker.Run(gctx); stopMQ(); return nil })
	g.Go(func() error { return httpx.Serve(gctx, mgmt, cfg.Server.ShutdownTimeout, log) })
	if err := g.Wait(); err != nil {
		return fmt.Errorf("worker: %w", err)
	}
	log.Info("worker stopped")
	return nil
}
