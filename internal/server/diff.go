package server

import (
	"github.com/example/cron-checker/internal/model"
	"github.com/example/cron-checker/internal/store"
)

// DiffReports compares two agent reports by entry fingerprint and returns
// entries added and removed between prev and cur.
func DiffReports(prev, cur *model.AgentReport) (added, removed []model.CronEntry) {
	prevSet := make(map[string]model.CronEntry, len(prev.Entries))
	for _, e := range prev.Entries {
		prevSet[store.Fingerprint(e)] = e
	}
	curSet := make(map[string]model.CronEntry, len(cur.Entries))
	for _, e := range cur.Entries {
		fp := store.Fingerprint(e)
		curSet[fp] = e
		if _, ok := prevSet[fp]; !ok {
			added = append(added, e)
		}
	}
	for _, e := range prev.Entries {
		if _, ok := curSet[store.Fingerprint(e)]; !ok {
			removed = append(removed, e)
		}
	}
	return added, removed
}
