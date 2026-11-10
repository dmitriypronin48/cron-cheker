package agent

import (
	"sync"
	"time"

	"github.com/example/cron-checker/internal/model"
)

// CachedCollector wraps a collect function with a time-based cache so that
// bursts of HTTP requests do not re-scan the host every time.
// Safe for concurrent use.
type CachedCollector struct {
	mu       sync.Mutex
	interval time.Duration
	collect  func() (*model.AgentReport, error)

	report    *model.AgentReport
	collected time.Time
	errors    int64
}

// NewCache returns a CachedCollector that reuses a cached report while it is
// younger than interval; older or missing reports trigger a fresh collect.
func NewCache(interval time.Duration, collect func() (*model.AgentReport, error)) *CachedCollector {
	return &CachedCollector{interval: interval, collect: collect}
}

// Get returns the cached report if still fresh; otherwise re-collects.
// On collect failure the previous report (if any) is returned along with the
// error, so callers can decide to serve stale data.
func (c *CachedCollector) Get() (*model.AgentReport, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.report != nil && now.Sub(c.collected) < c.interval {
		return c.report, nil
	}
	report, err := c.collect()
	if err != nil {
		c.errors++
		return c.report, err
	}
	c.report = report
	c.collected = now
	return report, nil
}

// ErrorsTotal returns the number of failed collect attempts since start.
func (c *CachedCollector) ErrorsTotal() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.errors
}

// LastCollect returns the time of the last successful collect.
func (c *CachedCollector) LastCollect() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.collected
}
