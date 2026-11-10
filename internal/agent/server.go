package agent

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

// NewHandler builds the agent's HTTP handler.
// Endpoints:
//   - GET /healthz     -> 200 {"status":"ok"} (no auth)
//   - GET /metrics     -> Prometheus metrics (no auth)
//   - GET /api/v1/cron -> model.AgentReport JSON (requires Bearer token)
func NewHandler(token string, cache *CachedCollector, m *Metrics, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthzHandler)
	if m != nil {
		mux.Handle("GET /metrics", m.Handler())
		mux.Handle("GET /api/v1/cron", bearerAuth(token, logger, cronHandler(cache, logger)))
		return m.observe(mux)
	}
	mux.Handle("GET /api/v1/cron", bearerAuth(token, logger, cronHandler(cache, logger)))
	return mux
}

func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// cronHandler serves the collected report from the cache. On a collect
// failure it still serves the last known report (stale data over none).
func cronHandler(cache *CachedCollector, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		report, err := cache.Get()
		if err != nil {
			logger.Error("collection failed, serving stale report if available", "err", err)
			if report == nil {
				writeJSONError(w, http.StatusInternalServerError, "collection failed")
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(report); err != nil {
			logger.Error("encoding report", "err", err)
		}
	})
}

// bearerAuth enforces `Authorization: Bearer <token>` using a constant-time
// comparison. Returns 401 on missing, malformed or wrong tokens.
func bearerAuth(token string, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, prefix) {
			unauthorized(w, logger)
			return
		}
		provided := strings.TrimPrefix(header, prefix)
		if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			unauthorized(w, logger)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func unauthorized(w http.ResponseWriter, logger *slog.Logger) {
	// Deliberately vague; do not leak whether the token was malformed
	// or simply wrong.
	logger.Debug("rejected request with missing or invalid bearer token")
	w.Header().Set("WWW-Authenticate", `Bearer realm="cron-checker"`)
	writeJSONError(w, http.StatusUnauthorized, "unauthorized")
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
