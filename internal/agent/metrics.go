package agent

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/example/cron-checker/internal/model"
)

const (
	metricsNamespace = "cron_checker"
	metricsSubsystem = "agent"
)

// Metrics holds the agent's Prometheus metrics, registered on a dedicated
// registry (no global default registerer pollution).
type Metrics struct {
	registry *prometheus.Registry

	Up            prometheus.Gauge
	LastCollect   prometheus.Gauge
	EntriesTotal  *prometheus.GaugeVec
	CollectErrors prometheus.Counter
	HTTPRequests  *prometheus.CounterVec
}

// NewMetrics creates and registers all agent metrics on a new registry.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Metrics{
		registry: reg,
		Up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "up",
			Help:      "Whether the agent process is up (always 1 while running).",
		}),
		LastCollect: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "last_collect_timestamp",
			Help:      "Unix timestamp of the last successful local collection.",
		}),
		EntriesTotal: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "entries_total",
			Help:      "Number of collected cron entries by source.",
		}, []string{"source"}),
		CollectErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "collect_errors_total",
			Help:      "Number of failed local collection attempts.",
		}),
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Subsystem: metricsSubsystem,
			Name:      "http_requests_total",
			Help:      "HTTP requests served, by route pattern, method and status code.",
		}, []string{"path", "method", "code"}),
	}
	m.Up.Set(1)
	reg.MustRegister(m.Up, m.LastCollect, m.EntriesTotal, m.CollectErrors, m.HTTPRequests)
	return m
}

// Handler returns an http.Handler serving the /metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// IncHTTP records one HTTP request. path should be the route pattern
// (e.g. "GET /api/v1/cron") to keep label cardinality bounded.
func (m *Metrics) IncHTTP(path, method, code string) {
	m.HTTPRequests.WithLabelValues(path, method, code).Inc()
}

// ObserveReport updates gauges from a freshly collected report.
func (m *Metrics) ObserveReport(r *model.AgentReport) {
	if r == nil {
		return
	}
	m.LastCollect.Set(float64(r.CollectedAt.Unix()))
	counts := make(map[string]float64)
	for _, e := range r.Entries {
		counts[e.Source]++
	}
	m.EntriesTotal.Reset()
	for source, n := range counts {
		m.EntriesTotal.WithLabelValues(source).Set(n)
	}
}

// statusRecorder captures the response status code for metrics.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// observe wraps a handler counting requests into the HTTPRequests metric.
// It must wrap the mux so that req.Pattern (route pattern) is already set.
func (m *Metrics) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, req)
		path := req.Pattern
		if path == "" {
			path = "unmatched"
		}
		m.IncHTTP(path, req.Method, strconv.Itoa(rec.status))
	})
}
