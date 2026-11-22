// Package broker is the RabbitMQ side of the worker (PLAN M4): the topology, a publisher that waits for the broker's
// confirm, and a consumer with retries and a dead letter queue.
//
// Layout, for a work queue called "push" and prefix "marpokat":
//
//	marpokat.events  topic   the outbox relay publishes here, routing key = event topic
//	marpokat.retry   direct  where a failed message is sent to wait, and where it comes back from
//	marpokat.dlx     direct  where a message is dead lettered
//
//	marpokat.push            work queue, bound to marpokat.events by the keys of the queue
//	                         and to marpokat.retry by "push" (the way back)
//	marpokat.push.retry.N    holds a message for the Nth delay, then dead letters it to marpokat.retry with "push"
//	marpokat.push.dlq        bound to marpokat.dlx by "push": what could not be handled (alert when not empty)
package broker

import (
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Names builds the names of exchanges and queues. Tests give each test its own prefix.
type Names struct{ Prefix string }

func DefaultNames() Names { return Names{Prefix: "marpokat"} }

func (n Names) Events() string               { return n.Prefix + ".events" }
func (n Names) Retry() string                { return n.Prefix + ".retry" }
func (n Names) Dead() string                 { return n.Prefix + ".dlx" }
func (n Names) Queue(name string) string     { return n.Prefix + "." + name }
func (n Names) DeadQueue(name string) string { return n.Prefix + "." + name + ".dlq" }
func (n Names) RetryQueue(name string, tier int) string {
	return fmt.Sprintf("%s.%s.retry.%d", n.Prefix, name, tier)
}
func retryKey(name string, tier int) string { return fmt.Sprintf("%s.retry.%d", name, tier) }

// DefaultRetryDelays: a failed job is tried again after 10 seconds, then a minute, then 10 minutes, then dead lettered.
// Changing them means deleting the retry queues first: RabbitMQ refuses to redeclare a queue with other arguments.
var DefaultRetryDelays = []time.Duration{10 * time.Second, time.Minute, 10 * time.Minute}

// WorkQueue is a queue of jobs.
type WorkQueue struct {
	Name        string   // short name, e.g. "push"
	Keys        []string // routing keys of the events it receives
	RetryDelays []time.Duration
}

func (w WorkQueue) delays() []time.Duration {
	if w.RetryDelays != nil {
		return w.RetryDelays
	}
	return DefaultRetryDelays
}

// DeclareExchanges declares the three exchanges. Declaring is idempotent.
func DeclareExchanges(ch *amqp.Channel, n Names) error {
	for _, e := range []struct{ name, kind string }{{n.Events(), "topic"}, {n.Retry(), "direct"}, {n.Dead(), "direct"}} {
		if err := ch.ExchangeDeclare(e.name, e.kind, true, false, false, false, nil); err != nil {
			return fmt.Errorf("broker: declare exchange %s: %w", e.name, err)
		}
	}
	return nil
}

// DeclareWorkQueue declares a work queue with its retry queues and dead letter queue, and the exchanges.
// Both sides (the relay publishes, the consumer reads) declare, so it does not matter which starts first.
func DeclareWorkQueue(ch *amqp.Channel, n Names, w WorkQueue) error {
	if err := DeclareExchanges(ch, n); err != nil {
		return err
	}
	queue := n.Queue(w.Name)
	if _, err := ch.QueueDeclare(queue, true, false, false, false, amqp.Table{
		"x-dead-letter-exchange":    n.Dead(),
		"x-dead-letter-routing-key": w.Name,
	}); err != nil {
		return fmt.Errorf("broker: declare queue %s: %w", queue, err)
	}
	for _, key := range w.Keys {
		if err := ch.QueueBind(queue, key, n.Events(), false, nil); err != nil {
			return fmt.Errorf("broker: bind %s to %s: %w", queue, key, err)
		}
	}
	if err := ch.QueueBind(queue, w.Name, n.Retry(), false, nil); err != nil { // the way back from the retry queues
		return fmt.Errorf("broker: bind %s to the retry exchange: %w", queue, err)
	}
	for i, delay := range w.delays() {
		tier := i + 1
		rq := n.RetryQueue(w.Name, tier)
		if _, err := ch.QueueDeclare(rq, true, false, false, false, amqp.Table{
			"x-message-ttl":             delay.Milliseconds(),
			"x-dead-letter-exchange":    n.Retry(),
			"x-dead-letter-routing-key": w.Name,
		}); err != nil {
			return fmt.Errorf("broker: declare queue %s: %w", rq, err)
		}
		if err := ch.QueueBind(rq, retryKey(w.Name, tier), n.Retry(), false, nil); err != nil {
			return fmt.Errorf("broker: bind %s: %w", rq, err)
		}
	}
	dlq := n.DeadQueue(w.Name)
	if _, err := ch.QueueDeclare(dlq, true, false, false, false, nil); err != nil {
		return fmt.Errorf("broker: declare queue %s: %w", dlq, err)
	}
	if err := ch.QueueBind(dlq, w.Name, n.Dead(), false, nil); err != nil {
		return fmt.Errorf("broker: bind %s: %w", dlq, err)
	}
	return nil
}

// AllQueues lists every queue of a work queue, for statistics.
func (w WorkQueue) AllQueues(n Names) []string {
	out := []string{n.Queue(w.Name)}
	for i := range w.delays() {
		out = append(out, n.RetryQueue(w.Name, i+1))
	}
	return append(out, n.DeadQueue(w.Name))
}
