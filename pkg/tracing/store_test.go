package tracing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestDisabledConfigReturnsNil(t *testing.T) {
	cfg := TraceConfig{Enabled: false}
	store, err := NewTraceStore(cfg)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if store != nil {
		t.Fatal("expected nil store when tracing disabled")
	}
}

func TestEnabledConfigCreatesStore(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfg := TraceConfig{
		Enabled: true,
		DBPath:  dbPath,
		WALMode: true,
	}
	store, err := NewTraceStore(cfg)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	defer store.Close()

	if store == nil {
		t.Fatal("expected non-nil store when tracing enabled")
	}
	if store.db == nil {
		t.Fatal("expected non-nil db")
	}
}

func TestDefaultValues(t *testing.T) {
	cfg := TraceConfig{Enabled: true}
	cfg.ApplyDefaults()
	if cfg.MaxDBSizeMB != 500 {
		t.Errorf("expected MaxDBSizeMB=500, got %d", cfg.MaxDBSizeMB)
	}
	if cfg.EventRetentionDays != 7 {
		t.Errorf("expected EventRetentionDays=7, got %d", cfg.EventRetentionDays)
	}
	if cfg.LLMCallRetentionDays != 30 {
		t.Errorf("expected LLMCallRetentionDays=30, got %d", cfg.LLMCallRetentionDays)
	}
	if cfg.SnippetMaxChars != 2048 {
		t.Errorf("expected SnippetMaxChars=2048, got %d", cfg.SnippetMaxChars)
	}
}

func newTestStore(t *testing.T) *TraceStore {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfg := TraceConfig{
		Enabled:               true,
		DBPath:                dbPath,
		WALMode:               true,
		MaxDBSizeMB:           500,
		EventRetentionDays:    7,
		LLMCallRetentionDays:  30,
		ContextRetentionDays:  7,
		SessionRetentionDays:  90,
		PruneIntervalMinutes:  60,
		SnippetMaxChars:       2048,
		SanitizeSecrets:       true,
	}
	store, err := NewTraceStore(cfg)
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestEventInsertAndQuery(t *testing.T) {
	store := newTestStore(t)

	// Insert an event directly
	ew := eventWrite{
		timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		eventKind:  "llm_response",
		agentID:    "test-agent",
		sessionKey: "test-session",
		turnID:     "turn-1",
		iteration:  1,
		tracePath:  "/test",
		payload:    `{"content_len": 100}`,
	}
	store.enqueueEvent(ew)

	// Wait for async write
	time.Sleep(300 * time.Millisecond)

	events, err := store.QueryEventsBySession("test-session", time.Time{}, 10)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].EventKind != "llm_response" {
		t.Errorf("expected event_kind=llm_response, got %s", events[0].EventKind)
	}
}

func TestLLMCallStartEndAndQuery(t *testing.T) {
	store := newTestStore(t)

	traceID := NewTraceID()
	req := LLMCallRequest{
		Provider:      "deepseek",
		Model:         "deepseek-v4-flash",
		MessagesCount: 10,
		ToolsCount:    3,
		MaxTokens:     4096,
		Temperature:   0.7,
		ThinkingMode:  "enabled",
		IsStreaming:   true,
	}

	err := store.LLMCallStart(traceID, "test-session", "test-agent", "turn-1", 1, req)
	if err != nil {
		t.Fatalf("LLMCallStart failed: %v", err)
	}

	resp := LLMCallResponse{
		LatencyMs:        1500,
		StatusCode:       200,
		ContentLen:       500,
		ToolCallsCount:   1,
		HasReasoning:     true,
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
		CacheHitTokens:   200,
		CacheMissTokens:  800,
	}

	err = store.LLMCallEnd(traceID, resp)
	if err != nil {
		t.Fatalf("LLMCallEnd failed: %v", err)
	}

	calls, err := store.QueryLLMCallsBySession("test-session", time.Time{}, 10)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Provider != "deepseek" {
		t.Errorf("expected provider=deepseek, got %s", calls[0].Provider)
	}
	if calls[0].Model != "deepseek-v4-flash" {
		t.Errorf("expected model=deepseek-v4-flash, got %s", calls[0].Model)
	}
	if calls[0].LatencyMs != 1500 {
		t.Errorf("expected latency_ms=1500, got %d", calls[0].LatencyMs)
	}
	if calls[0].PromptTokens != 1000 {
		t.Errorf("expected prompt_tokens=1000, got %d", calls[0].PromptTokens)
	}
}

func TestContextSnapshot(t *testing.T) {
	store := newTestStore(t)

	snap := ContextSnapshot{
		SessionKey:       "test-session",
		AgentID:          "test-agent",
		TurnID:           "turn-1",
		Iteration:        1,
		Trigger:          "post_llm",
		Timestamp:        time.Now(),
		UsedTokens:       5000,
		TotalTokens:      128000,
		CompressAtTokens: 102400,
		UsedPercent:      4,
		CacheHitTokens:   2000,
		CacheMissTokens:  3000,
	}

	err := store.RecordContextSnapshot(snap)
	if err != nil {
		t.Fatalf("RecordContextSnapshot failed: %v", err)
	}

	snaps, err := store.GetContextTrend("test-session", time.Time{})
	if err != nil {
		t.Fatalf("GetContextTrend failed: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snaps))
	}
	if snaps[0].UsedTokens != 5000 {
		t.Errorf("expected used_tokens=5000, got %d", snaps[0].UsedTokens)
	}
}

func TestSessionUpsert(t *testing.T) {
	store := newTestStore(t)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	store.enqueueBatch(batchItem{
		query: sqlUpsertSession,
		args: []any{
			"test-session", "test-agent", "telegram", "gpt-4o", 128000,
			100, 50, 20, 0.05,
			now, now,
			1, 2, 1, 0,
			10.0, 0,
		},
	})

	// Wait for async write
	time.Sleep(300 * time.Millisecond)

	sessions, err := store.QuerySessions(time.Time{}, 10)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].SessionKey != "test-session" {
		t.Errorf("expected session_key=test-session, got %s", sessions[0].SessionKey)
	}
}

func TestDBSize(t *testing.T) {
	store := newTestStore(t)

	size, err := store.DBSize()
	if err != nil {
		t.Fatalf("DBSize failed: %v", err)
	}
	if size <= 0 {
		t.Errorf("expected positive DB size, got %d", size)
	}
}

func TestSanitizeSecrets(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		contains string
		excludes string
	}{
		{
			name:     "bearer token",
			input:    `Authorization: Bearer sk-abc123def456ghi789jkl012mno345`,
			contains: "[REDACTED]",
			excludes: "sk-abc123def456ghi789jkl012mno345",
		},
		{
			name:     "openai key",
			input:    `api_key: sk-proj-abc123def456ghi789jkl012`,
			contains: "[REDACTED]",
			excludes: "sk-proj-abc123def456ghi789jkl012",
		},
		{
			name:     "anthropic key",
			input:    `key: sk-ant-api03-abc123def456ghi789jkl012mno345pqr678`,
			contains: "[REDACTED]",
			excludes: "sk-ant-api03-abc123def456ghi789jkl012mno345pqr678",
		},
		{
			name:     "telegram token",
			input:    `1234567890:AAH_a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6q`,
			contains: "[REDACTED]",
			excludes: "AAH_a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6q",
		},
		{
			name:     "no secrets",
			input:    `hello world this is safe`,
			contains: "hello world this is safe",
			excludes: "[REDACTED]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SanitizeSecrets(tt.input)
			if !contains(result, tt.contains) {
				t.Errorf("expected result to contain %q, got %q", tt.contains, result)
			}
			if tt.excludes != "" && contains(result, tt.excludes) {
				t.Errorf("expected result to exclude %q, got %q", tt.excludes, result)
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestMetricsCounters(t *testing.T) {
	var m MetricsCounters

	m.TotalLLMCalls.Add(5)
	m.IncrProvider("deepseek")
	m.IncrProvider("openai")
	m.IncrModel("gpt-4o")

	snap := m.Snapshot()
	if snap.TotalLLMCalls != 5 {
		t.Errorf("expected TotalLLMCalls=5, got %d", snap.TotalLLMCalls)
	}
	if snap.LLMCallsByProvider["deepseek"] != 1 {
		t.Errorf("expected deepseek=1, got %d", snap.LLMCallsByProvider["deepseek"])
	}
	if snap.LLMCallsByModel["gpt-4o"] != 1 {
		t.Errorf("expected gpt-4o=1, got %d", snap.LLMCallsByModel["gpt-4o"])
	}
}

func TestNilStoreMethods(t *testing.T) {
	var store *TraceStore

	// All methods should be no-ops on nil store
	if err := store.LLMCallStart("", "", "", "", 0, LLMCallRequest{}); err != nil {
		t.Errorf("expected nil error on nil store, got: %v", err)
	}
	if err := store.LLMCallEnd("", LLMCallResponse{}); err != nil {
		t.Errorf("expected nil error on nil store, got: %v", err)
	}
	if err := store.LLMCallError("", fmt.Errorf("test")); err != nil {
		t.Errorf("expected nil error on nil store, got: %v", err)
	}
	if err := store.RecordContextSnapshot(ContextSnapshot{}); err != nil {
		t.Errorf("expected nil error on nil store, got: %v", err)
	}
	store.UpdateSessionCost("", 0, 0, false)
}

func TestFallbackTrace(t *testing.T) {
	store := newTestStore(t)

	// Primary call
	primaryTraceID := NewTraceID()
	err := store.LLMCallStart(primaryTraceID, "test-session", "test-agent", "turn-1", 1, LLMCallRequest{
		Provider: "deepseek",
		Model:    "deepseek-v4-flash",
	})
	if err != nil {
		t.Fatalf("primary LLMCallStart failed: %v", err)
	}

	// Primary call fails
	err = store.LLMCallError(primaryTraceID, fmt.Errorf("rate limit"))
	if err != nil {
		t.Fatalf("primary LLMCallError failed: %v", err)
	}

	// Fallback call
	fallbackTraceID := NewTraceID()
	err = store.LLMCallStart(fallbackTraceID, "test-session", "test-agent", "turn-1", 1, LLMCallRequest{
		Provider: "openai",
		Model:    "gpt-4o",
	})
	if err != nil {
		t.Fatalf("fallback LLMCallStart failed: %v", err)
	}

	err = store.LLMCallEnd(fallbackTraceID, LLMCallResponse{
		LatencyMs:       800,
		StatusCode:      200,
		IsFallback:      true,
		FallbackAttempt: 1,
		FallbackReason:  "rate_limit",
	})
	if err != nil {
		t.Fatalf("fallback LLMCallEnd failed: %v", err)
	}

	calls, err := store.QueryLLMCallsBySession("test-session", time.Time{}, 10)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls (primary + fallback), got %d", len(calls))
	}

	// Find the primary and fallback calls
	var primary, fallback *LLMCallRow
	for i := range calls {
		if calls[i].TraceID == primaryTraceID {
			primary = &calls[i]
		}
		if calls[i].TraceID == fallbackTraceID {
			fallback = &calls[i]
		}
	}

	if primary == nil {
		t.Fatal("primary call not found")
	}
	if primary.Error == "" {
		t.Error("expected primary call to have error")
	}

	if fallback == nil {
		t.Fatal("fallback call not found")
	}
	if fallback.IsFallback != 1 {
		t.Error("expected fallback call to have is_fallback=1")
	}
	if fallback.FallbackReason != "rate_limit" {
		t.Errorf("expected fallback_reason=rate_limit, got %s", fallback.FallbackReason)
	}
}

func TestMain(m *testing.M) {
	// Suppress logger output during tests
	os.Exit(m.Run())
}

// Silence unused import warnings
var (
	_ = json.Marshal
	_ = atomic.Int64{}
	_ = filepath.Join
)
