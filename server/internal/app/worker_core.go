package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rizkysiregar/marpokat/server/internal/broker"
	"github.com/rizkysiregar/marpokat/server/internal/outbox"
	"github.com/rizkysiregar/marpokat/server/internal/platform/metrics"
	"github.com/rizkysiregar/marpokat/server/internal/platform/rabbitmq"
	"github.com/rizkysiregar/marpokat/server/internal/platform/redisx"
	"github.com/rizkysiregar/marpokat/server/internal/platform/secretbox"
	"github.com/rizkysiregar/marpokat/server/internal/push"
	"github.com/rizkysiregar/marpokat/server/internal/realtime"
	"github.com/rizkysiregar/marpokat/server/internal/store"
	"github.com/rizkysiregar/marpokat/server/internal/tenant"
)

// WorkerOptions are the settings of the background jobs. Production uses DefaultWorkerOptions; tests shorten them.
type WorkerOptions struct {
	Names         broker.Names
	RetryDelays   []time.Duration
	RelayInterval time.Duration
	PushMaxAge    time.Duration
	// FCMBaseURL is where push messages go; empty means Google's FCM.
	FCMBaseURL string
	StatsEvery time.Duration
}

func DefaultWorkerOptions() WorkerOptions {
	return WorkerOptions{
		Names:         broker.DefaultNames(),
		RetryDelays:   broker.DefaultRetryDelays,
		RelayInterval: 200 * time.Millisecond,
		PushMaxAge:    push.DefaultMaxAge,
		StatsEvery:    15 * time.Second,
	}
}

// WorkerDeps is what the worker needs.
type WorkerDeps struct {
	Pool    *pgxpool.Pool
	Redis   *redisx.Client
	Rabbit  *rabbitmq.Conn
	Box     *secretbox.Box
	Log     *slog.Logger
	Metrics *metrics.Registry // may be nil
	Options WorkerOptions
}

// Worker runs the background jobs: the outbox relay, the push consumer, and the housekeeping that goes with them.
type Worker struct {
	d       WorkerDeps
	push    broker.WorkQueue
	relay   *outbox.Relay
	cleaner *outbox.Cleaner
	pushC   *broker.Consumer
}

// PushQueue is the work queue of push notifications.
func pushQueue(o WorkerOptions) broker.WorkQueue {
	return broker.WorkQueue{Name: "push", Keys: []string{outbox.TopicMessageCreated}, RetryDelays: o.RetryDelays}
}

func NewWorker(d WorkerDeps) *Worker {
	o := d.Options
	w := &Worker{d: d, push: pushQueue(o)}

	w.relay = outbox.NewRelay(d.Pool, broker.NewPublisher(d.Rabbit, o.Names, w.push), d.Log, d.Metrics)
	w.relay.Ready = d.Rabbit.Ready
	w.relay.Interval = o.RelayInterval
	w.cleaner = outbox.NewCleaner(d.Pool, d.Log)

	fcm := push.NewFCM()
	if o.FCMBaseURL != "" {
		fcm.BaseURL = o.FCMBaseURL
	}
	q := store.New(d.Pool)
	processor := &push.Processor{
		Q:       q,
		Online:  presenceOnline{realtime.NewPresence(d.Redis, realtime.DefaultOptions().PresenceTTL)},
		Tenants: tenant.NewService(q, d.Box),
		Sender:  fcm,
		Dedup:   push.NewRedisDedup(d.Redis),
		Log:     d.Log.With("subsystem", "push"),
		Metrics: d.Metrics,
		MaxAge:  o.PushMaxAge,
		Now:     time.Now,
	}
	w.pushC = broker.NewConsumer(d.Rabbit, broker.ConsumerConfig{
		Names: o.Names, Queue: w.push, Handler: processor.Handle, Log: d.Log, Metrics: d.Metrics,
	})
	return w
}

// Run runs every job until ctx ends and returns when they have all stopped.
func (w *Worker) Run(ctx context.Context) {
	jobs := []func(context.Context){w.relay.Run, w.pushC.Run, w.cleaner.Run}
	if w.d.Metrics != nil {
		jobs = append(jobs,
			func(ctx context.Context) { w.relay.RunPendingGauge(ctx, w.d.Options.StatsEvery) },
			func(ctx context.Context) {
				broker.RunQueueStats(ctx, w.d.Rabbit, w.push.AllQueues(w.d.Options.Names), w.d.Metrics, w.d.Options.StatsEvery, w.d.Log)
			},
		)
	}
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job(ctx)
		}()
	}
	wg.Wait()
}

// presenceOnline adapts realtime.Presence to what the push processor asks.
type presenceOnline struct{ p *realtime.Presence }

func (a presenceOnline) Online(ctx context.Context, tenantID uuid.UUID, userIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	status, err := a.p.Of(ctx, tenantID, userIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(status))
	for id, st := range status {
		out[id] = st.Online
	}
	return out, nil
}
