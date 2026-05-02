package tracing

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// LLMCallRequest contains the request-side data for an LLM call.
type LLMCallRequest struct {
	Provider       string
	Model          string
	MessagesCount  int
	ToolsCount     int
	MaxTokens      int
	Temperature    float64
	ThinkingMode   string // "disabled", "high", "max"
	IsStreaming     bool
	RequestSnippet string // first N chars of messages JSON
}

// LLMCallResponse contains the response-side data for an LLM call.
type LLMCallResponse struct {
	LatencyMs        int64
	StatusCode       int
	ContentLen       int
	ToolCallsCount   int
	HasReasoning     bool
	ResponseSnippet  string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CacheHitTokens   int
	CacheMissTokens  int
	ReasoningTokens  int
	IsFallback       bool
	FallbackAttempt  int
	FallbackReason   string
}

// NewTraceID generates a unique trace ID using UUID.
func NewTraceID() string {
	return uuid.New().String()
}

// LLMCallStart records the request half of an LLM call.
func (ts *TraceStore) LLMCallStart(traceID, sessionKey, agentID, turnID string, iteration int, req LLMCallRequest) error {
	if ts == nil || ts.db == nil {
		return nil
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)

	snippet := req.RequestSnippet
	if ts.config.SanitizeSecrets {
		snippet = SanitizeSecrets(snippet)
	}
	if len(snippet) > ts.config.SnippetMaxChars {
		snippet = snippet[:ts.config.SnippetMaxChars]
	}

	isStreaming := 0
	if req.IsStreaming {
		isStreaming = 1
	}

	_, err := ts.db.Exec(sqlInsertLLMCall,
		traceID, sessionKey, agentID, turnID, iteration,
		req.Provider, req.Model,
		now, req.MessagesCount, req.ToolsCount, req.MaxTokens, req.Temperature,
		req.ThinkingMode, isStreaming, snippet,
	)
	if err != nil {
		return fmt.Errorf("tracing: LLM call start insert failed: %w", err)
	}

	ts.metrics.IncrProvider(req.Provider)
	ts.metrics.IncrModel(req.Model)

	return nil
}

// LLMCallEnd updates the LLM call with response data.
func (ts *TraceStore) LLMCallEnd(traceID string, resp LLMCallResponse) error {
	if ts == nil || ts.db == nil {
		return nil
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)

	snippet := resp.ResponseSnippet
	if ts.config.SanitizeSecrets {
		snippet = SanitizeSecrets(snippet)
	}
	if len(snippet) > ts.config.SnippetMaxChars {
		snippet = snippet[:ts.config.SnippetMaxChars]
	}

	hasReasoning := 0
	if resp.HasReasoning {
		hasReasoning = 1
	}
	isFallback := 0
	if resp.IsFallback {
		isFallback = 1
	}

	_, err := ts.db.Exec(sqlUpdateLLMCallResponse,
		now, resp.LatencyMs, resp.StatusCode,
		resp.ContentLen, resp.ToolCallsCount, hasReasoning,
		snippet,
		resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens,
		resp.CacheHitTokens, resp.CacheMissTokens, resp.ReasoningTokens,
		isFallback, resp.FallbackAttempt, resp.FallbackReason,
		traceID,
	)
	if err != nil {
		return fmt.Errorf("tracing: LLM call end update failed: %w", err)
	}

	ts.metrics.TotalTokensIn.Add(int64(resp.PromptTokens))
	ts.metrics.TotalTokensOut.Add(int64(resp.CompletionTokens))
	ts.metrics.TotalCacheHits.Add(int64(resp.CacheHitTokens))

	return nil
}

// LLMCallError records an error for an LLM call.
func (ts *TraceStore) LLMCallError(traceID string, callErr error) error {
	if ts == nil || ts.db == nil {
		return nil
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	errMsg := ""
	if callErr != nil {
		errMsg = callErr.Error()
	}

	_, err := ts.db.Exec(sqlUpdateLLMCallError,
		now, 0,
		errMsg,
		traceID,
	)
	if err != nil {
		return fmt.Errorf("tracing: LLM call error update failed: %w", err)
	}

	ts.metrics.TotalErrors.Add(1)
	return nil
}

// UpdateSessionCost updates the cost fields for a session.
func (ts *TraceStore) UpdateSessionCost(sessionKey string, costUSD, budgetUSD float64, budgetExceeded bool) {
	if ts == nil {
		return
	}

	budgetExceededInt := 0
	if budgetExceeded {
		budgetExceededInt = 1
	}

	ts.enqueueBatch(batchItem{
		query: sqlUpdateSessionCost,
		args:  []any{costUSD, budgetUSD, budgetExceededInt, sessionKey},
	})
}
