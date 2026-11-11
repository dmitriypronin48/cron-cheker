package server

import (
	"fmt"
	"os"
	"time"

	"github.com/example/cron-checker/internal/store"
	"gopkg.in/yaml.v3"
)

// Config is the unified server configuration file: server settings and the
// agent inventory live in a single YAML file (like a Prometheus scrape
// config — agents are a plain list of hostname/ip pairs).
type Config struct {
	Listen                string              `yaml:"listen"`
	DBPath                string              `yaml:"db_path"`
	Token                 string              `yaml:"token"`
	PollInterval          time.Duration       `yaml:"-"`
	PollConcurrency       int                 `yaml:"poll_concurrency"`
	PollTimeout           time.Duration       `yaml:"-"`
	AgentPort             string              `yaml:"agent_port"`
	SnapshotRetentionDays int                 `yaml:"snapshot_retention_days"`
	RemovedRetentionHours int                 `yaml:"removed_retention_hours"`
	LogLevel              string              `yaml:"log_level"`
	Agents                []store.AgentTarget `yaml:"agents"`
}

// rawConfig mirrors the YAML file; duration fields are strings there.
type rawConfig struct {
	Listen                string              `yaml:"listen"`
	DBPath                string              `yaml:"db_path"`
	Token                 string              `yaml:"token"`
	PollInterval          string              `yaml:"poll_interval"`
	PollConcurrency       int                 `yaml:"poll_concurrency"`
	PollTimeout           string              `yaml:"poll_timeout"`
	AgentPort             string              `yaml:"agent_port"`
	SnapshotRetentionDays int                 `yaml:"snapshot_retention_days"`
	RemovedRetentionHours int                 `yaml:"removed_retention_hours"`
	LogLevel              string              `yaml:"log_level"`
	Agents                []store.AgentTarget `yaml:"agents"`
}

// LoadConfig reads and validates the server config file. Defaults are applied
// for unset fields; token is required.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}

	cfg := &Config{
		Listen:                orDefault(raw.Listen, ":8090"),
		DBPath:                orDefault(raw.DBPath, "/data/cron-checker.db"),
		Token:                 raw.Token,
		PollConcurrency:       orDefaultInt(raw.PollConcurrency, 10),
		AgentPort:             orDefault(raw.AgentPort, "9300"),
		SnapshotRetentionDays: orDefaultInt(raw.SnapshotRetentionDays, 7),
		RemovedRetentionHours: orDefaultInt(raw.RemovedRetentionHours, 72),
		LogLevel:              orDefault(raw.LogLevel, "info"),
		Agents:                raw.Agents,
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("config %q: token is required", path)
	}
	if cfg.PollInterval, err = parseDuration(raw.PollInterval, 300*time.Second); err != nil {
		return nil, fmt.Errorf("config %q: poll_interval: %w", path, err)
	}
	if cfg.PollTimeout, err = parseDuration(raw.PollTimeout, 15*time.Second); err != nil {
		return nil, fmt.Errorf("config %q: poll_timeout: %w", path, err)
	}
	return cfg, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func orDefaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func parseDuration(v string, def time.Duration) (time.Duration, error) {
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, err
	}
	return d, nil
}
