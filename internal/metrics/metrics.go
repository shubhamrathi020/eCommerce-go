// Package metrics exposes Prometheus metrics: the RED method (rate, errors, duration) for every HTTP request, plus
// the operational numbers the API already tracks internally (cache hit ratio, circuit-breaker state, outbox backlog)
// republished in a format Prometheus can scrape. Gauges read from other services at scrape time; nothing is pushed.
package metrics

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/shubhamrathi020/eCommerce-go/internal/cache"
	"github.com/shubhamrathi020/eCommerce-go/internal/db"
	"github.com/shubhamrathi020/eCommerce-go/internal/resilience"
)

var circuitStateValue = map[string]float64{"closed": 0, "half_open": 1, "open": 2}

// Metrics owns the registry.
type Metrics struct {
	registry *prometheus.Registry
	duration *prometheus.HistogramVec
	total    *prometheus.CounterVec
}

// New builds the registry. breakers returns the current circuit-breaker stats.
func New(c *cache.Cache, store *db.Store, breakers func() []resilience.Stats) *Metrics {
	reg := prometheus.NewRegistry()
	// Process CPU/memory and the Go runtime's own baseline.
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), collectors.NewGoCollector())
	m := &Metrics{
		registry: reg,
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "http_request_duration_seconds", Help: "HTTP request duration in seconds", Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}}, []string{"method", "route", "status"}),
		total:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "http_requests_total", Help: "Total HTTP requests (rate and error-rate PromQL both derive from this: e.g. rate(http_requests_total{status=~\"5..\"}[5m]))"}, []string{"method", "route", "status"}),
	}
	reg.MustRegister(m.duration, m.total)
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "cache_hit_ratio", Help: "Catalog cache hit ratio since process start"}, func() float64 {
		s := c.Stats()
		if total := s.Hits + s.Misses; total > 0 {
			return float64(s.Hits) / float64(total)
		}
		return 0
	}))
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "outbox_unpublished_total", Help: "Outbox events not yet relayed to RabbitMQ: should stay near 0; a sustained rise means the relay or broker is stuck"}, func() float64 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		n, err := db.CountUnpublishedOutbox(ctx, store.Pool)
		if err != nil {
			return -1
		}
		return float64(n)
	}))
	reg.MustRegister(&breakerCollector{breakers: breakers, desc: prometheus.NewDesc("circuit_breaker_state", "Circuit breaker state per dependency: 0=closed, 1=half_open, 2=open", []string{"dependency"}, nil)})
	return m
}

type breakerCollector struct {
	breakers func() []resilience.Stats
	desc     *prometheus.Desc
}

func (b *breakerCollector) Describe(ch chan<- *prometheus.Desc) { ch <- b.desc }

func (b *breakerCollector) Collect(ch chan<- prometheus.Metric) {
	for _, s := range b.breakers() {
		v, ok := circuitStateValue[s.State]
		if !ok {
			v = -1
		}
		ch <- prometheus.MustNewConstMetric(b.desc, prometheus.GaugeValue, v, s.Name)
	}
}

// Observe records one finished request; route is the matched path pattern (/orders/:id), never the raw URL, so the
// number of time series stays bounded.
func (m *Metrics) Observe(method, route string, status int, seconds float64) {
	labels := prometheus.Labels{"method": method, "route": route, "status": strconv.Itoa(status)}
	m.duration.With(labels).Observe(seconds)
	m.total.With(labels).Inc()
}

// Register mounts GET /metrics: internal, unauthenticated, not for browsers, the same posture as /healthz.
func (m *Metrics) Register(mux *http.ServeMux) {
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
}
