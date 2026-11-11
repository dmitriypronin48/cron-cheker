// Package server implements the cron-checker HTTP server, UI and poller.
package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"html"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/example/cron-checker/internal/model"
	"github.com/example/cron-checker/internal/store"
	"github.com/example/cron-checker/web"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Server wires the store, templates and metrics into an http.Handler.
type Server struct {
	st      *store.Store
	tmpl    *template.Template
	metrics *Metrics
	log     *slog.Logger
	router  http.Handler
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

// NewServer builds the chi router.
func NewServer(st *store.Store, metrics *Metrics, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(web.FS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s := &Server{st: st, tmpl: tmpl, metrics: metrics, log: log}

	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer)

	r.Get("/healthz", s.handleHealthz)
	r.Handle("/metrics", metrics.Handler())

	staticFS, err := fs.Sub(web.FS, "static")
	if err != nil {
		return nil, err
	}
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServerFS(staticFS)))

	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/agents", s.handleAPIAgents)
		r.Get("/agents/{id}/cron", s.handleAPIAgentCron)
		r.Get("/agents/{id}/history", s.handleAPIAgentHistory)
		r.Post("/entries/{id}/ack", s.handleAPIAck)
		r.Post("/entries/ack-block", s.handleAPIAckBlock)
		r.Get("/search", s.handleAPISearch)
	})

	r.Get("/", s.handleIndex)
	r.Get("/agents/{id}", s.handleAgent)
	r.Get("/agents/{id}/history", s.handleHistory)
	r.Get("/search", s.handleSearch)

	s.router = r
	return s, nil
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"timeIn": func(t time.Time, loc *time.Location) string {
			if t.IsZero() {
				return "—"
			}
			return t.In(loc).Format("2006-01-02 15:04:05 MST")
		},
		"rel":      relTime,
		"srcTitle": sourceTitle,
		"isNew": func(e store.EntryRow, now time.Time) bool {
			return e.Status == model.EntryStatusActive && e.AckedAt == nil &&
				now.Sub(e.FirstSeenAt) < 24*time.Hour
		},
		"dict": func(pairs ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(pairs); i += 2 {
				m[pairs[i].(string)] = pairs[i+1]
			}
			return m
		},
		"base": path.Base,
		"inc":  func(n int) int { return n + 1 },
		"ids": func(lines []store.EntryRow) string {
			parts := make([]string, len(lines))
			for i, l := range lines {
				parts[i] = strconv.FormatInt(l.ID, 10)
			}
			return strings.Join(parts, ",")
		},
		"countNew": func(lines []store.EntryRow, now time.Time) int {
			n := 0
			for _, l := range lines {
				if l.Status == model.EntryStatusActive && l.AckedAt == nil && now.Sub(l.FirstSeenAt) < 24*time.Hour {
					n++
				}
			}
			return n
		},
		"countAckable": func(lines []store.EntryRow) int {
			n := 0
			for _, l := range lines {
				if l.Status == model.EntryStatusActive && l.AckedAt == nil {
					n++
				}
			}
			return n
		},
	}
}

func relTime(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " min ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + " h ago"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + " d ago"
	}
}

func sourceTitle(src string) string {
	switch src {
	case model.SourceUserCrontab:
		return "User crontabs"
	case model.SourceSystemCrontab:
		return "System crontab"
	case model.SourceCronD:
		return "cron.d"
	case model.SourceCronHourly:
		return "cron.hourly"
	case model.SourceCronDaily:
		return "cron.daily"
	case model.SourceCronWeekly:
		return "cron.weekly"
	case model.SourceCronMonthly:
		return "cron.monthly"
	case model.SourceSystemdTimer:
		return "Systemd timers"
	default:
		return src
	}
}

func loadLoc(tz string) *time.Location {
	loc, err := time.LoadLocation(tz)
	if err != nil || loc == nil {
		return time.UTC
	}
	return loc
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- API handlers ---

func (s *Server) handleAPIAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.st.ListAgents(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if agents == nil {
		agents = []store.AgentRow{}
	}
	writeJSON(w, http.StatusOK, agents)
}

func entryFilterFromQuery(r *http.Request) store.EntryFilter {
	q := r.URL.Query()
	return store.EntryFilter{
		Source:  q.Get("source"),
		User:    q.Get("user"),
		OnlyNew: q.Get("only_new") == "1" || strings.EqualFold(q.Get("only_new"), "true"),
	}
}

func (s *Server) agentID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

func (s *Server) handleAPIAgentCron(w http.ResponseWriter, r *http.Request) {
	id, ok := s.agentID(w, r)
	if !ok {
		return
	}
	entries, err := s.st.ListEntries(r.Context(), id, entryFilterFromQuery(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if entries == nil {
		entries = []store.EntryRow{}
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) handleAPIAgentHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := s.agentID(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 10
	}
	snaps, err := s.st.ListSnapshots(r.Context(), id, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if snaps == nil {
		snaps = []store.SnapshotRow{}
	}
	writeJSON(w, http.StatusOK, snaps)
}

func (s *Server) handleAPIAck(w http.ResponseWriter, r *http.Request) {
	id, ok := s.agentID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if err := s.st.AckEntry(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "entry not found or not active")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// HTMX request: re-render the row fragment.
	if r.Header.Get("HX-Request") != "" {
		e, err := s.st.GetEntry(ctx, id)
		if err != nil {
			writeError(w, http.StatusNoContent, "")
			return
		}
		agent, err := s.st.GetAgent(ctx, e.AgentID)
		if err != nil {
			writeError(w, http.StatusNoContent, "")
			return
		}
		s.render(w, "entryrow", map[string]any{
			"E": e, "Now": time.Now().UTC(), "Loc": loadLoc(agent.Timezone),
		})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAPIAckBlock acknowledges all ackable lines of a file block (a script
// in cron.weekly etc.) in one click and re-renders the block fragment.
func (s *Server) handleAPIAckBlock(w http.ResponseWriter, r *http.Request) {
	raw := r.FormValue("ids")
	if raw == "" {
		writeError(w, http.StatusBadRequest, "ids are required")
		return
	}
	var ids []int64
	for _, p := range strings.Split(raw, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid ids")
			return
		}
		ids = append(ids, id)
	}
	n, err := s.st.AckEntries(r.Context(), ids)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Reload the lines and re-render the file block fragment.
	var lines []store.EntryRow
	var agentID int64
	for _, id := range ids {
		e, err := s.st.GetEntry(r.Context(), id)
		if err != nil {
			continue
		}
		lines = append(lines, e)
		agentID = e.AgentID
	}
	if len(lines) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].LineNo < lines[j].LineNo })
	a, err := s.st.GetAgent(r.Context(), agentID)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.render(w, "fileblock", map[string]any{
		"B":   fileBlock{Path: lines[0].Path, User: lines[0].User, Lines: lines},
		"Now": time.Now().UTC(),
		"Loc": loadLoc(a.Timezone),
	})
}

func (s *Server) handleAPISearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	results, err := s.st.Search(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if results == nil {
		results = []store.SearchRow{}
	}
	writeJSON(w, http.StatusOK, results)
}

// --- UI handlers ---

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("render template", "template", name, "error", err)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	agents, err := s.st.ListAgents(r.Context())
	if err != nil {
		s.log.Error("list agents", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, "index", map[string]any{
		"Agents": agents,
		"Query":  "",
		"Now":    time.Now().UTC(),
		"Loc":    time.UTC,
	})
}

// fileBlock is a group of entries that are lines of the same file (e.g. a
// script in cron.weekly). Rendered in the UI as one expandable row with the
// full file content instead of one row per line.
type fileBlock struct {
	Path  string
	User  string
	Lines []store.EntryRow
}

// entryItem is either a standalone entry or a multi-line file block.
type entryItem struct {
	Single *store.EntryRow
	Block  *fileBlock
}

type entryGroup struct {
	Title string
	Items []entryItem
}

var sourceOrder = []string{
	model.SourceUserCrontab, model.SourceSystemCrontab, model.SourceCronD,
	model.SourceCronHourly, model.SourceCronDaily, model.SourceCronWeekly,
	model.SourceCronMonthly, model.SourceSystemdTimer,
}

// groupEntries groups entries by source (keeping sourceOrder), and within a
// source folds consecutive lines of the same file into one fileBlock.
func groupEntries(entries []store.EntryRow) []entryGroup {
	bySource := map[string][]store.EntryRow{}
	var others []string
	for _, e := range entries {
		if _, ok := bySource[e.Source]; !ok {
			others = append(others, e.Source)
			bySource[e.Source] = []store.EntryRow{e}
			continue
		}
		bySource[e.Source] = append(bySource[e.Source], e)
	}
	var groups []entryGroup
	seen := map[string]bool{}
	appendGroup := func(src string) {
		groups = append(groups, entryGroup{Title: sourceTitle(src), Items: foldFiles(bySource[src])})
		seen[src] = true
	}
	for _, src := range sourceOrder {
		if _, ok := bySource[src]; ok {
			appendGroup(src)
		}
	}
	for _, src := range others {
		if !seen[src] {
			appendGroup(src)
		}
	}
	return groups
}

// foldFiles folds runs of consecutive entries sharing the same non-empty
// path into a single fileBlock; single-line files stay standalone entries.
// Entries must be ordered by path, line_no (as returned by ListEntries).
func foldFiles(entries []store.EntryRow) []entryItem {
	var items []entryItem
	for i := 0; i < len(entries); {
		e := entries[i]
		if e.Path == "" {
			items = append(items, entryItem{Single: &entries[i]})
			i++
			continue
		}
		j := i + 1
		for j < len(entries) && entries[j].Path == e.Path {
			j++
		}
		if j-i == 1 {
			items = append(items, entryItem{Single: &entries[i]})
		} else {
			lines := make([]store.EntryRow, j-i)
			copy(lines, entries[i:j])
			items = append(items, entryItem{Block: &fileBlock{Path: e.Path, User: e.User, Lines: lines}})
		}
		i = j
	}
	return items
}

func (s *Server) loadAgent(w http.ResponseWriter, r *http.Request) (store.AgentRow, bool) {
	id, ok := s.agentID(w, r)
	if !ok {
		return store.AgentRow{}, false
	}
	a, err := s.st.GetAgent(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return store.AgentRow{}, false
	}
	return a, true
}

func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	a, ok := s.loadAgent(w, r)
	if !ok {
		return
	}
	filter := entryFilterFromQuery(r)
	entries, err := s.st.ListEntries(r.Context(), a.ID, filter)
	if err != nil {
		s.log.Error("list entries", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, "agent", map[string]any{
		"Agent":   a,
		"Groups":  groupEntries(entries),
		"Filter":  filter,
		"Sources": sourceOrder,
		"Query":   "",
		"Now":     time.Now().UTC(),
		"Loc":     loadLoc(a.Timezone),
	})
}

// SnapshotDiff describes changes between two consecutive snapshots.
type SnapshotDiff struct {
	CollectedAt time.Time
	Added       []model.CronEntry
	Removed     []model.CronEntry
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	a, ok := s.loadAgent(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 10
	}
	snaps, err := s.st.ListSnapshots(r.Context(), a.ID, limit)
	if err != nil {
		s.log.Error("list snapshots", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// snaps are newest-first; diff each snapshot against the previous (older) one.
	var diffs []SnapshotDiff
	for i := range snaps {
		var cur, prev *model.AgentReport
		if err := json.Unmarshal([]byte(snaps[i].Payload), &cur); err != nil {
			continue
		}
		if i+1 < len(snaps) {
			if err := json.Unmarshal([]byte(snaps[i+1].Payload), &prev); err != nil {
				prev = nil
			}
		}
		d := SnapshotDiff{CollectedAt: snaps[i].CollectedAt}
		if prev != nil {
			d.Added, d.Removed = DiffReports(prev, cur)
		}
		diffs = append(diffs, d)
	}
	s.render(w, "history", map[string]any{
		"Agent": a,
		"Diffs": diffs,
		"Query": "",
		"Now":   time.Now().UTC(),
		"Loc":   loadLoc(a.Timezone),
	})
}

type searchResult struct {
	store.SearchRow
	Highlighted template.HTML
}

func highlight(raw, q string) template.HTML {
	esc := html.EscapeString(raw)
	needle := html.EscapeString(q)
	if needle == "" {
		return template.HTML(esc)
	}
	lower := strings.ToLower(esc)
	lneedle := strings.ToLower(needle)
	var b strings.Builder
	i := 0
	for {
		j := strings.Index(lower[i:], lneedle)
		if j < 0 {
			b.WriteString(esc[i:])
			break
		}
		b.WriteString(esc[i : i+j])
		b.WriteString("<mark>")
		b.WriteString(esc[i+j : i+j+len(needle)])
		b.WriteString("</mark>")
		i += j + len(needle)
	}
	return template.HTML(b.String())
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var results []searchResult
	if q != "" {
		rows, err := s.st.Search(r.Context(), q)
		if err != nil {
			s.log.Error("search", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, row := range rows {
			results = append(results, searchResult{SearchRow: row, Highlighted: highlight(row.RawLine, q)})
		}
	}
	s.render(w, "search", map[string]any{
		"Query":   q,
		"Results": results,
		"Now":     time.Now().UTC(),
		"Loc":     time.UTC,
	})
}
