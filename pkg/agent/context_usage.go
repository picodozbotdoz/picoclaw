package agent

import (
        "github.com/sipeed/picoclaw/pkg/bus"
        "github.com/sipeed/picoclaw/pkg/logger"
        "github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

// computeContextUsage estimates current context window consumption for the
// given agent and session. Includes history, system prompt (with dynamic context,
// summary, and skills — mirroring BuildMessages composition), and tool definitions.
// The output reserve (MaxTokens) is not counted as "used" but reduces the
// effective budget, matching isOverContextBudget's compression trigger:
//
//      compress when: history + system + tools + maxTokens > contextWindow
//      equivalent to: history + system + tools > contextWindow - maxTokens
//
// When the agent has a ContextPartition configuration, per-partition token
// utilization is also computed for budget enforcement telemetry.
// Returns nil when the agent or session is unavailable.
func computeContextUsage(agent *AgentInstance, sessionKey string) *bus.ContextUsage {
        if agent == nil || agent.Sessions == nil {
                return nil
        }
        contextWindow := agent.ContextWindow
        if contextWindow <= 0 {
                return nil
        }

        // History tokens — use model-aware estimation when model is available
        history := agent.Sessions.GetHistory(sessionKey)
        historyTokens := 0
        for _, m := range history {
                historyTokens += EstimateMessageTokensForModel(m, agent.Model)
        }

        // System message tokens: uses EstimateSystemTokens which mirrors
        // the full system message composition in BuildMessages (static prompt,
        // dynamic context, active skills, summary with wrapping prefix).
        systemTokens := 0
        if agent.ContextBuilder != nil {
                summary := agent.Sessions.GetSummary(sessionKey)
                // Pass nil for active skills: skills are only injected when the user
                // explicitly activates them via /use, which is rare. Using nil matches
                // the common case and avoids over-counting all installed skills.
                systemTokens = agent.ContextBuilder.EstimateSystemTokens(summary, nil)
        }

        // Tool definition tokens — use model-aware estimation
        toolTokens := 0
        if agent.Tools != nil {
                toolTokens = EstimateToolDefsTokensForModel(agent.Tools.ToProviderDefs(), agent.Model)
        }

        // Used = history + system (includes summary) + tools
        usedTokens := historyTokens + systemTokens + toolTokens

        // Effective budget = contextWindow minus output reserve (maxTokens)
        effectiveWindow := contextWindow - agent.MaxTokens
        if effectiveWindow < 0 {
                effectiveWindow = contextWindow
        }

        // compressAt = effectiveWindow: aligns with isOverContextBudget's
        // proactive trigger (msgTokens + toolTokens + maxTokens > contextWindow).
        compressAt := effectiveWindow

        usedPercent := 0
        if compressAt > 0 {
                usedPercent = usedTokens * 100 / compressAt
        }
        if usedPercent > 100 {
                usedPercent = 100
        }

        usage := &bus.ContextUsage{
                UsedTokens:       usedTokens,
                TotalTokens:      contextWindow,
                CompressAtTokens: compressAt,
                UsedPercent:      usedPercent,
        }

        // WS 3.3: Populate partition breakdown when ContextPartition is configured.
        // This provides per-partition token utilization for budget enforcement
        // and monitoring, enabling "full codebase in context" workflows.
        if agent.ContextPartition != nil {
                usage.SystemPromptTokens = systemTokens
                usage.HistoryTokens = historyTokens
                usage.ToolDefTokens = toolTokens
                // WS 4.1: Populate InjectedContextTokens from the store's TotalTokens().
                if agent.InjectedContext != nil {
                        usage.InjectedContextTokens = agent.InjectedContext.TotalTokens()
                }
        }

        return usage
}

// UpdateCacheStats populates cache-related fields on a ContextUsage from
// provider API response usage data. This is called after each LLM response
// when the provider reports prefix cache statistics (e.g. DeepSeek V4's
// prompt_cache_hit_tokens in the usage block).
func UpdateCacheStats(usage *bus.ContextUsage, respUsage *protocoltypes.UsageInfo) {
        if usage == nil || respUsage == nil {
                return
        }
        usage.CacheHitTokens = respUsage.PromptCacheHitTokens
        // Cache miss tokens = total prompt tokens minus cache hit tokens.
        // This matches DeepSeek V4's convention where prompt_tokens is the
        // total and prompt_cache_hit_tokens is the subset served from cache.
        if respUsage.PromptTokens > respUsage.PromptCacheHitTokens {
                usage.CacheMissTokens = respUsage.PromptTokens - respUsage.PromptCacheHitTokens
        }
}

// UpdateUsageFromAPI replaces heuristic token estimates with accurate counts
// from the provider API response. When streaming with include_usage or using
// non-streaming calls that return usage data, the API reports exact
// prompt_tokens, completion_tokens, and prompt_cache_hit_tokens.
// This is more accurate than the EstimateMessageTokens heuristic and should
// be preferred whenever API usage data is available.
func UpdateUsageFromAPI(usage *bus.ContextUsage, respUsage *protocoltypes.UsageInfo) {
        if usage == nil || respUsage == nil {
                return
        }
        // Update cache stats
        UpdateCacheStats(usage, respUsage)

        // Update reasoning and output token breakdown
        if respUsage.CompletionTokens > 0 {
                usage.OutputTokens = respUsage.CompletionTokens
        }
        // CompletionTokensDetails may contain reasoning token info
        if respUsage.CompletionTokensDetails != nil {
                if respUsage.CompletionTokensDetails.ReasoningTokens > 0 {
                        usage.ReasoningTokens = respUsage.CompletionTokensDetails.ReasoningTokens
                }
        }

        // Log when API data replaces heuristics for observability
        logger.DebugCF("agent", "Context usage updated from API",
                map[string]any{
                        "prompt_tokens":     respUsage.PromptTokens,
                        "completion_tokens": respUsage.CompletionTokens,
                        "cache_hit_tokens":  respUsage.PromptCacheHitTokens,
                        "reasoning_tokens":  usage.ReasoningTokens,
                        "used_percent":      usage.UsedPercent,
                })
}
