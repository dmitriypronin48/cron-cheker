// Package agent implements the cron-checker agent: a passive HTTP server
// that collects cron entries from the local Linux host and exposes them
// over an authenticated API.
package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/example/cron-checker/internal/model"
)

// Config tells the collector where to look for cron data.
// All filesystem roots are injectable so tests can point them at a temp dir.
type Config struct {
	// SpoolCronCrontabs is the Debian/Ubuntu per-user crontab dir (/var/spool/cron/crontabs).
	SpoolCronCrontabs string
	// SpoolCron is the RHEL-style per-user crontab dir (/var/spool/cron).
	SpoolCron string
	// EtcCrontab is the system crontab file (/etc/crontab).
	EtcCrontab string
	// CronD is the cron.d directory (/etc/cron.d).
	CronD string
	// CronHourly/CronDaily/CronWeekly/CronMonthly are the periodic cron dirs.
	CronHourly  string
	CronDaily   string
	CronWeekly  string
	CronMonthly string
	// SystemdDirs are searched (in order) for *.timer unit files.
	SystemdDirs []string
	// PasswdPath is the passwd file used to enumerate users for crontab -l.
	PasswdPath string
	// ExecFunc runs external commands; injectable for tests.
	// Signature matches os/exec.Command(...).Output().
	ExecFunc func(name string, args ...string) ([]byte, error)
}

// DefaultConfig returns a Config with the real Linux paths.
func DefaultConfig() Config {
	return Config{
		SpoolCronCrontabs: "/var/spool/cron/crontabs",
		SpoolCron:         "/var/spool/cron",
		EtcCrontab:        "/etc/crontab",
		CronD:             "/etc/cron.d",
		CronHourly:        "/etc/cron.hourly",
		CronDaily:         "/etc/cron.daily",
		CronWeekly:        "/etc/cron.weekly",
		CronMonthly:       "/etc/cron.monthly",
		SystemdDirs:       []string{"/etc/systemd/system", "/usr/lib/systemd/system"},
		PasswdPath:        "/etc/passwd",
		ExecFunc:          defaultExecFunc,
	}
}

func defaultExecFunc(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// Collect gathers cron entries from all configured sources.
// Individual source failures (missing dirs, no systemctl, etc.) are skipped
// silently: a host without systemd timers or cron.d must still report.
func Collect(cfg Config) ([]model.CronEntry, error) {
	var entries []model.CronEntry
	entries = append(entries, collectSpoolDir(cfg.SpoolCronCrontabs, model.SourceUserCrontab, false)...)
	// /var/spool/cron may itself contain the crontabs subdir on Debian; skip it.
	entries = append(entries, collectSpoolDir(cfg.SpoolCron, model.SourceUserCrontab, true)...)
	entries = append(entries, collectCrontabFile(cfg.EtcCrontab, model.SourceSystemCrontab, "root")...)
	entries = append(entries, collectDirFiles(cfg.CronD, model.SourceCronD, "root")...)
	entries = append(entries, collectDirNames(cfg.CronHourly, model.SourceCronHourly)...)
	entries = append(entries, collectDirNames(cfg.CronDaily, model.SourceCronDaily)...)
	entries = append(entries, collectDirNames(cfg.CronWeekly, model.SourceCronWeekly)...)
	entries = append(entries, collectDirNames(cfg.CronMonthly, model.SourceCronMonthly)...)
	entries = append(entries, collectSystemdTimers(cfg)...)
	entries = append(entries, collectUserCrontabs(cfg)...)
	return entries, nil
}

// ParseCrontabContent parses crontab file content into entries.
// Comment lines and blank lines are skipped, but original 1-based line
// numbers are preserved. Raw lines are kept verbatim (not parsed into
// schedule fields).
func ParseCrontabContent(source, path, user string, content []byte) []model.CronEntry {
	var entries []model.CronEntry
	for i, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		entries = append(entries, model.CronEntry{
			Source:  source,
			Path:    path,
			User:    user,
			RawLine: line,
			LineNo:  i + 1,
			Enabled: true,
		})
	}
	return entries
}

// collectSpoolDir reads per-user crontab files from a spool directory.
// File names are the usernames. If skipCrontabs is set, a "crontabs"
// subdirectory is ignored (Debian nests it under /var/spool/cron).
func collectSpoolDir(dir, source string, skipCrontabs bool) []model.CronEntry {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var entries []model.CronEntry
	for _, f := range files {
		if f.IsDir() {
			if skipCrontabs && f.Name() == "crontabs" {
				continue
			}
			continue
		}
		path := filepath.Join(dir, f.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		entries = append(entries, ParseCrontabContent(source, path, f.Name(), content)...)
	}
	return entries
}

// collectCrontabFile parses a single crontab-format file.
func collectCrontabFile(path, source, user string) []model.CronEntry {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return ParseCrontabContent(source, path, user, content)
}

// collectDirFiles parses every regular file in dir as a crontab-format file.
func collectDirFiles(dir, source, user string) []model.CronEntry {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var entries []model.CronEntry
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		path := filepath.Join(dir, f.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		entries = append(entries, ParseCrontabContent(source, path, user, content)...)
	}
	return entries
}

// collectDirNames records one entry per executable file name in a periodic
// cron dir (cron.hourly etc.): raw_line is the file NAME only, line_no is 0,
// user is the file owner.
func collectDirNames(dir, source string) []model.CronEntry {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var entries []model.CronEntry
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		path := filepath.Join(dir, f.Name())
		owner := ""
		if info, err := os.Stat(path); err == nil {
			owner = fileOwner(info)
		}
		entries = append(entries, model.CronEntry{
			Source:  source,
			Path:    path,
			User:    owner,
			RawLine: f.Name(),
			LineNo:  0,
			Enabled: true,
		})
	}
	return entries
}

// timerRow mirrors one record of
// `systemctl list-timers --all --no-pager --output=json`.
type timerRow struct {
	Unit      string `json:"unit"`
	Next      string `json:"next"`
	Last      string `json:"last"`
	Activates string `json:"activates"`
	Active    string `json:"active"`
}

// collectSystemdTimers discovers systemd timer units. If systemctl is
// missing or fails, it returns an empty list without error.
func collectSystemdTimers(cfg Config) []model.CronEntry {
	if cfg.ExecFunc == nil {
		return nil
	}
	out, err := cfg.ExecFunc("systemctl", "list-timers", "--all", "--no-pager", "--output=json")
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		return nil
	}
	var rows []timerRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil
	}
	var entries []model.CronEntry
	for _, row := range rows {
		unit := row.Unit
		if !strings.HasSuffix(unit, ".timer") {
			continue
		}
		enabled := true
		if row.Active != "" {
			enabled = strings.EqualFold(row.Active, "active")
		}
		path, onCalendars, owner := findTimerUnit(cfg.SystemdDirs, unit)
		if owner == "" {
			owner = "root"
		}
		if len(onCalendars) == 0 {
			onCalendars = []string{""}
		}
		for _, schedule := range onCalendars {
			entries = append(entries, model.CronEntry{
				Source:   model.SourceSystemdTimer,
				Path:     path,
				User:     owner,
				RawLine:  unit,
				LineNo:   0,
				Schedule: schedule,
				Enabled:  enabled,
			})
		}
	}
	return entries
}

// findTimerUnit locates a *.timer unit file in the configured systemd dirs
// and extracts OnCalendar= values from its [Timer] section.
func findTimerUnit(dirs []string, unit string) (path string, onCalendars []string, owner string) {
	for _, dir := range dirs {
		candidate := filepath.Join(dir, unit)
		content, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		if info, err := os.Stat(candidate); err == nil {
			owner = fileOwner(info)
		}
		return candidate, parseOnCalendar(content), owner
	}
	return "", nil, ""
}

// parseOnCalendar extracts OnCalendar= values from the [Timer] section.
func parseOnCalendar(content []byte) []string {
	var result []string
	inTimer := false
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inTimer = strings.EqualFold(line, "[Timer]")
			continue
		}
		if !inTimer {
			continue
		}
		if name, value, ok := strings.Cut(line, "="); ok && strings.EqualFold(strings.TrimSpace(name), "OnCalendar") {
			result = append(result, strings.TrimSpace(value))
		}
	}
	return result
}

// collectUserCrontabs runs `crontab -l -u <user>` for every user from the
// passwd file. "no crontab" errors are ignored; other errors are skipped.
func collectUserCrontabs(cfg Config) []model.CronEntry {
	if cfg.ExecFunc == nil {
		return nil
	}
	users, err := passwdUsers(cfg.PasswdPath)
	if err != nil {
		return nil
	}
	var entries []model.CronEntry
	for _, user := range users {
		out, err := cfg.ExecFunc("crontab", "-l", "-u", user)
		if err != nil {
			var exitErr *exec.ExitError
			msg := err.Error()
			if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
				msg = string(exitErr.Stderr)
			}
			if !strings.Contains(strings.ToLower(msg), "no crontab") {
				continue
			}
			continue
		}
		entries = append(entries, ParseCrontabContent(model.SourceUserCrontab, "", user, out)...)
	}
	return entries
}

// passwdUsers parses /etc/passwd-style content and returns user names with
// UID >= 0, sorted for deterministic output.
func passwdUsers(path string) ([]string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var users []string
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 3 {
			continue
		}
		uid, err := strconv.Atoi(fields[2])
		if err != nil || uid < 0 {
			continue
		}
		users = append(users, fields[0])
	}
	sort.Strings(users)
	return users, nil
}
