package agent

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/example/cron-checker/internal/model"
)

func TestParseCrontabContent(t *testing.T) {
	content := []byte("# comment\n" +
		"\n" +
		"*/5 * * * * /usr/local/bin/backup.sh\n" +
		"MAILTO=root\n" +
		"0 2 * * 1-5 /opt/job.sh --flag=a b\n")
	entries := ParseCrontabContent(model.SourceSystemCrontab, "/etc/crontab", "root", content)

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d: %+v", len(entries), entries)
	}

	// Comment and blank lines skipped, original line numbers preserved.
	if entries[0].LineNo != 3 {
		t.Errorf("entry 0 line_no = %d, want 3", entries[0].LineNo)
	}
	if entries[0].RawLine != "*/5 * * * * /usr/local/bin/backup.sh" {
		t.Errorf("entry 0 raw_line = %q", entries[0].RawLine)
	}
	if entries[0].Source != model.SourceSystemCrontab || entries[0].Path != "/etc/crontab" ||
		entries[0].User != "root" || !entries[0].Enabled {
		t.Errorf("entry 0 metadata wrong: %+v", entries[0])
	}

	if entries[1].LineNo != 4 || entries[1].RawLine != "MAILTO=root" {
		t.Errorf("entry 1 = %+v", entries[1])
	}
	// Raw line kept verbatim, fields not parsed.
	if entries[2].LineNo != 5 || entries[2].RawLine != "0 2 * * 1-5 /opt/job.sh --flag=a b" {
		t.Errorf("entry 2 = %+v", entries[2])
	}
	if entries[2].Schedule != "" {
		t.Errorf("schedule should stay empty for crontab lines, got %q", entries[2].Schedule)
	}
}

// fakeExec dispatches canned command outputs for tests.
func fakeExec(t *testing.T, responses map[string]fakeResp) func(string, ...string) ([]byte, error) {
	return func(name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		resp, ok := responses[key]
		if !ok {
			t.Fatalf("unexpected exec call: %q", key)
			return nil, nil
		}
		return resp.out, resp.err
	}
}

type fakeResp struct {
	out []byte
	err error
}

func TestCollect(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Per-user spool crontabs (Debian layout).
	write("spool/crontabs/alice", "# hi\n30 3 * * * /home/alice/bin/nightly\n")
	write("spool/crontabs/bob", "@daily /usr/bin/bob-job\n")
	// RHEL-style spool (regular file directly in /var/spool/cron).
	write("spool/rhel/carol", "0 1 * * * /opt/carol.sh\n")
	// System crontab.
	write("etc/crontab", "SHELL=/bin/sh\n17 * * * * root run-parts /etc/cron.hourly\n")
	// cron.d.
	write("etc/cron.d/backup", "# backup\n15 2 * * * root /usr/local/bin/backup --full\n")
	// Periodic dirs: file NAME is the entry.
	write("etc/cron.hourly/0anacron", "#!/bin/sh\ntrue\n")
	write("etc/cron.daily/logrotate", "#!/bin/sh\n/usr/sbin/logrotate /etc/logrotate.conf\n")
	write("etc/cron.weekly/man-db", "#!/bin/sh\nman-db rebuilt\n")
	write("etc/cron.monthly/apt", "#!/bin/sh\napt update\n")
	// systemd timer unit file.
	write("etc/systemd/system/backup.timer", "[Unit]\nDescription=backup\n\n[Timer]\nOnCalendar=daily\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n")
	write("etc/systemd/system/multi.timer", "[Timer]\nOnCalendar=hourly\nOnCalendar=*-*-* 02:00:00\n")
	// passwd for per-user crontab -l.
	write("etc/passwd", "root:x:0:0:root:/root:/bin/bash\nalice:x:1000:1000:alice:/home/alice:/bin/bash\ndave:x:1001:1001::/home/dave:/bin/bash\n")

	systemdJSON := []byte(`[
		{"unit":"backup.timer","next":"Mon 2026-01-01 00:00:00 MSK","last":"-","activates":"backup.service"},
		{"unit":"multi.timer","next":"n/a","last":"n/a","activates":"multi.service"},
		{"unit":"nonexistent.timer","next":"n/a","last":"n/a","activates":"nonexistent.service"}
	]`)

	execFn := fakeExec(t, map[string]fakeResp{
		"systemctl list-timers --all --no-pager --output=json": {out: systemdJSON},
		"crontab -l -u alice": {out: []byte("0 6 * * * /home/alice/bin/morning\n")},
		"crontab -l -u dave":  {err: errors.New("exit status 1: no crontab for dave")},
		"crontab -l -u root":  {err: errors.New("exit status 1: no crontab for root")},
	})

	cfg := Config{
		SpoolCronCrontabs: filepath.Join(root, "spool/crontabs"),
		SpoolCron:         filepath.Join(root, "spool/rhel"),
		EtcCrontab:        filepath.Join(root, "etc/crontab"),
		CronD:             filepath.Join(root, "etc/cron.d"),
		CronHourly:        filepath.Join(root, "etc/cron.hourly"),
		CronDaily:         filepath.Join(root, "etc/cron.daily"),
		CronWeekly:        filepath.Join(root, "etc/cron.weekly"),
		CronMonthly:       filepath.Join(root, "etc/cron.monthly"),
		SystemdDirs:       []string{filepath.Join(root, "etc/systemd/system"), filepath.Join(root, "usr/lib/systemd/system")},
		PasswdPath:        filepath.Join(root, "etc/passwd"),
		ExecFunc:          execFn,
	}

	entries, err := Collect(cfg)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	byKey := map[string]model.CronEntry{}
	for _, e := range entries {
		key := e.Source + "|" + e.Path + "|" + e.RawLine
		if e.Source == model.SourceSystemdTimer {
			key += "|" + e.Schedule
		}
		if _, dup := byKey[key]; dup {
			t.Errorf("duplicate entry %q", key)
		}
		byKey[key] = e
	}

	// Spool crontab (Debian).
	e := byKey[model.SourceUserCrontab+"|"+filepath.Join(root, "spool/crontabs/alice")+"|30 3 * * * /home/alice/bin/nightly"]
	if e.User != "alice" || e.LineNo != 2 || !e.Enabled {
		t.Errorf("spool alice entry wrong: %+v", e)
	}
	// Spool crontab (RHEL).
	e = byKey[model.SourceUserCrontab+"|"+filepath.Join(root, "spool/rhel/carol")+"|0 1 * * * /opt/carol.sh"]
	if e.User != "carol" || e.LineNo != 1 {
		t.Errorf("spool carol entry wrong: %+v", e)
	}
	// System crontab: env line kept, cron line kept with original line numbers.
	e = byKey[model.SourceSystemCrontab+"|"+filepath.Join(root, "etc/crontab")+"|17 * * * * root run-parts /etc/cron.hourly"]
	if e.User != "root" || e.LineNo != 2 {
		t.Errorf("etc/crontab entry wrong: %+v", e)
	}
	// cron.d.
	e = byKey[model.SourceCronD+"|"+filepath.Join(root, "etc/cron.d/backup")+"|15 2 * * * root /usr/local/bin/backup --full"]
	if e.User != "root" || e.LineNo != 2 {
		t.Errorf("cron.d entry wrong: %+v", e)
	}
	// Periodic dirs: raw_line is the file NAME, line_no 0, user = owner ("" on non-Linux).
	e = byKey[model.SourceCronDaily+"|"+filepath.Join(root, "etc/cron.daily/logrotate")+"|logrotate"]
	if e.LineNo != 0 || !e.Enabled {
		t.Errorf("cron.daily entry wrong: %+v", e)
	}
	if runtime.GOOS == "linux" && e.User == "" {
		t.Errorf("cron.daily owner should resolve on linux: %+v", e)
	}
	e = byKey[model.SourceCronHourly+"|"+filepath.Join(root, "etc/cron.hourly/0anacron")+"|0anacron"]
	if e.Source != model.SourceCronHourly || e.RawLine != "0anacron" {
		t.Errorf("cron.hourly entry wrong: %+v", e)
	}
	e = byKey[model.SourceCronWeekly+"|"+filepath.Join(root, "etc/cron.weekly/man-db")+"|man-db"]
	if e.Source != model.SourceCronWeekly {
		t.Errorf("cron.weekly entry wrong: %+v", e)
	}
	e = byKey[model.SourceCronMonthly+"|"+filepath.Join(root, "etc/cron.monthly/apt")+"|apt"]
	if e.Source != model.SourceCronMonthly {
		t.Errorf("cron.monthly entry wrong: %+v", e)
	}
	// systemd timer: OnCalendar becomes schedule, raw_line is unit name.
	e = byKey[model.SourceSystemdTimer+"|"+filepath.Join(root, "etc/systemd/system/backup.timer")+"|backup.timer|daily"]
	if e.Schedule != "daily" || e.LineNo != 0 || !e.Enabled {
		t.Errorf("backup.timer entry wrong: %+v", e)
	}
	// Multiple OnCalendar lines produce multiple entries.
	multiKey := model.SourceSystemdTimer + "|" + filepath.Join(root, "etc/systemd/system/multi.timer") + "|multi.timer"
	e = byKey[multiKey+"|hourly"]
	if e.Schedule != "hourly" {
		e = byKey[multiKey+"|*-*-* 02:00:00"]
	}
	if e.Schedule != "hourly" && e.Schedule != "*-*-* 02:00:00" {
		t.Errorf("multi.timer entry wrong: %+v", e)
	}
	e = byKey[model.SourceSystemdTimer+"||nonexistent.timer"]
	if e.Path != "" || e.Schedule != "" {
		t.Errorf("nonexistent.timer entry wrong: %+v", e)
	}
	// Per-user crontab via `crontab -l`.
	e = byKey[model.SourceUserCrontab+"||0 6 * * * /home/alice/bin/morning"]
	if e.User != "alice" || e.Path != "" || e.LineNo != 1 {
		t.Errorf("crontab -l alice entry wrong: %+v", e)
	}

	// "no crontab for dave/root" must not appear.
	for key := range byKey {
		if strings.Contains(key, "|0 crontab") || strings.Contains(key, "dave") || strings.Contains(key, "root|no crontab") {
			t.Errorf("unexpected entry for user without crontab: %q", key)
		}
	}

	if len(entries) < 12 {
		t.Errorf("expected at least 12 entries, got %d", len(entries))
	}
}

func TestCollectMissingSystemctl(t *testing.T) {
	cfg := Config{
		SystemdDirs: []string{filepath.Join(t.TempDir(), "none")},
		PasswdPath:  filepath.Join(t.TempDir(), "no-passwd"),
		ExecFunc: func(name string, args ...string) ([]byte, error) {
			return nil, errors.New("exec: \"systemctl\": executable file not found in $PATH")
		},
	}
	entries, err := Collect(cfg)
	if err != nil {
		t.Fatalf("Collect with missing systemctl must not fail: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no entries, got %d", len(entries))
	}
}

func TestPasswdUsersSkipsMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "passwd")
	content := "# comment\n" +
		"root:x:0:0:root:/root:/bin/bash\n" +
		"brokenline\n" +
		"baduid:x:abc:1::/:/bin/sh\n" +
		"alice:x:1000:1000:a:/home/a:/bin/sh\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	users, err := passwdUsers(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0] != "alice" || users[1] != "root" {
		t.Errorf("passwdUsers = %v", users)
	}
}
