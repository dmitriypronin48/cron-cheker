// Package store provides SQLite persistence for the cron-checker server.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/example/cron-checker/internal/model"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS agents (
    id INTEGER PRIMARY KEY,
    hostname TEXT NOT NULL,
    ip TEXT NOT NULL,
    agent_version TEXT,
    timezone TEXT,
    last_seen_at TIMESTAMP,
    status TEXT CHECK(status IN ('online','offline')) NOT NULL DEFAULT 'offline',
    first_seen_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE(hostname, ip)
);
CREATE TABLE IF NOT EXISTS cron_entries (
    id INTEGER PRIMARY KEY,
    agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    source TEXT,
    path TEXT,
    user TEXT,
    raw_line TEXT,
    line_no INTEGER,
    schedule TEXT,
    enabled BOOLEAN,
    first_seen_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    last_seen_at TIMESTAMP NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    status TEXT CHECK(status IN ('active','removed','acked')) NOT NULL DEFAULT 'active',
    acked_at TIMESTAMP NULL,
    fingerprint TEXT NOT NULL,
    UNIQUE(agent_id, fingerprint)
);
CREATE TABLE IF NOT EXISTS cron_snapshots (
    id INTEGER PRIMARY KEY,
    agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    collected_at TIMESTAMP NOT NULL,
    payload TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_entries_agent ON cron_entries(agent_id);
CREATE INDEX IF NOT EXISTS idx_entries_agent_status ON cron_entries(agent_id, status);
CREATE INDEX IF NOT EXISTS idx_snapshots_agent ON cron_snapshots(agent_id, collected_at);
`

// Fingerprint returns a stable identifier for a cron entry on a given agent.
func Fingerprint(e model.CronEntry) string {
	h := sha256.Sum256([]byte(e.Source + "\x00" + e.Path + "\x00" + e.User + "\x00" + e.RawLine + "\x00" + strconv.Itoa(e.LineNo)))
	return hex.EncodeToString(h[:])
}

// AgentTarget is one agent entry from the server config file (inventory).
type AgentTarget struct {
	Hostname string `yaml:"hostname"`
	IP       string `yaml:"ip"`
}

// AgentRow is a single agent with computed entry counts.
type AgentRow struct {
	ID           int64
	Hostname     string
	IP           string
	AgentVersion string
	Timezone     string
	LastSeenAt   time.Time
	Status       string
	FirstSeenAt  time.Time
	EntriesCount int
	NewCount     int
}

// EntryRow is a single cron entry with its DB id and timestamps.
type EntryRow struct {
	ID          int64
	AgentID     int64
	Source      string
	Path        string
	User        string
	RawLine     string
	LineNo      int
	Schedule    string
	Enabled     bool
	FirstSeenAt time.Time
	LastSeenAt  time.Time
	Status      string
	AckedAt     *time.Time
	Fingerprint string
}

// EntryFilter filters ListEntries results. Zero values are ignored.
type EntryFilter struct {
	Source  string
	User    string
	OnlyNew bool
}

// SearchRow is a cron entry joined with its agent.
type SearchRow struct {
	EntryRow
	Hostname string
	IP       string
}

// SnapshotRow is a stored agent report payload.
type SnapshotRow struct {
	ID          int64
	AgentID     int64
	CollectedAt time.Time
	Payload     string
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if necessary) the SQLite database at path and runs migrations.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// AddAgent registers a new agent and returns its id. Existing (hostname, ip) returns the existing id.
func (s *Store) AddAgent(ctx context.Context, hostname, ip string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO agents(hostname, ip) VALUES(?, ?) ON CONFLICT(hostname, ip) DO NOTHING`, hostname, ip)
	if err != nil {
		return 0, fmt.Errorf("add agent: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("add agent last insert id: %w", err)
	}
	if id == 0 {
		row := s.db.QueryRowContext(ctx, `SELECT id FROM agents WHERE hostname = ? AND ip = ?`, hostname, ip)
		if err := row.Scan(&id); err != nil {
			return 0, fmt.Errorf("add agent lookup: %w", err)
		}
	}
	return id, nil
}

const agentSelect = `SELECT a.id, a.hostname, a.ip, a.agent_version, a.timezone, a.last_seen_at,
    a.status, a.first_seen_at,
    (SELECT COUNT(*) FROM cron_entries e WHERE e.agent_id = a.id),
    (SELECT COUNT(*) FROM cron_entries e WHERE e.agent_id = a.id
        AND e.first_seen_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-24 hours')
        AND e.status = 'active' AND e.acked_at IS NULL)
FROM agents a`

func scanAgent(row interface{ Scan(...any) error }) (AgentRow, error) {
	var a AgentRow
	var lastSeen, firstSeen, version, tz sql.NullString
	err := row.Scan(&a.ID, &a.Hostname, &a.IP, &version, &tz, &lastSeen,
		&a.Status, &firstSeen, &a.EntriesCount, &a.NewCount)
	if err != nil {
		return a, err
	}
	a.AgentVersion = version.String
	a.Timezone = tz.String
	a.LastSeenAt = parseDBTime(lastSeen.String, lastSeen.Valid)
	a.FirstSeenAt = parseDBTime(firstSeen.String, firstSeen.Valid)
	return a, nil
}

// GetAgent returns a single agent by id.
func (s *Store) GetAgent(ctx context.Context, id int64) (AgentRow, error) {
	row := s.db.QueryRowContext(ctx, agentSelect+` WHERE a.id = ?`, id)
	a, err := scanAgent(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return a, fmt.Errorf("agent %d: %w", id, sql.ErrNoRows)
		}
		return a, fmt.Errorf("get agent: %w", err)
	}
	return a, nil
}

// ListAgents returns all agents ordered by hostname.
func (s *Store) ListAgents(ctx context.Context) ([]AgentRow, error) {
	rows, err := s.db.QueryContext(ctx, agentSelect+` ORDER BY a.hostname`)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer rows.Close()
	var out []AgentRow
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, fmt.Errorf("list agents scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpsertReport stores an agent report: updates agent info, upserts entries,
// marks missing active entries as removed, and stores the raw snapshot.
func (s *Store) UpsertReport(ctx context.Context, agentID int64, report *model.AgentReport) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("upsert report begin: %w", err)
	}
	defer tx.Rollback()

	now := report.CollectedAt.UTC().Format(time.RFC3339)
	_, err = tx.ExecContext(ctx,
		`UPDATE agents SET agent_version = ?, timezone = ?, last_seen_at = ?, status = 'online' WHERE id = ?`,
		report.AgentVersion, report.Timezone, now, agentID)
	if err != nil {
		return fmt.Errorf("upsert report agent: %w", err)
	}

	seen := make(map[string]bool, len(report.Entries))
	for _, e := range report.Entries {
		fp := Fingerprint(e)
		seen[fp] = true
		enabled := 0
		if e.Enabled {
			enabled = 1
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO cron_entries(agent_id, source, path, user, raw_line, line_no, schedule, enabled, fingerprint, last_seen_at)
             VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
             ON CONFLICT(agent_id, fingerprint) DO UPDATE SET
                 last_seen_at = excluded.last_seen_at,
                 status = CASE WHEN cron_entries.status = 'removed' THEN 'active' ELSE cron_entries.status END,
                 schedule = excluded.schedule,
                 enabled = excluded.enabled`,
			agentID, e.Source, e.Path, e.User, e.RawLine, e.LineNo, e.Schedule, enabled, fp, now)
		if err != nil {
			return fmt.Errorf("upsert entry: %w", err)
		}
	}

	// Mark active entries not present in this report as removed.
	missing, err := s.missingFingerprints(ctx, tx, agentID, seen)
	if err != nil {
		return err
	}
	for _, fp := range missing {
		if _, err := tx.ExecContext(ctx,
			`UPDATE cron_entries SET status = 'removed', last_seen_at = ? WHERE agent_id = ? AND fingerprint = ?`,
			now, agentID, fp); err != nil {
			return fmt.Errorf("mark entry removed: %w", err)
		}
	}

	payload, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO cron_snapshots(agent_id, collected_at, payload) VALUES(?, ?, ?)`,
		agentID, now, string(payload)); err != nil {
		return fmt.Errorf("insert snapshot: %w", err)
	}
	return tx.Commit()
}

// missingFingerprints returns fingerprints of active entries for the agent not in seen.
func (s *Store) missingFingerprints(ctx context.Context, tx *sql.Tx, agentID int64, seen map[string]bool) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT fingerprint FROM cron_entries WHERE agent_id = ? AND status = 'active'`, agentID)
	if err != nil {
		return nil, fmt.Errorf("list active fingerprints: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			return nil, fmt.Errorf("scan fingerprint: %w", err)
		}
		if !seen[fp] {
			out = append(out, fp)
		}
	}
	return out, rows.Err()
}

// MarkAgentOffline sets the agent status to offline.
func (s *Store) MarkAgentOffline(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET status = 'offline' WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("mark agent offline: %w", err)
	}
	return nil
}

// DeleteAgent removes an agent with all its entries and snapshots
// (foreign keys are ON DELETE CASCADE).
func (s *Store) DeleteAgent(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete agent: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("delete agent %d: %w", id, sql.ErrNoRows)
	}
	return nil
}

// SyncAgents reconciles the agents table with the desired inventory from the
// config file: missing agents are added, agents absent from the inventory are
// deleted (together with their entries and snapshots). Returns the numbers of
// added and removed agents.
func (s *Store) SyncAgents(ctx context.Context, targets []AgentTarget) (added, removed int, err error) {
	desired := make(map[string]bool, len(targets))
	for _, t := range targets {
		hostname, ip := strings.TrimSpace(t.Hostname), strings.TrimSpace(t.IP)
		if hostname == "" || ip == "" {
			return 0, 0, fmt.Errorf("agent inventory: hostname and ip are required (got %q, %q)", t.Hostname, t.IP)
		}
		desired[hostname+"\x00"+ip] = true
		res, err := s.db.ExecContext(ctx,
			`INSERT INTO agents(hostname, ip) VALUES(?, ?) ON CONFLICT(hostname, ip) DO NOTHING`, hostname, ip)
		if err != nil {
			return 0, 0, fmt.Errorf("sync add agent: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}

	current, err := s.ListAgents(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, a := range current {
		if desired[a.Hostname+"\x00"+a.IP] {
			continue
		}
		if err := s.DeleteAgent(ctx, a.ID); err != nil {
			return added, removed, err
		}
		removed++
	}
	return added, removed, nil
}

// ListNewEntries returns entries first seen within the last 24h that are still
// active and unacknowledged, joined with the agent's hostname and ip. Used by
// the cron_checker_new_entry metric.
func (s *Store) ListNewEntries(ctx context.Context) ([]SearchRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+entryColumns+`, a.hostname, a.ip
        FROM cron_entries e JOIN agents a ON a.id = e.agent_id
        WHERE e.first_seen_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-24 hours')
          AND e.status = 'active' AND e.acked_at IS NULL
        ORDER BY a.hostname, e.source, e.path, e.line_no`)
	if err != nil {
		return nil, fmt.Errorf("list new entries: %w", err)
	}
	defer rows.Close()
	var out []SearchRow
	for rows.Next() {
		var r SearchRow
		if err := scanSearchRow(rows, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListEntries returns entries for an agent, optionally filtered.
func (s *Store) ListEntries(ctx context.Context, agentID int64, f EntryFilter) ([]EntryRow, error) {
	q := `SELECT id, agent_id, source, path, user, raw_line, line_no, schedule, enabled,
        first_seen_at, last_seen_at, status, acked_at, fingerprint
        FROM cron_entries WHERE agent_id = ?`
	var args []any
	args = append(args, agentID)
	if f.Source != "" {
		q += ` AND source = ?`
		args = append(args, f.Source)
	}
	if f.User != "" {
		q += ` AND user = ?`
		args = append(args, f.User)
	}
	if f.OnlyNew {
		q += ` AND first_seen_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-24 hours')
               AND status = 'active' AND acked_at IS NULL`
	}
	q += ` ORDER BY source, path, line_no`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}
	defer rows.Close()
	var out []EntryRow
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

const entryColumns = `e.id, e.agent_id, e.source, e.path, e.user, e.raw_line, e.line_no, e.schedule, e.enabled,
    e.first_seen_at, e.last_seen_at, e.status, e.acked_at, e.fingerprint`

const entrySelect = `SELECT ` + entryColumns + ` FROM cron_entries e`

func scanEntry(row interface{ Scan(...any) error }) (EntryRow, error) {
	var e EntryRow
	var firstSeen, lastSeen sql.NullString
	var ackedAt sql.NullString
	var enabled int
	err := row.Scan(&e.ID, &e.AgentID, &e.Source, &e.Path, &e.User, &e.RawLine, &e.LineNo,
		&e.Schedule, &enabled, &firstSeen, &lastSeen, &e.Status, &ackedAt, &e.Fingerprint)
	if err != nil {
		return e, fmt.Errorf("scan entry: %w", err)
	}
	e.Enabled = enabled != 0
	e.FirstSeenAt = parseDBTime(firstSeen.String, firstSeen.Valid)
	e.LastSeenAt = parseDBTime(lastSeen.String, lastSeen.Valid)
	if ackedAt.Valid {
		t := parseDBTime(ackedAt.String, true)
		e.AckedAt = &t
	}
	return e, nil
}

// GetEntry returns a single entry by id.
func (s *Store) GetEntry(ctx context.Context, id int64) (EntryRow, error) {
	row := s.db.QueryRowContext(ctx, entrySelect+` WHERE e.id = ?`, id)
	e, err := scanEntry(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return e, fmt.Errorf("entry %d: %w", id, sql.ErrNoRows)
		}
		return e, fmt.Errorf("get entry: %w", err)
	}
	return e, nil
}

// AckEntry marks an entry as acknowledged (clears the NEW badge).
func (s *Store) AckEntry(ctx context.Context, id int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`UPDATE cron_entries SET acked_at = ?, status = 'acked' WHERE id = ? AND status = 'active'`, now, id)
	if err != nil {
		return fmt.Errorf("ack entry: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("ack entry rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("ack entry %d: %w", id, sql.ErrNoRows)
	}
	return nil
}

// AckEntries marks all active entries among the given ids as acknowledged.
// Returns the number of entries actually acked.
func (s *Store) AckEntries(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	q := `UPDATE cron_entries SET acked_at = ?, status = 'acked' WHERE status = 'active' AND id IN (`
	args := []any{time.Now().UTC().Format(time.RFC3339)}
	for i, id := range ids {
		if i > 0 {
			q += ","
		}
		q += "?"
		args = append(args, id)
	}
	q += ")"
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("ack entries: %w", err)
	}
	return res.RowsAffected()
}

// Search finds entries by raw_line substring across all agents.
func (s *Store) Search(ctx context.Context, q string) ([]SearchRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+entryColumns+`, a.hostname, a.ip
        FROM cron_entries e JOIN agents a ON a.id = e.agent_id
        WHERE e.raw_line LIKE ? ESCAPE '\'
        ORDER BY a.hostname, e.source LIMIT 200`, "%"+escapeLike(q)+"%")
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()
	var out []SearchRow
	for rows.Next() {
		var r SearchRow
		if err := scanSearchRow(rows, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanSearchRow(rows *sql.Rows, r *SearchRow) error {
	var firstSeen, lastSeen, ackedAt sql.NullString
	var enabled int
	err := rows.Scan(&r.ID, &r.AgentID, &r.Source, &r.Path, &r.User, &r.RawLine, &r.LineNo,
		&r.Schedule, &enabled, &firstSeen, &lastSeen, &r.Status, &ackedAt, &r.Fingerprint,
		&r.Hostname, &r.IP)
	if err != nil {
		return fmt.Errorf("scan search row: %w", err)
	}
	r.Enabled = enabled != 0
	r.FirstSeenAt = parseDBTime(firstSeen.String, firstSeen.Valid)
	r.LastSeenAt = parseDBTime(lastSeen.String, lastSeen.Valid)
	if ackedAt.Valid {
		t := parseDBTime(ackedAt.String, true)
		r.AckedAt = &t
	}
	return nil
}

func escapeLike(s string) string {
	r := ""
	for _, c := range s {
		switch c {
		case '\\', '%', '_':
			r += "\\"
		}
		r += string(c)
	}
	return r
}

// ListSnapshots returns the most recent snapshots for an agent.
func (s *Store) ListSnapshots(ctx context.Context, agentID int64, limit int) ([]SnapshotRow, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, agent_id, collected_at, payload FROM cron_snapshots
         WHERE agent_id = ? ORDER BY collected_at DESC LIMIT ?`, agentID, limit)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	defer rows.Close()
	var out []SnapshotRow
	for rows.Next() {
		var sn SnapshotRow
		var collected sql.NullString
		if err := rows.Scan(&sn.ID, &sn.AgentID, &collected, &sn.Payload); err != nil {
			return nil, fmt.Errorf("scan snapshot: %w", err)
		}
		sn.CollectedAt = parseDBTime(collected.String, collected.Valid)
		out = append(out, sn)
	}
	return out, rows.Err()
}

// Cleanup deletes old snapshots and long-removed entries.
func (s *Store) Cleanup(ctx context.Context, snapshotRetentionDays, removedRetentionHours int) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM cron_snapshots WHERE collected_at < strftime('%Y-%m-%dT%H:%M:%fZ','now', ?)`,
		fmt.Sprintf("-%d days", snapshotRetentionDays)); err != nil {
		return fmt.Errorf("cleanup snapshots: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM cron_entries WHERE status = 'removed' AND last_seen_at < strftime('%Y-%m-%dT%H:%M:%fZ','now', ?)`,
		fmt.Sprintf("-%d hours", removedRetentionHours)); err != nil {
		return fmt.Errorf("cleanup entries: %w", err)
	}
	return nil
}

// AgentsByStatus returns counts of online and offline agents.
func (s *Store) AgentsByStatus(ctx context.Context) (online, offline int, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FILTER (WHERE status = 'online'), COUNT(*) FILTER (WHERE status = 'offline') FROM agents`).Scan(&online, &offline)
	if err != nil {
		return 0, 0, fmt.Errorf("agents by status: %w", err)
	}
	return online, offline, nil
}

// parseDBTime parses SQLite timestamps; returns zero time on failure.
func parseDBTime(v string, valid bool) time.Time {
	if !valid || v == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse("2006-01-02 15:04:05.999999999", v); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse("2006-01-02T15:04:05.999999999", v); err == nil {
		return t.UTC()
	}
	return time.Time{}
}
