package agent

import (
        "context"
        "fmt"
        "strings"
        "sync"
        "time"

        "github.com/sipeed/picoclaw/pkg/logger"
        "github.com/sipeed/picoclaw/pkg/providers"
)

// legacyContextManager wraps the existing summarization/compression logic
// as a ContextManager implementation. It is the default when no other
// ContextManager is configured.
type legacyContextManager struct {
        al          *AgentLoop
        summarizing sync.Map // dedup for async Compact (post-turn)
}

func (m *legacyContextManager) Assemble(_ context.Context, req *AssembleRequest) (*AssembleResponse, error) {
        // Legacy: read history from session, return as-is.
        // Budget enforcement happens in BuildMessages caller via
        // isOverContextBudget + forceCompression.
        agent := m.al.registry.GetDefaultAgent()
        if agent == nil {
                return &AssembleResponse{}, nil
        }
        history := agent.Sessions.GetHistory(req.SessionKey)
        summary := agent.Sessions.GetSummary(req.SessionKey)
        return &AssembleResponse{
                History: history,
                Summary: summary,
        }, nil
}

func (m *legacyContextManager) Compact(_ context.Context, req *CompactRequest) error {
        switch req.Reason {
        case ContextCompressReasonProactive, ContextCompressReasonRetry:
                // Sync emergency compression — budget exceeded.
                // When Partition is set (e.g., "history"), use targeted compression
                // that drops only the oldest turns rather than the generic 50% cut.
                if result, ok := m.forceCompression(req.SessionKey, req.Partition, req.Model); ok {
                        m.al.emitEvent(
                                EventKindContextCompress,
                                m.al.newTurnEventScope("", req.SessionKey, nil).meta(0, "forceCompression", "turn.context.compress"),
                                ContextCompressPayload{
                                        Reason:            req.Reason,
                                        DroppedMessages:   result.DroppedMessages,
                                        RemainingMessages: result.RemainingMessages,
                                },
                        )
                }
        case ContextCompressReasonSummarize:
                m.maybeSummarize(req.SessionKey)
        }
        return nil
}

func (m *legacyContextManager) Ingest(_ context.Context, _ *IngestRequest) error {
        // Legacy: no-op. Messages are persisted by Sessions JSONL.
        return nil
}

func (m *legacyContextManager) Clear(_ context.Context, sessionKey string) error {
        agent := m.al.registry.GetDefaultAgent()
        if agent == nil || agent.Sessions == nil {
                return fmt.Errorf("sessions not initialized")
        }
        agent.Sessions.SetHistory(sessionKey, []providers.Message{})
        agent.Sessions.SetSummary(sessionKey, "")
        return agent.Sessions.Save(sessionKey)
}

// maybeSummarize triggers summarization if the session history exceeds thresholds.
// It runs asynchronously in a goroutine.
func (m *legacyContextManager) maybeSummarize(sessionKey string) {
        agent := m.al.registry.GetDefaultAgent()
        if agent == nil {
                return
        }

        // FullContextMode: skip summarization entirely, only allow emergency compression
        if agent.FullContextMode {
                return
        }

        newHistory := agent.Sessions.GetHistory(sessionKey)
        tokenEstimate := m.estimateTokens(newHistory)

        // Adaptive compression thresholds based on context window size
        messageThreshold := agent.SummarizeMessageThreshold
        tokenPercent := agent.SummarizeTokenPercent

        switch agent.CompressionStrategy {
        case "adaptive":
                // Scale message threshold proportionally to context window
                // For 128K context with ~2K tokens/turn: threshold ≈ 48 messages at 75% fill
                // For 1M context with ~2K tokens/turn: threshold ≈ 375 messages at 75% fill
                const averageTurnTokens = 2000
                const targetFillPercent = 75
                adaptiveMsgThreshold := agent.ContextWindow * targetFillPercent / 100 / averageTurnTokens
                if adaptiveMsgThreshold > messageThreshold {
                        messageThreshold = adaptiveMsgThreshold
                }
                // Scale token percent based on context window size
                if agent.ContextWindow > 512000 {
                        tokenPercent = 90
                } else if agent.ContextWindow > 128000 {
                        tokenPercent = 85
                }
        case "conservative":
                // Only compress on proactive budget check, never on message count
                messageThreshold = int(^uint(0) >> 1) // MaxInt — effectively disables count-based trigger
                if agent.ContextWindow > 512000 {
                        tokenPercent = 95
                } else if agent.ContextWindow > 128000 {
                        tokenPercent = 90
                }
        default: // "eager"
                // Current behavior — fixed thresholds
        }

        threshold := agent.ContextWindow * tokenPercent / 100

        if len(newHistory) > messageThreshold || tokenEstimate > threshold {
                summarizeKey := agent.ID + ":" + sessionKey
                if _, loading := m.summarizing.LoadOrStore(summarizeKey, true); !loading {
                        go func() {
                                defer m.summarizing.Delete(summarizeKey)
                                defer func() {
                                        if r := recover(); r != nil {
                                                logger.WarnCF("agent", "Summarization panic recovered", map[string]any{
                                                        "session_key": sessionKey,
                                                        "panic":       r,
                                                })
                                        }
                                }()
                                logger.Debug("Memory threshold reached. Optimizing conversation history...")
                                m.summarizeSession(agent, sessionKey)
                        }()
                }
        }
}

type compressionResult struct {
        DroppedMessages   int
        RemainingMessages int
}

// forceCompression aggressively reduces context when the limit is hit.
// When partition is "history", it uses targeted compression: drops only the
// oldest turns (one at a time) until the history partition fits within its
// budget, preserving the most recent context. This is more efficient than the
// generic 50% cut for partition-aware workflows like DeepSeek V4's 1M context.
// When partition is empty or any other value, the original 50% cut is used.
func (m *legacyContextManager) forceCompression(sessionKey string, partition string, model string) (compressionResult, bool) {
        agent := m.al.registry.GetDefaultAgent()
        if agent == nil {
                return compressionResult{}, false
        }

        history := agent.Sessions.GetHistory(sessionKey)
        if len(history) <= 2 {
                return compressionResult{}, false
        }

        // Targeted compression for history partition overflow:
        // Instead of cutting 50% of turns, progressively drop the oldest
        // turn until the history partition fits within its budget. This
        // preserves more recent context and is especially important for
        // V4 models with 1M context where a 50% cut is wasteful.
        if partition == "partition overflow: history" && agent.ContextPartition != nil {
                return m.targetedHistoryCompression(agent, sessionKey, model)
        }

        // Default: generic 50% cut at turn boundary
        turns := parseTurnBoundaries(history)
        var mid int
        if len(turns) >= 2 {
                mid = turns[len(turns)/2]
        } else {
                mid = findSafeBoundary(history, len(history)/2)
        }
        var keptHistory []providers.Message
        if mid <= 0 {
                for i := len(history) - 1; i >= 0; i-- {
                        if history[i].Role == "user" {
                                keptHistory = []providers.Message{history[i]}
                                break
                        }
                }
        } else {
                keptHistory = history[mid:]
        }

        droppedCount := len(history) - len(keptHistory)
        m.applyCompression(agent, sessionKey, history, 0, droppedCount, keptHistory)
        return compressionResult{
                DroppedMessages:   droppedCount,
                RemainingMessages: len(keptHistory),
        }, true
}

// targetedHistoryCompression drops the oldest turns one at a time until
// the history partition fits within its budget. This is more surgical than
// the generic 50% cut: it preserves the maximum amount of recent context
// while still resolving the overflow. Uses model-aware estimation when
// the model is available.
func (m *legacyContextManager) targetedHistoryCompression(agent *AgentInstance, sessionKey string, model string) (compressionResult, bool) {
        history := agent.Sessions.GetHistory(sessionKey)
        if len(history) <= 2 {
                return compressionResult{}, false
        }

        budgets := ComputePartitionBudgets(agent.ContextPartition, agent.ContextWindow)
        if budgets == nil || budgets.HistoryTokens <= 0 {
                // No valid partition budget — fall back to generic 50% cut
                return m.forceCompression(sessionKey, "", model)
        }

        // Compute initial history token estimate
        estimateHistoryTokens := func(msgs []providers.Message) int {
                total := 0
                for _, msg := range msgs {
                        if model != "" {
                                total += EstimateMessageTokensForModel(msg, model)
                        } else {
                                total += EstimateMessageTokens(msg)
                        }
                }
                return total
        }

        // Progressively drop the oldest turn until under budget.
        // We iterate from the oldest turn boundary, removing one turn
        // at a time, until the history tokens fit within the partition budget.
        turns := parseTurnBoundaries(history)
        cutIndex := 0
        keptHistory := history

        for {
                currentTokens := estimateHistoryTokens(keptHistory)
                if currentTokens <= budgets.HistoryTokens {
                        break // Under budget
                }

                // Find the next turn boundary to cut at
                if len(turns) == 0 {
                        break
                }

                // Remove the oldest turn: advance cutIndex to the next turn boundary
                nextCut := turns[0]
                turns = turns[1:]

                // Don't cut everything — always keep at least the last turn
                if nextCut >= len(history)-1 {
                        break
                }

                cutIndex = nextCut
                keptHistory = history[cutIndex:]

                if len(keptHistory) <= 1 {
                        break
                }
        }

        if len(keptHistory) == len(history) {
                // Could not reduce — fall back to generic 50% cut
                return m.forceCompression(sessionKey, "", model)
        }

        droppedCount := len(history) - len(keptHistory)
        m.applyCompression(agent, sessionKey, history, cutIndex, droppedCount, keptHistory)
        return compressionResult{
                DroppedMessages:   droppedCount,
                RemainingMessages: len(keptHistory),
        }, true
}

// applyCompression applies the compression result to the session: updates
// the summary with a compression note, logs reasoning content loss, and
// persists the new history.
func (m *legacyContextManager) applyCompression(agent *AgentInstance, sessionKey string, history []providers.Message, cutIndex int, droppedCount int, keptHistory []providers.Message) {
        existingSummary := agent.Sessions.GetSummary(sessionKey)
        compressionNote := fmt.Sprintf(
                "[Emergency compression dropped %d oldest messages due to context limit]",
                droppedCount,
        )
        if existingSummary != "" {
                compressionNote = existingSummary + "\n\n" + compressionNote
        }
        agent.Sessions.SetSummary(sessionKey, compressionNote)

        // Count reasoning tokens lost
        if cutIndex > 0 && cutIndex <= len(history) {
                reasoningLost := 0
                for _, msg := range history[:cutIndex] {
                        if msg.ReasoningContent != "" {
                                reasoningLost += len(msg.ReasoningContent) / 2 // rough token estimate
                        }
                }
                if reasoningLost > 0 {
                        logger.WarnCF("agent", "Emergency compression dropped reasoning content", map[string]any{
                                "session_key":              sessionKey,
                                "reasoning_tokens_lost_est": reasoningLost,
                        })
                }
        }

        agent.Sessions.SetHistory(sessionKey, keptHistory)
        agent.Sessions.Save(sessionKey)

        logFields := map[string]any{
                "session_key":  sessionKey,
                "dropped_msgs": droppedCount,
                "new_count":    len(keptHistory),
        }
        if agent.FullContextMode {
                logFields["full_context_mode"] = true
        }
        logger.WarnCF("agent", "Forced compression executed", logFields)
}

func (m *legacyContextManager) summarizeSession(agent *AgentInstance, sessionKey string) {
        ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
        defer cancel()

        history := agent.Sessions.GetHistory(sessionKey)
        summary := agent.Sessions.GetSummary(sessionKey)

        if len(history) <= 4 {
                return
        }

        safeCut := findSafeBoundary(history, len(history)-4)
        if safeCut <= 0 {
                return
        }
        keepCount := len(history) - safeCut
        toSummarize := history[:safeCut]

        maxMessageTokens := agent.ContextWindow / 2
        validMessages := make([]providers.Message, 0)
        omitted := false

        for _, msg := range toSummarize {
                if msg.Role != "user" && msg.Role != "assistant" {
                        continue
                }
                msgTokens := len(msg.Content) / 2
                if msgTokens > maxMessageTokens {
                        omitted = true
                        continue
                }
                validMessages = append(validMessages, msg)
        }

        if len(validMessages) == 0 {
                return
        }

        const (
                maxSummarizationMessages = 10
                llmMaxRetries            = 3
        )

        var finalSummary string
        if len(validMessages) > maxSummarizationMessages {
                mid := len(validMessages) / 2
                mid = m.findNearestUserMessage(validMessages, mid)

                part1 := validMessages[:mid]
                part2 := validMessages[mid:]

                s1, _ := m.summarizeBatch(ctx, agent, part1, "")
                s2, _ := m.summarizeBatch(ctx, agent, part2, "")

                mergePrompt := fmt.Sprintf(
                        "Merge these two conversation summaries into one cohesive summary:\n\n1: %s\n\n2: %s",
                        s1, s2,
                )

                resp, err := m.retryLLMCall(ctx, agent, mergePrompt, llmMaxRetries)
                if err == nil && resp.Content != "" {
                        finalSummary = resp.Content
                } else {
                        finalSummary = s1 + " " + s2
                }
        } else {
                finalSummary, _ = m.summarizeBatch(ctx, agent, validMessages, summary)
        }

        if omitted && finalSummary != "" {
                finalSummary += "\n[Note: Some oversized messages were omitted from this summary for efficiency.]"
        }

        if finalSummary != "" {
                agent.Sessions.SetSummary(sessionKey, finalSummary)
                agent.Sessions.TruncateHistory(sessionKey, keepCount)
                agent.Sessions.Save(sessionKey)
                m.al.emitEvent(
                        EventKindSessionSummarize,
                        m.al.newTurnEventScope(agent.ID, sessionKey, nil).meta(0, "summarizeSession", "turn.session.summarize"),
                        SessionSummarizePayload{
                                SummarizedMessages: len(validMessages),
                                KeptMessages:       keepCount,
                                SummaryLen:         len(finalSummary),
                                OmittedOversized:   omitted,
                        },
                )
        }
}

func (m *legacyContextManager) findNearestUserMessage(messages []providers.Message, mid int) int {
        originalMid := mid

        for mid > 0 && messages[mid].Role != "user" {
                mid--
        }

        if messages[mid].Role == "user" {
                return mid
        }

        mid = originalMid
        for mid < len(messages) && messages[mid].Role != "user" {
                mid++
        }

        if mid < len(messages) {
                return mid
        }

        return originalMid
}

func (m *legacyContextManager) retryLLMCall(
        ctx context.Context,
        agent *AgentInstance,
        prompt string,
        maxRetries int,
) (*providers.LLMResponse, error) {
        const llmTemperature = 0.3

        var resp *providers.LLMResponse
        var err error

        for attempt := 0; attempt < maxRetries; attempt++ {
                m.al.activeRequests.Add(1)
                resp, err = func() (*providers.LLMResponse, error) {
                        defer m.al.activeRequests.Done()
                        return agent.Provider.Chat(
                                ctx,
                                []providers.Message{{Role: "user", Content: prompt}},
                                nil,
                                agent.Model,
                                map[string]any{
                                        "max_tokens":       agent.MaxTokens,
                                        "temperature":      llmTemperature,
                                        "prompt_cache_key": agent.ID,
                                },
                        )
                }()

                if err == nil && resp != nil && resp.Content != "" {
                        return resp, nil
                }
                if attempt < maxRetries-1 {
                        time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
                }
        }

        return resp, err
}

func (m *legacyContextManager) summarizeBatch(
        ctx context.Context,
        agent *AgentInstance,
        batch []providers.Message,
        existingSummary string,
) (string, error) {
        const (
                llmMaxRetries             = 3
                fallbackMinContentLength  = 200
                fallbackMaxContentPercent = 10
        )

        var sb strings.Builder
        sb.WriteString("Provide a concise summary of this conversation segment, preserving core context and key points.\n")
        if existingSummary != "" {
                sb.WriteString("Existing context: ")
                sb.WriteString(existingSummary)
                sb.WriteString("\n")
        }
        sb.WriteString("\nCONVERSATION:\n")
        for _, msg := range batch {
                fmt.Fprintf(&sb, "%s: %s\n", msg.Role, msg.Content)
                if msg.ReasoningContent != "" {
                        fmt.Fprintf(&sb, "[reasoning]: %s\n", msg.ReasoningContent)
                }
        }
        prompt := sb.String()

        response, err := m.retryLLMCall(ctx, agent, prompt, llmMaxRetries)
        if err == nil && response.Content != "" {
                return strings.TrimSpace(response.Content), nil
        }

        var fallback strings.Builder
        fallback.WriteString("Conversation summary: ")
        for i, msg := range batch {
                if i > 0 {
                        fallback.WriteString(" | ")
                }
                content := strings.TrimSpace(msg.Content)
                if msg.ReasoningContent != "" {
                        content += "\n[reasoning]: " + strings.TrimSpace(msg.ReasoningContent)
                }
                runes := []rune(content)
                if len(runes) == 0 {
                        fallback.WriteString(fmt.Sprintf("%s: ", msg.Role))
                        continue
                }

                keepLength := len(runes) * fallbackMaxContentPercent / 100
                if keepLength < fallbackMinContentLength {
                        keepLength = fallbackMinContentLength
                }
                if keepLength > len(runes) {
                        keepLength = len(runes)
                }

                content = string(runes[:keepLength])
                if keepLength < len(runes) {
                        content += "..."
                }
                fallback.WriteString(fmt.Sprintf("%s: %s", msg.Role, content))
        }
        return fallback.String(), nil
}

func (m *legacyContextManager) estimateTokens(messages []providers.Message) int {
        total := 0
        for _, msg := range messages {
                total += EstimateMessageTokens(msg)
        }
        return total
}
