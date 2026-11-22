// Package metrics owns the Prometheus registry served on the management port.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry is a dedicated registry (not the global default) so tests can create their own.
type Registry struct {
	*prometheus.Registry
	HTTPRequests *prometheus.CounterVec
	HTTPDuration *prometheus.HistogramVec

	WSConnections     prometheus.Gauge
	WSClosed          *prometheus.CounterVec
	WSFramesIn        *prometheus.CounterVec
	WSEventsDelivered *prometheus.CounterVec

	OutboxPending         prometheus.Gauge
	OutboxPublished       prometheus.Counter
	OutboxPublishFailures prometheus.Counter
	OutboxLag             prometheus.Histogram
	QueueMessages         *prometheus.GaugeVec
	ConsumerHandled       *prometheus.CounterVec
	PushSent              *prometheus.CounterVec
	PushFailed            *prometheus.CounterVec
	PushSkipped           *prometheus.CounterVec
}

func New(version string) *Registry {
	r := prometheus.NewRegistry()
	r.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	build := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "marpokat_build_info",
		Help:        "Always 1, labelled with the running version.",
		ConstLabels: prometheus.Labels{"version": version},
	})
	build.Set(1)

	m := &Registry{
		Registry: r,
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "marpokat_http_requests_total",
			Help: "HTTP requests by route pattern, method and status code.",
		}, []string{"route", "method", "code"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "marpokat_http_request_duration_seconds",
			Help:    "HTTP request latency by route pattern and method.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"route", "method"}),
		WSConnections: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "marpokat_ws_connections",
			Help: "WebSocket connections open on this pod.",
		}),
		WSClosed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "marpokat_ws_closed_total",
			Help: "WebSocket connections that ended, by close code.",
		}, []string{"code"}),
		WSFramesIn: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "marpokat_ws_frames_in_total",
			Help: "Frames received from clients, by type.",
		}, []string{"type"}),
		WSEventsDelivered: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "marpokat_ws_events_delivered_total",
			Help: "Events written to client sockets, by type.",
		}, []string{"type"}),
		OutboxPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "marpokat_outbox_pending",
			Help: "Events committed but not yet handed to RabbitMQ. Grows while the broker is down.",
		}),
		OutboxPublished: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "marpokat_outbox_published_total",
			Help: "Events handed to RabbitMQ (and confirmed by it).",
		}),
		OutboxPublishFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "marpokat_outbox_publish_failures_total",
			Help: "Batches the relay could not publish.",
		}),
		OutboxLag: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "marpokat_outbox_lag_seconds",
			Help:    "Time from an event being committed to it being published.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 15, 60, 300, 1800},
		}),
		QueueMessages: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "marpokat_queue_messages",
			Help: "Messages waiting in a RabbitMQ queue. A dead letter queue (*.dlq) that is not empty needs a look.",
		}, []string{"queue"}),
		ConsumerHandled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "marpokat_consumer_handled_total",
			Help: "Messages a consumer finished with: ok, retry (sent to a retry queue) or dead (dead lettered).",
		}, []string{"queue", "result"}),
		PushSent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "marpokat_push_sent_total",
			Help: "Push notifications accepted by FCM, by platform.",
		}, []string{"platform"}),
		PushFailed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "marpokat_push_failed_total",
			Help: "Push notifications FCM did not accept: invalid_token (the device was removed), retryable, permanent.",
		}, []string{"reason"}),
		PushSkipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "marpokat_push_skipped_total",
			Help: "Messages or recipients that got no push, by reason: system, stale, no_credentials, online, duplicate.",
		}, []string{"reason"}),
	}
	r.MustRegister(build, m.HTTPRequests, m.HTTPDuration, m.WSConnections, m.WSClosed, m.WSFramesIn, m.WSEventsDelivered,
		m.OutboxPending, m.OutboxPublished, m.OutboxPublishFailures, m.OutboxLag, m.QueueMessages, m.ConsumerHandled,
		m.PushSent, m.PushFailed, m.PushSkipped)
	return m
}

func (m *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{Registry: m.Registry})
}
