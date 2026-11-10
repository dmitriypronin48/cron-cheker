package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/example/cron-checker/internal/model"
)

func testStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, context.Background()
}

func entry(src, raw string, line int) model.CronEntry {
	return model.CronEntry{
		Source:   src,
		Path:     "/etc/crontab",
		User:     "root",
		RawLine:  raw,
		LineNo:   line,
		Schedule: "0 0 * * *",
		Enabled:  true,
	}
}

func report(entries ...model.CronEntry) *model.AgentReport {
	return &model.AgentReport{
		AgentVersion: model.AgentVersion,
		Hostname:     "host1",
		IP:           "10.0.0.1",
		CollectedAt:  time.Now().UTC(),
		Timezone:     "UTC",
		Entries:      entries,
	}
}

func TestFingerprintKnownVector(t *testing.T) {
	e := entry(model.SourceUserCrontab, "0 0 * * * /bin/true", 7)
	got := Fingerprint(e)
	want := "140691a0b386e03a515a58c6ec9570e00199b71dc32fa18818fb77fc02d8a5ca" // sha256 per spec formula
	if got != want {
		t.Fatalf("fingerprint mismatch:\n got %s\nwant %s", got, want)
	}
	if Fingerprint(e) != Fingerprint(entry(model.SourceUserCrontab, "0 0 * * * /bin/true", 7)) {
		t.Fatal("fingerprint not stable")
	}
	if Fingerprint(e) == Fingerprint(entry(model.SourceUserCrontab, "0 0 * * * /bin/true", 8)) {
		t.Fatal("fingerprint does not depend on line_no")
	}
}

func TestUpsertReportLifecycle(t *testing.T) {
	s, ctx := testStore(t)

	id, err := s.AddAgent(ctx, "host1", "10.0.0.1")
	if err != nil {
		t.Fatalf("add agent: %v", err)
	}
	if id2, err := s.AddAgent(ctx, "host1", "10.0.0.1"); err != nil || id2 != id {
		t.Fatalf("add agent dup: id2=%d err=%v", id2, err)
	}

	e1 := entry(model.SourceCronD, "0 1 * * * /opt/job1.sh", 1)
	e2 := entry(model.SourceCronD, "0 2 * * * /opt/job2.sh", 2)

	if err := s.UpsertReport(ctx, id, report(e1, e2)); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}

	a, err := s.GetAgent(ctx, id)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if a.Status != model.AgentStatusOnline || a.EntriesCount != 2 || a.NewCount != 2 {
		t.Fatalf("agent after upsert: %+v", a)
	}
	if a.Timezone != "UTC" || a.AgentVersion != model.AgentVersion {
		t.Fatalf("agent meta: %+v", a)
	}

	// Second report: e1 unchanged, e2 replaced by e3.
	e3 := entry(model.SourceCronD, "0 3 * * * /opt/job3.sh", 3)
	if err := s.UpsertReport(ctx, id, report(e1, e3)); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}

	entries, err := s.ListEntries(ctx, id, EntryFilter{})
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries: %d", len(entries))
	}
	byRaw := map[string]EntryRow{}
	for _, e := range entries {
		byRaw[e.RawLine] = e
	}
	if byRaw[e2.RawLine].Status != model.EntryStatusRemoved {
		t.Fatalf("e2 should be removed: %+v", byRaw[e2.RawLine])
	}
	if byRaw[e1.RawLine].Status != model.EntryStatusActive || byRaw[e3.RawLine].Status != model.EntryStatusActive {
		t.Fatal("e1/e3 should be active")
	}

	// New count: only e3 is new now (first_seen within 24h, unacked);
	// e1 was first seen in the first report — but "new" uses first_seen_at,
	// and both were first seen seconds ago, so new_count = 2 (e1, e3), e2 is removed.
	a, _ = s.GetAgent(ctx, id)
	if a.NewCount != 2 {
		t.Fatalf("new count after upsert 2: %d", a.NewCount)
	}

	// Ack e3: new count drops to 1.
	if err := s.AckEntry(ctx, byRaw[e3.RawLine].ID); err != nil {
		t.Fatalf("ack: %v", err)
	}
	a, _ = s.GetAgent(ctx, id)
	if a.NewCount != 1 {
		t.Fatalf("new count after ack: %d", a.NewCount)
	}
	e3row, err := s.ListEntries(ctx, id, EntryFilter{OnlyNew: true})
	if err != nil {
		t.Fatalf("list only-new: %v", err)
	}
	if len(e3row) != 1 || e3row[0].RawLine != e1.RawLine {
		t.Fatalf("only-new filter: %+v", e3row)
	}

	// Ack of non-active entry fails.
	removedID := byRaw[e2.RawLine].ID
	if err := s.AckEntry(ctx, removedID); err == nil {
		t.Fatal("ack of removed entry should fail")
	}

	// Mark offline / agents by status.
	if err := s.MarkAgentOffline(ctx, id); err != nil {
		t.Fatalf("mark offline: %v", err)
	}
	online, offline, err := s.AgentsByStatus(ctx)
	if err != nil || online != 0 || offline != 1 {
		t.Fatalf("by status: %d/%d err=%v", online, offline, err)
	}

	// Search.
	res, err := s.Search(ctx, "job3")
	if err != nil || len(res) != 1 {
		t.Fatalf("search: %v %d", err, len(res))
	}
	if res[0].Hostname != "host1" || res[0].RawLine != e3.RawLine {
		t.Fatalf("search row: %+v", res[0])
	}

	// Snapshots.
	snaps, err := s.ListSnapshots(ctx, id, 10)
	if err != nil || len(snaps) != 2 {
		t.Fatalf("snapshots: %v %d", err, len(snaps))
	}
	var decoded model.AgentReport
	if err := json.Unmarshal([]byte(snaps[0].Payload), &decoded); err != nil {
		t.Fatalf("snapshot payload: %v", err)
	}
	if decoded.Hostname != "host1" {
		t.Fatalf("snapshot payload decode: %+v", decoded)
	}
}

func TestCleanup(t *testing.T) {
	s, ctx := testStore(t)

	id, err := s.AddAgent(ctx, "host1", "10.0.0.1")
	if err != nil {
		t.Fatalf("add agent: %v", err)
	}
	e1 := entry(model.SourceCronD, "0 1 * * * /opt/job1.sh", 1)
	if err := s.UpsertReport(ctx, id, report(e1)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	e2 := entry(model.SourceCronD, "0 2 * * * /opt/job2.sh", 2)
	if err := s.UpsertReport(ctx, id, report(e2)); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}

	// Age the removed entry and the snapshot.
	old := time.Now().UTC().Add(-8 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE cron_entries SET last_seen_at=? WHERE agent_id=? AND status='removed'`,
		old, id); err != nil {
		t.Fatalf("age entry: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE cron_snapshots SET collected_at=? WHERE agent_id=?`, old, id); err != nil {
		t.Fatalf("age snapshot: %v", err)
	}

	if err := s.Cleanup(ctx, 7, 24); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	entries, _ := s.ListEntries(ctx, id, EntryFilter{})
	if len(entries) != 1 {
		t.Fatalf("entries after cleanup: %d", len(entries))
	}
	snaps, _ := s.ListSnapshots(ctx, id, 10)
	if len(snaps) != 0 {
		t.Fatalf("snapshots after cleanup: %d", len(snaps))
	}
}

func TestListAgentsAndFilters(t *testing.T) {
	s, ctx := testStore(t)
	id, err := s.AddAgent(ctx, "host1", "10.0.0.1")
	if err != nil {
		t.Fatalf("add agent: %v", err)
	}
	e1 := entry(model.SourceUserCrontab, "0 1 * * * /bin/a", 1)
	e1.User = "alice"
	e2 := entry(model.SourceCronD, "0 2 * * * /bin/b", 2)
	e2.User = "root"
	if err := s.UpsertReport(ctx, id, report(e1, e2)); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	agents, err := s.ListAgents(ctx)
	if err != nil || len(agents) != 1 {
		t.Fatalf("list agents: %v %d", err, len(agents))
	}
	if agents[0].EntriesCount != 2 || agents[0].NewCount != 2 {
		t.Fatalf("agent counts: %+v", agents[0])
	}

	bySource, err := s.ListEntries(ctx, id, EntryFilter{Source: model.SourceCronD})
	if err != nil || len(bySource) != 1 || bySource[0].User != "root" {
		t.Fatalf("filter source: %v %+v", err, bySource)
	}
	byUser, err := s.ListEntries(ctx, id, EntryFilter{User: "alice"})
	if err != nil || len(byUser) != 1 || byUser[0].RawLine != e1.RawLine {
		t.Fatalf("filter user: %v %+v", err, byUser)
	}
}
