package agent

import (
        "fmt"
        "math"
        "strings"
        "sync"

        "github.com/sipeed/picoclaw/pkg/logger"
        "github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
        "github.com/sipeed/picoclaw/pkg/routing"
)

// SessionCostTracker tracks session-level cost and token usage.
// It wraps routing.SessionSpend with higher-level operations for
// the /cost command and agent loop.
type SessionCostTracker struct {
        mu    sync.RWMutex
        spend *routing.SessionSpend
}

// NewSessionCostTracker creates a new cost tracker.
func NewSessionCostTracker() *SessionCostTracker {
        return &SessionCostTracker{
                spend: routing.NewSessionSpend(),
        }
}

// RecordLLMUsage records token usage from an API response.
// The model parameter is the model name used for the request.
func (t *SessionCostTracker) RecordLLMUsage(model string, usage *protocoltypes.UsageInfo) {
        if usage == nil {
                return
        }

        cacheHits := usage.PromptCacheHitTokens
        t.spend.RecordUsage(model, usage.PromptTokens, usage.CompletionTokens, cacheHits)

        logger.DebugCF("agent", "LLM usage recorded",
                map[string]any{
                        "model":             model,
                        "prompt_tokens":     usage.PromptTokens,
                        "completion_tokens": usage.CompletionTokens,
                        "cache_hit_tokens":  cacheHits,
                        "total_cost_usd":    fmt.Sprintf("%.6f", t.TotalCostUSD()),
                })
}

// GetSpend returns a read-only snapshot of the current spend.
func (t *SessionCostTracker) GetSpend() routing.SpendSnapshot {
        return t.spend.Snapshot()
}

// TotalCostUSD returns the total estimated cost in USD.
func (t *SessionCostTracker) TotalCostUSD() float64 {
        return t.spend.TotalSpendUSD()
}

// CacheHitRate returns the cache hit rate as a percentage (0-100).
// Returns 0 if no input tokens have been processed.
func (t *SessionCostTracker) CacheHitRate() float64 {
        snap := t.spend.Snapshot()
        totalInput := snap.TotalInputTokens
        if totalInput == 0 {
                return 0
        }
        return float64(snap.TotalCacheHits) / float64(totalInput) * 100
}

// CostBreakdown returns a formatted string for the /cost command.
// If modelName is provided, it shows additional model-specific details.
func (t *SessionCostTracker) CostBreakdown(modelName string) string {
        snap := t.spend.Snapshot()

        var sb strings.Builder
        sb.WriteString("Session Cost Breakdown\n")
        sb.WriteString(fmt.Sprintf("  Total cost: $%.6f\n", snap.TotalCostUSD))

        if snap.BudgetUSD > 0 {
                percent := snap.TotalCostUSD / snap.BudgetUSD * 100
                sb.WriteString(fmt.Sprintf("  Budget: $%.2f (%.1f%% used)\n", snap.BudgetUSD, percent))
                remaining := snap.BudgetUSD - snap.TotalCostUSD
                if remaining < 0 {
                        remaining = 0
                }
                sb.WriteString(fmt.Sprintf("  Remaining: $%.6f\n", remaining))
        }

        sb.WriteString(fmt.Sprintf("  Input tokens: %s\n", FormatNumber(snap.TotalInputTokens)))
        sb.WriteString(fmt.Sprintf("  Output tokens: %s\n", FormatNumber(snap.TotalOutputTokens)))
        sb.WriteString(fmt.Sprintf("  Cache hits: %s\n", FormatNumber(snap.TotalCacheHits)))

        hitRate := t.CacheHitRate()
        sb.WriteString(fmt.Sprintf("  Cache hit rate: %.1f%%\n", hitRate))

        if len(snap.PerModel) > 0 {
                sb.WriteString("\n  Per-model breakdown:\n")
                for model, entry := range snap.PerModel {
                        sb.WriteString(fmt.Sprintf("    %s:\n", model))
                        sb.WriteString(fmt.Sprintf("      Input: %s tokens\n", FormatNumber(entry.InputTokens)))
                        sb.WriteString(fmt.Sprintf("      Output: %s tokens\n", FormatNumber(entry.OutputTokens)))
                        sb.WriteString(fmt.Sprintf("      Cache hits: %s tokens\n", FormatNumber(entry.CacheHits)))
                        sb.WriteString(fmt.Sprintf("      Cost: $%.6f\n", entry.CostUSD))
                }
        }

        if modelName != "" {
                sb.WriteString(fmt.Sprintf("\n  Current model: %s\n", modelName))
        }

        return sb.String()
}

// SetBudget sets an optional budget limit in USD.
func (t *SessionCostTracker) SetBudget(budgetUSD float64) {
        t.spend.SetBudget(budgetUSD)
        logger.InfoCF("agent", "Session budget set",
                map[string]any{
                        "budget_usd": fmt.Sprintf("%.2f", budgetUSD),
                })
}

// IsApproachingBudget returns true when approaching the budget limit.
// Logs a warning when the budget is near or exceeded.
func (t *SessionCostTracker) IsApproachingBudget() bool {
        approaching := t.spend.IsApproachingBudget()
        if approaching {
                snap := t.spend.Snapshot()
                percent := 0.0
                if snap.BudgetUSD > 0 {
                        percent = snap.TotalCostUSD / snap.BudgetUSD * 100
                }
                logLevel := "approaching"
                if snap.TotalCostUSD >= snap.BudgetUSD {
                        logLevel = "exceeded"
                }
                logger.WarnCF("agent", "Session budget "+logLevel,
                        map[string]any{
                                "budget_usd":    fmt.Sprintf("%.2f", snap.BudgetUSD),
                                "spent_usd":     fmt.Sprintf("%.6f", snap.TotalCostUSD),
                                "percent_used":  fmt.Sprintf("%.1f", percent),
                                "total_input":   snap.TotalInputTokens,
                                "total_output":  snap.TotalOutputTokens,
                        })
        }
        return approaching
}

// Spend returns the underlying SessionSpend for direct access.
func (t *SessionCostTracker) Spend() *routing.SessionSpend {
        return t.spend
}

// FormatNumber formats an integer with comma separators.
// E.g., 1234567 → "1,234,567"
func FormatNumber(n int) string {
        if n < 0 {
                return "-" + FormatNumber(-n)
        }

        s := fmt.Sprintf("%d", n)
        if len(s) <= 3 {
                return s
        }

        // Insert commas from right
        var result strings.Builder
        remainder := len(s) % 3
        if remainder > 0 {
                result.WriteString(s[:remainder])
        }
        for i := remainder; i < len(s); i += 3 {
                if result.Len() > 0 {
                        result.WriteByte(',')
                }
                result.WriteString(s[i : i+3])
        }

        return result.String()
}

// formatFloat formats a float with the given precision, trimming trailing zeros.
func formatFloat(f float64, prec int) string {
        s := fmt.Sprintf(fmt.Sprintf("%%.%df", prec), f)
        // Trim trailing zeros after decimal point
        if strings.Contains(s, ".") {
                s = strings.TrimRight(s, "0")
                s = strings.TrimRight(s, ".")
        }
        return s
}

// roundTo6 rounds a float64 to 6 decimal places.
func roundTo6(f float64) float64 {
        return math.Round(f*1e6) / 1e6
}
