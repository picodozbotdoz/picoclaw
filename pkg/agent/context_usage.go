package agent

import (
        "github.com/sipeed/picoclaw/pkg/bus"
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
// Returns nil when the agent or session is unavailable.
func computeContextUsage(agent *AgentInstance, sessionKey string) *bus.ContextUsage {
        if agent == nil || agent.Sessions == nil {
                return nil
        }
        contextWindow := agent.ContextWindow
        if contextWindow <= 0 {
                return nil
        }

        // History tokens
        history := agent.Sessions.GetHistory(sessionKey)
        historyTokens := 0
        for _, m := range history {
                historyTokens += EstimateMessageTokens(m)
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

        // Tool definition tokens
        toolTokens := 0
        if agent.Tools != nil {
                toolTokens = EstimateToolDefsTokens(agent.Tools.ToProviderDefs())
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

        return &bus.ContextUsage{
                UsedTokens:       usedTokens,
                TotalTokens:      contextWindow,
                CompressAtTokens: compressAt,
                UsedPercent:      usedPercent,
        }
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
