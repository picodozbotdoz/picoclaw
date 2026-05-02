package routing

import (
        "path/filepath"
        "strings"
        "sync"

        "github.com/sipeed/picoclaw/pkg/providers"
)

// CostConfig holds per-model pricing information.
// All prices are in USD per million tokens.
type CostConfig struct {
        InputPricePerMillion    float64
        CacheHitPricePerMillion float64
        OutputPricePerMillion   float64
}

// defaultPricing defines the default pricing for known models.
var defaultPricing = map[string]CostConfig{
        "deepseek-v4-flash": {
                InputPricePerMillion:    0.14,
                CacheHitPricePerMillion: 0.0028,
                OutputPricePerMillion:   0.28,
        },
        "deepseek-v4-pro": {
                InputPricePerMillion:    0.435,
                CacheHitPricePerMillion: 0.003625,
                OutputPricePerMillion:   0.87,
        },
}

// DefaultPricing returns a copy of the default pricing map.
func DefaultPricing() map[string]CostConfig {
        cp := make(map[string]CostConfig, len(defaultPricing))
        for k, v := range defaultPricing {
                cp[k] = v
        }
        return cp
}

// SpendSnapshot is a read-only snapshot of session spend.
type SpendSnapshot struct {
        TotalInputTokens  int
        TotalOutputTokens int
        TotalCacheHits    int
        TotalCostUSD      float64
        PerModel          map[string]ModelSpendEntry
        BudgetUSD         float64
}

// ModelSpendEntry holds per-model token and cost breakdown.
type ModelSpendEntry struct {
        InputTokens  int
        OutputTokens int
        CacheHits    int
        CostUSD      float64
}

// SessionSpend tracks token usage and cost across the session.
// It is safe for concurrent use.
type SessionSpend struct {
        mu       sync.RWMutex
        perModel map[string]*ModelSpendEntry
        budget   float64
}

// NewSessionSpend creates a new SessionSpend tracker.
func NewSessionSpend() *SessionSpend {
        return &SessionSpend{
                perModel: make(map[string]*ModelSpendEntry),
        }
}

// RecordUsage records token usage from an API response for the given model.
func (s *SessionSpend) RecordUsage(model string, inputTokens, outputTokens, cacheHitTokens int) {
        s.mu.Lock()
        defer s.mu.Unlock()

        model = normalizeModelName(model)
        entry, ok := s.perModel[model]
        if !ok {
                entry = &ModelSpendEntry{}
                s.perModel[model] = entry
        }

        entry.InputTokens += inputTokens
        entry.OutputTokens += outputTokens
        entry.CacheHits += cacheHitTokens

        // Recalculate cost for this model
        pricing, found := defaultPricing[model]
        if !found {
                // Unknown model: use V4-Flash pricing as fallback
                pricing = defaultPricing["deepseek-v4-flash"]
        }
        cacheMissTokens := entry.InputTokens - entry.CacheHits
        if cacheMissTokens < 0 {
                cacheMissTokens = 0
        }
        entry.CostUSD = (float64(cacheMissTokens)*pricing.InputPricePerMillion +
                float64(entry.CacheHits)*pricing.CacheHitPricePerMillion +
                float64(entry.OutputTokens)*pricing.OutputPricePerMillion) / 1_000_000
}

// Snapshot returns a read-only copy of the current spend state.
func (s *SessionSpend) Snapshot() SpendSnapshot {
        s.mu.RLock()
        defer s.mu.RUnlock()

        snap := SpendSnapshot{
                PerModel:  make(map[string]ModelSpendEntry, len(s.perModel)),
                BudgetUSD: s.budget,
        }

        for model, entry := range s.perModel {
                snap.PerModel[model] = *entry
                snap.TotalInputTokens += entry.InputTokens
                snap.TotalOutputTokens += entry.OutputTokens
                snap.TotalCacheHits += entry.CacheHits
                snap.TotalCostUSD += entry.CostUSD
        }

        return snap
}

// TotalSpendUSD returns the total cost across all models.
func (s *SessionSpend) TotalSpendUSD() float64 {
        s.mu.RLock()
        defer s.mu.RUnlock()

        var total float64
        for _, entry := range s.perModel {
                total += entry.CostUSD
        }
        return total
}

// SetBudget sets an optional budget limit in USD.
func (s *SessionSpend) SetBudget(budgetUSD float64) {
        s.mu.Lock()
        defer s.mu.Unlock()
        s.budget = budgetUSD
}

// IsApproachingBudget returns true when the session has used >= 80% of budget.
// Returns false if no budget is set.
func (s *SessionSpend) IsApproachingBudget() bool {
        s.mu.RLock()
        defer s.mu.RUnlock()

        if s.budget <= 0 {
                return false
        }
        total := 0.0
        for _, entry := range s.perModel {
                total += entry.CostUSD
        }
        return total >= s.budget*0.8
}

// CostAwareRouter extends Router with cost-aware model selection.
type CostAwareRouter struct {
        Router
        spend *SessionSpend
}

// Verify CostAwareRouter implements RouterInterface at compile time.
var _ RouterInterface = (*CostAwareRouter)(nil)

// NewCostAwareRouter creates a CostAwareRouter wrapping the given config and spend tracker.
func NewCostAwareRouter(cfg RouterConfig, spend *SessionSpend) *CostAwareRouter {
        return &CostAwareRouter{
                Router: *New(cfg),
                spend:  spend,
        }
}

// SelectModelWithCost selects a model considering the session budget.
// If the budget is approaching, it prefers the light model more aggressively.
func (r *CostAwareRouter) SelectModelWithCost(
        msg string,
        history []providers.Message,
        primaryModel string,
        thinkingLevel float64,
        budgetUSD float64,
) (model string, usedLight bool, score float64) {
        model, usedLight, score = r.Router.SelectModel(msg, history, primaryModel)

        // If approaching budget, force light model unless the task is very complex
        if r.spend != nil && r.spend.IsApproachingBudget() && score < 0.7 {
                return r.cfg.LightModel, true, score
        }

        return model, usedLight, score
}

// SelectModel implements RouterInterface.
func (r *CostAwareRouter) SelectModel(
        msg string,
        history []providers.Message,
        primaryModel string,
) (model string, usedLight bool, score float64) {
        return r.SelectModelWithCost(msg, history, primaryModel, 0, 0)
}

// EstimateSwitchCost estimates the cost impact of switching from one model to another.
// Returns the estimated additional cost in USD for a typical request.
func (r *CostAwareRouter) EstimateSwitchCost(fromModel, toModel string, estimatedInputTokens, estimatedOutputTokens int) float64 {
        fromPricing, fromFound := defaultPricing[normalizeModelName(fromModel)]
        toPricing, toFound := defaultPricing[normalizeModelName(toModel)]

        if !fromFound {
                fromPricing = defaultPricing["deepseek-v4-flash"]
        }
        if !toFound {
                toPricing = defaultPricing["deepseek-v4-flash"]
        }

        fromCost := (float64(estimatedInputTokens)*fromPricing.InputPricePerMillion +
                float64(estimatedOutputTokens)*fromPricing.OutputPricePerMillion) / 1_000_000

        toCost := (float64(estimatedInputTokens)*toPricing.InputPricePerMillion +
                float64(estimatedOutputTokens)*toPricing.OutputPricePerMillion) / 1_000_000

        return toCost - fromCost
}

// IsSameProviderFamily returns true if both models belong to the same provider family.
// Currently checks if both contain "deepseek".
func IsSameProviderFamily(model1, model2 string) bool {
        return strings.Contains(strings.ToLower(model1), "deepseek") &&
                strings.Contains(strings.ToLower(model2), "deepseek")
}

// normalizeModelName normalizes a model name for pricing lookups.
func normalizeModelName(model string) string {
        model = strings.ToLower(strings.TrimSpace(model))
        // Strip path prefixes like "openrouter/" or "deepseek/"
        if idx := strings.LastIndex(model, "/"); idx >= 0 {
                model = model[idx+1:]
        }
        // Normalize common aliases
        model = strings.ReplaceAll(model, "deepseek-v4-", "deepseek-v4-")
        model = strings.ReplaceAll(model, "deepseek_chat", "deepseek-v4-flash")
        model = strings.ReplaceAll(model, "deepseek-reasoner", "deepseek-v4-pro")
        return model
}

// modelMatchesGlob checks if a model name matches a glob pattern.
func modelMatchesGlob(model, pattern string) bool {
        matched, _ := filepath.Match(pattern, model)
        return matched
}
