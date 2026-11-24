package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rizkysiregar/marpokat/server/internal/broker"
	"github.com/rizkysiregar/marpokat/server/internal/media"
	"github.com/rizkysiregar/marpokat/server/internal/message"
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
	// FFmpeg is the path of the binary that makes the poster of a video. Empty: videos are kept without thumbnail.
	FFmpeg string
	// AttachmentGrace is how long an upload nobody uses is kept; default a day.
	AttachmentGrace time.Duration
	// PurgeEvery is how often deleted projects past their grace period are looked for; default 10 minutes.
	PurgeEvery time.Duration
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
	Storage *media.Storage
	Box     *secretbox.Box
	Log     *slog.Logger
	Metrics *metrics.Registry // may be nil
	Options WorkerOptions
}

// Worker runs the background jobs: the outbox relay, the push consumer, and the housekeeping that goes with them.
type Worker struct {
	d       WorkerDeps
	push    broker.WorkQueue
	media   broker.WorkQueue
	relay   *outbox.Relay
	cleaner *outbox.Cleaner
	files   *media.Cleaner
	purger  *tenant.Purger
	pushC   *broker.Consumer
	mediaC  *broker.Consumer
}

// pushQueue is the work queue of push notifications, mediaQueue the one of thumbnails.
func pushQueue(o WorkerOptions) broker.WorkQueue {
	return broker.WorkQueue{Name: "push", Keys: []string{outbox.TopicMessageCreated}, RetryDelays: o.RetryDelays}
}

func mediaQueue(o WorkerOptions) broker.WorkQueue {
	return broker.WorkQueue{Name: "media", Keys: []string{outbox.TopicAttachmentUploaded}, RetryDelays: o.RetryDelays}
}

func NewWorker(d WorkerDeps) *Worker {
	o := d.Options
	w := &Worker{d: d, push: pushQueue(o), media: mediaQueue(o)}

	queues := []broker.WorkQueue{w.push}
	if d.Storage != nil {
		queues = append(queues, w.media)
	}
	w.relay = outbox.NewRelay(d.Pool, broker.NewPublisher(d.Rabbit, o.Names, queues...), d.Log, d.Metrics)
	w.relay.Ready = d.Rabbit.Ready
	w.relay.Interval = o.RelayInterval
	w.cleaner = outbox.NewCleaner(d.Pool, d.Log)
	// projects whose deletion grace period is over (PLAN D75): their files too, when there is object storage
	w.purger = &tenant.Purger{Pool: d.Pool, Log: d.Log.With("subsystem", "tenant-purge"), Every: o.PurgeEvery}
	if d.Storage != nil {
		w.purger.Objects = d.Storage
	}

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

	if d.Storage == nil { // no object storage, no media jobs
		return w
	}
	// thumbnails: the worker tells the connected clients itself (through Redis, like the API), not via the API
	hydrator := message.NewHydrator(q, media.NewSigner(d.Storage))
	thumbnails := &media.Processor{
		Pool: d.Pool, Objects: d.Storage, FFmpeg: media.FFmpeg{Path: o.FFmpeg},
		Messages: hydrator, Notifier: realtime.NewPublisher(d.Redis, q, nil, d.Log),
		Log: d.Log.With("subsystem", "media"), Metrics: d.Metrics,
	}
	w.mediaC = broker.NewConsumer(d.Rabbit, broker.ConsumerConfig{
		Names: o.Names, Queue: w.media, Handler: thumbnails.Handle, Workers: 2, Prefetch: 4, Log: d.Log, Metrics: d.Metrics,
	})
	w.files = media.NewCleaner(q, d.Storage, d.Log)
	if o.AttachmentGrace > 0 {
		w.files.Grace = o.AttachmentGrace
	}
	return w
}

// Run runs every job until ctx ends and returns when they have all stopped.
func (w *Worker) Run(ctx context.Context) {
	jobs := []func(context.Context){w.relay.Run, w.pushC.Run, w.cleaner.Run, w.purger.Run}
	if w.mediaC != nil {
		jobs = append(jobs, w.mediaC.Run, w.files.Run)
	}
	if w.d.Metrics != nil {
		jobs = append(jobs,
			func(ctx context.Context) { w.relay.RunPendingGauge(ctx, w.d.Options.StatsEvery) },
			func(ctx context.Context) {
				queues := w.push.AllQueues(w.d.Options.Names)
				if w.mediaC != nil {
					queues = append(queues, w.media.AllQueues(w.d.Options.Names)...)
				}
				broker.RunQueueStats(ctx, w.d.Rabbit, queues, w.d.Metrics, w.d.Options.StatsEvery, w.d.Log)
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
