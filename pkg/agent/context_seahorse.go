//go:build !mipsle && !netbsd && !(freebsd && arm)

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
	"github.com/sipeed/picoclaw/pkg/seahorse"
	"github.com/sipeed/picoclaw/pkg/session"
	"github.com/sipeed/picoclaw/pkg/tokenizer"
)

// semanticRecall runs nmem context/recall to provide topic-relevant long-term memory.
// It uses a background goroutine to keep "nmem context" (O(1), always-on) fresh,
// and optionally queries "nmem recall" when the user message contains substantive keywords.
type semanticRecall struct {
	mu             sync.RWMutex
	contextCache   string    // cached nmem context output
	lastRefresh    time.Time // when context was last fetched
	nmemBin        string    // path to nmem-wrapper
	agentID        string    // neural-memory agent ID
	refreshInterval time.Duration
}

const (
	semanticRecallMaxTokens  = 300   // max tokens for recall injection
	semanticRecallMinLength  = 10    // min user message length to trigger recall
	semanticRefreshInterval  = 5 * time.Minute
)

// newSemanticRecall creates a semantic recall hook for the given agent.
func newSemanticRecall(agentID string) *semanticRecall {
	sr := &semanticRecall{
		nmemBin:         "/home/plain/.picoclaw/bin/nmem-wrapper",
		agentID:         agentID,
		refreshInterval: semanticRefreshInterval,
	}

	// Try to find nmem binary
	if _, err := exec.LookPath(sr.nmemBin); err != nil {
		// Try without wrapper
		if _, err2 := exec.LookPath("nmem"); err2 == nil {
			sr.nmemBin = "nmem"
		} else {
			logger.WarnCF("seahorse", "semantic recall: nmem not found, disabled",
				map[string]any{"error": err.Error()})
			return nil
		}
	}

	// Initial fetch (non-blocking)
	go sr.refreshContext(context.Background())

	return sr
}

// getContext returns the cached nmem context (recent memories).
// Returns empty string if cache is cold or recall is disabled.
func (sr *semanticRecall) getContext() string {
	if sr == nil {
		return ""
	}
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	return sr.contextCache
}

// recallForMessage runs a semantic recall query based on the user message.
// Returns relevant context or empty string. Never blocks longer than 3 seconds.
func (sr *semanticRecall) recallForMessage(ctx context.Context, userMessage string) string {
	if sr == nil || sr.nmemBin == "" {
		return ""
	}
	if len(strings.TrimSpace(userMessage)) < semanticRecallMinLength {
		return ""
	}

	// Use context with 3s timeout
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, sr.nmemBin, sr.agentID, "recall",
		userMessage,
		"--max-tokens", fmt.Sprintf("%d", semanticRecallMaxTokens),
		"--depth", "1",
	)

	out, err := cmd.Output()
	if err != nil {
		logger.DebugCF("seahorse", "semantic recall failed",
			map[string]any{"error": err.Error()})
		return ""
	}

	result := strings.TrimSpace(string(out))
	if len(result) > 0 && result != "No relevant memories found." {
		logger.DebugCF("seahorse", "semantic recall hit",
			map[string]any{"result_len": len(result)})
		return result
	}
	return ""
}

// refreshContext periodically refreshes the nmem context cache in background.
func (sr *semanticRecall) refreshContext(ctx context.Context) {
	if sr == nil {
		return
	}

	ticker := time.NewTicker(sr.refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sr.fetchContext(ctx)
		}
	}
}

// fetchContext runs nmem context and updates the cache.
func (sr *semanticRecall) fetchContext(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, sr.nmemBin, sr.agentID, "context",
		"--limit", "5",
		"--fresh-only",
	)

	out, err := cmd.Output()
	if err != nil {
		return
	}

	result := strings.TrimSpace(string(out))
	if len(result) > 0 {
		sr.mu.Lock()
		sr.contextCache = result
		sr.lastRefresh = time.Now()
		sr.mu.Unlock()
	}
}

// seahorseContextManager adapts seahorse.Engine to agent.ContextManager.
type seahorseContextManager struct {
	engine        *seahorse.Engine
	sessions      session.SessionStore // for startup bootstrap
	semanticRecall *semanticRecall      // neural-memory semantic context carry
}

// newSeahorseContextManager creates a seahorse-backed ContextManager.
func newSeahorseContextManager(_ json.RawMessage, al *AgentLoop) (ContextManager, error) {
	if al == nil {
		return nil, fmt.Errorf("seahorse: AgentLoop is required")
	}

	// Resolve workspace for DB path
	// DB stores session data, so it goes in sessions/ directory
	agent := al.registry.GetDefaultAgent()
	dbPath := agent.Workspace + "/sessions/seahorse.db"

	// Create CompleteFn from provider
	completeFn := providerToCompleteFn(agent.Provider, agent.Model)

	// Create engine
	engine, err := seahorse.NewEngine(seahorse.Config{
		DBPath: dbPath,
	}, completeFn)
	if err != nil {
		return nil, fmt.Errorf("seahorse: create engine: %w", err)
	}

	// Determine agent ID for neural-memory
	agentID := "default"
	if agent != nil && agent.ID != "" {
		agentID = agent.ID
	}

	mgr := &seahorseContextManager{
		engine:        engine,
		sessions:      agent.Sessions,
		semanticRecall: newSemanticRecall(agentID),
	}

	// Register seahorse tools with the agent's tool registry
	retrieval := mgr.engine.GetRetrieval()
	al.RegisterTool(seahorse.NewGrepTool(retrieval))
	al.RegisterTool(seahorse.NewExpandTool(retrieval))

	// Bootstrap all existing sessions at startup
	if agent.Sessions != nil {
		ctx := context.Background()
		for _, sessionKey := range agent.Sessions.ListSessions() {
			mgr.bootstrapSession(ctx, sessionKey)
		}
	}

	return mgr, nil
}

// providerToCompleteFn wraps providers.LLMProvider as a seahorse.CompleteFn.
func providerToCompleteFn(provider providers.LLMProvider, model string) seahorse.CompleteFn {
	return func(ctx context.Context, prompt string, opts seahorse.CompleteOptions) (string, error) {
		resp, err := provider.Chat(
			ctx,
			[]providers.Message{{Role: "user", Content: prompt}},
			nil, // no tools for summarization
			model,
			map[string]any{
				"max_tokens":       opts.MaxTokens,
				"temperature":      opts.Temperature,
				"prompt_cache_key": "seahorse",
			},
		)
		if err != nil {
			return "", err
		}
		return resp.Content, nil
	}
}

// Assemble builds budget-aware context from seahorse SQLite.
// It also performs semantic context carry: when UserMessage is set, it queries
// neural-memory for topic-relevant long-term memory and appends it to the summary.
func (m *seahorseContextManager) Assemble(ctx context.Context, req *AssembleRequest) (*AssembleResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("seahorse assemble: nil request")
	}

	budget := req.Budget
	if budget <= 0 {
		budget = 100000
	}

	// Reserve space for model response (spec lines 1400-1410)
	effectiveBudget := budget - req.MaxTokens
	if effectiveBudget <= 0 {
		// MaxTokens >= budget is a configuration problem
		// Use 50% as minimum to avoid guaranteed overflow
		logger.WarnCF("agent", "MaxTokens >= budget, using 50% fallback",
			map[string]any{"budget": budget, "max_tokens": req.MaxTokens})
		effectiveBudget = budget / 2
	}

	result, err := m.engine.Assemble(ctx, req.SessionKey, seahorse.AssembleInput{
		Budget: effectiveBudget,
	})
	if err != nil {
		return nil, fmt.Errorf("seahorse assemble: %w", err)
	}

	history := seahorseToProviderMessages(result)

	// Summary is already formatted as XML with system prompt addition by assembler
	summary := result.Summary

	// Semantic Context Carry: enrich summary with neural-memory context.
	// This runs in background to avoid blocking the assemble path.
	if m.semanticRecall != nil && req.UserMessage != "" {
		semanticCtx := m.buildSemanticContext(ctx, req.UserMessage)
		if semanticCtx != "" {
			if summary != "" {
				summary += "\n\n"
			}
			summary += semanticCtx
		}
	}

	return &AssembleResponse{
		History: history,
		Summary: summary,
	}, nil
}

// buildSemanticContext composes neural-memory context from cached recent memories
// and topic-specific recall results.
func (m *seahorseContextManager) buildSemanticContext(ctx context.Context, userMessage string) string {
	var parts []string

	// Part 1: Recent hot memories from cache (O(1), no LLM call)
	if cached := m.semanticRecall.getContext(); cached != "" {
		parts = append(parts, cached)
	}

	// Part 2: Topic-specific recall (runs nmem recall with timeout)
	// Only for substantive messages (> semanticRecallMinLength)
	if len(strings.TrimSpace(userMessage)) >= semanticRecallMinLength {
		if topicCtx := m.semanticRecall.recallForMessage(ctx, userMessage); topicCtx != "" {
			parts = append(parts, topicCtx)
		}
	}

	if len(parts) == 0 {
		return ""
	}

	return "LONG_TERM_MEMORY_CONTEXT (from neural-memory, for reference):\n" +
		strings.Join(parts, "\n---\n") +
		"\n(End of long-term memory context. Use nmem_recall or grep tools to dig deeper.)"
}

// Compact compresses conversation history via seahorse summarization.
func (m *seahorseContextManager) Compact(ctx context.Context, req *CompactRequest) error {
	if req == nil {
		return nil
	}

	// For retry (LLM overflow), use aggressive CompactUntilUnder to guarantee
	// context shrinks below budget (spec lines ~1410).
	if req.Reason == ContextCompressReasonRetry && req.Budget > 0 {
		_, err := m.engine.CompactUntilUnder(ctx, req.SessionKey, req.Budget)
		return err
	}

	_, err := m.engine.Compact(ctx, req.SessionKey, seahorse.CompactInput{
		Force:  req.Reason == ContextCompressReasonRetry,
		Budget: &req.Budget,
	})
	return err
}

// Ingest records a message into seahorse SQLite.
// All existing sessions are bootstrapped at startup, so this only ingests new messages.
func (m *seahorseContextManager) Ingest(ctx context.Context, req *IngestRequest) error {
	if req == nil {
		return nil
	}

	msg := providerToSeahorseMessage(req.Message)
	_, err := m.engine.Ingest(ctx, req.SessionKey, []seahorse.Message{msg})
	return err
}

// Clear removes all stored context for a session (seahorse DB + JSONL).
func (m *seahorseContextManager) Clear(ctx context.Context, sessionKey string) error {
	if err := m.engine.ClearSession(ctx, sessionKey); err != nil {
		return err
	}
	if m.sessions != nil {
		m.sessions.SetHistory(sessionKey, []providers.Message{})
		m.sessions.SetSummary(sessionKey, "")
		return m.sessions.Save(sessionKey)
	}
	return nil
}

// bootstrapSession reconciles JSONL session history into seahorse SQLite.
func (m *seahorseContextManager) bootstrapSession(ctx context.Context, sessionKey string) {
	if m.sessions == nil {
		return
	}

	history := m.sessions.GetHistory(sessionKey)
	if len(history) == 0 {
		return
	}

	// Convert provider messages to seahorse messages
	msgs := make([]seahorse.Message, len(history))
	for i, h := range history {
		msgs[i] = providerToSeahorseMessage(h)
	}

	if err := m.engine.Bootstrap(ctx, sessionKey, msgs); err != nil {
		logger.WarnCF("seahorse", "bootstrap", map[string]any{
			"session": sessionKey,
			"error":   err.Error(),
		})
	}
}

// providerToSeahorseMessage converts a providers.Message to a seahorse.Message.
func providerToSeahorseMessage(msg protocoltypes.Message) seahorse.Message {
	result := seahorse.Message{
		Role:             msg.Role,
		Content:          msg.Content,
		ReasoningContent: msg.ReasoningContent,
		TokenCount:       tokenizer.EstimateMessageTokens(msg),
	}

	// Convert ToolCalls → MessageParts
	for _, tc := range msg.ToolCalls {
		part := seahorse.MessagePart{
			Type:       "tool_use",
			Name:       tc.Function.Name,
			Arguments:  tc.Function.Arguments,
			ToolCallID: tc.ID,
		}
		result.Parts = append(result.Parts, part)
	}

	// Convert tool result
	if msg.ToolCallID != "" {
		part := seahorse.MessagePart{
			Type:       "tool_result",
			ToolCallID: msg.ToolCallID,
			Text:       msg.Content,
		}
		result.Parts = append(result.Parts, part)
	}

	// Convert media attachments
	for _, mediaURI := range msg.Media {
		part := seahorse.MessagePart{
			Type:     "media",
			MediaURI: mediaURI,
		}
		result.Parts = append(result.Parts, part)
	}

	return result
}

// seahorseToProviderMessages converts a seahorse.AssembleResult to []providers.Message.
func seahorseToProviderMessages(result *seahorse.AssembleResult) []protocoltypes.Message {
	messages := make([]protocoltypes.Message, 0, len(result.Messages))

	// Convert assembled messages (which already include summary XML messages)
	for _, msg := range result.Messages {
		pm := protocoltypes.Message{
			Role:             msg.Role,
			Content:          msg.Content,
			ReasoningContent: msg.ReasoningContent,
		}

		// Reconstruct ToolCalls from parts
		for _, part := range msg.Parts {
			if part.Type == "tool_use" {
				pm.ToolCalls = append(pm.ToolCalls, protocoltypes.ToolCall{
					ID:   part.ToolCallID,
					Type: "function", // Required by OpenAI-compatible APIs (GLM, etc.)
					Function: &protocoltypes.FunctionCall{
						Name:      part.Name,
						Arguments: part.Arguments,
					},
				})
			}
			if part.Type == "tool_result" {
				pm.ToolCallID = part.ToolCallID
				if pm.Content == "" && part.Text != "" {
					pm.Content = part.Text
				}
			}
			if part.Type == "media" && part.MediaURI != "" {
				pm.Media = append(pm.Media, part.MediaURI)
			}
		}

		messages = append(messages, pm)
	}

	return messages
}

func init() {
	if err := RegisterContextManager("seahorse", newSeahorseContextManager); err != nil {
		panic(fmt.Sprintf("register seahorse context manager: %v", err))
	}
}
