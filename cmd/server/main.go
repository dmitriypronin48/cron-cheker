// Command server is the cron-checker central server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/example/cron-checker/internal/server"
	"github.com/example/cron-checker/internal/store"
)

type config struct {
	Listen                string
	DBPath                string
	Token                 string
	PollInterval          time.Duration
	PollConcurrency       int
	PollTimeout           time.Duration
	AgentPort             string
	SnapshotRetentionDays int
	RemovedRetentionHours int
	LogLevel              string
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func loadConfig() config {
	healthcheck := flag.Bool("healthcheck", false, "perform a GET /healthz against SERVER_LISTEN and exit 0/1 (for container healthchecks)")
	flag.Parse()
	if *healthcheck {
		url := "http://" + env("SERVER_LISTEN", ":8090") + "/healthz"
		resp, err := http.Get(url)
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				_ = resp.Body.Close()
			}
			fmt.Fprintln(os.Stderr, "healthcheck failed:", err)
			os.Exit(1)
		}
		_ = resp.Body.Close()
		os.Exit(0)
	}
	cfg := config{
		Listen:                env("SERVER_LISTEN", ":8090"),
		DBPath:                env("DB_PATH", "/data/cron-checker.db"),
		Token:                 os.Getenv("CRON_SERVER_TOKEN"),
		PollInterval:          envDur("POLL_INTERVAL", 300*time.Second),
		PollConcurrency:       envInt("POLL_CONCURRENCY", 10),
		PollTimeout:           envDur("POLL_TIMEOUT", 15*time.Second),
		AgentPort:             env("AGENT_PORT", "9300"),
		SnapshotRetentionDays: envInt("SNAPSHOT_RETENTION_DAYS", 7),
		RemovedRetentionHours: envInt("REMOVED_RETENTION_HOURS", 72),
		LogLevel:              env("LOG_LEVEL", "info"),
	}
	return cfg
}

func main() {
	cfg := loadConfig()

	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	if cfg.Token == "" {
		log.Error("CRON_SERVER_TOKEN is required")
		os.Exit(1)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("open database", "path", cfg.DBPath, "error", err)
		os.Exit(1)
	}
	defer st.Close()
	log.Info("database opened", "path", cfg.DBPath)

	metrics := server.NewMetrics(st, log)
	srv, err := server.NewServer(st, metrics, log)
	if err != nil {
		log.Error("create server", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	poller := server.NewPoller(server.PollerConfig{
		Interval:    cfg.PollInterval,
		Concurrency: cfg.PollConcurrency,
		Timeout:     cfg.PollTimeout,
		Token:       cfg.Token,
		AgentPort:   cfg.AgentPort,
	}, st, log, metrics)
	go poller.Run(ctx)
	go server.CleanupLoop(ctx, st, cfg.SnapshotRetentionDays, cfg.RemovedRetentionHours, log)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Info("HTTP server listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("HTTP server", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown", "error", err)
	}
}
