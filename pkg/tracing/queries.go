package tracing

import (
	"fmt"
	"time"
)

// EventRow represents a row from the events table.
type EventRow struct {
	ID         int64
	Timestamp  string
	EventKind  string
	AgentID    string
	SessionKey string
	TurnID     string
	Iteration  int
	TracePath  string
	Payload    string
	CreatedAt  string
}

// LLMCallRow represents a row from the llm_calls table.
type LLMCallRow struct {
	ID              int64
	TraceID         string
	SessionKey      string
	AgentID         string
	TurnID          string
	Iteration       int
	Provider        string
	Model           string
	RequestTime     string
	MessagesCount   int
	ToolsCount      int
	MaxTokens       int
	Temperature     float64
	ThinkingMode    string
	IsStreaming      int
	RequestSnippet  string
	ResponseTime    string
	LatencyMs       int64
	StatusCode      int
	ContentLen      int
	ToolCallsCount  int
	HasReasoning    int
	ResponseSnippet string
	PromptTokens    int
	CompletionTokens int
	TotalTokens     int
	CacheHitTokens  int
	CacheMissTokens int
	ReasoningTokens int
	IsFallback      int
	FallbackAttempt int
	FallbackReason  string
	Error           string
	CreatedAt       string
}

// SessionRow represents a row from the sessions table.
type SessionRow struct {
	SessionKey       string
	AgentID          string
	Channel          string
	Model            string
	ContextWindow    int
	TotalInputTokens  int
	TotalOutputTokens int
	TotalCacheHits    int
	TotalCostUSD     float64
	FirstEventAt     string
	LastEventAt      string
	LLMCallCount     int
	ToolCallCount    int
	TurnCount        int
	CompressCount    int
	BudgetUSD        float64
	BudgetExceeded   int
	CreatedAt        string
}

// SessionCostRow is a convenience alias for SessionRow used in cost queries.
type SessionCostRow = SessionRow

// QueryEventsBySession returns events for a given session since the specified time.
func (ts *TraceStore) QueryEventsBySession(sessionKey string, since time.Time, limit int) ([]EventRow, error) {
	if ts == nil || ts.db == nil {
		return nil, nil
	}

	if limit <= 0 {
		limit = 100
	}
	sinceStr := since.UTC().Format(time.RFC3339Nano)

	rows, err := ts.db.Query(sqlQueryEventsBySession, sessionKey, sinceStr, limit)
	if err != nil {
		return nil, fmt.Errorf("tracing: events query failed: %w", err)
	}
	defer rows.Close()

	var result []EventRow
	for rows.Next() {
		var r EventRow
		if err := rows.Scan(
			&r.ID, &r.Timestamp, &r.EventKind, &r.AgentID, &r.SessionKey,
			&r.TurnID, &r.Iteration, &r.TracePath, &r.Payload, &r.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("tracing: events scan failed: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// QueryLLMCallsBySession returns LLM calls for a given session since the specified time.
func (ts *TraceStore) QueryLLMCallsBySession(sessionKey string, since time.Time, limit int) ([]LLMCallRow, error) {
	if ts == nil || ts.db == nil {
		return nil, nil
	}

	if limit <= 0 {
		limit = 100
	}
	sinceStr := since.UTC().Format(time.RFC3339Nano)

	rows, err := ts.db.Query(sqlQueryLLMCallsBySession, sessionKey, sinceStr, limit)
	if err != nil {
		return nil, fmt.Errorf("tracing: llm calls query failed: %w", err)
	}
	defer rows.Close()

	var result []LLMCallRow
	for rows.Next() {
		var r LLMCallRow
		if err := rows.Scan(
			&r.ID, &r.TraceID, &r.SessionKey, &r.AgentID, &r.TurnID, &r.Iteration,
			&r.Provider, &r.Model,
			&r.RequestTime, &r.MessagesCount, &r.ToolsCount, &r.MaxTokens, &r.Temperature,
			&r.ThinkingMode, &r.IsStreaming, &r.RequestSnippet,
			&r.ResponseTime, &r.LatencyMs, &r.StatusCode,
			&r.ContentLen, &r.ToolCallsCount, &r.HasReasoning, &r.ResponseSnippet,
			&r.PromptTokens, &r.CompletionTokens, &r.TotalTokens,
			&r.CacheHitTokens, &r.CacheMissTokens, &r.ReasoningTokens,
			&r.IsFallback, &r.FallbackAttempt, &r.FallbackReason,
			&r.Error, &r.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("tracing: llm calls scan failed: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// QuerySessionCost returns the cost breakdown for a specific session.
func (ts *TraceStore) QuerySessionCost(sessionKey string) (*SessionCostRow, error) {
	if ts == nil || ts.db == nil {
		return nil, nil
	}

	var r SessionCostRow
	err := ts.db.QueryRow(sqlQuerySessionCost, sessionKey).Scan(
		&r.SessionKey, &r.AgentID, &r.Channel, &r.Model, &r.ContextWindow,
		&r.TotalInputTokens, &r.TotalOutputTokens, &r.TotalCacheHits, &r.TotalCostUSD,
		&r.FirstEventAt, &r.LastEventAt, &r.LLMCallCount, &r.ToolCallCount, &r.TurnCount,
		&r.CompressCount, &r.BudgetUSD, &r.BudgetExceeded, &r.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: session cost query failed: %w", err)
	}
	return &r, nil
}

// QuerySessions returns sessions with activity since the specified time.
func (ts *TraceStore) QuerySessions(since time.Time, limit int) ([]SessionRow, error) {
	if ts == nil || ts.db == nil {
		return nil, nil
	}

	if limit <= 0 {
		limit = 50
	}
	sinceStr := since.UTC().Format(time.RFC3339Nano)

	rows, err := ts.db.Query(sqlQuerySessions, sinceStr, limit)
	if err != nil {
		return nil, fmt.Errorf("tracing: sessions query failed: %w", err)
	}
	defer rows.Close()

	var result []SessionRow
	for rows.Next() {
		var r SessionRow
		if err := rows.Scan(
			&r.SessionKey, &r.AgentID, &r.Channel, &r.Model, &r.ContextWindow,
			&r.TotalInputTokens, &r.TotalOutputTokens, &r.TotalCacheHits, &r.TotalCostUSD,
			&r.FirstEventAt, &r.LastEventAt, &r.LLMCallCount, &r.ToolCallCount, &r.TurnCount,
			&r.CompressCount, &r.BudgetUSD, &r.BudgetExceeded, &r.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("tracing: sessions scan failed: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
