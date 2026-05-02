package agent

import (
        "math"
        "strings"
        "testing"

        "github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

// ── SessionCostTracker construction ─────────────────────────────────────────

func TestNewSessionCostTracker(t *testing.T) {
        tracker := NewSessionCostTracker()
        if tracker == nil {
                t.Fatal("tracker should not be nil")
        }
        if tracker.TotalCostUSD() != 0 {
                t.Error("new tracker should have zero cost")
        }
}

// ── RecordLLMUsage ──────────────────────────────────────────────────────────

func TestSessionCostTracker_RecordLLMUsage_Basic(t *testing.T) {
        tracker := NewSessionCostTracker()
        usage := &protocoltypes.UsageInfo{
                PromptTokens:     1000,
                CompletionTokens: 200,
                TotalTokens:      1200,
        }
        tracker.RecordLLMUsage("deepseek-v4-flash", usage)
        snap := tracker.GetSpend()
        if snap.TotalInputTokens != 1000 {
                t.Errorf("input tokens: got %d, want 1000", snap.TotalInputTokens)
        }
        if snap.TotalOutputTokens != 200 {
                t.Errorf("output tokens: got %d, want 200", snap.TotalOutputTokens)
        }
}

func TestSessionCostTracker_RecordLLMUsage_Nil(t *testing.T) {
        tracker := NewSessionCostTracker()
        tracker.RecordLLMUsage("deepseek-v4-flash", nil)
        if tracker.TotalCostUSD() != 0 {
                t.Error("nil usage should not change cost")
        }
}

func TestSessionCostTracker_RecordLLMUsage_WithCache(t *testing.T) {
        tracker := NewSessionCostTracker()
        usage := &protocoltypes.UsageInfo{
                PromptTokens:          1000,
                CompletionTokens:      200,
                PromptCacheHitTokens:  800,
                TotalTokens:           1200,
        }
        tracker.RecordLLMUsage("deepseek-v4-flash", usage)
        snap := tracker.GetSpend()
        if snap.TotalCacheHits != 800 {
                t.Errorf("cache hits: got %d, want 800", snap.TotalCacheHits)
        }
}

func TestSessionCostTracker_RecordLLMUsage_MultipleCalls(t *testing.T) {
        tracker := NewSessionCostTracker()
        usage1 := &protocoltypes.UsageInfo{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}
        usage2 := &protocoltypes.UsageInfo{PromptTokens: 500, CompletionTokens: 50, TotalTokens: 550}
        tracker.RecordLLMUsage("deepseek-v4-flash", usage1)
        tracker.RecordLLMUsage("deepseek-v4-flash", usage2)
        snap := tracker.GetSpend()
        if snap.TotalInputTokens != 1500 {
                t.Errorf("accumulated input: got %d, want 1500", snap.TotalInputTokens)
        }
}

func TestSessionCostTracker_RecordLLMUsage_MultipleModels(t *testing.T) {
        tracker := NewSessionCostTracker()
        flashUsage := &protocoltypes.UsageInfo{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}
        proUsage := &protocoltypes.UsageInfo{PromptTokens: 500, CompletionTokens: 50, TotalTokens: 550}
        tracker.RecordLLMUsage("deepseek-v4-flash", flashUsage)
        tracker.RecordLLMUsage("deepseek-v4-pro", proUsage)
        snap := tracker.GetSpend()
        if len(snap.PerModel) != 2 {
                t.Errorf("per-model count: got %d, want 2", len(snap.PerModel))
        }
}

// ── TotalCostUSD ────────────────────────────────────────────────────────────

func TestSessionCostTracker_TotalCostUSD_FlashOnly(t *testing.T) {
        tracker := NewSessionCostTracker()
        usage := &protocoltypes.UsageInfo{
                PromptTokens:     1_000_000,
                CompletionTokens: 1_000_000,
                TotalTokens:      2_000_000,
        }
        tracker.RecordLLMUsage("deepseek-v4-flash", usage)
        total := tracker.TotalCostUSD()
        // V4-Flash: (1M * 0.14 + 1M * 0.28) / 1M = 0.42
        if math.Abs(total-0.42) > 0.0001 {
                t.Errorf("total cost: got %f, want ~0.42", total)
        }
}

func TestSessionCostTracker_TotalCostUSD_Zero(t *testing.T) {
        tracker := NewSessionCostTracker()
        if tracker.TotalCostUSD() != 0 {
                t.Error("empty tracker should have zero cost")
        }
}

// ── CacheHitRate ────────────────────────────────────────────────────────────

func TestSessionCostTracker_CacheHitRate_Zero(t *testing.T) {
        tracker := NewSessionCostTracker()
        if tracker.CacheHitRate() != 0 {
                t.Error("empty tracker should have zero cache hit rate")
        }
}

func TestSessionCostTracker_CacheHitRate_NoCache(t *testing.T) {
        tracker := NewSessionCostTracker()
        usage := &protocoltypes.UsageInfo{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}
        tracker.RecordLLMUsage("deepseek-v4-flash", usage)
        if tracker.CacheHitRate() != 0 {
                t.Error("no cache hits should give 0% rate")
        }
}

func TestSessionCostTracker_CacheHitRate_WithCache(t *testing.T) {
        tracker := NewSessionCostTracker()
        usage := &protocoltypes.UsageInfo{
                PromptTokens:         1000,
                CompletionTokens:     100,
                PromptCacheHitTokens: 800,
                TotalTokens:          1100,
        }
        tracker.RecordLLMUsage("deepseek-v4-flash", usage)
        rate := tracker.CacheHitRate()
        // 800 / 1000 * 100 = 80%
        if math.Abs(rate-80.0) > 0.1 {
                t.Errorf("cache hit rate: got %f, want ~80%%", rate)
        }
}

func TestSessionCostTracker_CacheHitRate_FullCache(t *testing.T) {
        tracker := NewSessionCostTracker()
        usage := &protocoltypes.UsageInfo{
                PromptTokens:         1000,
                CompletionTokens:     100,
                PromptCacheHitTokens: 1000,
                TotalTokens:          1100,
        }
        tracker.RecordLLMUsage("deepseek-v4-flash", usage)
        rate := tracker.CacheHitRate()
        if math.Abs(rate-100.0) > 0.1 {
                t.Errorf("full cache: got %f, want ~100%%", rate)
        }
}

// ── CostBreakdown ───────────────────────────────────────────────────────────

func TestSessionCostTracker_CostBreakdown_Basic(t *testing.T) {
        tracker := NewSessionCostTracker()
        usage := &protocoltypes.UsageInfo{
                PromptTokens:     1000,
                CompletionTokens: 200,
                TotalTokens:      1200,
        }
        tracker.RecordLLMUsage("deepseek-v4-flash", usage)
        breakdown := tracker.CostBreakdown("")
        if !strings.Contains(breakdown, "Session Cost Breakdown") {
                t.Error("should contain header")
        }
        if !strings.Contains(breakdown, "Total cost") {
                t.Error("should contain total cost")
        }
        if !strings.Contains(breakdown, "1,000") {
                t.Error("should contain formatted input tokens")
        }
}

func TestSessionCostTracker_CostBreakdown_WithModel(t *testing.T) {
        tracker := NewSessionCostTracker()
        usage := &protocoltypes.UsageInfo{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}
        tracker.RecordLLMUsage("deepseek-v4-flash", usage)
        breakdown := tracker.CostBreakdown("deepseek-v4-flash")
        if !strings.Contains(breakdown, "deepseek-v4-flash") {
                t.Error("should mention model name")
        }
}

func TestSessionCostTracker_CostBreakdown_WithBudget(t *testing.T) {
        tracker := NewSessionCostTracker()
        tracker.SetBudget(10.0)
        usage := &protocoltypes.UsageInfo{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}
        tracker.RecordLLMUsage("deepseek-v4-flash", usage)
        breakdown := tracker.CostBreakdown("")
        if !strings.Contains(breakdown, "Budget") {
                t.Error("should show budget info")
        }
        if !strings.Contains(breakdown, "Remaining") {
                t.Error("should show remaining budget")
        }
}

func TestSessionCostTracker_CostBreakdown_MultipleModels(t *testing.T) {
        tracker := NewSessionCostTracker()
        flashUsage := &protocoltypes.UsageInfo{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}
        proUsage := &protocoltypes.UsageInfo{PromptTokens: 500, CompletionTokens: 50, TotalTokens: 550}
        tracker.RecordLLMUsage("deepseek-v4-flash", flashUsage)
        tracker.RecordLLMUsage("deepseek-v4-pro", proUsage)
        breakdown := tracker.CostBreakdown("")
        if !strings.Contains(breakdown, "Per-model") {
                t.Error("should show per-model breakdown")
        }
}

func TestSessionCostTracker_CostBreakdown_Empty(t *testing.T) {
        tracker := NewSessionCostTracker()
        breakdown := tracker.CostBreakdown("")
        if !strings.Contains(breakdown, "Session Cost Breakdown") {
                t.Error("should have header even when empty")
        }
}

// ── SetBudget ───────────────────────────────────────────────────────────────

func TestSessionCostTracker_SetBudget(t *testing.T) {
        tracker := NewSessionCostTracker()
        tracker.SetBudget(5.0)
        snap := tracker.GetSpend()
        if snap.BudgetUSD != 5.0 {
                t.Errorf("budget: got %f, want 5.0", snap.BudgetUSD)
        }
}

// ── IsApproachingBudget ────────────────────────────────────────────────────

func TestSessionCostTracker_IsApproachingBudget_NoBudget(t *testing.T) {
        tracker := NewSessionCostTracker()
        if tracker.IsApproachingBudget() {
                t.Error("should not approach budget when no budget set")
        }
}

func TestSessionCostTracker_IsApproachingBudget_UnderLimit(t *testing.T) {
        tracker := NewSessionCostTracker()
        tracker.SetBudget(100.0)
        usage := &protocoltypes.UsageInfo{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}
        tracker.RecordLLMUsage("deepseek-v4-flash", usage)
        if tracker.IsApproachingBudget() {
                t.Error("should not approach budget when well under limit")
        }
}

func TestSessionCostTracker_IsApproachingBudget_AtLimit(t *testing.T) {
        tracker := NewSessionCostTracker()
        tracker.SetBudget(1.0)
        // Burn most of the budget
        usage := &protocoltypes.UsageInfo{PromptTokens: 4_000_000, CompletionTokens: 1_000_000, TotalTokens: 5_000_000}
        tracker.RecordLLMUsage("deepseek-v4-flash", usage)
        if !tracker.IsApproachingBudget() {
                t.Error("should approach budget when spend is high")
        }
}

// ── Spend accessor ─────────────────────────────────────────────────────────

func TestSessionCostTracker_Spend(t *testing.T) {
        tracker := NewSessionCostTracker()
        if tracker.Spend() == nil {
                t.Error("Spend() should not return nil")
        }
}

// ── FormatNumber ────────────────────────────────────────────────────────────

func TestFormatNumber_Small(t *testing.T) {
        if got := FormatNumber(42); got != "42" {
                t.Errorf("FormatNumber(42): got %q, want %q", got, "42")
        }
}

func TestFormatNumber_Thousands(t *testing.T) {
        if got := FormatNumber(1234); got != "1,234" {
                t.Errorf("FormatNumber(1234): got %q, want %q", got, "1,234")
        }
}

func TestFormatNumber_Millions(t *testing.T) {
        if got := FormatNumber(1234567); got != "1,234,567" {
                t.Errorf("FormatNumber(1234567): got %q, want %q", got, "1,234,567")
        }
}

func TestFormatNumber_Zero(t *testing.T) {
        if got := FormatNumber(0); got != "0" {
                t.Errorf("FormatNumber(0): got %q, want %q", got, "0")
        }
}

func TestFormatNumber_Negative(t *testing.T) {
        if got := FormatNumber(-1234); got != "-1,234" {
                t.Errorf("FormatNumber(-1234): got %q, want %q", got, "-1,234")
        }
}

func TestFormatNumber_ExactThousand(t *testing.T) {
        if got := FormatNumber(1000); got != "1,000" {
                t.Errorf("FormatNumber(1000): got %q, want %q", got, "1,000")
        }
}

// ── GetSpend snapshot ───────────────────────────────────────────────────────

func TestSessionCostTracker_GetSpend_IsSnapshot(t *testing.T) {
        tracker := NewSessionCostTracker()
        usage := &protocoltypes.UsageInfo{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}
        tracker.RecordLLMUsage("deepseek-v4-flash", usage)
        snap := tracker.GetSpend()
        // Modify snapshot
        snap.TotalInputTokens = 99999
        // Original should be unchanged
        snap2 := tracker.GetSpend()
        if snap2.TotalInputTokens == 99999 {
                t.Error("GetSpend should return a copy")
        }
}

// ── Concurrent access ───────────────────────────────────────────────────────

func TestSessionCostTracker_ConcurrentAccess(t *testing.T) {
        tracker := NewSessionCostTracker()
        done := make(chan bool)

        for i := 0; i < 10; i++ {
                go func() {
                        usage := &protocoltypes.UsageInfo{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}
                        tracker.RecordLLMUsage("deepseek-v4-flash", usage)
                        _ = tracker.GetSpend()
                        _ = tracker.TotalCostUSD()
                        _ = tracker.CacheHitRate()
                        _ = tracker.CostBreakdown("")
                        done <- true
                }()
        }

        for i := 0; i < 10; i++ {
                <-done
        }

        snap := tracker.GetSpend()
        if snap.TotalInputTokens != 10_000 {
                t.Errorf("concurrent total input: got %d, want 10000", snap.TotalInputTokens)
        }
}

// ── Integration ─────────────────────────────────────────────────────────────

func TestSessionCostTracker_Integration_RealisticSession(t *testing.T) {
        tracker := NewSessionCostTracker()
        tracker.SetBudget(5.0)

        // First turn: simple query
        usage1 := &protocoltypes.UsageInfo{PromptTokens: 500, CompletionTokens: 50, TotalTokens: 550}
        tracker.RecordLLMUsage("deepseek-v4-flash", usage1)

        // Second turn: complex query with cache
        usage2 := &protocoltypes.UsageInfo{
                PromptTokens:         2000,
                CompletionTokens:     500,
                PromptCacheHitTokens: 1500,
                TotalTokens:          2500,
        }
        tracker.RecordLLMUsage("deepseek-v4-pro", usage2)

        // Verify totals
        snap := tracker.GetSpend()
        if snap.TotalInputTokens != 2500 {
                t.Errorf("total input: got %d, want 2500", snap.TotalInputTokens)
        }
        if snap.TotalOutputTokens != 550 {
                t.Errorf("total output: got %d, want 550", snap.TotalOutputTokens)
        }
        if snap.TotalCacheHits != 1500 {
                t.Errorf("total cache hits: got %d, want 1500", snap.TotalCacheHits)
        }

        // Verify cost is positive
        total := tracker.TotalCostUSD()
        if total <= 0 {
                t.Error("total cost should be positive")
        }

        // Verify cache hit rate
        rate := tracker.CacheHitRate()
        // 1500 / 2500 * 100 = 60%
        if math.Abs(rate-60.0) > 0.1 {
                t.Errorf("cache hit rate: got %f, want ~60%%", rate)
        }

        // Verify breakdown
        breakdown := tracker.CostBreakdown("deepseek-v4-pro")
        if !strings.Contains(breakdown, "deepseek-v4-pro") {
                t.Error("breakdown should mention current model")
        }
}

// ── Routing integration ────────────────────────────────────────────────────

func TestSessionCostTracker_Spend_WithRouting(t *testing.T) {
        tracker := NewSessionCostTracker()
        spend := tracker.Spend()
        if spend == nil {
                t.Fatal("spend should not be nil")
        }

        // Can use spend directly with routing
        spend.RecordUsage("deepseek-v4-flash", 1000, 100, 0)
        total := tracker.TotalCostUSD()
        if total <= 0 {
                t.Error("cost should be positive after direct spend recording")
        }
}
