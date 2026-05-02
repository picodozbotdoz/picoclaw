// PicoClaw - Ultra-lightweight personal AI agent
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package agent

import (
        "github.com/sipeed/picoclaw/pkg/providers"
        "github.com/sipeed/picoclaw/pkg/tokenizer"
)

// parseTurnBoundaries returns the starting index of each Turn in the history.
// A Turn is a complete "user input → LLM iterations → final response" cycle
// (as defined in #1316). Each Turn begins at a user message and extends
// through all subsequent assistant/tool messages until the next user message.
//
// Cutting at a Turn boundary guarantees that no tool-call sequence
// (assistant+ToolCalls → tool results) is split across the cut.
func parseTurnBoundaries(history []providers.Message) []int {
        var starts []int
        for i, msg := range history {
                if msg.Role == "user" {
                        starts = append(starts, i)
                }
        }
        return starts
}

// isSafeBoundary reports whether index is a valid Turn boundary — i.e.,
// a position where the kept portion (history[index:]) begins at a user
// message, so no tool-call sequence is torn apart.
func isSafeBoundary(history []providers.Message, index int) bool {
        if index <= 0 || index >= len(history) {
                return true
        }
        return history[index].Role == "user"
}

// findSafeBoundary locates the nearest Turn boundary to targetIndex.
// It prefers the boundary at or before targetIndex (preserving more recent
// context). Falls back to the nearest boundary after targetIndex, and
// returns targetIndex unchanged only when no Turn boundary exists at all.
func findSafeBoundary(history []providers.Message, targetIndex int) int {
        if len(history) == 0 {
                return 0
        }
        if targetIndex <= 0 {
                return 0
        }
        if targetIndex >= len(history) {
                return len(history)
        }

        turns := parseTurnBoundaries(history)
        if len(turns) == 0 {
                return targetIndex
        }

        // Find the last Turn boundary at or before targetIndex.
        // Prefer backward: keeps more recent messages.
        backward := -1
        for _, t := range turns {
                if t <= targetIndex {
                        backward = t
                }
        }
        if backward > 0 {
                return backward
        }

        // No valid Turn boundary before target (or only at index 0 which
        // would keep everything). Use the first Turn after targetIndex.
        for _, t := range turns {
                if t > targetIndex {
                        return t
                }
        }

        // No Turn boundary after targetIndex either. The only boundary is at
        // index 0, meaning the entire history is a single Turn. Return 0 to
        // signal that safe compression is not possible — callers check for
        // mid <= 0 and skip compression in that case.
        return 0
}

// EstimateMessageTokens estimates the token count for a single message.
// Delegates to the shared tokenizer package for consistency across agent and seahorse.
func EstimateMessageTokens(msg providers.Message) int {
        return tokenizer.EstimateMessageTokens(msg)
}

// EstimateMessageTokensForModel estimates the token count for a single message
// using a model-specific tokens-per-character ratio when available. When the
// model has been seen in prior LLM calls, the learned ratio replaces the
// generic 2.5 chars/token heuristic, reducing the 20-40% error rate for
// models with different tokenizers (e.g., DeepSeek V4).
func EstimateMessageTokensForModel(msg providers.Message, model string) int {
        return tokenizer.EstimateMessageTokensForModel(msg, model)
}

// EstimateToolDefsTokens estimates the total token cost of tool definitions
// as they appear in the LLM request. Delegates to the shared tokenizer package.
func EstimateToolDefsTokens(defs []providers.ToolDefinition) int {
        return tokenizer.EstimateToolDefsTokens(defs)
}

// EstimateToolDefsTokensForModel estimates tool definition tokens using a
// model-specific ratio when available.
func EstimateToolDefsTokensForModel(defs []providers.ToolDefinition, model string) int {
        return tokenizer.EstimateToolDefsTokensForModel(defs, model)
}

// isOverContextBudget checks whether the assembled messages plus tool definitions
// and output reserve would exceed the model's context window. This enables
// proactive compression before calling the LLM, rather than reacting to 400 errors.
//
// When model is non-empty, uses model-aware token estimation (via
// EstimateMessageTokensForModel) for better accuracy on models whose tokenizers
// differ from the generic 2.5 chars/token heuristic.
func isOverContextBudget(
        contextWindow int,
        messages []providers.Message,
        toolDefs []providers.ToolDefinition,
        maxTokens int,
        model string,
) bool {
        msgTokens := 0
        if model != "" {
                for _, m := range messages {
                        msgTokens += EstimateMessageTokensForModel(m, model)
                }
        } else {
                for _, m := range messages {
                        msgTokens += EstimateMessageTokens(m)
                }
        }

        var toolTokens int
        if model != "" {
                toolTokens = EstimateToolDefsTokensForModel(toolDefs, model)
        } else {
                toolTokens = EstimateToolDefsTokens(toolDefs)
        }
        total := msgTokens + toolTokens + maxTokens

        return total > contextWindow
}
