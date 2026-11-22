package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/rizkysiregar/marpokat/server/internal/platform/metrics"
	"github.com/rizkysiregar/marpokat/server/internal/platform/rabbitmq"
)

const (
	headerAttempt = "x-attempt"
	headerError   = "x-error"
	handleTimeout = 30 * time.Second
)

// Delivery tells a handler about the message it was given.
type Delivery struct {
	// Attempt is how many times the job already failed: 0 the first time.
	Attempt int
	// Redelivered: the broker hands out a message it had already given to a consumer that did not finish it.
	Redelivered bool
}

// Handler does the work for one event. nil: done. An error: try again later, through the retry queues, until the
// delays run out and the message is dead lettered. Permanent(err): it can never work, dead letter it at once.
type Handler func(ctx context.Context, env Envelope, d Delivery) error

type permanent struct{ err error }

func (p permanent) Error() string { return p.err.Error() }
func (p permanent) Unwrap() error { return p.err }

// Permanent marks an error that retrying cannot fix.
func Permanent(err error) error { return permanent{err} }

// IsPermanent reports whether err was marked with Permanent.
func IsPermanent(err error) bool { var p permanent; return errors.As(err, &p) }

// ConsumerConfig describes one consumer.
type ConsumerConfig struct {
	Names   Names
	Queue   WorkQueue
	Handler Handler
	// Prefetch is how many unacknowledged messages RabbitMQ gives this consumer at a time (default 20), Workers how
	// many are handled at once (default 4).
	Prefetch int
	Workers  int
	Log      *slog.Logger
	Metrics  *metrics.Registry // may be nil
}

// Consumer reads a work queue and runs the handler, with retries and dead lettering. It survives the loss of the
// broker: it waits for the connection and starts over.
type Consumer struct {
	conn *rabbitmq.Conn
	cfg  ConsumerConfig
}

func NewConsumer(conn *rabbitmq.Conn, cfg ConsumerConfig) *Consumer {
	if cfg.Prefetch <= 0 {
		cfg.Prefetch = 20
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	cfg.Log = cfg.Log.With("subsystem", "consumer", "queue", cfg.Names.Queue(cfg.Queue.Name))
	return &Consumer{conn: conn, cfg: cfg}
}

// Run consumes until ctx ends.
func (c *Consumer) Run(ctx context.Context) {
	reconnected := c.conn.OnReconnect()
	attempt := 0
	for ctx.Err() == nil {
		if !c.conn.Ready() {
			select {
			case <-ctx.Done():
				return
			case <-reconnected:
			case <-time.After(2 * time.Second):
			}
			continue
		}
		err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		wait := rabbitmq.Backoff(attempt, 500*time.Millisecond, 15*time.Second)
		attempt++
		c.cfg.Log.Warn("consuming stopped, starting again", "error", err, "retry_in", wait.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err == nil {
			attempt = 0
		}
	}
}

// session consumes over one channel until it breaks or ctx ends.
func (c *Consumer) session(ctx context.Context) error {
	ch, err := c.conn.Channel()
	if err != nil {
		return err
	}
	defer func() { _ = ch.Close() }()
	if err := DeclareWorkQueue(ch, c.cfg.Names, c.cfg.Queue); err != nil {
		return err
	}
	// retries and dead letters are published on a channel of their own, in confirm mode: a message is acknowledged
	// only after its replacement is safe in the broker
	pub, err := c.conn.Channel()
	if err != nil {
		return err
	}
	defer func() { _ = pub.Close() }()
	if err := pub.Confirm(false); err != nil {
		return fmt.Errorf("broker: confirm mode: %w", err)
	}
	if err := ch.Qos(c.cfg.Prefetch, 0, false); err != nil {
		return fmt.Errorf("broker: qos: %w", err)
	}
	tag := "marpokat-" + uuid.NewString()
	deliveries, err := ch.Consume(c.cfg.Names.Queue(c.cfg.Queue.Name), tag, false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("broker: consume: %w", err)
	}
	closed := ch.NotifyClose(make(chan *amqp.Error, 1))
	c.cfg.Log.Info("consuming", "workers", c.cfg.Workers, "prefetch", c.cfg.Prefetch)

	var wg sync.WaitGroup
	for range c.cfg.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range deliveries {
				c.handle(ctx, d, pub)
			}
		}()
	}
	var cause error
	select {
	case <-ctx.Done():
		// stop taking new messages, let the ones in progress finish and be acknowledged
		_ = ch.Cancel(tag, false)
	case amqpErr := <-closed:
		cause = errors.New("channel closed")
		if amqpErr != nil {
			cause = fmt.Errorf("channel closed: %w", amqpErr)
		}
	}
	wg.Wait()
	return cause
}

func (c *Consumer) handle(ctx context.Context, d amqp.Delivery, pub *amqp.Channel) {
	var env Envelope
	if err := json.Unmarshal(d.Body, &env); err != nil {
		c.finish(ctx, d, pub, "dead", fmt.Errorf("undecodable message: %w", err), attemptOf(d))
		return
	}
	attempt := attemptOf(d)
	// in progress work is not abandoned when the consumer is asked to stop
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), handleTimeout)
	defer cancel()
	err := c.safely(hctx, env, Delivery{Attempt: attempt, Redelivered: d.Redelivered})
	switch {
	case err == nil:
		_ = d.Ack(false)
		c.count("ok")
	case IsPermanent(err) || attempt >= len(c.cfg.Queue.delays()):
		c.finish(ctx, d, pub, "dead", err, attempt)
	default:
		c.finish(ctx, d, pub, "retry", err, attempt)
	}
}

// safely turns a panic of the handler into an error: one bad message must not take the worker down.
func (c *Consumer) safely(ctx context.Context, env Envelope, d Delivery) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panicked: %v", r)
		}
	}()
	return c.cfg.Handler(ctx, env, d)
}

// finish moves a message that was not handled to a retry queue or to the dead letter queue, and only then
// acknowledges the original.
func (c *Consumer) finish(ctx context.Context, d amqp.Delivery, pub *amqp.Channel, result string, cause error, attempt int) {
	exchange, key := c.cfg.Names.Dead(), c.cfg.Queue.Name
	nextAttempt := attempt
	if result == "retry" {
		nextAttempt = attempt + 1
		exchange, key = c.cfg.Names.Retry(), retryKey(c.cfg.Queue.Name, nextAttempt)
	}
	c.cfg.Log.Warn("job not done", "result", result, "attempt", attempt, "event_id", d.MessageId, "error", cause)
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	headers := amqp.Table{}
	for k, v := range d.Headers {
		headers[k] = v
	}
	headers[headerAttempt] = int32(nextAttempt) //nolint:gosec // a handful of retries
	headers[headerError] = truncate(cause.Error(), 500)
	dc, err := pub.PublishWithDeferredConfirmWithContext(pctx, exchange, key, false, false, amqp.Publishing{
		ContentType: d.ContentType, DeliveryMode: amqp.Persistent, MessageId: d.MessageId, Timestamp: d.Timestamp,
		Headers: headers, Body: d.Body,
	})
	acked := false
	if err == nil {
		acked, err = dc.WaitContext(pctx)
	}
	if err != nil || !acked {
		// could not park it: give it back to the broker rather than lose it
		c.cfg.Log.Error("cannot move the message, putting it back", "event_id", d.MessageId, "error", err)
		_ = d.Nack(false, true)
		time.Sleep(500 * time.Millisecond)
		return
	}
	_ = d.Ack(false)
	c.count(result)
}

func (c *Consumer) count(result string) {
	if c.cfg.Metrics != nil {
		c.cfg.Metrics.ConsumerHandled.WithLabelValues(c.cfg.Queue.Name, result).Inc()
	}
}

func attemptOf(d amqp.Delivery) int {
	switch v := d.Headers[headerAttempt].(type) {
	case int32:
		return int(v)
	case int64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
