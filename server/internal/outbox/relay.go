package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rizkysiregar/marpokat/server/internal/broker"
	"github.com/rizkysiregar/marpokat/server/internal/platform/metrics"
	"github.com/rizkysiregar/marpokat/server/internal/platform/rabbitmq"
	"github.com/rizkysiregar/marpokat/server/internal/store"
)

// BatchPublisher hands events to the broker and returns nil only when the broker confirmed all of them.
type BatchPublisher interface {
	Publish(ctx context.Context, envs []broker.Envelope) error
}

// Relay moves events from the outbox table to RabbitMQ (PLAN D6, D7). It polls instead of listening because
// LISTEN/NOTIFY does not work through pgbouncer, and it locks rows with SKIP LOCKED so any number of workers can run.
//
// Each round is one transaction: lock a batch, publish it, mark it published. Events are marked only after the broker
// confirmed them, so a crash never loses one; it may publish one twice, which consumers tolerate.
type Relay struct {
	pool *pgxpool.Pool
	pub  BatchPublisher
	log  *slog.Logger
	m    *metrics.Registry // may be nil

	// Ready says whether the broker can be reached; while it cannot, the relay waits without touching the table.
	Ready    func() bool
	Batch    int
	Interval time.Duration
}

func NewRelay(pool *pgxpool.Pool, pub BatchPublisher, log *slog.Logger, m *metrics.Registry) *Relay {
	return &Relay{pool: pool, pub: pub, log: log.With("subsystem", "outbox-relay"), m: m, Batch: 100, Interval: 200 * time.Millisecond}
}

// Run relays until ctx ends.
func (r *Relay) Run(ctx context.Context) {
	failures := 0
	for ctx.Err() == nil {
		wait := r.Interval
		switch {
		case r.Ready != nil && !r.Ready():
			wait = 500 * time.Millisecond
		default:
			n, err := r.Once(ctx)
			switch {
			case err != nil:
				failures++
				if r.m != nil {
					r.m.OutboxPublishFailures.Inc()
				}
				if failures == 1 || failures%20 == 0 {
					r.log.Warn("cannot relay events, will retry", "error", err, "failures", failures)
				}
				wait = rabbitmq.Backoff(failures-1, 200*time.Millisecond, 10*time.Second)
			case n >= r.Batch: // more are waiting
				failures = 0
				continue
			default:
				if failures > 0 {
					r.log.Info("relaying again", "after_failures", failures)
				}
				failures = 0
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Once relays one batch and returns how many events it published.
func (r *Relay) Once(ctx context.Context) (int, error) {
	published := 0
	var publishErr error
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		rows, err := q.LockOutboxBatch(ctx, int32(r.Batch)) //nolint:gosec // a small configured batch size
		if err != nil || len(rows) == 0 {
			return err
		}
		ids := make([]string, len(rows))
		envs := make([]broker.Envelope, len(rows))
		for i, row := range rows {
			ids[i] = strconv.FormatInt(row.ID, 10)
			envs[i] = broker.Envelope{
				EventID: row.EventID.String(), TenantID: row.TenantID.String(), Topic: row.Topic,
				Payload: []byte(row.Payload), CreatedAt: row.CreatedAt,
			}
		}
		pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := r.pub.Publish(pctx, envs); err != nil {
			publishErr = err
			// the rows stay unpublished; what went wrong is recorded and committed
			return q.MarkOutboxFailed(ctx, store.MarkOutboxFailedParams{Ids: strings.Join(ids, ","), Error: ptr(truncate(err.Error(), 500))})
		}
		if err := q.MarkOutboxPublished(ctx, strings.Join(ids, ",")); err != nil {
			return err
		}
		published = len(rows)
		if r.m != nil {
			r.m.OutboxPublished.Add(float64(published))
			for _, row := range rows {
				r.m.OutboxLag.Observe(time.Since(row.CreatedAt).Seconds())
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("outbox relay: %w", err)
	}
	if publishErr != nil {
		return 0, publishErr
	}
	return published, nil
}

// RunPendingGauge keeps the metric marpokat_outbox_pending up to date until ctx ends.
func (r *Relay) RunPendingGauge(ctx context.Context, every time.Duration) {
	if r.m == nil {
		return
	}
	q := store.New(r.pool)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if n, err := q.CountOutboxPending(ctx); err == nil {
			r.m.OutboxPending.Set(float64(n))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Cleaner deletes events that were published long ago: the table must not grow forever.
type Cleaner struct {
	pool      *pgxpool.Pool
	log       *slog.Logger
	Retention time.Duration // default 7 days
	Every     time.Duration // default 10 minutes
}

func NewCleaner(pool *pgxpool.Pool, log *slog.Logger) *Cleaner {
	return &Cleaner{pool: pool, log: log.With("subsystem", "outbox-cleaner"), Retention: 7 * 24 * time.Hour, Every: 10 * time.Minute}
}

// Once deletes everything older than the retention and returns how many rows went.
func (c *Cleaner) Once(ctx context.Context) (int64, error) {
	q := store.New(c.pool)
	var total int64
	for {
		n, err := q.DeleteOutboxPublishedBefore(ctx, ptr(time.Now().Add(-c.Retention)))
		if err != nil {
			return total, fmt.Errorf("outbox cleaner: %w", err)
		}
		total += n
		if n < 5000 {
			return total, nil
		}
	}
}

// Run cleans until ctx ends.
func (c *Cleaner) Run(ctx context.Context) {
	ticker := time.NewTicker(c.Every)
	defer ticker.Stop()
	for {
		if n, err := c.Once(ctx); err != nil {
			c.log.Warn("cleaning failed", "error", err)
		} else if n > 0 {
			c.log.Info("old events deleted", "rows", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func ptr[T any](v T) *T { return &v }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
