package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/example/cron-checker/internal/model"
	"github.com/example/cron-checker/internal/store"
)

// PollerConfig configures the background poll loop.
type PollerConfig struct {
	Interval    time.Duration
	Concurrency int
	Timeout     time.Duration
	Token       string
	AgentPort   string
	// Targets returns the desired agent inventory from the config file.
	// Re-read on every poll cycle; a non-nil error aborts the sync (the
	// previous agent set is kept) but not polling.
	Targets func() ([]store.AgentTarget, error)
}

// Poller periodically polls all agents over HTTP and stores their reports.
type Poller struct {
	cfg     PollerConfig
	store   *store.Store
	client  *http.Client
	log     *slog.Logger
	metrics *Metrics
}

// NewPoller creates a Poller. Defaults are applied for zero values.
func NewPoller(cfg PollerConfig, st *store.Store, log *slog.Logger, m *Metrics) *Poller {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 10
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.AgentPort == "" {
		cfg.AgentPort = "9300"
	}
	if log == nil {
		log = slog.Default()
	}
	return &Poller{
		cfg:     cfg,
		store:   st,
		client:  &http.Client{Timeout: cfg.Timeout},
		log:     log,
		metrics: m,
	}
}

// Run starts the poll loop and blocks until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	p.syncAgents(ctx)
	p.pollAll(ctx) // initial sweep so the UI has data immediately
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.syncAgents(ctx)
			p.pollAll(ctx)
		}
	}
}

// syncAgents reconciles the database with the config file inventory:
// agents added to the file appear in the UI, agents removed from the file
// are deleted from the DB (with their entries and snapshots).
func (p *Poller) syncAgents(ctx context.Context) {
	if p.cfg.Targets == nil {
		return
	}
	targets, err := p.cfg.Targets()
	if err != nil {
		p.log.WarnContext(ctx, "agent inventory reload failed, keeping previous agents", "error", err)
		return
	}
	added, removed, err := p.store.SyncAgents(ctx, targets)
	if err != nil {
		p.log.WarnContext(ctx, "agent inventory sync failed", "error", err)
		return
	}
	if added > 0 || removed > 0 {
		p.log.InfoContext(ctx, "agent inventory synced", "added", added, "removed", removed)
	}
}

func (p *Poller) pollAll(ctx context.Context) {
	start := time.Now()
	agents, err := p.store.ListAgents(ctx)
	if err != nil {
		p.log.WarnContext(ctx, "poll: list agents failed", "error", err)
		return
	}

	sem := make(chan struct{}, p.cfg.Concurrency)
	var wg sync.WaitGroup
	var okCount, failCount int
	var mu sync.Mutex

	for _, a := range agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := p.pollOne(ctx, a); err != nil {
				mu.Lock()
				failCount++
				mu.Unlock()
				p.log.WarnContext(ctx, "poll agent failed", "hostname", a.Hostname, "ip", a.IP, "error", err)
			} else {
				mu.Lock()
				okCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	p.log.InfoContext(ctx, "poll cycle complete",
		"agents", len(agents), "ok", okCount, "failed", failCount, "duration", time.Since(start).Round(time.Millisecond))
}

// pollOne performs a single poll attempt of one agent (no retries).
func (p *Poller) pollOne(ctx context.Context, a store.AgentRow) error {
	start := time.Now()
	defer func() {
		if p.metrics != nil {
			p.metrics.ObservePollDuration(time.Since(start).Seconds())
		}
	}()
	url := fmt.Sprintf("http://%s:%s/api/v1/cron", a.IP, p.cfg.AgentPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	resp, err := p.client.Do(req)
	if err != nil {
		p.incPollError(a.Hostname)
		p.markOffline(a.ID)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		p.incPollError(a.Hostname)
		p.markOffline(a.ID)
		return fmt.Errorf("agent returned status %d", resp.StatusCode)
	}
	var report model.AgentReport
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		p.incPollError(a.Hostname)
		p.markOffline(a.ID)
		return fmt.Errorf("decode report: %w", err)
	}
	if err := p.store.UpsertReport(ctx, a.ID, &report); err != nil {
		return fmt.Errorf("store report: %w", err)
	}
	return nil
}

func (p *Poller) markOffline(id int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.store.MarkAgentOffline(ctx, id); err != nil {
		p.log.WarnContext(ctx, "mark agent offline failed", "agent_id", id, "error", err)
	}
}

func (p *Poller) incPollError(hostname string) {
	if p.metrics != nil {
		p.metrics.IncPollError(hostname)
	}
}
