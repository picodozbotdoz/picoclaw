package tracing

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HandlerMux is the interface for registering HTTP handlers.
type HandlerMux interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// RegisterHTTPHandlers registers all tracing REST API endpoints on the given mux.
// The authToken is used for bearer-token authentication on all endpoints.
func (ts *TraceStore) RegisterHTTPHandlers(mux HandlerMux, authToken string) {
	if ts == nil || mux == nil {
		return
	}

	mux.HandleFunc("/api/traces/events", ts.authMiddleware(authToken, ts.handleEvents))
	mux.HandleFunc("/api/traces/llm-calls", ts.authMiddleware(authToken, ts.handleLLMCalls))
	mux.HandleFunc("/api/traces/context", ts.authMiddleware(authToken, ts.handleContext))
	mux.HandleFunc("/api/traces/sessions", ts.authMiddleware(authToken, ts.handleSessions))
	mux.HandleFunc("/api/traces/session-cost", ts.authMiddleware(authToken, ts.handleSessionCost))
}

// RegisterMetricsHandler registers the /metrics endpoint on the given mux.
func (ts *TraceStore) RegisterMetricsHandler(mux HandlerMux) {
	if ts == nil || mux == nil {
		return
	}
	mux.HandleFunc("/metrics", ts.handleMetrics)
}

// authMiddleware wraps a handler with bearer-token authentication.
func (ts *TraceStore) authMiddleware(authToken string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if authToken != "" {
			given := extractAuthBearerToken(r.Header.Get("Authorization"))
			if given == "" || subtle.ConstantTimeCompare([]byte(given), []byte(authToken)) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
		}
		next(w, r)
	}
}

// extractAuthBearerToken extracts the bearer token from an Authorization header.
func extractAuthBearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) < len(prefix) || header[:len(prefix)] != prefix {
		return ""
	}
	return header[len(prefix):]
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// parseQueryParams extracts common query parameters from the request.
func parseQueryParams(r *http.Request) (sessionKey, sinceStr, limitStr string) {
	q := r.URL.Query()
	sessionKey = q.Get("session")
	sinceStr = q.Get("since")
	limitStr = q.Get("limit")
	return
}

// handleEvents handles GET /api/traces/events
func (ts *TraceStore) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	sessionKey, sinceStr, limitStr := parseQueryParams(r)
	since := parseSince(sinceStr)
	limit := parseLimit(limitStr, 100)

	events, err := ts.QueryEventsBySession(sessionKey, since, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if events == nil {
		events = []EventRow{}
	}
	writeJSON(w, http.StatusOK, events)
}

// handleLLMCalls handles GET /api/traces/llm-calls
func (ts *TraceStore) handleLLMCalls(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	sessionKey, sinceStr, limitStr := parseQueryParams(r)
	since := parseSince(sinceStr)
	limit := parseLimit(limitStr, 100)

	calls, err := ts.QueryLLMCallsBySession(sessionKey, since, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if calls == nil {
		calls = []LLMCallRow{}
	}
	writeJSON(w, http.StatusOK, calls)
}

// handleContext handles GET /api/traces/context
func (ts *TraceStore) handleContext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	sessionKey, sinceStr, _ := parseQueryParams(r)
	since := parseSince(sinceStr)

	snaps, err := ts.GetContextTrend(sessionKey, since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if snaps == nil {
		snaps = []ContextSnapshotRow{}
	}
	writeJSON(w, http.StatusOK, snaps)
}

// handleSessions handles GET /api/traces/sessions
func (ts *TraceStore) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	_, sinceStr, limitStr := parseQueryParams(r)
	since := parseSince(sinceStr)
	limit := parseLimit(limitStr, 50)

	sessions, err := ts.QuerySessions(since, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if sessions == nil {
		sessions = []SessionRow{}
	}
	writeJSON(w, http.StatusOK, sessions)
}

// handleSessionCost handles GET /api/traces/session-cost
func (ts *TraceStore) handleSessionCost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	sessionKey := r.URL.Query().Get("session")
	if sessionKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session parameter required"})
		return
	}

	cost, err := ts.QuerySessionCost(sessionKey)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, cost)
}

// handleMetrics serves metrics in Prometheus text exposition format.
func (ts *TraceStore) handleMetrics(w http.ResponseWriter, r *http.Request) {
	snap := ts.metrics.Snapshot()

	var b strings.Builder

	// LLM metrics
	writeCounter(&b, "picoclaw_llm_calls_total", snap.TotalLLMCalls)
	writeGauge(&b, "picoclaw_llm_tokens_input_total", snap.TotalTokensIn)
	writeGauge(&b, "picoclaw_llm_tokens_output_total", snap.TotalTokensOut)
	writeGauge(&b, "picoclaw_llm_tokens_cache_hit_total", snap.TotalCacheHits)
	writeCounter(&b, "picoclaw_llm_errors_total", snap.TotalErrors)

	// LLM calls by provider
	for provider, count := range snap.LLMCallsByProvider {
		writeLabeledCounter(&b, "picoclaw_llm_calls_by_provider_total", "provider", provider, count)
	}

	// LLM calls by model
	for model, count := range snap.LLMCallsByModel {
		writeLabeledCounter(&b, "picoclaw_llm_calls_by_model_total", "model", model, count)
	}

	// Tool metrics
	writeCounter(&b, "picoclaw_tool_calls_total", snap.TotalToolCalls)

	// Session metrics
	writeGauge(&b, "picoclaw_sessions_active", snap.ActiveSessions)

	// EventBus metrics
	for kind, count := range snap.EventsEmitted {
		writeLabeledCounter(&b, "picoclaw_events_emitted_total", "kind", kind, count)
	}
	for kind, count := range snap.EventsDropped {
		writeLabeledCounter(&b, "picoclaw_events_dropped_total", "kind", kind, count)
	}

	// System metrics
	writeGauge(&b, "picoclaw_tracing_db_size_bytes", snap.DBSizeBytes)
	writeGauge(&b, "picoclaw_tracing_events_pending", snap.EventsPending)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(b.String()))
}

func writeCounter(b *strings.Builder, name string, value int64) {
	fmt.Fprintf(b, "# TYPE %s counter\n%s %d\n", name, name, value)
}

func writeGauge(b *strings.Builder, name string, value int64) {
	fmt.Fprintf(b, "# TYPE %s gauge\n%s %d\n", name, name, value)
}

func writeLabeledCounter(b *strings.Builder, name, label, value string, count int64) {
	fmt.Fprintf(b, "# TYPE %s counter\n%s{%s=%q} %d\n", name, name, label, value, count)
}

// parseSince parses a "since" query parameter as either a duration or RFC3339 timestamp.
func parseSince(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	// Try duration (e.g., "24h", "7d")
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d)
	}
	// Try days shorthand
	if strings.HasSuffix(s, "d") {
		if days, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil {
			return time.Now().AddDate(0, 0, -days)
		}
	}
	// Try RFC3339
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// parseLimit parses a limit query parameter with a default.
func parseLimit(s string, defaultVal int) int {
	if s == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return defaultVal
	}
	return n
}
