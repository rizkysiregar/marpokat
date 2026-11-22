// Package rabbitmq keeps one AMQP connection alive and reconnects with backoff when it drops.
//
// Publishers and consumers (outbox relay, push, webhook, media; M4) ask for channels with Channel and re-run their
// setup when OnReconnect fires. The broker being down never crashes the process: events wait in the outbox table
// (PLAN D6) and the readiness probe reports the outage.
package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

var ErrNotConnected = errors.New("rabbitmq: not connected")

type Conn struct {
	url string
	log *slog.Logger

	mu        sync.RWMutex
	conn      *amqp.Connection
	listeners []chan struct{}

	// overridable in tests
	dial       func(url string) (*amqp.Connection, error)
	minBackoff time.Duration
	maxBackoff time.Duration
}

func New(url string, log *slog.Logger) *Conn {
	return &Conn{
		url:        url,
		log:        log.With("subsystem", "rabbitmq"),
		dial:       dial,
		minBackoff: 500 * time.Millisecond,
		maxBackoff: 30 * time.Second,
	}
}

func dial(url string) (*amqp.Connection, error) {
	return amqp.DialConfig(url, amqp.Config{
		Heartbeat:  10 * time.Second,
		Properties: amqp.Table{"connection_name": "marpokat"},
	})
}

// Run connects and keeps the connection alive until ctx is done, then closes it.
func (c *Conn) Run(ctx context.Context) {
	attempt := 0
	for {
		conn, err := c.dial(c.url)
		if err != nil {
			wait := Backoff(attempt, c.minBackoff, c.maxBackoff)
			attempt++
			c.log.Warn("connect failed, retrying", "error", err, "retry_in", wait.String(), "attempt", attempt)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
				continue
			}
		}
		attempt = 0
		closed := conn.NotifyClose(make(chan *amqp.Error, 1))
		c.setConn(conn)
		c.log.Info("connected")
		c.notify()

		select {
		case <-ctx.Done():
			c.setConn(nil)
			_ = conn.Close()
			return
		case amqpErr := <-closed:
			c.setConn(nil)
			c.log.Warn("connection lost", "error", amqpErr)
		}
	}
}

// Ready reports whether a connection is open. Used by the readiness probe.
func (c *Conn) Ready() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.conn != nil && !c.conn.IsClosed()
}

// Check is Ready as an error, for the health registry.
func (c *Conn) Check(context.Context) error {
	if !c.Ready() {
		return ErrNotConnected
	}
	return nil
}

// Channel opens a new AMQP channel on the current connection.
func (c *Conn) Channel() (*amqp.Channel, error) {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return nil, ErrNotConnected
	}
	ch, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("rabbitmq: open channel: %w", err)
	}
	return ch, nil
}

// OnReconnect returns a channel that receives a value after every successful (re)connect, including the first
// one if it has not happened yet. Consumers wait on it to (re)declare their topology and restart consuming.
func (c *Conn) OnReconnect() <-chan struct{} {
	ch := make(chan struct{}, 1)
	c.mu.Lock()
	c.listeners = append(c.listeners, ch)
	connected := c.conn != nil
	c.mu.Unlock()
	if connected {
		ch <- struct{}{}
	}
	return ch
}

func (c *Conn) setConn(conn *amqp.Connection) {
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
}

func (c *Conn) notify() {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, l := range c.listeners {
		select {
		case l <- struct{}{}:
		default: // a signal is already pending, the listener will set up again anyway
		}
	}
}

// Backoff returns an exponential delay with full jitter: random in [min, min*2^attempt], capped at max.
func Backoff(attempt int, min, max time.Duration) time.Duration {
	if attempt > 30 {
		attempt = 30
	}
	ceiling := min << attempt
	if ceiling <= 0 || ceiling > max {
		ceiling = max
	}
	if ceiling <= min {
		return min
	}
	return min + rand.N(ceiling-min) //nolint:gosec // jitter for retry delays, not security sensitive
}
