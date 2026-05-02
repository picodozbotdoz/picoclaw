package routing

import (
	"math"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// ── CostConfig / DefaultPricing ─────────────────────────────────────────────

func TestDefaultPricing_ReturnsCopy(t *testing.T) {
	p1 := DefaultPricing()
	p2 := DefaultPricing()
	if len(p1) == 0 {
		t.Error("DefaultPricing should not be empty")
	}
	// Modifying the copy should not affect the original
	p1["test-model"] = CostConfig{InputPricePerMillion: 999}
	if _, exists := p2["test-model"]; exists {
		t.Error("DefaultPricing should return a copy, not the original")
	}
}

func TestDefaultPricing_V4Flash(t *testing.T) {
	p := DefaultPricing()
	flash, ok := p["deepseek-v4-flash"]
	if !ok {
		t.Fatal("deepseek-v4-flash pricing not found")
	}
	if flash.InputPricePerMillion != 0.14 {
		t.Errorf("V4-Flash input: got %f, want 0.14", flash.InputPricePerMillion)
	}
	if flash.CacheHitPricePerMillion != 0.0028 {
		t.Errorf("V4-Flash cache hit: got %f, want 0.0028", flash.CacheHitPricePerMillion)
	}
	if flash.OutputPricePerMillion != 0.28 {
		t.Errorf("V4-Flash output: got %f, want 0.28", flash.OutputPricePerMillion)
	}
}

func TestDefaultPricing_V4Pro(t *testing.T) {
	p := DefaultPricing()
	pro, ok := p["deepseek-v4-pro"]
	if !ok {
		t.Fatal("deepseek-v4-pro pricing not found")
	}
	if pro.InputPricePerMillion != 0.435 {
		t.Errorf("V4-Pro input: got %f, want 0.435", pro.InputPricePerMillion)
	}
	if pro.CacheHitPricePerMillion != 0.003625 {
		t.Errorf("V4-Pro cache hit: got %f, want 0.003625", pro.CacheHitPricePerMillion)
	}
	if pro.OutputPricePerMillion != 0.87 {
		t.Errorf("V4-Pro output: got %f, want 0.87", pro.OutputPricePerMillion)
	}
}

// ── SessionSpend ─────────────────────────────────────────────────────────────

func TestSessionSpend_New(t *testing.T) {
	s := NewSessionSpend()
	snap := s.Snapshot()
	if snap.TotalCostUSD != 0 {
		t.Errorf("new spend should have zero cost, got %f", snap.TotalCostUSD)
	}
	if len(snap.PerModel) != 0 {
		t.Errorf("new spend should have no models, got %d", len(snap.PerModel))
	}
}

func TestSessionSpend_RecordUsage_SingleModel(t *testing.T) {
	s := NewSessionSpend()
	s.RecordUsage("deepseek-v4-flash", 1000, 200, 500)
	snap := s.Snapshot()
	if snap.TotalInputTokens != 1000 {
		t.Errorf("input: got %d, want 1000", snap.TotalInputTokens)
	}
	if snap.TotalOutputTokens != 200 {
		t.Errorf("output: got %d, want 200", snap.TotalOutputTokens)
	}
	if snap.TotalCacheHits != 500 {
		t.Errorf("cache hits: got %d, want 500", snap.TotalCacheHits)
	}
}

func TestSessionSpend_RecordUsage_MultipleModels(t *testing.T) {
	s := NewSessionSpend()
	s.RecordUsage("deepseek-v4-flash", 1000, 100, 500)
	s.RecordUsage("deepseek-v4-pro", 500, 50, 200)
	snap := s.Snapshot()
	if snap.TotalInputTokens != 1500 {
		t.Errorf("total input: got %d, want 1500", snap.TotalInputTokens)
	}
	if len(snap.PerModel) != 2 {
		t.Errorf("per-model count: got %d, want 2", len(snap.PerModel))
	}
}

func TestSessionSpend_RecordUsage_Accumulates(t *testing.T) {
	s := NewSessionSpend()
	s.RecordUsage("deepseek-v4-flash", 1000, 100, 0)
	s.RecordUsage("deepseek-v4-flash", 500, 50, 0)
	snap := s.Snapshot()
	entry := snap.PerModel["deepseek-v4-flash"]
	if entry.InputTokens != 1500 {
		t.Errorf("accumulated input: got %d, want 1500", entry.InputTokens)
	}
	if entry.OutputTokens != 150 {
		t.Errorf("accumulated output: got %d, want 150", entry.OutputTokens)
	}
}

func TestSessionSpend_TotalSpendUSD(t *testing.T) {
	s := NewSessionSpend()
	s.RecordUsage("deepseek-v4-flash", 1_000_000, 1_000_000, 0)
	total := s.TotalSpendUSD()
	// Expected: (1M * 0.14 + 1M * 0.28) / 1M = 0.42
	if math.Abs(total-0.42) > 0.0001 {
		t.Errorf("total spend: got %f, want ~0.42", total)
	}
}

func TestSessionSpend_TotalSpendUSD_WithCache(t *testing.T) {
	s := NewSessionSpend()
	s.RecordUsage("deepseek-v4-flash", 1_000_000, 100_000, 800_000)
	total := s.TotalSpendUSD()
	// Cache miss: 200K, Cache hit: 800K, Output: 100K
	// (200K * 0.14 + 800K * 0.0028 + 100K * 0.28) / 1M = (28000 + 2240 + 28000) / 1M = 0.05824
	expected := (200_000*0.14 + 800_000*0.0028 + 100_000*0.28) / 1_000_000
	if math.Abs(total-expected) > 0.0001 {
		t.Errorf("total spend with cache: got %f, want %f", total, expected)
	}
}

func TestSessionSpend_Snapshot_IsCopy(t *testing.T) {
	s := NewSessionSpend()
	s.RecordUsage("deepseek-v4-flash", 1000, 100, 0)
	snap := s.Snapshot()
	// Modify snapshot, should not affect original
	snap.TotalInputTokens = 99999
	snap2 := s.Snapshot()
	if snap2.TotalInputTokens == 99999 {
		t.Error("Snapshot should be a copy, not a reference")
	}
}

func TestSessionSpend_SetBudget(t *testing.T) {
	s := NewSessionSpend()
	s.SetBudget(10.0)
	snap := s.Snapshot()
	if snap.BudgetUSD != 10.0 {
		t.Errorf("budget: got %f, want 10.0", snap.BudgetUSD)
	}
}

func TestSessionSpend_IsApproachingBudget_NoBudget(t *testing.T) {
	s := NewSessionSpend()
	// No budget set
	if s.IsApproachingBudget() {
		t.Error("should not approach budget when no budget is set")
	}
}

func TestSessionSpend_IsApproachingBudget_Under80(t *testing.T) {
	s := NewSessionSpend()
	s.SetBudget(10.0)
	s.RecordUsage("deepseek-v4-flash", 1_000_000, 100_000, 0)
	// Cost is well under $8
	if s.IsApproachingBudget() {
		t.Error("should not approach budget when spend is under 80%")
	}
}

func TestSessionSpend_IsApproachingBudget_At80(t *testing.T) {
	s := NewSessionSpend()
	s.SetBudget(1.0)
	// Generate enough usage to hit 80% of $1 budget = $0.80
	// V4-Flash: input=$0.14/M, output=$0.28/M
	// 2M input + 1M output = 0.28 + 0.28 = 0.56 ... need more
	// 3M input + 1M output = 0.42 + 0.28 = 0.70 ... still under
	// 4M input + 1M output = 0.56 + 0.28 = 0.84 > 0.80
	s.RecordUsage("deepseek-v4-flash", 4_000_000, 1_000_000, 0)
	if !s.IsApproachingBudget() {
		t.Error("should approach budget when spend is >= 80%")
	}
}

func TestSessionSpend_ZeroBudget(t *testing.T) {
	s := NewSessionSpend()
	s.SetBudget(0)
	if s.IsApproachingBudget() {
		t.Error("zero budget should not trigger approaching")
	}
}

func TestSessionSpend_CostCalculation_UnknownModel(t *testing.T) {
	s := NewSessionSpend()
	s.RecordUsage("unknown-model-xyz", 1_000_000, 1_000_000, 0)
	// Should fall back to V4-Flash pricing
	total := s.TotalSpendUSD()
	expected := 0.42 // same as V4-Flash
	if math.Abs(total-expected) > 0.0001 {
		t.Errorf("unknown model cost: got %f, want ~%f", total, expected)
	}
}

// ── normalizeModelName ──────────────────────────────────────────────────────

func TestNormalizeModelName_Simple(t *testing.T) {
	if got := normalizeModelName("DeepSeek-V4-Flash"); got != "deepseek-v4-flash" {
		t.Errorf("got %q, want %q", got, "deepseek-v4-flash")
	}
}

func TestNormalizeModelName_WithPathPrefix(t *testing.T) {
	if got := normalizeModelName("openrouter/deepseek-v4-flash"); got != "deepseek-v4-flash" {
		t.Errorf("got %q, want %q", got, "deepseek-v4-flash")
	}
}

func TestNormalizeModelName_ChatAlias(t *testing.T) {
	if got := normalizeModelName("deepseek_chat"); got != "deepseek-v4-flash" {
		t.Errorf("got %q, want %q", got, "deepseek-v4-flash")
	}
}

func TestNormalizeModelName_ReasonerAlias(t *testing.T) {
	if got := normalizeModelName("deepseek-reasoner"); got != "deepseek-v4-pro" {
		t.Errorf("got %q, want %q", got, "deepseek-v4-pro")
	}
}

// ── IsSameProviderFamily ────────────────────────────────────────────────────

func TestIsSameProviderFamily_BothDeepSeek(t *testing.T) {
	if !IsSameProviderFamily("deepseek-v4-flash", "deepseek-v4-pro") {
		t.Error("both deepseek models should be same family")
	}
}

func TestIsSameProviderFamily_DifferentFamilies(t *testing.T) {
	if IsSameProviderFamily("deepseek-v4-flash", "gpt-4o") {
		t.Error("deepseek and gpt should not be same family")
	}
}

func TestIsSameProviderFamily_NeitherDeepSeek(t *testing.T) {
	if IsSameProviderFamily("gpt-4o", "claude-sonnet") {
		t.Error("non-deepseek models should not be same family")
	}
}

func TestIsSameProviderFamily_CaseInsensitive(t *testing.T) {
	if !IsSameProviderFamily("DeepSeek-V4-Flash", "DEEPSEEK-V4-PRO") {
		t.Error("should be case insensitive")
	}
}

// ── CostAwareRouter ─────────────────────────────────────────────────────────

func TestCostAwareRouter_SelectModel_Simple(t *testing.T) {
	spend := NewSessionSpend()
	r := NewCostAwareRouter(RouterConfig{LightModel: "flash", Threshold: 0.35}, spend)
	model, usedLight, _ := r.SelectModel("hi", nil, "pro")
	if !usedLight {
		t.Error("simple message should use light model")
	}
	if model != "flash" {
		t.Errorf("model: got %q, want %q", model, "flash")
	}
}

func TestCostAwareRouter_SelectModel_Complex(t *testing.T) {
	spend := NewSessionSpend()
	r := NewCostAwareRouter(RouterConfig{LightModel: "flash", Threshold: 0.35}, spend)
	model, usedLight, _ := r.SelectModel("```go\nfmt.Println()\n```", nil, "pro")
	if usedLight {
		t.Error("code block should use primary model")
	}
	if model != "pro" {
		t.Errorf("model: got %q, want %q", model, "pro")
	}
}

func TestCostAwareRouter_SelectModelWithCost_BudgetNotApproaching(t *testing.T) {
	spend := NewSessionSpend()
	spend.SetBudget(100.0) // Large budget
	r := NewCostAwareRouter(RouterConfig{LightModel: "flash", Threshold: 0.35}, spend)
	_, usedLight, _ := r.SelectModelWithCost("```go\nfmt.Println()\n```", nil, "pro", 0, 100)
	if usedLight {
		t.Error("complex task with large budget should use primary model")
	}
}

func TestCostAwareRouter_SelectModelWithCost_BudgetApproaching(t *testing.T) {
	spend := NewSessionSpend()
	spend.SetBudget(1.0)
	// Burn through most of the budget
	spend.RecordUsage("deepseek-v4-flash", 4_000_000, 1_000_000, 0)
	r := NewCostAwareRouter(RouterConfig{LightModel: "flash", Threshold: 0.35}, spend)
	model, usedLight, _ := r.SelectModelWithCost("```go\nfmt.Println()\n```", nil, "pro", 0, 1)
	// Score for code block is 0.40 which is < 0.7, so approaching budget forces light
	if !usedLight {
		t.Error("approaching budget should force light model for moderate scores")
	}
	_ = model
}

func TestCostAwareRouter_SelectModelWithCost_VeryComplex_StillPrimary(t *testing.T) {
	spend := NewSessionSpend()
	spend.SetBudget(1.0)
	spend.RecordUsage("deepseek-v4-flash", 4_000_000, 1_000_000, 0)
	r := NewCostAwareRouter(RouterConfig{LightModel: "flash", Threshold: 0.35}, spend)
	// Attachment gives score 1.0 >= 0.7, so should still use primary
	msg := "analyze this data:image/png;base64,abc"
	_, usedLight, _ := r.SelectModelWithCost(msg, nil, "pro", 0, 1)
	if usedLight {
		t.Error("very complex task should still use primary model even when approaching budget")
	}
}

func TestCostAwareRouter_ImplementsRouterInterface(t *testing.T) {
	var _ RouterInterface = &CostAwareRouter{}
}

// ── NewCostAware factory ─────────────────────────────────────────────────────

func TestNewCostAware_NilSpend(t *testing.T) {
	r := NewCostAware(RouterConfig{LightModel: "flash"}, nil)
	if _, ok := r.(*Router); !ok {
		t.Error("nil spend should return basic Router")
	}
}

func TestNewCostAware_WithSpend(t *testing.T) {
	spend := NewSessionSpend()
	r := NewCostAware(RouterConfig{LightModel: "flash"}, spend)
	if _, ok := r.(*CostAwareRouter); !ok {
		t.Error("with spend should return CostAwareRouter")
	}
}

// ── EstimateSwitchCost ──────────────────────────────────────────────────────

func TestEstimateSwitchCost_FlashToPro(t *testing.T) {
	spend := NewSessionSpend()
	r := NewCostAwareRouter(RouterConfig{LightModel: "flash"}, spend)
	cost := r.EstimateSwitchCost("deepseek-v4-flash", "deepseek-v4-pro", 1000, 500)
	// Flash: (1000*0.14 + 500*0.28)/1M = 280/1M = 0.00028
	// Pro:   (1000*0.435 + 500*0.87)/1M = 870/1M = 0.00087
	// Diff = 0.00087 - 0.00028 = 0.00059
	expected := 0.00059
	if math.Abs(cost-expected) > 0.000001 {
		t.Errorf("switch cost: got %f, want ~%f", cost, expected)
	}
}

func TestEstimateSwitchCost_ProToFlash(t *testing.T) {
	spend := NewSessionSpend()
	r := NewCostAwareRouter(RouterConfig{LightModel: "flash"}, spend)
	cost := r.EstimateSwitchCost("deepseek-v4-pro", "deepseek-v4-flash", 1000, 500)
	// Should be negative (saving money)
	if cost >= 0 {
		t.Errorf("switching from pro to flash should save money, got %f", cost)
	}
}

func TestEstimateSwitchCost_SameModel(t *testing.T) {
	spend := NewSessionSpend()
	r := NewCostAwareRouter(RouterConfig{LightModel: "flash"}, spend)
	cost := r.EstimateSwitchCost("deepseek-v4-flash", "deepseek-v4-flash", 1000, 500)
	if cost != 0 {
		t.Errorf("switching to same model should cost 0, got %f", cost)
	}
}

func TestEstimateSwitchCost_UnknownModel(t *testing.T) {
	spend := NewSessionSpend()
	r := NewCostAwareRouter(RouterConfig{LightModel: "flash"}, spend)
	cost := r.EstimateSwitchCost("unknown-a", "unknown-b", 1000, 500)
	// Both fallback to V4-Flash pricing, so diff should be 0
	if cost != 0 {
		t.Errorf("unknown models with same fallback should have 0 diff, got %f", cost)
	}
}

// ── DeepSeekV4Classifier ────────────────────────────────────────────────────

func TestDeepSeekV4Classifier_InjectedContext(t *testing.T) {
	c := &DeepSeekV4Classifier{InjectedContextTokens: 1000}
	score := c.Score(Features{})
	if score < 0.15 {
		t.Errorf("injected context should add at least 0.15, got %f", score)
	}
}

func TestDeepSeekV4Classifier_NoInjectedContext(t *testing.T) {
	c := &DeepSeekV4Classifier{InjectedContextTokens: 0}
	score := c.Score(Features{})
	if score != 0.0 {
		t.Errorf("no injected context with no features should be 0, got %f", score)
	}
}

func TestDeepSeekV4Classifier_ToolCount(t *testing.T) {
	c := &DeepSeekV4Classifier{ToolCount: 15}
	score := c.Score(Features{})
	if score < 0.10 {
		t.Errorf(">10 tools should add at least 0.10, got %f", score)
	}
}

func TestDeepSeekV4Classifier_ToolCountUnder10(t *testing.T) {
	c := &DeepSeekV4Classifier{ToolCount: 5}
	score := c.Score(Features{})
	if score != 0.0 {
		t.Errorf("<=10 tools with no features should be 0, got %f", score)
	}
}

func TestDeepSeekV4Classifier_Combined(t *testing.T) {
	c := &DeepSeekV4Classifier{InjectedContextTokens: 500, ToolCount: 15}
	score := c.Score(Features{})
	if score < 0.25 {
		t.Errorf("combined should add at least 0.25, got %f", score)
	}
}

func TestDeepSeekV4Classifier_CapAtOne(t *testing.T) {
	c := &DeepSeekV4Classifier{
		InjectedContextTokens: 500,
		ToolCount:             15,
	}
	// Features with all max signals: long + code + tools + depth + attachments
	f := Features{
		TokenEstimate:     500,
		CodeBlockCount:    5,
		RecentToolCalls:   10,
		ConversationDepth: 20,
		HasAttachments:    true,
	}
	score := c.Score(f)
	if score > 1.0 {
		t.Errorf("score should not exceed 1.0, got %f", score)
	}
}

func TestDeepSeekV4Classifier_ExtendsRuleClassifier(t *testing.T) {
	c := &DeepSeekV4Classifier{}
	// Should still score like RuleClassifier for basic features
	score := c.Score(Features{CodeBlockCount: 1})
	if score < 0.40 {
		t.Errorf("should inherit RuleClassifier scoring, got %f", score)
	}
}

// ── modelMatchesGlob ────────────────────────────────────────────────────────

func TestModelMatchesGlob_Exact(t *testing.T) {
	if !modelMatchesGlob("deepseek-v4-flash", "deepseek-v4-flash") {
		t.Error("exact match should work")
	}
}

func TestModelMatchesGlob_Wildcard(t *testing.T) {
	if !modelMatchesGlob("deepseek-v4-flash", "deepseek-*") {
		t.Error("wildcard match should work")
	}
}

func TestModelMatchesGlob_NoMatch(t *testing.T) {
	if modelMatchesGlob("gpt-4o", "deepseek-*") {
		t.Error("should not match different prefix")
	}
}

// ── Concurrent access ───────────────────────────────────────────────────────

func TestSessionSpend_ConcurrentAccess(t *testing.T) {
	s := NewSessionSpend()
	done := make(chan bool)

	for i := 0; i < 10; i++ {
		go func() {
			s.RecordUsage("deepseek-v4-flash", 1000, 100, 50)
			_ = s.Snapshot()
			_ = s.TotalSpendUSD()
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}

	snap := s.Snapshot()
	entry := snap.PerModel["deepseek-v4-flash"]
	if entry.InputTokens != 10_000 {
		t.Errorf("concurrent input: got %d, want 10000", entry.InputTokens)
	}
	if entry.OutputTokens != 1000 {
		t.Errorf("concurrent output: got %d, want 1000", entry.OutputTokens)
	}
	if entry.CacheHits != 500 {
		t.Errorf("concurrent cache: got %d, want 500", entry.CacheHits)
	}
}

// ── SpendSnapshot ───────────────────────────────────────────────────────────

func TestSpendSnapshot_Empty(t *testing.T) {
	s := NewSessionSpend()
	snap := s.Snapshot()
	if snap.TotalCostUSD != 0 {
		t.Error("empty spend should have zero cost")
	}
	if snap.TotalInputTokens != 0 {
		t.Error("empty spend should have zero input tokens")
	}
	if snap.BudgetUSD != 0 {
		t.Error("empty spend should have zero budget")
	}
}

func TestSpendSnapshot_WithBudget(t *testing.T) {
	s := NewSessionSpend()
	s.SetBudget(5.0)
	snap := s.Snapshot()
	if snap.BudgetUSD != 5.0 {
		t.Errorf("budget: got %f, want 5.0", snap.BudgetUSD)
	}
}

// ── Integration: CostAwareRouter + SessionSpend ─────────────────────────────

func TestCostAwareRouter_Integration_RealisticSession(t *testing.T) {
	spend := NewSessionSpend()
	spend.SetBudget(1.0)
	r := NewCostAwareRouter(RouterConfig{LightModel: "flash", Threshold: 0.35}, spend)

	// First message: simple greeting → light
	_, usedLight, _ := r.SelectModel("hello", nil, "pro")
	if !usedLight {
		t.Error("greeting should use light model")
	}
	spend.RecordUsage("deepseek-v4-flash", 500, 50, 0)

	// Second message: code question → primary
	msg := "```python\nprint('hello')\n```"
	_, usedLight, _ = r.SelectModel(msg, nil, "pro")
	if usedLight {
		t.Error("code question should use primary model")
	}
	spend.RecordUsage("deepseek-v4-pro", 2000, 500, 1000)

	// Check total cost
	total := spend.TotalSpendUSD()
	if total <= 0 {
		t.Error("total cost should be positive")
	}
}

func TestCostAwareRouter_NilSpendField(t *testing.T) {
	r := &CostAwareRouter{
		Router: *New(RouterConfig{LightModel: "flash", Threshold: 0.35}),
		spend:  nil,
	}
	// Should not panic with nil spend
	_, _, _ = r.SelectModelWithCost("hello", nil, "pro", 0, 0)
}

// ── providers import verification ───────────────────────────────────────────

func TestCostAwareRouter_WithHistory(t *testing.T) {
	spend := NewSessionSpend()
	r := NewCostAwareRouter(RouterConfig{LightModel: "flash", Threshold: 0.35}, spend)

	history := []providers.Message{
		{Role: "assistant", ToolCalls: []providers.ToolCall{{Name: "exec"}}},
	}
	model, _, _ := r.SelectModel("ok", history, "pro")
	if model == "" {
		t.Error("model should not be empty")
	}
}
