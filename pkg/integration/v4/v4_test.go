//go:build integration

package v4_integration

import (
        "bytes"
        "context"
        "encoding/json"
        "fmt"
        "io"
        "net/http"
        "strings"
        "testing"
        "time"

        "github.com/sipeed/picoclaw/pkg/tokenizer"
        "github.com/sipeed/picoclaw/pkg/providers/openai_compat"
        "github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

// Aliases for shorter code
type Message = protocoltypes.Message

// newV4Provider creates a Provider configured for DeepSeek V4.
func newV4Provider(t *testing.T) *openai_compat.Provider {
        t.Helper()
        p := openai_compat.NewProvider(
                getDeepSeekAPIKey(t),
                "https://api.deepseek.com",
                "",
                openai_compat.WithProviderName("deepseek"),
                openai_compat.WithRequestTimeout(90*time.Second),
        )
        return p
}

// ============================================================================
// Test 1: Basic non-streaming chat with thinking enabled
// Verifies: V4 model responds, reasoning_content is populated, usage data returned
// ============================================================================

func TestV4Flash_ChatWithThinking(t *testing.T) {
        p := newV4Provider(t)
        ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
        defer cancel()

        resp, err := p.Chat(ctx,
                []Message{
                        {Role: "user", Content: "What is 17 * 23? Think step by step, then give the final answer as just the number."},
                },
                nil,
                "deepseek-v4-flash",
                map[string]any{
                        "thinking_level": "high",
                        "max_tokens":     512,
                },
        )
        if err != nil {
                t.Fatalf("Chat() error = %v", err)
        }

        if resp.Content == "" {
                t.Fatal("Content is empty — expected a response")
        }
        t.Logf("Content: %s", resp.Content)

        if resp.ReasoningContent == "" {
                t.Error("ReasoningContent is empty — expected V4 reasoning with thinking_level=high")
        } else {
                t.Logf("ReasoningContent length: %d chars", len(resp.ReasoningContent))
                t.Logf("ReasoningContent (first 300): %.300s", resp.ReasoningContent)
        }

        if resp.Usage == nil {
                t.Error("Usage is nil — expected token usage data from V4 API")
        } else {
                t.Logf("Usage: prompt=%d completion=%d total=%d cache_hit=%d",
                        resp.Usage.PromptTokens, resp.Usage.CompletionTokens,
                        resp.Usage.TotalTokens, resp.Usage.PromptCacheHitTokens)
        }
}

// ============================================================================
// Test 2: Non-streaming chat with thinking OFF
// Verifies: V4 model works in non-think mode, no reasoning_content
// ============================================================================

func TestV4Flash_ChatNoThinking(t *testing.T) {
        p := newV4Provider(t)
        ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
        defer cancel()

        resp, err := p.Chat(ctx,
                []Message{
                        {Role: "user", Content: "Say exactly: pong"},
                },
                nil,
                "deepseek-v4-flash",
                map[string]any{
                        "thinking_level": "off",
                        "max_tokens":     64,
                },
        )
        if err != nil {
                t.Fatalf("Chat() error = %v", err)
        }

        if resp.Content == "" {
                t.Fatal("Content is empty")
        }
        t.Logf("Content: %s", resp.Content)

        if resp.ReasoningContent != "" {
                t.Errorf("ReasoningContent should be empty with thinking_level=off, got %d chars", len(resp.ReasoningContent))
        }

        if resp.Usage != nil {
                t.Logf("Usage: prompt=%d completion=%d total=%d",
                        resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
        }
}

// ============================================================================
// Test 3: Streaming chat with thinking enabled
// Verifies: SSE streaming works, reasoning_content accumulated from deltas,
// onChunk callback fires, usage returned with stream_include_usage
// ============================================================================

func TestV4Flash_StreamingWithThinking(t *testing.T) {
        p := newV4Provider(t)
        ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
        defer cancel()

        var chunkCount int
        var lastAccumulated string
        resp, err := p.ChatStream(ctx,
                []Message{
                        {Role: "user", Content: "Count from 1 to 5, one number per line."},
                },
                nil,
                "deepseek-v4-flash",
                map[string]any{
                        "thinking_level":      "medium",
                        "max_tokens":          256,
                        "stream_include_usage": true,
                },
                func(accumulated string) {
                        chunkCount++
                        lastAccumulated = accumulated
                },
        )
        if err != nil {
                t.Fatalf("ChatStream() error = %v", err)
        }

        if chunkCount == 0 {
                t.Fatal("Expected streaming chunks, got 0")
        }
        t.Logf("Streaming chunks received: %d", chunkCount)

        if resp.Content == "" {
                t.Fatal("Final content is empty")
        }
        t.Logf("Final content: %s", resp.Content)

        if resp.ReasoningContent == "" {
                t.Error("ReasoningContent is empty — expected V4 reasoning with thinking_level=medium")
        } else {
                t.Logf("ReasoningContent length: %d chars", len(resp.ReasoningContent))
        }

        if resp.Usage == nil {
                t.Error("Usage is nil — expected usage data with stream_include_usage=true")
        } else {
                t.Logf("Usage: prompt=%d completion=%d total=%d cache_hit=%d",
                        resp.Usage.PromptTokens, resp.Usage.CompletionTokens,
                        resp.Usage.TotalTokens, resp.Usage.PromptCacheHitTokens)
        }

        t.Logf("Last accumulated chunk (first 200): %.200s", lastAccumulated)
}

// ============================================================================
// Test 4: Multi-turn conversation with reasoning_content preservation
// Verifies: V4 interleaved thinking — reasoning from earlier turns is preserved
// ============================================================================

func TestV4Flash_MultiTurnReasoningPreservation(t *testing.T) {
        p := newV4Provider(t)
        ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
        defer cancel()

        // Turn 1
        resp1, err := p.Chat(ctx,
                []Message{
                        {Role: "user", Content: "What is the capital of France? Answer in one word."},
                },
                nil,
                "deepseek-v4-flash",
                map[string]any{
                        "thinking_level": "medium",
                        "max_tokens":     64,
                },
        )
        if err != nil {
                t.Fatalf("Turn 1 Chat() error = %v", err)
        }
        t.Logf("Turn 1 content: %s", resp1.Content)
        if resp1.ReasoningContent == "" {
                t.Fatal("Turn 1: Expected reasoning_content, got empty")
        }
        t.Logf("Turn 1 reasoning length: %d chars", len(resp1.ReasoningContent))

        // Turn 2: Include previous reasoning in conversation
        resp2, err := p.Chat(ctx,
                []Message{
                        {Role: "user", Content: "What is the capital of France? Answer in one word."},
                        {Role: "assistant", Content: resp1.Content, ReasoningContent: resp1.ReasoningContent},
                        {Role: "user", Content: "What about Germany? Also one word."},
                },
                nil,
                "deepseek-v4-flash",
                map[string]any{
                        "thinking_level": "medium",
                        "max_tokens":     64,
                },
        )
        if err != nil {
                t.Fatalf("Turn 2 Chat() error = %v", err)
        }
        t.Logf("Turn 2 content: %s", resp2.Content)
        if resp2.ReasoningContent == "" {
                t.Error("Turn 2: Expected reasoning_content, got empty")
        }

        content := strings.ToLower(resp2.Content)
        if !strings.Contains(content, "berlin") && !strings.Contains(content, "germany") {
                t.Errorf("Turn 2: Expected answer about Berlin/Germany, got: %s", resp2.Content)
        }
}

// ============================================================================
// Test 5: Tool calling with V4
// Verifies: V4 model can make tool calls, reasoning is preserved when tools present
// ============================================================================

func TestV4Flash_ToolCalling(t *testing.T) {
        p := newV4Provider(t)
        ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
        defer cancel()

        tools := []protocoltypes.ToolDefinition{
                {
                        Type: "function",
                        Function: protocoltypes.ToolFunctionDefinition{
                                Name:        "get_weather",
                                Description: "Get the current weather for a city",
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

        resp, err := p.Chat(ctx,
                []Message{
                        {Role: "user", Content: "What is the weather in Tokyo?"},
                },
                tools,
                "deepseek-v4-flash",
                map[string]any{
                        "thinking_level": "medium",
                        "max_tokens":     256,
                },
        )
        if err != nil {
                t.Fatalf("Chat() with tools error = %v", err)
        }

        t.Logf("Content: %s", resp.Content)
        t.Logf("ReasoningContent length: %d", len(resp.ReasoningContent))

        if len(resp.ToolCalls) == 0 {
                t.Fatal("Expected tool calls, got 0")
        }

        tc := resp.ToolCalls[0]
        // Log the full tool call structure for debugging
        t.Logf("Tool call: ID=%s Type=%s Name=%s", tc.ID, tc.Type, tc.Name)
        if tc.Function != nil {
                t.Logf("Tool call function: name=%s arguments=%s", tc.Function.Name, tc.Function.Arguments)
        } else {
                t.Logf("Tool call Function is nil (may use top-level Name field)")
        }

        // Check tool call name (could be in Function.Name or tc.Name)
        toolName := tc.Name
        if tc.Function != nil {
                toolName = tc.Function.Name
        }
        if toolName != "get_weather" {
                t.Errorf("Expected tool call to get_weather, got %s", toolName)
        }

        // Check arguments contain Tokyo
        // Arguments can be in tc.Function.Arguments (string JSON) or tc.Arguments (map)
        toolArgs := ""
        if tc.Function != nil {
                toolArgs = tc.Function.Arguments
        }
        if toolArgs == "" && len(tc.Arguments) > 0 {
                if argsJSON, err := json.Marshal(tc.Arguments); err == nil {
                        toolArgs = string(argsJSON)
                }
        }
        t.Logf("Tool call arguments: %s", toolArgs)
        if !strings.Contains(toolArgs, "Tokyo") && !strings.Contains(toolArgs, "tokyo") {
                t.Errorf("Expected tool call arguments to contain 'Tokyo', got %s", toolArgs)
        }

        if resp.ReasoningContent == "" {
                t.Error("ReasoningContent is empty — V4 with tools should preserve reasoning")
        }
}

// ============================================================================
// Test 6: Token estimator feedback loop
// Verifies: UpdateModelTokenRate + GetModelTokenRate + EstimateMessageTokensForModel
// ============================================================================

func TestV4Flash_TokenEstimatorFeedbackLoop(t *testing.T) {
        p := newV4Provider(t)
        ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
        defer cancel()

        msg := Message{Role: "user", Content: "Hello, what is 2+2? Answer with just the number."}
        resp, err := p.Chat(ctx, []Message{msg}, nil, "deepseek-v4-flash",
                map[string]any{
                        "thinking_level": "off",
                        "max_tokens":     32,
                },
        )
        if err != nil {
                t.Fatalf("Chat() error = %v", err)
        }
        if resp.Usage == nil {
                t.Fatal("Usage is nil — cannot run feedback loop test without usage data")
        }

        model := "deepseek-v4-flash"
        promptTokens := resp.Usage.PromptTokens
        t.Logf("Actual prompt_tokens from API: %d", promptTokens)

        contentChars := len([]rune(msg.Content))
        actualRatio := float64(promptTokens) / float64(contentChars)
        t.Logf("Actual tokens/char: %.4f (chars=%d)", actualRatio, contentChars)

        defaultRate := tokenizer.GetModelTokenRate(model)
        t.Logf("Default rate before update: %.4f", defaultRate)

        tokenizer.UpdateModelTokenRate(model, actualRatio)

        learnedRate := tokenizer.GetModelTokenRate(model)
        t.Logf("Learned rate after update: %.4f", learnedRate)

        if learnedRate == defaultRate {
                t.Error("Learned rate should differ from default after UpdateModelTokenRate")
        }

        estimatedTokens := tokenizer.EstimateMessageTokensForModel(msg, model)
        t.Logf("Estimated tokens with model-aware rate: %d", estimatedTokens)

        genericTokens := tokenizer.EstimateMessageTokens(msg)
        t.Logf("Estimated tokens with generic rate: %d", genericTokens)

        modelAwareError := absInt(estimatedTokens - promptTokens)
        genericError := absInt(genericTokens - promptTokens)
        t.Logf("Model-aware error: %d tokens, Generic error: %d tokens", modelAwareError, genericError)

        if modelAwareError >= genericError {
                t.Logf("WARNING: Model-aware estimate not more accurate than generic (can happen with few data points)")
        }

        lowerBound := promptTokens / 2
        upperBound := promptTokens * 3 / 2
        if estimatedTokens < lowerBound || estimatedTokens > upperBound {
                t.Errorf("Model-aware estimate %d is outside reasonable range [%d, %d]",
                        estimatedTokens, lowerBound, upperBound)
        }
}

// ============================================================================
// Test 7: Streaming with error handling
// Verifies: Normal streaming returns properly, no panics
// ============================================================================

func TestV4Flash_StreamingNoPanic(t *testing.T) {
        p := newV4Provider(t)
        ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
        defer cancel()

        var chunks []string
        resp, err := p.ChatStream(ctx,
                []Message{
                        {Role: "user", Content: "Say 'test ok'"},
                },
                nil,
                "deepseek-v4-flash",
                map[string]any{
                        "thinking_level": "off",
                        "max_tokens":     128,
                },
                func(accumulated string) {
                        chunks = append(chunks, accumulated)
                },
        )
        if err != nil {
                t.Fatalf("ChatStream() error = %v", err)
        }
        if resp.Content == "" {
                t.Fatal("Content is empty")
        }
        t.Logf("Content: %s", resp.Content)
        t.Logf("Chunks: %d", len(chunks))
}

// ============================================================================
// Test 8: Raw API call to verify thinking/reasoning_content fields
// This bypasses the provider to verify the raw V4 API behavior
// ============================================================================

func TestV4Flash_RawAPI_ThinkingFields(t *testing.T) {
        apiKey := getDeepSeekAPIKey(t)
        ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
        defer cancel()

        // Build raw request body with thinking enabled
        reqBody := map[string]any{
                "model": "deepseek-v4-flash",
                "messages": []map[string]any{
                        {"role": "user", "content": "What is 3+4? Answer with just the number."},
                },
                "thinking": map[string]any{
                        "type": "enabled",
                },
                "reasoning_effort": "high",
                "max_tokens":       64,
        }

        jsonData, _ := json.Marshal(reqBody)
        req, err := http.NewRequestWithContext(ctx, "POST",
                "https://api.deepseek.com/chat/completions", bytes.NewReader(jsonData))
        if err != nil {
                t.Fatalf("Failed to create request: %v", err)
        }
        req.Header.Set("Content-Type", "application/json")
        req.Header.Set("Authorization", "Bearer "+apiKey)

        client := &http.Client{Timeout: 90 * time.Second}
        resp, err := client.Do(req)
        if err != nil {
                t.Fatalf("HTTP request error: %v", err)
        }
        defer resp.Body.Close()

        body, _ := io.ReadAll(resp.Body)
        t.Logf("HTTP status: %d", resp.StatusCode)

        if resp.StatusCode != 200 {
                t.Fatalf("API returned non-200: %s", string(body))
        }

        // Parse the response to verify reasoning_content structure
        var result map[string]any
        if err := json.Unmarshal(body, &result); err != nil {
                t.Fatalf("Failed to parse response JSON: %v", err)
        }

        choices, ok := result["choices"].([]any)
        if !ok || len(choices) == 0 {
                t.Fatal("No choices in response")
        }

        choice, ok := choices[0].(map[string]any)
        if !ok {
                t.Fatal("Invalid choice format")
        }

        message, ok := choice["message"].(map[string]any)
        if !ok {
                t.Fatal("Invalid message format")
        }

        t.Logf("Response message keys: %v", sortedKeys(message))

        content, _ := message["content"].(string)
        t.Logf("Content: %s", content)

        reasoningContent, _ := message["reasoning_content"].(string)
        if reasoningContent == "" {
                t.Error("reasoning_content is empty in raw API response — thinking may not be working")
        } else {
                t.Logf("reasoning_content length: %d chars", len(reasoningContent))
                t.Logf("reasoning_content (first 200): %.200s", reasoningContent)
        }

        if usage, ok := result["usage"].(map[string]any); ok {
                t.Logf("Usage from raw API: %v", usage)
                if cacheHit, ok := usage["prompt_cache_hit_tokens"].(float64); ok && cacheHit > 0 {
                        t.Logf("Cache hit tokens: %.0f", cacheHit)
                }
        }
}

// ============================================================================
// Test 9: Full V4 pipeline — thinking → tool call → result → feedback
// ============================================================================

func TestV4Flash_FullPipeline(t *testing.T) {
        p := newV4Provider(t)
        model := "deepseek-v4-flash"
        ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
        defer cancel()

        tools := []protocoltypes.ToolDefinition{
                {
                        Type: "function",
                        Function: protocoltypes.ToolFunctionDefinition{
                                Name:        "calculator",
                                Description: "Performs arithmetic calculations",
                                Parameters: map[string]any{
                                        "type": "object",
                                        "properties": map[string]any{
                                                "expression": map[string]any{
                                                        "type":        "string",
                                                        "description": "Math expression to evaluate",
                                                },
                                        },
                                        "required": []string{"expression"},
                                },
                        },
                },
        }

        // Step 1: User asks a question that requires tool use
        messages := []Message{
                {Role: "user", Content: "What is 157 * 283? Use the calculator tool."},
        }

        resp1, err := p.ChatStream(ctx, messages, tools, model,
                map[string]any{
                        "thinking_level":      "high",
                        "max_tokens":          512,
                        "stream_include_usage": true,
                },
                func(accumulated string) {},
        )
        if err != nil {
                t.Fatalf("Step 1 ChatStream() error = %v", err)
        }

        if resp1.ReasoningContent == "" {
                t.Error("Step 1: Expected reasoning_content with thinking_level=high")
        }
        t.Logf("Step 1: reasoning_content length=%d", len(resp1.ReasoningContent))

        if len(resp1.ToolCalls) == 0 {
                t.Fatalf("Step 1: Expected tool call, got none. Content: %s", resp1.Content)
        }
        tc := resp1.ToolCalls[0]
        // Log tool call structure for debugging
        if tc.Function != nil {
                t.Logf("Step 1: tool call function=%s args=%s", tc.Function.Name, tc.Function.Arguments)
        } else {
                t.Logf("Step 1: tool call ID=%s Type=%s Name=%s (Function is nil)", tc.ID, tc.Type, tc.Name)
        }

        toolName := tc.Name
        if tc.Function != nil {
                toolName = tc.Function.Name
        }
        if toolName != "calculator" {
                t.Errorf("Expected calculator tool call, got %s", toolName)
        }

        // Step 2: Return tool result
        messages = append(messages, Message{
                Role:             "assistant",
                Content:          resp1.Content,
                ReasoningContent: resp1.ReasoningContent,
                ToolCalls:        resp1.ToolCalls,
        })
        messages = append(messages, Message{
                Role:       "tool",
                Content:    fmt.Sprintf("Result: %d", 157*283),
                ToolCallID: tc.ID,
        })

        resp2, err := p.Chat(ctx, messages, tools, model,
                map[string]any{
                        "thinking_level": "medium",
                        "max_tokens":     256,
                },
        )
        if err != nil {
                t.Fatalf("Step 2 Chat() error = %v", err)
        }

        t.Logf("Step 2: content=%s", resp2.Content)
        t.Logf("Step 2: reasoning length=%d", len(resp2.ReasoningContent))

        content := resp2.Content
        if !strings.Contains(content, "44431") && !strings.Contains(content, "44,431") {
                t.Errorf("Step 2: Expected answer to contain 44431, got: %s", content)
        }

        // Step 3: Token estimator feedback loop
        if resp2.Usage != nil {
                promptTokens := resp2.Usage.PromptTokens
                completionTokens := resp2.Usage.CompletionTokens

                totalChars := 0
                for _, m := range messages {
                        totalChars += len([]rune(m.Content))
                        totalChars += len([]rune(m.ReasoningContent))
                }

                if totalChars > 0 {
                        ratio := float64(promptTokens) / float64(totalChars)
                        tokenizer.UpdateModelTokenRate(model, ratio)
                        learned := tokenizer.GetModelTokenRate(model)
                        t.Logf("Step 3: Token feedback — prompt=%d completion=%d chars=%d ratio=%.4f learned=%.4f",
                                promptTokens, completionTokens, totalChars, ratio, learned)
                }

                if resp2.Usage.PromptCacheHitTokens > 0 {
                        t.Logf("Step 3: Cache hit tokens = %d (prefix caching working!)", resp2.Usage.PromptCacheHitTokens)
                } else {
                        t.Logf("Step 3: No cache hits reported")
                }
        } else {
                t.Error("Step 3: Usage is nil — cannot run token feedback loop")
        }

        t.Logf("Full pipeline test PASSED")
}

// ============================================================================
// Test 10: V4 Pro model
// ============================================================================

func TestV4Pro_ChatWithThinking(t *testing.T) {
        p := newV4Provider(t)
        ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
        defer cancel()

        resp, err := p.Chat(ctx,
                []Message{
                        {Role: "user", Content: "What is 2+2? Answer with just the number."},
                },
                nil,
                "deepseek-v4-pro",
                map[string]any{
                        "thinking_level": "high",
                        "max_tokens":     64,
                },
        )
        if err != nil {
                t.Fatalf("Chat() with V4 Pro error = %v", err)
        }

        if resp.Content == "" {
                t.Fatal("Content is empty")
        }
        t.Logf("V4 Pro content: %s", resp.Content)
        t.Logf("V4 Pro reasoning length: %d chars", len(resp.ReasoningContent))

        if resp.Usage != nil {
                t.Logf("V4 Pro usage: prompt=%d completion=%d total=%d",
                        resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
        }
}

// ============================================================================
// Test 11: Streaming with tool calls
// Verifies: V4 can produce tool calls via streaming, reasoning present
// ============================================================================

func TestV4Flash_StreamingToolCall(t *testing.T) {
        p := newV4Provider(t)
        ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
        defer cancel()

        tools := []protocoltypes.ToolDefinition{
                {
                        Type: "function",
                        Function: protocoltypes.ToolFunctionDefinition{
                                Name:        "search",
                                Description: "Search the web for information",
                                Parameters: map[string]any{
                                        "type": "object",
                                        "properties": map[string]any{
                                                "query": map[string]any{
                                                        "type":        "string",
                                                        "description": "Search query",
                                                },
                                        },
                                        "required": []string{"query"},
                                },
                        },
                },
        }

        var chunkCount int
        resp, err := p.ChatStream(ctx,
                []Message{
                        {Role: "user", Content: "Search for the population of Paris"},
                },
                tools,
                "deepseek-v4-flash",
                map[string]any{
                        "thinking_level":      "medium",
                        "max_tokens":          256,
                        "stream_include_usage": true,
                },
                func(accumulated string) {
                        chunkCount++
                },
        )
        if err != nil {
                t.Fatalf("ChatStream() with tools error = %v", err)
        }

        t.Logf("Chunks: %d, Content: %s", chunkCount, resp.Content)
        t.Logf("Reasoning length: %d", len(resp.ReasoningContent))

        if len(resp.ToolCalls) == 0 {
                t.Fatal("Expected tool calls via streaming, got 0")
        }

        tc := resp.ToolCalls[0]
        if tc.Function != nil {
                t.Logf("Tool call: function=%s arguments=%s", tc.Function.Name, tc.Function.Arguments)
        } else {
                t.Logf("Tool call: ID=%s Type=%s Name=%s (Function is nil)", tc.ID, tc.Type, tc.Name)
        }

        toolName := tc.Name
        if tc.Function != nil {
                toolName = tc.Function.Name
        }
        if toolName != "search" {
                t.Errorf("Expected search tool call, got %s", toolName)
        }

        if resp.ReasoningContent == "" {
                t.Error("ReasoningContent empty — V4 with tools via streaming should have reasoning")
        }

        if resp.Usage != nil {
                t.Logf("Usage: prompt=%d completion=%d total=%d cache_hit=%d",
                        resp.Usage.PromptTokens, resp.Usage.CompletionTokens,
                        resp.Usage.TotalTokens, resp.Usage.PromptCacheHitTokens)
        }
}

// ============================================================================
// Helpers
// ============================================================================

func absInt(x int) int {
        if x < 0 {
                return -x
        }
        return x
}

func sortedKeys(m map[string]any) []string {
        keys := make([]string, 0, len(m))
        for k := range m {
                keys = append(keys, k)
        }
        return keys
}
