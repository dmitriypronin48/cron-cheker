package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	prommodel "github.com/prometheus/client_model/go"

	"github.com/example/cron-checker/internal/model"
	"github.com/example/cron-checker/internal/store"
)

func findGauge(t *testing.T, families []*prommodel.MetricFamily, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			match := true
			for k, v := range labels {
				found := false
				for _, lp := range m.GetLabel() {
					if lp.GetName() == k && lp.GetValue() == v {
						found = true
						break
					}
				}
				if !found {
					match = false
					break
				}
			}
			if match {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

func TestNewEntryMetricTransitionsToZero(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, _, err := st.SyncAgents(ctx, []store.AgentTarget{{Hostname: "host1", IP: "10.0.0.1"}}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	agents, err := st.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	report := &model.AgentReport{
		AgentVersion: model.AgentVersion,
		Hostname:     "host1",
		IP:           "10.0.0.1",
		CollectedAt:  time.Now().UTC(),
		Timezone:     "UTC",
		Entries: []model.CronEntry{
			{Source: model.SourceSystemCrontab, Path: "/etc/crontab", User: "root", RawLine: "0 0 * * * /bin/new-job", LineNo: 1, Enabled: true},
		},
	}
	if err := st.UpsertReport(ctx, agents[0].ID, report); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	m := NewMetrics(st, nil)
	m.refresh(ctx)
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"hostname": "host1", "ip": "10.0.0.1", "source": model.SourceSystemCrontab, "raw_line": "0 0 * * * /bin/new-job"}
	v, ok := findGauge(t, families, "cron_checker_new_entry", labels)
	if !ok || v != 1 {
		t.Fatalf("new entry should be 1, got %v (present=%v)", v, ok)
	}

	// After Ack the same series must stay exported with value 0.
	entries, err := st.ListEntries(ctx, agents[0].ID, store.EntryFilter{})
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries: %v %v", entries, err)
	}
	if err := st.AckEntry(ctx, entries[0].ID); err != nil {
		t.Fatal(err)
	}
	m.refresh(ctx)
	families, err = m.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	v, ok = findGauge(t, families, "cron_checker_new_entry", labels)
	if !ok || v != 0 {
		t.Fatalf("acked entry should stay exported as 0, got %v (present=%v)", v, ok)
	}
}

func TestAgentsTotalHasHostnameIPLabels(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, _, err := st.SyncAgents(ctx, []store.AgentTarget{{Hostname: "web1", IP: "10.0.0.1"}, {Hostname: "db1", IP: "10.0.0.2"}}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	m := NewMetrics(st, nil)
	m.refresh(ctx)
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}

	v, ok := findGauge(t, families, "cron_checker_agents_total",
		map[string]string{"hostname": "web1", "ip": "10.0.0.1", "status": model.AgentStatusOffline})
	if !ok || v != 1 {
		t.Fatalf("agents_total for web1/offline should be 1, got %v (present=%v)", v, ok)
	}
	v, ok = findGauge(t, families, "cron_checker_agents_total",
		map[string]string{"hostname": "db1", "ip": "10.0.0.2", "status": model.AgentStatusOffline})
	if !ok || v != 1 {
		t.Fatalf("agents_total for db1/offline should be 1, got %v (present=%v)", v, ok)
	}
	// A stale series with the other status must not exist.
	if _, ok := findGauge(t, families, "cron_checker_agents_total",
		map[string]string{"hostname": "web1", "ip": "10.0.0.1", "status": model.AgentStatusOnline}); ok {
		t.Fatal("stale online series must not be present")
	}
}
