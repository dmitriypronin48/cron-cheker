package agent

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/cron-checker/internal/model"
)

func testReport(n int) *model.AgentReport {
	return &model.AgentReport{
		AgentVersion: model.AgentVersion,
		Hostname:     "testhost",
		Entries:      make([]model.CronEntry, n),
	}
}

func TestCacheFreshThenCached(t *testing.T) {
	var calls atomic.Int32
	cache := NewCache(time.Minute, func() (*model.AgentReport, error) {
		return testReport(int(calls.Add(1))), nil
	})

	r1, err := cache.Get()
	if err != nil {
		t.Fatal(err)
	}
	if r1 == nil || len(r1.Entries) != 1 {
		t.Fatalf("first report wrong: %+v", r1)
	}

	// Within the interval the cached report is reused.
	r2, err := cache.Get()
	if err != nil {
		t.Fatal(err)
	}
	if r2 != r1 {
		t.Errorf("expected cached report pointer, got %+v", r2)
	}
	if calls.Load() != 1 {
		t.Errorf("collect called %d times, want 1", calls.Load())
	}
}

func TestCacheReCollectsAfterInterval(t *testing.T) {
	var calls atomic.Int32
	cache := NewCache(30*time.Millisecond, func() (*model.AgentReport, error) {
		return testReport(int(calls.Add(1))), nil
	})

	if _, err := cache.Get(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond) // let the cache entry go stale

	r2, err := cache.Get()
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("collect called %d times, want 2", calls.Load())
	}
	if r2 == nil || len(r2.Entries) != 2 {
		t.Errorf("re-collected report wrong: %+v", r2)
	}
}

func TestCacheErrorReturnsStaleAndCounts(t *testing.T) {
	var calls atomic.Int32
	cache := NewCache(time.Minute, func() (*model.AgentReport, error) {
		if calls.Add(1) == 1 {
			return testReport(1), nil
		}
		return nil, errors.New("boom")
	})

	// Force the interval to zero so the next Get re-collects.
	cache.interval = 0

	r1, err := cache.Get()
	if err != nil || r1 == nil {
		t.Fatalf("first get: %v", err)
	}
	r2, err := cache.Get()
	if err == nil {
		t.Error("expected error from failing collect")
	}
	if r2 != r1 {
		t.Errorf("stale report should be returned on error, got %+v", r2)
	}
	if cache.ErrorsTotal() != 1 {
		t.Errorf("ErrorsTotal = %d, want 1", cache.ErrorsTotal())
	}
	if cache.LastCollect().IsZero() {
		t.Error("LastCollect should be set after first successful collect")
	}
}

func TestCacheConcurrent(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	cache := NewCache(50*time.Millisecond, func() (*model.AgentReport, error) {
		return testReport(int(calls.Add(1))), nil
	})

	const goroutines = 16
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if _, err := cache.Get(); err != nil {
					t.Errorf("Get: %v", err)
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}
	wg.Wait()

	total := calls.Load()
	// ~400 gets over ~125ms with 50ms interval: expect a handful of
	// re-collects, but nowhere near one per request.
	if total < 1 || total > 20 {
		t.Errorf("collect calls = %d, want a small bounded number", total)
	}
	if cache.ErrorsTotal() != 0 {
		t.Errorf("ErrorsTotal = %d, want 0", cache.ErrorsTotal())
	}
}
