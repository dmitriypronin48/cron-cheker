package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.yaml")
	content := `
listen: ":9999"
db_path: /tmp/x.db
token: secret
poll_interval: 60s
poll_timeout: 5s
poll_concurrency: 3
agent_port: "9998"
snapshot_retention_days: 2
removed_retention_hours: 5
log_level: debug
agents:
  - hostname: web1
    ip: 192.168.0.10
  - hostname: db1
    ip: 192.168.0.20
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen != ":9999" || cfg.DBPath != "/tmp/x.db" || cfg.Token != "secret" {
		t.Errorf("basic fields: %+v", cfg)
	}
	if cfg.PollInterval != time.Minute || cfg.PollTimeout != 5*time.Second {
		t.Errorf("durations: %v / %v", cfg.PollInterval, cfg.PollTimeout)
	}
	if cfg.PollConcurrency != 3 || cfg.AgentPort != "9998" ||
		cfg.SnapshotRetentionDays != 2 || cfg.RemovedRetentionHours != 5 ||
		cfg.LogLevel != "debug" {
		t.Errorf("numeric fields: %+v", cfg)
	}
	if len(cfg.Agents) != 2 || cfg.Agents[0].Hostname != "web1" || cfg.Agents[1].IP != "192.168.0.20" {
		t.Errorf("agents: %+v", cfg.Agents)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.yaml")
	if err := os.WriteFile(path, []byte("token: s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen != ":8090" || cfg.DBPath != "/data/cron-checker.db" ||
		cfg.PollInterval != 300*time.Second || cfg.PollConcurrency != 10 ||
		cfg.PollTimeout != 15*time.Second || cfg.AgentPort != "9300" ||
		cfg.SnapshotRetentionDays != 7 || cfg.RemovedRetentionHours != 72 ||
		cfg.LogLevel != "info" {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if len(cfg.Agents) != 0 {
		t.Errorf("agents should be empty: %+v", cfg.Agents)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	dir := t.TempDir()

	// Missing file.
	if _, err := LoadConfig(filepath.Join(dir, "nope.yaml")); err == nil {
		t.Fatal("missing file must fail")
	}

	// Token is required.
	p1 := filepath.Join(dir, "no-token.yaml")
	if err := os.WriteFile(p1, []byte("listen: :8090\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p1); err == nil {
		t.Fatal("missing token must fail")
	}

	// Broken duration.
	p2 := filepath.Join(dir, "bad-dur.yaml")
	if err := os.WriteFile(p2, []byte("token: x\npoll_interval: soon\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p2); err == nil {
		t.Fatal("bad duration must fail")
	}

	// Broken YAML.
	p3 := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(p3, []byte("token: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p3); err == nil {
		t.Fatal("broken yaml must fail")
	}
}
