package tokenizer

import (
        "encoding/json"
        "sync"
        "unicode/utf8"

        "github.com/sipeed/picoclaw/pkg/providers"
)

// modelTokenRates stores per-model tokens-per-character ratios learned from
// API usage data. When a provider returns actual token counts, the ratio
// (prompt_tokens / total_chars) is recorded here and used to improve future
// estimates. This compensates for the 20-40% error rate of the generic
// chars*2/5 heuristic across different model tokenizers (e.g., DeepSeek's
// tokenizer vs. GPT's CL100K).
//
// The ratio is updated using an exponential moving average (EMA) with a
// weight of 0.5, so recent observations have more influence than older ones.
var (
        modelTokenRates   = map[string]float64{}
        modelTokenRatesMu sync.RWMutex
)

// DefaultTokensPerChar is the generic heuristic ratio (2.5 chars per token).
const DefaultTokensPerChar = 0.4 // 1 / 2.5

// UpdateModelTokenRate records a tokens-per-character ratio for a specific
// model, learned from actual API usage data. Uses an exponential moving
// average (EMA) to smooth out per-request variance.
func UpdateModelTokenRate(model string, ratio float64) {
        if model == "" || ratio <= 0 || ratio > 2.0 {
                return // Sanity check: reject unreasonable ratios
        }
        modelTokenRatesMu.Lock()
        if existing, ok := modelTokenRates[model]; ok {
                modelTokenRates[model] = (existing + ratio) / 2 // EMA with weight 0.5
        } else {
                modelTokenRates[model] = ratio
        }
        modelTokenRatesMu.Unlock()
}

// GetModelTokenRate returns the learned tokens-per-character ratio for a model,
// or the default heuristic ratio if no data is available.
func GetModelTokenRate(model string) float64 {
        modelTokenRatesMu.RLock()
        ratio, ok := modelTokenRates[model]
        modelTokenRatesMu.RUnlock()
        if !ok {
                return DefaultTokensPerChar
        }
        return ratio
}

// EstimateMessageTokens estimates the token count for a single message,
// including Content, ReasoningContent, ToolCalls arguments, ToolCallID
// metadata, and Media items. Uses a heuristic of 2.5 characters per token.
func EstimateMessageTokens(msg providers.Message) int {
        contentChars := utf8.RuneCountInString(msg.Content)

        // SystemParts are structured system blocks used for cache-aware adapters.
        // They carry the same content as Content, but in multiple blocks.
        // We estimate them as an alternative representation, not additive.
        systemPartsChars := 0
        if len(msg.SystemParts) > 0 {
                for _, part := range msg.SystemParts {
                        systemPartsChars += utf8.RuneCountInString(part.Text)
                }
                // Per-part overhead for JSON structure (type, text, cache_control).
                const perPartOverhead = 20
                systemPartsChars += len(msg.SystemParts) * perPartOverhead
        }

        // Use the larger of the two representations to stay conservative.
        chars := contentChars
        if systemPartsChars > chars {
                chars = systemPartsChars
        }

        chars += utf8.RuneCountInString(msg.ReasoningContent)

        for _, tc := range msg.ToolCalls {
                chars += len(tc.ID) + len(tc.Type)
                if tc.Function != nil {
                        // Count function name + arguments (the wire format for most providers).
                        // tc.Name mirrors tc.Function.Name — count only once to avoid double-counting.
                        chars += len(tc.Function.Name) + len(tc.Function.Arguments)
                } else {
                        // Fallback: some provider formats use top-level Name without Function.
                        chars += len(tc.Name)
                }
        }

        if msg.ToolCallID != "" {
                chars += len(msg.ToolCallID)
        }

        // Per-message overhead for role label, JSON structure, separators.
        const messageOverhead = 12
        chars += messageOverhead

        tokens := chars * 2 / 5

        // Media items (images, files) are serialized by provider adapters into
        // multipart or image_url payloads. Add a fixed per-item token estimate
        // directly (not through the chars heuristic) since actual cost depends
        // on resolution and provider-specific image tokenization.
        const mediaTokensPerItem = 256
        tokens += len(msg.Media) * mediaTokensPerItem

        return tokens
}

// EstimateToolDefsTokens estimates the total token cost of tool definitions
// as they appear in the LLM request.
func EstimateToolDefsTokens(defs []providers.ToolDefinition) int {
        if len(defs) == 0 {
                return 0
        }

        totalChars := 0
        for _, d := range defs {
                totalChars += len(d.Function.Name) + len(d.Function.Description)

                if d.Function.Parameters != nil {
                        if paramJSON, err := json.Marshal(d.Function.Parameters); err == nil {
                                totalChars += len(paramJSON)
                        }
                }

                // Per-tool overhead: type field, JSON structure, separators.
                totalChars += 20
        }

        return totalChars * 2 / 5
}
