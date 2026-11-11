package store

import (
	"testing"

	"github.com/example/cron-checker/internal/model"
)

func TestSyncAgents(t *testing.T) {
	s, ctx := testStore(t)

	// Initial inventory: two agents, one with a report (entries + snapshot).
	targets := []AgentTarget{{Hostname: "host1", IP: "10.0.0.1"}, {Hostname: "host2", IP: "10.0.0.2"}}
	added, removed, err := s.SyncAgents(ctx, targets)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if added != 2 || removed != 0 {
		t.Fatalf("first sync: added=%d removed=%d, want 2/0", added, removed)
	}

	agents, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var id1 int64
	for _, a := range agents {
		if a.Hostname == "host1" {
			id1 = a.ID
		}
	}
	if id1 == 0 {
		t.Fatal("host1 not found")
	}
	if err := s.UpsertReport(ctx, id1, report(entry(model.SourceSystemCrontab, "0 0 * * * /bin/true", 1))); err != nil {
		t.Fatalf("upsert report: %v", err)
	}

	// Re-sync with the same inventory: nothing changes.
	added, removed, err = s.SyncAgents(ctx, targets)
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if added != 0 || removed != 0 {
		t.Fatalf("idempotent sync: added=%d removed=%d, want 0/0", added, removed)
	}

	// Shrink the inventory: host2 disappears with all its data.
	added, removed, err = s.SyncAgents(ctx, targets[:1])
	if err != nil {
		t.Fatalf("shrink sync: %v", err)
	}
	if added != 0 || removed != 1 {
		t.Fatalf("shrink sync: added=%d removed=%d, want 0/1", added, removed)
	}
	agents, err = s.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Hostname != "host1" {
		t.Fatalf("agents after shrink: %+v", agents)
	}

	// host1 entries must survive; host2's were cascade-deleted.
	entries, err := s.ListEntries(ctx, id1, EntryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("host1 entries = %d, want 1", len(entries))
	}

	// Invalid target (missing ip) must fail and change nothing.
	if _, _, err := s.SyncAgents(ctx, []AgentTarget{{Hostname: "bad"}}); err == nil {
		t.Fatal("sync with empty ip must fail")
	}
}

func TestListNewEntries(t *testing.T) {
	s, ctx := testStore(t)

	targets := []AgentTarget{{Hostname: "host1", IP: "10.0.0.1"}}
	if _, _, err := s.SyncAgents(ctx, targets); err != nil {
		t.Fatalf("sync: %v", err)
	}
	agents, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertReport(ctx, agents[0].ID, report(
		entry(model.SourceSystemCrontab, "0 0 * * * /bin/new-job", 1),
		entry(model.SourceCronD, "30 1 * * * root /opt/x", 1),
	)); err != nil {
		t.Fatalf("upsert report: %v", err)
	}

	newEntries, err := s.ListNewEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(newEntries) != 2 {
		t.Fatalf("new entries = %d, want 2: %+v", len(newEntries), newEntries)
	}
	for _, e := range newEntries {
		if e.Hostname != "host1" || e.IP != "10.0.0.1" {
			t.Errorf("agent labels wrong: %+v", e)
		}
	}

	// Ack one entry: only the remaining one is "new".
	if err := s.AckEntry(ctx, newEntries[0].ID); err != nil {
		t.Fatal(err)
	}
	newEntries, err = s.ListNewEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(newEntries) != 1 {
		t.Fatalf("new entries after ack = %d, want 1", len(newEntries))
	}
}
