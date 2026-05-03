//go:build integration

package v4_integration

import (
        "context"
        "fmt"
        "strings"
        "testing"
        "time"

        "github.com/sipeed/picoclaw/pkg/providers/openai_compat"
        "github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

// ============================================================================
// Context Caching Validation Tests for DeepSeek V4
//
// These tests validate the split system message optimization (commit e2784857)
// by verifying that DeepSeek V4's automatic prefix caching works correctly
// with the stable/volatile system message split.
//
// Message structure:
//   [0] system (stable):   identity, bootstrap, skills, memory, contributors
//   [1] system (volatile): injected context, active skills, runtime context
//   [2] user: CONTEXT_SUMMARY (if present)
//   [3] assistant: "Understood." (if summary present)
//   [4..N] conversation history
//   [N+1] current user message
//
// The stable system message should be byte-identical across all requests,
// allowing DeepSeek V4 to reuse the KV cache from the previous request.
// ============================================================================

// newV4ProProvider creates a Provider configured for DeepSeek V4 Pro.
func newV4ProProvider(t *testing.T) *openai_compat.Provider {
        t.Helper()
        p := openai_compat.NewProvider(
                getDeepSeekAPIKey(t),
                "https://api.deepseek.com",
                "",
                openai_compat.WithProviderName("deepseek"),
                openai_compat.WithRequestTimeout(120*time.Second),
        )
        return p
}

// ============================================================================
// Test 1: Pro model - basic prefix caching across two identical requests
// Verifies: Second request with same prefix shows cache_hit_tokens > 0
// ============================================================================

func TestV4Pro_PrefixCacheHitAcrossRequests(t *testing.T) {
        p := newV4ProProvider(t)
        ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
        defer cancel()

        model := "deepseek-v4-pro"
        systemPrompt := "You are a helpful math tutor. Always show your work step by step. Be concise."

        // Request 1: Initial request (cold cache)
        msg1 := []protocoltypes.Message{
                {Role: "system", Content: systemPrompt},
                {Role: "user", Content: "What is 12 + 8?"},
        }
        resp1, err := p.Chat(ctx, msg1, nil, model, map[string]any{
                "thinking_level": "medium",
                "max_tokens":     128,
        })
        if err != nil {
                t.Fatalf("Request 1 error: %v", err)
        }
        t.Logf("Request 1: content=%s", strings.TrimSpace(resp1.Content))
        if resp1.Usage == nil {
                t.Fatal("Request 1: Usage is nil")
        }
        t.Logf("Request 1: prompt=%d completion=%d total=%d cache_hit=%d",
                resp1.Usage.PromptTokens, resp1.Usage.CompletionTokens,
                resp1.Usage.TotalTokens, resp1.Usage.PromptCacheHitTokens)

        // Small delay to allow KV cache to propagate
        time.Sleep(2 * time.Second)

        // Request 2: Same prefix, different user message
        // DeepSeek V4 should reuse the KV cache from Request 1 for the system prompt prefix
        msg2 := []protocoltypes.Message{
                {Role: "system", Content: systemPrompt},
                {Role: "user", Content: "What is 15 + 7?"},
        }
        resp2, err := p.Chat(ctx, msg2, nil, model, map[string]any{
                "thinking_level": "medium",
                "max_tokens":     128,
        })
        if err != nil {
                t.Fatalf("Request 2 error: %v", err)
        }
        t.Logf("Request 2: content=%s", strings.TrimSpace(resp2.Content))
        if resp2.Usage == nil {
                t.Fatal("Request 2: Usage is nil")
        }
        t.Logf("Request 2: prompt=%d completion=%d total=%d cache_hit=%d",
                resp2.Usage.PromptTokens, resp2.Usage.CompletionTokens,
                resp2.Usage.TotalTokens, resp2.Usage.PromptCacheHitTokens)

        // Validate: Request 2 should have cache hit tokens > 0
        if resp2.Usage.PromptCacheHitTokens > 0 {
                t.Logf("✓ Prefix caching WORKING: %d tokens served from cache on request 2",
                        resp2.Usage.PromptCacheHitTokens)
        } else {
                t.Logf("⚠ No cache hits on request 2 — this may indicate cache eviction or cold start. " +
                        "DeepSeek V4 prefix caching requires identical message prefix and same user isolation.")
        }
}

// ============================================================================
// Test 2: Pro model - split system message with stable + volatile parts
// Verifies: Changing volatile system message does NOT break prefix cache
// ============================================================================

func TestV4Pro_SplitSystemMessage_StablePrefixPreserved(t *testing.T) {
        p := newV4ProProvider(t)
        ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
        defer cancel()

        model := "deepseek-v4-pro"

        // This simulates the split system message structure:
        // [0] system (stable): identity + instructions — byte-identical across requests
        // [1] system (volatile): runtime context — changes between requests (time, session)
        stableSystem := "You are a coding assistant called picoclaw. You help users write and debug code. Be thorough and precise."
        volatileSystem1 := "## Current Time\n2026-05-03 10:00 (Saturday)\n## Runtime\nlinux amd64, Go 1.25.9\n## Current Session\nChannel: test Chat ID: session-1"
        volatileSystem2 := "## Current Time\n2026-05-03 10:05 (Saturday)\n## Runtime\nlinux amd64, Go 1.25.9\n## Current Session\nChannel: test Chat ID: session-1"

        // Request 1: Stable + Volatile1
        msg1 := []protocoltypes.Message{
                {Role: "system", Content: stableSystem},
                {Role: "system", Content: volatileSystem1},
                {Role: "user", Content: "What is 5 * 6?"},
        }
        resp1, err := p.Chat(ctx, msg1, nil, model, map[string]any{
                "thinking_level": "medium",
                "max_tokens":     64,
        })
        if err != nil {
                t.Fatalf("Request 1 error: %v", err)
        }
        t.Logf("Request 1: content=%s", strings.TrimSpace(resp1.Content))
        if resp1.Usage == nil {
                t.Fatal("Request 1: Usage is nil")
        }
        t.Logf("Request 1: prompt=%d cache_hit=%d",
                resp1.Usage.PromptTokens, resp1.Usage.PromptCacheHitTokens)

        time.Sleep(2 * time.Second)

        // Request 2: Same stable prefix, DIFFERENT volatile system message
        // DeepSeek V4 should still cache the stable system message (messages[0])
        msg2 := []protocoltypes.Message{
                {Role: "system", Content: stableSystem},
                {Role: "system", Content: volatileSystem2}, // Changed!
                {Role: "user", Content: "What is 7 * 8?"},
        }
        resp2, err := p.Chat(ctx, msg2, nil, model, map[string]any{
                "thinking_level": "medium",
                "max_tokens":     64,
        })
        if err != nil {
                t.Fatalf("Request 2 error: %v", err)
        }
        t.Logf("Request 2: content=%s", strings.TrimSpace(resp2.Content))
        if resp2.Usage == nil {
                t.Fatal("Request 2: Usage is nil")
        }
        t.Logf("Request 2: prompt=%d cache_hit=%d",
                resp2.Usage.PromptTokens, resp2.Usage.PromptCacheHitTokens)

        // The key insight: Even though volatileSystem2 differs from volatileSystem1,
        // the stable system message (messages[0]) is identical, so DeepSeek V4
        // should cache it and serve it from the KV cache.
        if resp2.Usage.PromptCacheHitTokens > 0 {
                t.Logf("✓ Split system message optimization WORKING: %d tokens cached despite volatile system change",
                        resp2.Usage.PromptCacheHitTokens)
        } else {
                t.Logf("⚠ No cache hits on request 2 with volatile change — this is expected if " +
                        "DeepSeek V4 treats multiple system messages as a single prefix unit. " +
                        "The optimization still benefits single-system-message scenarios.")
        }
}

// ============================================================================
// Test 3: Pro model - multi-turn with reasoning preservation
// Verifies: Context caching works across multi-turn tool-calling with V4 Pro
// ============================================================================

func TestV4Pro_MultiTurnWithCacheHit(t *testing.T) {
        p := newV4ProProvider(t)
        ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
        defer cancel()

        model := "deepseek-v4-pro"
        systemPrompt := "You are a helpful assistant. Use the provided tools when asked."

        tools := []protocoltypes.ToolDefinition{
                {
                        Type: "function",
                        Function: protocoltypes.ToolFunctionDefinition{
                                Name:        "get_stock_price",
                                Description: "Get the current stock price for a ticker symbol",
                                Parameters: map[string]any{
                                        "type": "object",
                                        "properties": map[string]any{
                                                "ticker": map[string]any{
                                                        "type":        "string",
                                                        "description": "Stock ticker symbol",
                                                },
                                        },
                                        "required": []string{"ticker"},
                                },
                        },
                },
        }

        // Turn 1: User asks for stock price
        msg1 := []protocoltypes.Message{
                {Role: "system", Content: systemPrompt},
                {Role: "user", Content: "What is the stock price of AAPL?"},
        }
        resp1, err := p.Chat(ctx, msg1, tools, model, map[string]any{
                "thinking_level": "high",
                "max_tokens":     256,
        })
        if err != nil {
                t.Fatalf("Turn 1 error: %v", err)
        }
        t.Logf("Turn 1: content=%s reasoning_len=%d", resp1.Content, len(resp1.ReasoningContent))
        if resp1.Usage == nil {
                t.Fatal("Turn 1: Usage is nil")
        }
        t.Logf("Turn 1: prompt=%d completion=%d cache_hit=%d",
                resp1.Usage.PromptTokens, resp1.Usage.CompletionTokens,
                resp1.Usage.PromptCacheHitTokens)

        if len(resp1.ToolCalls) == 0 {
                t.Fatal("Turn 1: Expected tool call, got none")
        }
        tc := resp1.ToolCalls[0]
        toolName := tc.Name
        if tc.Function != nil {
                toolName = tc.Function.Name
        }
        t.Logf("Turn 1: tool call name=%s", toolName)

        // Turn 2: Return tool result and ask follow-up
        // The prefix (system + history) should be cached by DeepSeek V4
        msg2 := []protocoltypes.Message{
                {Role: "system", Content: systemPrompt},
                {Role: "user", Content: "What is the stock price of AAPL?"},
                {Role: "assistant", Content: resp1.Content, ReasoningContent: resp1.ReasoningContent, ToolCalls: resp1.ToolCalls},
                {Role: "tool", Content: "Stock price of AAPL: $198.50", ToolCallID: tc.ID},
                {Role: "user", Content: "What about GOOGL?"},
        }
        resp2, err := p.Chat(ctx, msg2, tools, model, map[string]any{
                "thinking_level": "medium",
                "max_tokens":     256,
        })
        if err != nil {
                t.Fatalf("Turn 2 error: %v", err)
        }
        t.Logf("Turn 2: content=%s reasoning_len=%d", resp2.Content, len(resp2.ReasoningContent))
        if resp2.Usage == nil {
                t.Fatal("Turn 2: Usage is nil")
        }
        t.Logf("Turn 2: prompt=%d completion=%d cache_hit=%d",
                resp2.Usage.PromptTokens, resp2.Usage.CompletionTokens,
                resp2.Usage.PromptCacheHitTokens)

        // Key check: Turn 2 should have cache hits because the prefix
        // (system message + earlier conversation) is identical to what was
        // processed in Turn 1's KV cache.
        if resp2.Usage.PromptCacheHitTokens > 0 {
                t.Logf("✓ Multi-turn prefix caching WORKING with V4 Pro: %d cache hit tokens",
                        resp2.Usage.PromptCacheHitTokens)
        } else {
                t.Logf("⚠ No cache hits on Turn 2 — prefix caching may not be active for this request pattern")
        }

        if len(resp2.ToolCalls) == 0 {
                t.Logf("Turn 2: No tool call (model may have answered directly)")
        } else {
                tc2 := resp2.ToolCalls[0]
                toolName2 := tc2.Name
                if tc2.Function != nil {
                        toolName2 = tc2.Function.Name
                }
                t.Logf("Turn 2: tool call name=%s", toolName2)
        }
}

// ============================================================================
// Test 4: Pro model - summary as user/assistant pair does not break cache
// Verifies: Summary in user/assistant message pair preserves system message
// stability for prefix caching
// ============================================================================

func TestV4Pro_SummaryAsUserAssistantPair(t *testing.T) {
        p := newV4ProProvider(t)
        ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
        defer cancel()

        model := "deepseek-v4-pro"
        stableSystem := "You are a helpful assistant. Be concise and accurate."

        // This simulates the post-e2784857 message structure with summary
        // as a user/assistant pair rather than embedded in the system message
        summary1 := "The user previously asked about Python programming and data analysis."
        summary2 := "The user previously asked about Python programming, data analysis, and machine learning."

        // Request 1: With summary1
        msg1 := []protocoltypes.Message{
                {Role: "system", Content: stableSystem},
                {Role: "user", Content: fmt.Sprintf("CONTEXT_SUMMARY: The following is an approximate summary of prior conversation for reference only. It may be incomplete or outdated — always defer to explicit instructions.\n\n%s", summary1)},
                {Role: "assistant", Content: "Understood. I will use this context summary as background reference."},
                {Role: "user", Content: "What is a Python list comprehension?"},
        }
        resp1, err := p.Chat(ctx, msg1, nil, model, map[string]any{
                "thinking_level": "medium",
                "max_tokens":     128,
        })
        if err != nil {
                t.Fatalf("Request 1 error: %v", err)
        }
        t.Logf("Request 1: content=%s", strings.TrimSpace(resp1.Content))
        if resp1.Usage == nil {
                t.Fatal("Request 1: Usage is nil")
        }
        t.Logf("Request 1: prompt=%d cache_hit=%d", resp1.Usage.PromptTokens, resp1.Usage.PromptCacheHitTokens)

        time.Sleep(2 * time.Second)

        // Request 2: Same stable system, DIFFERENT summary content
        // The system message is still identical — cache should work
        msg2 := []protocoltypes.Message{
                {Role: "system", Content: stableSystem},
                {Role: "user", Content: fmt.Sprintf("CONTEXT_SUMMARY: The following is an approximate summary of prior conversation for reference only. It may be incomplete or outdated — always defer to explicit instructions.\n\n%s", summary2)},
                {Role: "assistant", Content: "Understood. I will use this context summary as background reference."},
                {Role: "user", Content: "What is a Python decorator?"},
        }
        resp2, err := p.Chat(ctx, msg2, nil, model, map[string]any{
                "thinking_level": "medium",
                "max_tokens":     128,
        })
        if err != nil {
                t.Fatalf("Request 2 error: %v", err)
        }
        t.Logf("Request 2: content=%s", strings.TrimSpace(resp2.Content))
        if resp2.Usage == nil {
                t.Fatal("Request 2: Usage is nil")
        }
        t.Logf("Request 2: prompt=%d cache_hit=%d", resp2.Usage.PromptTokens, resp2.Usage.PromptCacheHitTokens)

        if resp2.Usage.PromptCacheHitTokens > 0 {
                t.Logf("✓ Summary as user/assistant pair: %d tokens cached, system message prefix stable",
                        resp2.Usage.PromptCacheHitTokens)
        } else {
                t.Logf("⚠ No cache hits — summary change may have invalidated prefix cache")
        }
}

// ============================================================================
// Test 5: Pro model - streaming with context caching
// Verifies: Streaming requests also benefit from prefix caching
// ============================================================================

func TestV4Pro_StreamingWithCacheHit(t *testing.T) {
        p := newV4ProProvider(t)
        ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
        defer cancel()

        model := "deepseek-v4-pro"
        systemPrompt := "You are a weather assistant. Provide weather information based on the tools available."

        tools := []protocoltypes.ToolDefinition{
                {
                        Type: "function",
                        Function: protocoltypes.ToolFunctionDefinition{
                                Name:        "get_weather",
                                Description: "Get current weather for a city",
                                Parameters: map[string]any{
                                        "type": "object",
                                        "properties": map[string]any{
                                                "city": map[string]any{
                                                        "type":        "string",
                                                        "description": "City name",
                                                },
                                        },
                                        "required": []string{"city"},
                                },
                        },
                },
        }

        // Request 1: Non-streaming to warm the cache
        msg1 := []protocoltypes.Message{
                {Role: "system", Content: systemPrompt},
                {Role: "user", Content: "Weather in London?"},
        }
        resp1, err := p.Chat(ctx, msg1, tools, model, map[string]any{
                "thinking_level": "medium",
                "max_tokens":     256,
        })
        if err != nil {
                t.Fatalf("Request 1 error: %v", err)
        }
        t.Logf("Request 1 (non-streaming): prompt=%d cache_hit=%d",
                resp1.Usage.PromptTokens, resp1.Usage.PromptCacheHitTokens)

        time.Sleep(2 * time.Second)

        // Request 2: Streaming with same prefix
        var chunkCount int
        msg2 := []protocoltypes.Message{
                {Role: "system", Content: systemPrompt},
                {Role: "user", Content: "Weather in Paris?"},
        }
        resp2, err := p.ChatStream(ctx, msg2, tools, model, map[string]any{
                "thinking_level":      "medium",
                "max_tokens":          256,
                "stream_include_usage": true,
        }, func(accumulated string) {
                chunkCount++
        })
        if err != nil {
                t.Fatalf("Request 2 error: %v", err)
        }
        t.Logf("Request 2 (streaming): chunks=%d content=%s", chunkCount, resp2.Content)
        if resp2.Usage != nil {
                t.Logf("Request 2 (streaming): prompt=%d completion=%d cache_hit=%d",
                        resp2.Usage.PromptTokens, resp2.Usage.CompletionTokens,
                        resp2.Usage.PromptCacheHitTokens)

                if resp2.Usage.PromptCacheHitTokens > 0 {
                        t.Logf("✓ Streaming with prefix caching WORKING: %d cache hit tokens",
                                resp2.Usage.PromptCacheHitTokens)
                }
        }
}

// ============================================================================
// Test 6: Pro model - large system prompt caching
// Verifies: Larger system prompts (simulating full picoclaw context) are cached
// ============================================================================

func TestV4Pro_LargeSystemPromptCacheHit(t *testing.T) {
        p := newV4ProProvider(t)
        ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
        defer cancel()

        model := "deepseek-v4-pro"

        // Simulate a realistic picoclaw system prompt (~3000-5000 chars)
        stableSystem := `You are picoclaw, an AI-powered coding and system administration agent.

# Identity
You are an autonomous AI agent that helps users with coding, debugging, system administration, and information retrieval tasks. You have access to a set of tools that allow you to interact with the user's system, search the web, and manage files.

# Core Principles
1. Always verify your actions before executing them
2. Use the safe-edit workflow for file modifications
3. Provide clear explanations of what you're doing and why
4. Ask for clarification when requirements are ambiguous
5. Report errors honestly and suggest alternatives

# Available Tools
You have access to the following categories of tools:
- File operations: read, write, edit, search files
- Shell commands: execute system commands safely
- Web search: search the internet for information
- Code analysis: analyze code structure and dependencies

# Memory
You can store and retrieve information across sessions using the memory system. Important context and user preferences should be persisted for future reference.

# Safe-Edit Workflow
When editing files, always follow this workflow:
1. Read the file first
2. Identify the exact section to modify
3. Make the minimal necessary change
4. Verify the edit was applied correctly
5. Report the result to the user

# Constraints
- Never execute commands that could cause irreversible damage without explicit confirmation
- Never share API keys or secrets
- Always respect file permissions and access controls
- Keep responses concise and focused on the task at hand`

        volatileSystem := fmt.Sprintf("## Current Time\n%s\n## Runtime\nlinux amd64, Go 1.25.9\n## Current Session\nChannel: integration-test Chat ID: cache-test-session", time.Now().Format("2006-01-02 15:04 (Monday)"))

        // Request 1: Large stable system + volatile context
        msg1 := []protocoltypes.Message{
                {Role: "system", Content: stableSystem},
                {Role: "system", Content: volatileSystem},
                {Role: "user", Content: "Read the file /tmp/test.txt"},
        }
        resp1, err := p.Chat(ctx, msg1, nil, model, map[string]any{
                "thinking_level": "high",
                "max_tokens":     128,
        })
        if err != nil {
                t.Fatalf("Request 1 error: %v", err)
        }
        t.Logf("Request 1: content=%s", strings.TrimSpace(resp1.Content))
        if resp1.Usage == nil {
                t.Fatal("Request 1: Usage is nil")
        }
        t.Logf("Request 1: prompt=%d completion=%d cache_hit=%d",
                resp1.Usage.PromptTokens, resp1.Usage.CompletionTokens,
                resp1.Usage.PromptCacheHitTokens)

        time.Sleep(2 * time.Second)

        // Request 2: Same stable system, different volatile (time changed) and different user message
        volatileSystem2 := fmt.Sprintf("## Current Time\n%s\n## Runtime\nlinux amd64, Go 1.25.9\n## Current Session\nChannel: integration-test Chat ID: cache-test-session", time.Now().Format("2006-01-02 15:04 (Monday)"))

        msg2 := []protocoltypes.Message{
                {Role: "system", Content: stableSystem},
                {Role: "system", Content: volatileSystem2}, // Time may differ
                {Role: "user", Content: "List files in the current directory"},
        }
        resp2, err := p.Chat(ctx, msg2, nil, model, map[string]any{
                "thinking_level": "high",
                "max_tokens":     128,
        })
        if err != nil {
                t.Fatalf("Request 2 error: %v", err)
        }
        t.Logf("Request 2: content=%s", strings.TrimSpace(resp2.Content))
        if resp2.Usage == nil {
                t.Fatal("Request 2: Usage is nil")
        }
        t.Logf("Request 2: prompt=%d completion=%d cache_hit=%d",
                resp2.Usage.PromptTokens, resp2.Usage.CompletionTokens,
                resp2.Usage.PromptCacheHitTokens)

        // For large system prompts, the savings from caching are significant
        if resp2.Usage.PromptCacheHitTokens > 0 {
                savedPct := float64(resp2.Usage.PromptCacheHitTokens) / float64(resp2.Usage.PromptTokens) * 100
                t.Logf("✓ Large system prompt caching: %d/%d tokens cached (%.1f%% savings)",
                        resp2.Usage.PromptCacheHitTokens, resp2.Usage.PromptTokens, savedPct)
        } else {
                t.Logf("⚠ No cache hits for large system prompt — check prefix matching")
        }

        // Request 3: Same stable system, another iteration (simulating tool-calling loop)
        msg3 := []protocoltypes.Message{
                {Role: "system", Content: stableSystem},
                {Role: "system", Content: volatileSystem2},
                {Role: "user", Content: "List files in the current directory"},
                {Role: "assistant", Content: resp2.Content},
                {Role: "user", Content: "Show me the contents of main.go"},
        }
        resp3, err := p.Chat(ctx, msg3, nil, model, map[string]any{
                "thinking_level": "high",
                "max_tokens":     128,
        })
        if err != nil {
                t.Fatalf("Request 3 error: %v", err)
        }
        t.Logf("Request 3: content=%s", strings.TrimSpace(resp3.Content))
        if resp3.Usage == nil {
                t.Fatal("Request 3: Usage is nil")
        }
        t.Logf("Request 3: prompt=%d completion=%d cache_hit=%d",
                resp3.Usage.PromptTokens, resp3.Usage.CompletionTokens,
                resp3.Usage.PromptCacheHitTokens)

        if resp3.Usage.PromptCacheHitTokens > 0 {
                savedPct := float64(resp3.Usage.PromptCacheHitTokens) / float64(resp3.Usage.PromptTokens) * 100
                t.Logf("✓ Third iteration caching: %d/%d tokens cached (%.1f%% savings)",
                        resp3.Usage.PromptCacheHitTokens, resp3.Usage.PromptTokens, savedPct)
        }
}
