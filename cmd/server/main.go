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
	"syscall"
	"time"

	"github.com/example/cron-checker/internal/server"
	"github.com/example/cron-checker/internal/store"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	configPath := flag.String("config",
		env("CRON_CHECKER_CONFIG", "/etc/cron-checker/server.yaml"),
		"path to the server config file (YAML: settings + agent inventory)")
	healthcheck := flag.Bool("healthcheck", false, "perform a GET /healthz against the listen address and exit 0/1 (for container healthchecks)")
	flag.Parse()
	if *healthcheck {
		listen := ":8090"
		if cfg, err := server.LoadConfig(*configPath); err == nil && cfg.Listen != "" {
			listen = cfg.Listen
		} else if v := os.Getenv("SERVER_LISTEN"); v != "" {
			listen = v
		}
		url := "http://" + listen + "/healthz"
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

	cfg, err := server.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		os.Exit(1)
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	log.Info("config loaded", "path", *configPath, "agents", len(cfg.Agents))

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
		Targets:     func() ([]store.AgentTarget, error) { c, err := server.LoadConfig(*configPath); return c.Agents, err },
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
