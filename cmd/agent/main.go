// Command cron-checker-agent is a passive HTTP server that collects cron
// entries from the local Linux host and exposes them to the central
// cron-checker server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/example/cron-checker/internal/agent"
	"github.com/example/cron-checker/internal/model"
	"gopkg.in/yaml.v3"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

// agentConfig mirrors the optional agent YAML config file.
// Precedence for every field: flag > env var > config file > built-in default.
type agentConfig struct {
	Listen          string `yaml:"listen"`
	Token           string `yaml:"token"`
	CollectInterval string `yaml:"collect_interval"`
	LogLevel        string `yaml:"log_level"`
}

func loadAgentConfig(path string) (agentConfig, error) {
	var cfg agentConfig
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil // config file is optional
		}
		return cfg, fmt.Errorf("read config %q: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %q: %w", path, err)
	}
	return cfg, nil
}

func run() error {
	fs := flag.NewFlagSet("cron-checker-agent", flag.ExitOnError)
	configPath := fs.String("config", envOr("CRON_AGENT_CONFIG", "/etc/cron-checker/agent.yaml"), "path to the agent config file (YAML, optional)")
	listenF := fs.String("listen", "", "HTTP listen address (env LISTEN_ADDR)")
	tokenF := fs.String("token", "", "bearer token for /api/v1/cron (env CRON_AGENT_TOKEN)")
	intervalF := fs.Duration("collect-interval", 0, "how often to re-scan local cron files: 3m, 1h, ... (env COLLECT_INTERVAL)")
	logLevelF := fs.String("log-level", "", "log level: debug|info|warn|error (env LOG_LEVEL)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	fileCfg, err := loadAgentConfig(*configPath)
	if err != nil {
		return err
	}

	listen := *listenF
	if listen == "" {
		listen = envOr("LISTEN_ADDR", fileCfg.Listen)
	}
	if listen == "" {
		listen = ":9300"
	}

	token := *tokenF
	if token == "" {
		token = envOr("CRON_AGENT_TOKEN", fileCfg.Token)
	}
	if token == "" {
		return fmt.Errorf("token is required: set it in the config file, CRON_AGENT_TOKEN env or -token flag")
	}

	interval := *intervalF
	if interval == 0 && !explicit["collect-interval"] {
		interval = envDurationOr("COLLECT_INTERVAL", 0)
	}
	if interval == 0 && fileCfg.CollectInterval != "" {
		d, err := time.ParseDuration(fileCfg.CollectInterval)
		if err != nil {
			return fmt.Errorf("config: invalid collect_interval %q: %w", fileCfg.CollectInterval, err)
		}
		interval = d
	}
	if interval == 0 {
		interval = 300 * time.Second // default: re-scan local cron files every 5 minutes
	}

	logLevel := *logLevelF
	if logLevel == "" {
		logLevel = envOr("LOG_LEVEL", fileCfg.LogLevel)
	}
	if logLevel == "" {
		logLevel = "info"
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(logLevel)); err != nil {
		return fmt.Errorf("invalid log level %q: %w", logLevel, err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	logger.Info("starting cron-checker-agent",
		"version", model.AgentVersion,
		"listen", listen,
		"collect_interval", interval.String(),
	)

	metrics := agent.NewMetrics()
	cfg := agent.DefaultConfig()

	collect := func() (*model.AgentReport, error) {
		report, err := collectReport(cfg, logger)
		if err != nil {
			metrics.CollectErrors.Inc()
			return nil, err
		}
		metrics.ObserveReport(report)
		return report, nil
	}

	cache := agent.NewCache(interval, collect)
	handler := agent.NewHandler(token, cache, metrics, logger)

	srv := &http.Server{
		Addr:              listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("stopped")
	return nil
}

// collectReport gathers host info and cron entries into a report.
// Raw cron lines are never logged: only counts and metadata, at DEBUG level.
func collectReport(cfg agent.Config, logger *slog.Logger) (*model.AgentReport, error) {
	host := agent.CollectHostInfo(agent.DefaultHostInfoConfig())
	entries, err := agent.Collect(cfg)
	if err != nil {
		return nil, err
	}
	logger.Debug("collection complete",
		"hostname", host.Hostname,
		"entries", len(entries),
	)
	return &model.AgentReport{
		AgentVersion: model.AgentVersion,
		Hostname:     host.Hostname,
		IP:           host.IP,
		CollectedAt:  time.Now().UTC(),
		Timezone:     host.Timezone,
		Entries:      entries,
	}, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDurationOr(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: invalid %s=%q, using %s\n", key, v, fallback)
			return fallback
		}
		return d
	}
	return fallback
}
