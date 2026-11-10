// Package model contains shared types used by both the agent and the server.
package model

import "time"

// AgentVersion is the current version of the agent binary.
const AgentVersion = "0.1.0"

// Sources of cron entries.
const (
	SourceUserCrontab   = "user_crontab"
	SourceSystemCrontab = "system_crontab"
	SourceCronD         = "cron_d"
	SourceCronHourly    = "cron_hourly"
	SourceCronDaily     = "cron_daily"
	SourceCronWeekly    = "cron_weekly"
	SourceCronMonthly   = "cron_monthly"
	SourceSystemdTimer  = "systemd_timer"
)

// Entry statuses stored on the server.
const (
	EntryStatusActive  = "active"
	EntryStatusRemoved = "removed"
	EntryStatusAcked   = "acked"
)

// Agent statuses stored on the server.
const (
	AgentStatusOnline  = "online"
	AgentStatusOffline = "offline"
)

// CronEntry is a single discovered cron line (or systemd timer).
type CronEntry struct {
	Source   string `json:"source"`
	Path     string `json:"path"`
	User     string `json:"user"`
	RawLine  string `json:"raw_line"`
	LineNo   int    `json:"line_no"`
	Schedule string `json:"schedule"`
	Enabled  bool   `json:"enabled"`
}

// AgentReport is the payload returned by GET /api/v1/cron on the agent.
type AgentReport struct {
	AgentVersion string      `json:"agent_version"`
	Hostname     string      `json:"hostname"`
	IP           string      `json:"ip"`
	CollectedAt  time.Time   `json:"collected_at"`
	Timezone     string      `json:"timezone"`
	Entries      []CronEntry `json:"entries"`
}
