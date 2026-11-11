package server

import (
	"context"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/example/cron-checker/internal/model"
	"github.com/example/cron-checker/internal/store"
)

func testServerWithAgent(t *testing.T) (*Server, *store.Store, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	if _, _, err := st.SyncAgents(ctx, []store.AgentTarget{{Hostname: "host1", IP: "10.0.0.1"}}); err != nil {
		t.Fatalf("sync agents: %v", err)
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
			{Source: model.SourceCronWeekly, Path: "/etc/cron.weekly/test", User: "root", RawLine: "#!/bin/sh", LineNo: 1, Enabled: true},
			{Source: model.SourceCronWeekly, Path: "/etc/cron.weekly/test", User: "root", RawLine: "0 * * * * echo \"Helloy\"", LineNo: 2, Enabled: true},
			{Source: model.SourceSystemCrontab, Path: "/etc/crontab", User: "root", RawLine: "17 * * * * root run-parts /etc/cron.hourly", LineNo: 2, Enabled: true},
		},
	}
	if err := st.UpsertReport(ctx, agents[0].ID, report); err != nil {
		t.Fatalf("upsert report: %v", err)
	}

	srv, err := NewServer(st, NewMetrics(st, nil), nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, st, agents[0].ID
}

func TestAgentPageFoldsFileLinesIntoBlock(t *testing.T) {
	srv, _, agentID := testServerWithAgent(t)

	req := httptest.NewRequest("GET", "/agents/"+strconv.FormatInt(agentID, 10), nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	html := string(body)

	// The two lines of /etc/cron.weekly/test must be folded into ONE block.
	if !strings.Contains(html, `id="block-`) {
		t.Error("file block row not rendered")
	}
	if !strings.Contains(html, "Ack all") {
		t.Error("Ack all button not rendered")
	}
	if !strings.Contains(html, "2 lines") {
		t.Error("line count not rendered")
	}
	if !strings.Contains(html, `echo &#34;Helloy&#34;`) && !strings.Contains(html, `echo "Helloy"`) {
		t.Error("file content line not rendered in block")
	}
	// The single-line /etc/crontab entry stays a plain row.
	if !strings.Contains(html, "run-parts /etc/cron.hourly") {
		t.Error("single entry row missing")
	}
}

func TestAckBlockEndpoint(t *testing.T) {
	srv, st, agentID := testServerWithAgent(t)
	ctx := context.Background()

	entries, err := st.ListEntries(ctx, agentID, store.EntryFilter{Source: model.SourceCronWeekly})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	var ids []string
	for _, e := range entries {
		ids = append(ids, strconv.FormatInt(e.ID, 10))
	}

	req := httptest.NewRequest("POST", "/api/v1/entries/ack-block", strings.NewReader("ids="+strings.Join(ids, ",")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `id="block-`) {
		t.Error("block fragment not re-rendered")
	}

	entries, err = st.ListEntries(ctx, agentID, store.EntryFilter{Source: model.SourceCronWeekly})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Status != model.EntryStatusAcked || e.AckedAt == nil {
			t.Errorf("entry %d not acked: %+v", e.ID, e)
		}
	}
}
