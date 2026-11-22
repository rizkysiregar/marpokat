package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/rizkysiregar/marpokat/server/internal/platform/rabbitmq"
)

// Envelope is an event as it travels through RabbitMQ: what the outbox row said, plus where it came from.
type Envelope struct {
	EventID   string          `json:"event_id"`
	TenantID  string          `json:"tenant_id"`
	Topic     string          `json:"topic"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

// Publisher publishes envelopes to the events exchange and waits until the broker has confirmed every one of them
// (publisher confirms): an event is only marked published after RabbitMQ took responsibility for it.
type Publisher struct {
	conn   *rabbitmq.Conn
	names  Names
	queues []WorkQueue

	mu sync.Mutex
	ch *amqp.Channel
}

// NewPublisher makes a publisher. It declares the queues too, not only the exchange: RabbitMQ drops a message that no
// queue is bound for, and the relay would have marked the event published. Whoever starts first (relay or consumer),
// the bindings exist before the first event goes out.
func NewPublisher(conn *rabbitmq.Conn, names Names, queues ...WorkQueue) *Publisher {
	return &Publisher{conn: conn, names: names, queues: queues}
}

// channel returns the confirm mode channel, opening it (and declaring the exchanges) when there is none.
func (p *Publisher) channel() (*amqp.Channel, error) {
	if p.ch != nil && !p.ch.IsClosed() {
		return p.ch, nil
	}
	ch, err := p.conn.Channel()
	if err != nil {
		return nil, err
	}
	if err := ch.Confirm(false); err != nil {
		_ = ch.Close()
		return nil, fmt.Errorf("broker: confirm mode: %w", err)
	}
	if err := DeclareExchanges(ch, p.names); err != nil {
		_ = ch.Close()
		return nil, err
	}
	for _, q := range p.queues {
		if err := DeclareWorkQueue(ch, p.names, q); err != nil {
			_ = ch.Close()
			return nil, err
		}
	}
	p.ch = ch
	return ch, nil
}

func (p *Publisher) drop() {
	if p.ch != nil {
		_ = p.ch.Close()
		p.ch = nil
	}
}

// Publish sends the envelopes and returns nil only when RabbitMQ confirmed all of them. After an error some may
// have been published anyway; consumers must tolerate that, they deduplicate on the event id.
func (p *Publisher) Publish(ctx context.Context, envs []Envelope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch, err := p.channel()
	if err != nil {
		return err
	}
	confirms := make([]*amqp.DeferredConfirmation, 0, len(envs))
	for _, env := range envs {
		body, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("broker: encode event %s: %w", env.EventID, err)
		}
		dc, err := ch.PublishWithDeferredConfirmWithContext(ctx, p.names.Events(), env.Topic, false, false, amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			MessageId:    env.EventID,
			Timestamp:    env.CreatedAt,
			Body:         body,
		})
		if err != nil {
			p.drop()
			return fmt.Errorf("broker: publish %s: %w", env.EventID, err)
		}
		confirms = append(confirms, dc)
	}
	for i, dc := range confirms {
		acked, err := dc.WaitContext(ctx)
		if err != nil || !acked {
			p.drop()
			if err == nil {
				err = errors.New("the broker refused it (nack)")
			}
			return fmt.Errorf("broker: event %s not confirmed: %w", envs[i].EventID, err)
		}
	}
	return nil
}

// Close releases the channel.
func (p *Publisher) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.drop()
}
