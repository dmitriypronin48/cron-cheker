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
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("cron-checker-agent", flag.ExitOnError)
	listen := fs.String("listen", envOr("LISTEN_ADDR", ":9300"), "HTTP listen address (env LISTEN_ADDR)")
	token := fs.String("token", envOr("CRON_AGENT_TOKEN", ""), "bearer token for /api/v1/cron (env CRON_AGENT_TOKEN)")
	interval := fs.Duration("collect-interval", envDurationOr("COLLECT_INTERVAL", 300*time.Second), "how often to re-scan local cron files (env COLLECT_INTERVAL)")
	logLevel := fs.String("log-level", envOr("LOG_LEVEL", "info"), "log level: debug|info|warn|error (env LOG_LEVEL)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	if *token == "" {
		return fmt.Errorf("CRON_AGENT_TOKEN is required (or pass -token)")
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("invalid log level %q: %w", *logLevel, err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	logger.Info("starting cron-checker-agent",
		"version", model.AgentVersion,
		"listen", *listen,
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

	cache := agent.NewCache(*interval, collect)
	handler := agent.NewHandler(*token, cache, metrics, logger)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", *listen)
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
