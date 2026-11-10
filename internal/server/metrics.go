package server

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/example/cron-checker/internal/model"
	"github.com/example/cron-checker/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds all prometheus metrics and refreshes store-backed gauges on scrape.
type Metrics struct {
	registry *prometheus.Registry
	store    *store.Store
	log      *slog.Logger

	agentsTotal     *prometheus.GaugeVec
	agentUp         *prometheus.GaugeVec
	agentLastSeen   *prometheus.GaugeVec
	entriesTotal    *prometheus.GaugeVec
	newEntries      *prometheus.GaugeVec
	pollDuration    prometheus.Histogram
	pollErrorsTotal *prometheus.CounterVec

	mu      sync.Mutex
	lastErr float64 // exported as cron_checker_store_refresh_errors_total-ish simplicity: logged only
}

// NewMetrics creates a Metrics bound to its own registry.
func NewMetrics(st *store.Store, log *slog.Logger) *Metrics {
	if log == nil {
		log = slog.Default()
	}
	m := &Metrics{
		store: st,
		log:   log,
		agentsTotal: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "cron_checker",
			Name:      "agents_total",
			Help:      "Number of agents by status.",
		}, []string{"status"}),
		agentUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "cron_checker",
			Name:      "agent_up",
			Help:      "Whether the agent is online (1) or offline (0).",
		}, []string{"hostname", "ip"}),
		agentLastSeen: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "cron_checker",
			Name:      "agent_last_seen_timestamp",
			Help:      "Unix timestamp of the last successful poll.",
		}, []string{"hostname", "ip"}),
		entriesTotal: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "cron_checker",
			Name:      "entries_total",
			Help:      "Number of cron entries by agent, source and status.",
		}, []string{"hostname", "source", "status"}),
		newEntries: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "cron_checker",
			Name:      "new_entries_24h",
			Help:      "Number of entries first seen within the last 24h, unacked.",
		}, []string{"hostname"}),
		pollDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "cron_checker",
			Name:      "poll_duration_seconds",
			Help:      "Time spent polling a single agent.",
			Buckets:   prometheus.DefBuckets,
		}),
		pollErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "cron_checker",
			Name:      "poll_errors_total",
			Help:      "Number of failed poll attempts per agent.",
		}, []string{"hostname"}),
	}
	m.registry = prometheus.NewRegistry()
	m.registry.MustRegister(
		collectors.NewGoCollector(),
		m.agentsTotal, m.agentUp, m.agentLastSeen, m.entriesTotal,
		m.newEntries, m.pollDuration, m.pollErrorsTotal,
	)
	return m
}

// ObservePollDuration records a single-agent poll duration.
func (m *Metrics) ObservePollDuration(seconds float64) {
	m.pollDuration.Observe(seconds)
}

// IncPollError increments the per-hostname poll error counter.
func (m *Metrics) IncPollError(hostname string) {
	m.pollErrorsTotal.WithLabelValues(hostname).Inc()
}

// Handler returns an http.Handler serving /metrics, refreshing gauges first.
func (m *Metrics) Handler() http.Handler {
	inner := promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.refresh(r.Context())
		inner.ServeHTTP(w, r)
	})
}

// refresh reloads store-backed gauges. Called on each scrape.
func (m *Metrics) refresh(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	agents, err := m.store.ListAgents(ctx)
	if err != nil {
		m.log.WarnContext(ctx, "metrics refresh: list agents", "error", err)
		return
	}
	online, offline, err := m.store.AgentsByStatus(ctx)
	if err != nil {
		m.log.WarnContext(ctx, "metrics refresh: agents by status", "error", err)
	} else {
		m.agentsTotal.WithLabelValues(model.AgentStatusOnline).Set(float64(online))
		m.agentsTotal.WithLabelValues(model.AgentStatusOffline).Set(float64(offline))
	}

	m.agentUp.Reset()
	m.agentLastSeen.Reset()
	m.newEntries.Reset()
	for _, a := range agents {
		up := 0.0
		if a.Status == model.AgentStatusOnline {
			up = 1
		}
		m.agentUp.WithLabelValues(a.Hostname, a.IP).Set(up)
		if !a.LastSeenAt.IsZero() {
			m.agentLastSeen.WithLabelValues(a.Hostname, a.IP).Set(float64(a.LastSeenAt.Unix()))
		}
		m.newEntries.WithLabelValues(a.Hostname).Set(float64(a.NewCount))
	}

	m.entriesTotal.Reset()
	type key struct {
		host, source, status string
	}
	counts := map[key]int{}
	for _, a := range agents {
		entries, err := m.store.ListEntries(ctx, a.ID, store.EntryFilter{})
		if err != nil {
			m.log.WarnContext(ctx, "metrics refresh: list entries", "hostname", a.Hostname, "error", err)
			continue
		}
		for _, e := range entries {
			counts[key{a.Hostname, e.Source, e.Status}]++
		}
	}
	for k, n := range counts {
		m.entriesTotal.WithLabelValues(k.host, k.source, k.status).Set(float64(n))
	}
}

// CleanupLoop runs store cleanup once an hour until ctx is cancelled.
func CleanupLoop(ctx context.Context, st *store.Store, snapshotRetentionDays, removedRetentionHours int, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	do := func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := st.Cleanup(cctx, snapshotRetentionDays, removedRetentionHours); err != nil {
			log.WarnContext(cctx, "cleanup failed", "error", err)
		}
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			do()
		}
	}
}
