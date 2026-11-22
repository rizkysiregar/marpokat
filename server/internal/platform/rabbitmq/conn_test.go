package rabbitmq

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestBackoffStaysInRange(t *testing.T) {
	min, max := 500*time.Millisecond, 30*time.Second
	for attempt := 0; attempt < 100; attempt++ {
		for i := 0; i < 50; i++ {
			d := Backoff(attempt, min, max)
			if d < min || d > max {
				t.Fatalf("Backoff(%d) = %v, outside [%v, %v]", attempt, d, min, max)
			}
		}
	}
	if d := Backoff(0, min, max); d > min*2 {
		t.Fatalf("first retry %v should be close to min", d)
	}
}

func TestRunRetriesUntilCancelled(t *testing.T) {
	var dials atomic.Int32
	c := New("amqp://unused", slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.minBackoff, c.maxBackoff = time.Millisecond, 2*time.Millisecond
	c.dial = func(string) (*amqp.Connection, error) {
		dials.Add(1)
		return nil, errors.New("refused")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	c.Run(ctx)

	if dials.Load() < 3 {
		t.Fatalf("expected several dial attempts, got %d", dials.Load())
	}
	if c.Ready() {
		t.Fatal("Ready() = true without a connection")
	}
	if err := c.Check(ctx); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Check() = %v, want ErrNotConnected", err)
	}
	if _, err := c.Channel(); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Channel() = %v, want ErrNotConnected", err)
	}
}
