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
	}
	r.MustRegister(build, m.HTTPRequests, m.HTTPDuration, m.WSConnections, m.WSClosed, m.WSFramesIn, m.WSEventsDelivered)
	return m
}

func (m *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{Registry: m.Registry})
}
