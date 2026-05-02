package commands

import (
        "context"
        "fmt"
)

func contextCommand() Definition {
        return Definition{
                Name:        "context",
                Description: "Show current session context and token usage",
                Usage:       "/context",
                Handler: func(_ context.Context, req Request, rt *Runtime) error {
                        if rt == nil || rt.GetContextStats == nil {
                                return req.Reply(unavailableMsg)
                        }
                        stats := rt.GetContextStats()
                        if stats == nil {
                                return req.Reply("No active session context.")
                        }
                        return req.Reply(formatContextStats(stats))
                },
        }
}

func formatContextStats(s *ContextStats) string {
        remaining := s.CompressAtTokens - s.UsedTokens
        if remaining < 0 {
                remaining = 0
        }
        usedWindowPercent := s.UsedTokens * 100 / max(s.TotalTokens, 1)

        var result string
        result = fmt.Sprintf(
                "Context usage  \nMessages: %d  \nUsed: ~%d / %d tokens (%d%%)  \nCompress at: %d tokens  \nCompression progress: %d%%  \nRemaining: ~%d tokens",
                s.MessageCount,
                s.UsedTokens,
                s.TotalTokens,
                usedWindowPercent,
                s.CompressAtTokens,
                s.UsedPercent,
                remaining,
        )

        // WS 4.3: Show cache statistics when available
        if s.CacheHitTokens > 0 || s.CacheMissTokens > 0 {
                totalInput := s.CacheHitTokens + s.CacheMissTokens
                hitRate := 0.0
                if totalInput > 0 {
                        hitRate = float64(s.CacheHitTokens) / float64(totalInput) * 100
                }
                result += fmt.Sprintf(
                        "  \n\nCache Statistics:  \n  Cache hits: %d tokens  \n  Cache misses: %d tokens  \n  Hit rate: %.1f%%",
                        s.CacheHitTokens,
                        s.CacheMissTokens,
                        hitRate,
                )
        }

        // WS 4.3: Show partition breakdown when available
        if s.SystemPromptTokens > 0 || s.InjectedContextTokens > 0 || s.HistoryTokens > 0 || s.ToolDefTokens > 0 {
                // Compute history tokens from UsedTokens minus known partitions when not directly provided
                historyTokens := s.HistoryTokens
                if historyTokens == 0 && s.UsedTokens > 0 {
                        historyTokens = s.UsedTokens - s.SystemPromptTokens - s.InjectedContextTokens - s.ToolDefTokens
                        if historyTokens < 0 {
                                historyTokens = 0
                        }
                }
                result += fmt.Sprintf(
                        "  \n\nPartition Breakdown:  \n  System prompt: ~%d tokens  \n  Injected context: %d tokens  \n  History: ~%d tokens  \n  Tools: ~%d tokens",
                        s.SystemPromptTokens,
                        s.InjectedContextTokens,
                        historyTokens,
                        s.ToolDefTokens,
                )
        }

        return result
}
