package server

import (
	"testing"

	"github.com/example/cron-checker/internal/model"
)

func entry(src, raw string, line int) model.CronEntry {
	return model.CronEntry{Source: src, Path: "/etc/crontab", User: "root", RawLine: raw, LineNo: line}
}

func TestDiffReports(t *testing.T) {
	e1 := entry(model.SourceCronD, "0 1 * * * /bin/a", 1)
	e2 := entry(model.SourceCronD, "0 2 * * * /bin/b", 2)
	e3 := entry(model.SourceUserCrontab, "0 3 * * * /bin/c", 3)

	prev := &model.AgentReport{Entries: []model.CronEntry{e1, e2}}
	cur := &model.AgentReport{Entries: []model.CronEntry{e1, e3}}

	added, removed := DiffReports(prev, cur)
	if len(added) != 1 || added[0].RawLine != e3.RawLine {
		t.Fatalf("added: %+v", added)
	}
	if len(removed) != 1 || removed[0].RawLine != e2.RawLine {
		t.Fatalf("removed: %+v", removed)
	}

	// Identical reports: no diff.
	added, removed = DiffReports(cur, cur)
	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("identical reports should have empty diff: %v %v", added, removed)
	}

	// Empty previous: everything added.
	added, removed = DiffReports(&model.AgentReport{}, cur)
	if len(added) != 2 || len(removed) != 0 {
		t.Fatalf("empty prev: %v %v", added, removed)
	}

	// Empty current: everything removed.
	added, removed = DiffReports(prev, &model.AgentReport{})
	if len(added) != 0 || len(removed) != 2 {
		t.Fatalf("empty cur: %v %v", added, removed)
	}
}
