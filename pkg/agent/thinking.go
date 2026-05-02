package agent

import "strings"

// ThinkingLevel controls how the provider sends thinking parameters.
//
//   - "adaptive": sends {thinking: {type: "adaptive"}} + output_config.effort (Claude 4.6+)
//   - "low"/"medium"/"high"/"xhigh": sends {thinking: {type: "enabled", budget_tokens: N}} (all models)
//   - "off": disables thinking
type ThinkingLevel string

const (
        ThinkingOff      ThinkingLevel = "off"
        ThinkingLow      ThinkingLevel = "low"
        ThinkingMedium   ThinkingLevel = "medium"
        ThinkingHigh     ThinkingLevel = "high"
        ThinkingXHigh    ThinkingLevel = "xhigh"
        ThinkingAdaptive ThinkingLevel = "adaptive"
)

// parseThinkingLevel normalizes a config string to a ThinkingLevel.
// Case-insensitive and whitespace-tolerant for user-facing config values.
// Returns ThinkingOff for unknown or empty values.
func parseThinkingLevel(level string) ThinkingLevel {
        switch strings.ToLower(strings.TrimSpace(level)) {
        case "adaptive":
                return ThinkingAdaptive
        case "low":
                return ThinkingLow
        case "medium":
                return ThinkingMedium
        case "high":
                return ThinkingHigh
        case "xhigh":
                return ThinkingXHigh
        default:
                return ThinkingOff
        }
}

// DynamicThinkingMode controls whether the agent automatically switches
// thinking levels between iterations within a turn.
//
//   - "auto": Automatically switch to non-think after tool execution,
//     switch back to the configured level for fresh reasoning iterations.
//   - "fixed": Always use the configured ThinkingLevel (no dynamic switching).
type DynamicThinkingMode string

const (
        DynamicThinkingAuto   DynamicThinkingMode = "auto"
        DynamicThinkingFixed  DynamicThinkingMode = "fixed"
)

// parseDynamicThinkingMode normalizes a config string to a DynamicThinkingMode.
// Returns DynamicThinkingAuto for "auto", DynamicThinkingFixed otherwise.
func parseDynamicThinkingMode(mode string) DynamicThinkingMode {
        switch strings.ToLower(strings.TrimSpace(mode)) {
        case "auto":
                return DynamicThinkingAuto
        default:
                return DynamicThinkingFixed
        }
}

// ThinkingModeStat records the thinking level used for a single LLM iteration.
type ThinkingModeStat struct {
        Iteration     int          `json:"iteration"`
        ThinkingLevel ThinkingLevel `json:"thinking_level"`
        Reason        string       `json:"reason"` // "initial", "post_tool", "steering_resume", "retry", "configured"
}

// resolveThinkingLevelForIteration determines the appropriate thinking level
// for a given iteration based on the dynamic thinking mode and iteration context.
//
// Logic (when DynamicThinkingMode == "auto"):
//   - First iteration: Use the configured ThinkingLevel, but boost to XHigh
//     if the complexity score is high (>0.7) and configured level is High.
//   - Post-tool-call iteration: Use ThinkingOff (tool result processing is fast)
//   - Steering-resume iteration: Use the configured ThinkingLevel (new reasoning needed)
//   - Retry after compression: Use the configured ThinkingLevel
//
// The complexityScore parameter (0.0–1.0) comes from the model router's
// SelectModel function and enables three-tier dynamic thinking:
//   - non-think (post-tool): fast tool result interpretation
//   - think-high (normal): standard reasoning
//   - think-max/xhigh (complex): deep analysis for complex tasks
//
// When DynamicThinkingMode == "fixed", always returns the configured level.
func resolveThinkingLevelForIteration(
        configuredLevel ThinkingLevel,
        dynamicMode DynamicThinkingMode,
        iteration int,
        postToolCall bool,
        steeringResumed bool,
        isRetry bool,
        complexityScore float64,
) (ThinkingLevel, string) {
        if dynamicMode == DynamicThinkingFixed || configuredLevel == ThinkingOff {
                return configuredLevel, "configured"
        }

        // Dynamic mode: switch based on context
        if iteration == 1 || isRetry || steeringResumed {
                // For complex initial/steering/retry turns, boost to higher thinking
                // if available. This gives the model more reasoning budget when the
                // task complexity warrants it (e.g., multi-step code refactoring,
                // complex debugging, or architecture decisions).
                if configuredLevel == ThinkingHigh && complexityScore > 0.7 {
                        return ThinkingXHigh, "complexity_boost"
                }
                if iteration == 1 {
                        return configuredLevel, "initial"
                }
                if isRetry {
                        return configuredLevel, "retry"
                }
                return configuredLevel, "steering_resume"
        }

        if postToolCall {
                // After tool execution, use non-think mode for faster processing.
                // Tool results don't need deep reasoning — the model just needs to
                // interpret the result and decide the next step.
                return ThinkingOff, "post_tool"
        }

        return configuredLevel, "configured"
}
