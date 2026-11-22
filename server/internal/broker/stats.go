package broker

import (
	"context"
	"log/slog"
	"time"

	"github.com/rizkysiregar/marpokat/server/internal/platform/metrics"
	"github.com/rizkysiregar/marpokat/server/internal/platform/rabbitmq"
)

// RunQueueStats publishes the length of the given queues as the gauge marpokat_queue_messages until ctx ends.
// An unexpected length of a retry queue or of a dead letter queue is how trouble shows up.
func RunQueueStats(ctx context.Context, conn *rabbitmq.Conn, queues []string, m *metrics.Registry, every time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	// the queues are declared by the publisher and the consumer, which start together with this: look a moment later
	select {
	case <-ctx.Done():
		return
	case <-time.After(min(2*time.Second, every)):
	}
	for {
		for _, q := range queues {
			if n, ok := queueLength(conn, q); ok {
				m.QueueMessages.WithLabelValues(q).Set(float64(n))
			} else {
				log.Debug("cannot read the length of a queue", "queue", q)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// queueLength asks over a channel of its own: asking about a queue that does not exist closes the channel.
func queueLength(conn *rabbitmq.Conn, queue string) (int, bool) {
	ch, err := conn.Channel()
	if err != nil {
		return 0, false
	}
	defer func() { _ = ch.Close() }()
	q, err := ch.QueueDeclarePassive(queue, true, false, false, false, nil)
	if err != nil {
		return 0, false
	}
	return q.Messages, true
}

// QueueLength is queueLength for tests and tools.
func QueueLength(conn *rabbitmq.Conn, queue string) (int, bool) { return queueLength(conn, queue) }
