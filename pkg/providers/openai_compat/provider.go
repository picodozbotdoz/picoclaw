package openai_compat

import (
        "bufio"
        "bytes"
        "context"
        "encoding/json"
        "fmt"
        "io"
        "log"
        "maps"
        "net/http"
        "net/url"
        "strings"
        "time"

        "github.com/sipeed/picoclaw/pkg/providers/common"
        "github.com/sipeed/picoclaw/pkg/providers/messageutil"
        "github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

type (
        ToolCall               = protocoltypes.ToolCall
        FunctionCall           = protocoltypes.FunctionCall
        LLMResponse            = protocoltypes.LLMResponse
        UsageInfo              = protocoltypes.UsageInfo
        Message                = protocoltypes.Message
        ToolDefinition         = protocoltypes.ToolDefinition
        ToolFunctionDefinition = protocoltypes.ToolFunctionDefinition
        ExtraContent           = protocoltypes.ExtraContent
        GoogleExtra            = protocoltypes.GoogleExtra
        ReasoningDetail        = protocoltypes.ReasoningDetail
)

type Provider struct {
        apiKey         string
        apiBase        string
        providerName   string
        maxTokensField string // Field name for max tokens (e.g., "max_completion_tokens" for o1/glm models)
        httpClient     *http.Client
        extraBody      map[string]any // Additional fields to inject into request body
        customHeaders  map[string]string
        userAgent      string
}

type Option func(*Provider)

const defaultRequestTimeout = common.DefaultRequestTimeout

var stripModelPrefixProviders = map[string]struct{}{
        "litellm":    {},
        "venice":     {},
        "moonshot":   {},
        "nvidia":     {},
        "groq":       {},
        "ollama":     {},
        "deepseek":   {},
        "google":     {},
        "openrouter": {},
        "zhipu":      {},
        "mistral":    {},
        "vivgrid":    {},
        "minimax":    {},
        "novita":     {},
        "lmstudio":   {},
}

func WithMaxTokensField(maxTokensField string) Option {
        return func(p *Provider) {
                p.maxTokensField = maxTokensField
        }
}

func WithUserAgent(userAgent string) Option {
        return func(p *Provider) {
                p.userAgent = userAgent
        }
}

func WithRequestTimeout(timeout time.Duration) Option {
        return func(p *Provider) {
                if timeout > 0 {
                        p.httpClient.Timeout = timeout
                }
        }
}

func WithExtraBody(extraBody map[string]any) Option {
        return func(p *Provider) {
                p.extraBody = extraBody
        }
}

func WithCustomHeaders(customHeaders map[string]string) Option {
        return func(p *Provider) {
                p.customHeaders = customHeaders
        }
}

func WithProviderName(providerName string) Option {
        return func(p *Provider) {
                p.providerName = strings.ToLower(strings.TrimSpace(providerName))
        }
}

func NewProvider(apiKey, apiBase, proxy string, opts ...Option) *Provider {
        p := &Provider{
                apiKey:     apiKey,
                apiBase:    strings.TrimRight(apiBase, "/"),
                httpClient: common.NewHTTPClient(proxy),
        }

        for _, opt := range opts {
                if opt != nil {
                        opt(p)
                }
        }

        return p
}

func NewProviderWithMaxTokensField(apiKey, apiBase, proxy, maxTokensField string) *Provider {
        return NewProvider(apiKey, apiBase, proxy, WithMaxTokensField(maxTokensField))
}

func NewProviderWithMaxTokensFieldAndTimeout(
        apiKey, apiBase, proxy, maxTokensField string,
        requestTimeoutSeconds int,
) *Provider {
        return NewProvider(
                apiKey,
                apiBase,
                proxy,
                WithMaxTokensField(maxTokensField),
                WithRequestTimeout(time.Duration(requestTimeoutSeconds)*time.Second),
        )
}

// buildRequestBody constructs the common request body for Chat and ChatStream.
func (p *Provider) buildRequestBody(
        messages []Message, tools []ToolDefinition, model string, options map[string]any,
) map[string]any {
        model = normalizeModel(model, p.apiBase)

        requestBody := map[string]any{
                "model":    model,
                "messages": common.SerializeMessages(p.prepareMessagesForRequest(messages, model, tools)),
        }

        // When fallback uses a different provider (e.g. DeepSeek), that provider must not inject web_search_preview.
        nativeSearch, _ := options["native_search"].(bool)
        nativeSearch = nativeSearch && isNativeSearchHost(p.apiBase)
        if len(tools) > 0 || nativeSearch {
                requestBody["tools"] = buildToolsList(tools, nativeSearch)
                requestBody["tool_choice"] = "auto"
        }

        if maxTokens, ok := common.AsInt(options["max_tokens"]); ok {
                fieldName := p.maxTokensField
                if fieldName == "" {
                        lowerModel := strings.ToLower(model)
                        if strings.Contains(lowerModel, "glm") || strings.Contains(lowerModel, "o1") ||
                                strings.Contains(lowerModel, "gpt-5") {
                                fieldName = "max_completion_tokens"
                        } else {
                                fieldName = "max_tokens"
                        }
                }
                requestBody[fieldName] = maxTokens
        }

        if temperature, ok := common.AsFloat(options["temperature"]); ok {
                lowerModel := strings.ToLower(model)
                if strings.Contains(lowerModel, "kimi") && strings.Contains(lowerModel, "k2") {
                        requestBody["temperature"] = 1.0
                } else {
                        requestBody["temperature"] = temperature
                }
        }

        // Prompt caching: pass a stable cache key so OpenAI can bucket requests
        // with the same key and reuse prefix KV cache across calls.
        // Prompt caching is only supported by OpenAI-native endpoints.
        // Non-OpenAI providers reject unknown fields with 422 errors.
        if cacheKey, ok := options["prompt_cache_key"].(string); ok && cacheKey != "" {
                if supportsPromptCacheKey(p.apiBase) {
                        requestBody["prompt_cache_key"] = cacheKey
                }
        }

        // DeepSeek V4 reasoning mode support.
        // V4 models accept thinking.type + reasoning_effort parameters.
        // Map PicoClaw's thinking_level to DeepSeek V4's API parameters:
        //   off    → thinking.type: "disabled"
        //   low/medium → thinking.type: "enabled", reasoning_effort: "high"
        //   high/xhigh → thinking.type: "enabled", reasoning_effort: "max"
        //   adaptive   → thinking.type: "enabled", reasoning_effort: "high" (V4 default)
        if p.isDeepSeekReasoningProvider() && isDeepSeekV4ModelName(model) {
                if level, ok := options["thinking_level"].(string); ok && level != "" {
                        switch level {
                        case "off":
                                requestBody["thinking"] = map[string]any{"type": "disabled"}
                        case "low", "medium", "adaptive":
                                requestBody["thinking"] = map[string]any{"type": "enabled"}
                                requestBody["reasoning_effort"] = "high"
                        case "high", "xhigh":
                                requestBody["thinking"] = map[string]any{"type": "enabled"}
                                requestBody["reasoning_effort"] = "max"
                        }
                }

                // user_id for KV cache isolation in multi-tenant scenarios.
                // DeepSeek's prefix caching is isolated per user_id.
                if uid, ok := options["user_id"].(string); ok && uid != "" {
                        requestBody["user"] = uid
                }
        }

        // Chat Prefix Completion (Beta): Pre-seed assistant response with a prefix.
        // When prefix_completion is set, append an assistant message with prefix: true.
        // Supports optional reasoning_content prefix for guided reasoning continuation.
        if prefix, ok := options["prefix_completion"].(string); ok && prefix != "" {
                messagesSlice, _ := requestBody["messages"].([]any)
                assistantMsg := map[string]any{
                        "role":    "assistant",
                        "content": prefix,
                        "prefix":  true,
                }
                // Add reasoning_content prefix for guided reasoning (DeepSeek V4).
                if reasoningPrefix, ok := options["reasoning_prefix"].(string); ok && reasoningPrefix != "" {
                        assistantMsg["reasoning_content"] = reasoningPrefix
                }
                messagesSlice = append(messagesSlice, assistantMsg)
                requestBody["messages"] = messagesSlice
        }

        // JSON Output Mode: Force the model to produce valid JSON output.
        if responseFmt, ok := options["response_format"].(string); ok && responseFmt != "" {
                requestBody["response_format"] = map[string]any{"type": responseFmt}
        }

        // Strict Mode for Tool Calls (Beta): Add strict: true to function definitions.
        strictMode, _ := options["strict_tool_calls"].(bool)
        if strictMode && len(tools) > 0 {
                strictTools := buildStrictToolsList(tools)
                requestBody["tools"] = strictTools
        }

        // Merge extra body fields configured per-provider/model.
        // These are injected last so they take precedence over defaults.
        maps.Copy(requestBody, p.extraBody)

        return requestBody
}

func (p *Provider) applyCustomHeaders(req *http.Request) {
        for k, v := range p.customHeaders {
                if strings.TrimSpace(k) == "" {
                        continue
                }
                req.Header.Set(k, v)
        }
}

func (p *Provider) SetProviderName(providerName string) {
        p.providerName = strings.ToLower(strings.TrimSpace(providerName))
}

// SupportsThinking implements providers.ThinkingCapable.
// DeepSeek V4 and other reasoning-capable providers support extended thinking.
func (p *Provider) SupportsThinking() bool {
        return p.isDeepSeekReasoningProvider()
}

func (p *Provider) prepareMessagesForRequest(messages []Message, model string, tools []ToolDefinition) []Message {
        if len(messages) == 0 {
                return nil
        }

        if p.isDeepSeekReasoningProvider() {
                // DeepSeek V4 models (deepseek-v4-flash, deepseek-v4-pro) require
                // reasoning_content to be preserved in multi-turn conversations per API docs.
                // For V4 models, we only strip transient thought-only messages that
                // have no content or tool calls.
                //
                // Interleaved Thinking (V4): The V4 paper distinguishes two behaviors:
                // - Tool-calling scenarios: ALL reasoning preserved across ALL turns
                // - General conversational: reasoning from earlier turns discarded
                // In practice, preserving all reasoning_content is always safe and provides
                // the best quality for agent workflows. The drop_thinking optimization is
                // available via the "drop_thinking" option when token savings are needed.
                if isDeepSeekV4ModelName(model) {
                        return filterDeepSeekV4ReasoningMessages(messages)
                }
                return filterDeepSeekReasoningMessages(messages)
        }
        return stripReasoningMessages(messages)
}

func (p *Provider) isDeepSeekReasoningProvider() bool {
        return p.providerName == "deepseek" || isDeepSeekHost(p.apiBase)
}

// isDeepSeekV4Model is no longer used; V4 detection is now based on the
// model name string via isDeepSeekV4ModelName, which is more reliable than
// inferring V4 from message content.

func isDeepSeekHost(apiBase string) bool {
        parsed, err := url.Parse(strings.TrimSpace(apiBase))
        if err != nil {
                return false
        }
        host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
        return host == "deepseek.com" || strings.HasSuffix(host, ".deepseek.com")
}

// deepSeekBetaEndpoint returns the beta API endpoint for DeepSeek V4 features
// (Chat Prefix Completion, Strict Mode). If the apiBase already uses the beta
// path or is not a DeepSeek host, it returns the apiBase unchanged.
func deepSeekBetaEndpoint(apiBase string) string {
        if !isDeepSeekHost(apiBase) {
                return apiBase // Not a DeepSeek host, no rewriting needed
        }
        parsed, err := url.Parse(strings.TrimSpace(apiBase))
        if err != nil {
                return apiBase
        }
        // If already on beta path, no change needed
        if strings.HasSuffix(parsed.Path, "/beta") || strings.Contains(parsed.Path, "/beta/") {
                return apiBase
        }
        // Rewrite to the beta endpoint
        parsed.Path = strings.TrimRight(parsed.Path, "/") + "/beta"
        return parsed.String()
}

// effectiveAPIBase returns the API base URL to use for the current request.
// When DeepSeek V4 beta features (prefix completion, strict mode) are active,
// it automatically switches to the beta endpoint for DeepSeek hosts.
func (p *Provider) effectiveAPIBase(options map[string]any) string {
        needsBeta := false
        if _, ok := options["prefix_completion"].(string); ok && options["prefix_completion"].(string) != "" {
                needsBeta = true
        }
        if strict, _ := options["strict_tool_calls"].(bool); strict {
                needsBeta = true
        }
        if needsBeta && isDeepSeekHost(p.apiBase) {
                return deepSeekBetaEndpoint(p.apiBase)
        }
        return p.apiBase
}

func filterDeepSeekReasoningMessages(messages []Message) []Message {
        out := make([]Message, 0, len(messages))
        start := 0

        flush := func(end int) {
                if end <= start {
                        return
                }
                out = append(out, filterDeepSeekReasoningTurn(messages[start:end])...)
                start = end
        }

        for i := 1; i < len(messages); i++ {
                if messages[i].Role == "user" {
                        flush(i)
                }
        }
        flush(len(messages))

        return out
}

// filterDeepSeekV4ReasoningMessages preserves reasoning_content in all turns
// for DeepSeek V4 models. The V4 API docs state: "When concatenating subsequent
// multi-round messages, include the reasoning_content field to maintain the
// context of the model's reasoning." We still filter transient thought-only
// messages that have no content or tool calls.
func filterDeepSeekV4ReasoningMessages(messages []Message) []Message {
        out := make([]Message, 0, len(messages))
        for _, msg := range messages {
                if messageutil.IsTransientAssistantThoughtMessage(msg) {
                        continue
                }
                cloned := msg
                // V4: keep reasoning_content in all assistant messages, even non-tool turns.
                // This is required for the model to maintain its reasoning context.
                if assistantMessageEmpty(cloned) {
                        continue
                }
                out = append(out, cloned)
        }
        return out
}

func filterDeepSeekReasoningTurn(messages []Message) []Message {
        hasToolInteraction := false
        for _, msg := range messages {
                if msg.Role == "tool" || (msg.Role == "assistant" && len(msg.ToolCalls) > 0) {
                        hasToolInteraction = true
                        break
                }
        }

        out := make([]Message, 0, len(messages))
        for _, msg := range messages {
                if messageutil.IsTransientAssistantThoughtMessage(msg) {
                        continue
                }

                cloned := msg
                // DeepSeek thinking-mode replay only requires reasoning_content for
                // turns that participate in a tool interaction round. For plain
                // assistant turns between two user messages, the docs say the API will
                // ignore reasoning_content on replay, so we strip it here.
                if cloned.Role == "assistant" && strings.TrimSpace(cloned.ReasoningContent) != "" && !hasToolInteraction {
                        cloned.ReasoningContent = ""
                }
                if assistantMessageEmpty(cloned) {
                        continue
                }
                out = append(out, cloned)
        }

        return out
}

func stripReasoningMessages(messages []Message) []Message {
        out := make([]Message, 0, len(messages))
        for _, msg := range messages {
                if messageutil.IsTransientAssistantThoughtMessage(msg) {
                        continue
                }

                cloned := msg
                cloned.ReasoningContent = ""
                if assistantMessageEmpty(cloned) {
                        continue
                }
                out = append(out, cloned)
        }
        return out
}

func assistantMessageEmpty(msg Message) bool {
        return msg.Role == "assistant" &&
                strings.TrimSpace(msg.Content) == "" &&
                strings.TrimSpace(msg.ReasoningContent) == "" &&
                len(msg.ToolCalls) == 0 &&
                len(msg.Media) == 0 &&
                len(msg.Attachments) == 0 &&
                strings.TrimSpace(msg.ToolCallID) == ""
}

func (p *Provider) Chat(
        ctx context.Context,
        messages []Message,
        tools []ToolDefinition,
        model string,
        options map[string]any,
) (*LLMResponse, error) {
        if p.apiBase == "" {
                return nil, fmt.Errorf("API base not configured")
        }

        requestBody := p.buildRequestBody(messages, tools, model, options)

        jsonData, err := json.Marshal(requestBody)
        if err != nil {
                return nil, fmt.Errorf("failed to marshal request: %w", err)
        }

        // WS 5.2/5.3: Auto-detect beta endpoint for DeepSeek V4 features
        // (Chat Prefix Completion, Strict Mode).
        endpointBase := p.effectiveAPIBase(options)

        req, err := http.NewRequestWithContext(ctx, "POST", endpointBase+"/chat/completions", bytes.NewReader(jsonData))
        if err != nil {
                return nil, fmt.Errorf("failed to create request: %w", err)
        }

        req.Header.Set("Content-Type", "application/json")
        if p.userAgent != "" {
                req.Header.Set("User-Agent", p.userAgent)
        }
        if p.apiKey != "" {
                req.Header.Set("Authorization", "Bearer "+p.apiKey)
        }
        p.applyCustomHeaders(req)

        resp, err := p.httpClient.Do(req)
        if err != nil {
                return nil, fmt.Errorf("failed to send request: %w", err)
        }
        defer resp.Body.Close()

        if resp.StatusCode != http.StatusOK {
                return nil, common.HandleErrorResponse(resp, p.apiBase)
        }

        response, err := common.ReadAndParseResponse(resp, p.apiBase)
        if err != nil {
                return nil, err
        }

        // Check for DSML-formatted tool calls in the response content.
        // This handles local inference engines (vLLM, Ollama) that return
        // DSML instead of structured JSON tool_calls.
        return maybeParseDSMLResponse(response), nil
}

// maybeParseDSMLResponse checks if the response content contains DSML-formatted
// tool calls and parses them into the structured ToolCall format. This handles
// the case where local inference engines (vLLM, Ollama) return DSML instead of
// structured JSON tool_calls in the API response.
func maybeParseDSMLResponse(response *LLMResponse) *LLMResponse {
        if response == nil || len(response.ToolCalls) > 0 {
                return response // Already has structured tool calls from API
        }

        if !HasDSMLToolCalls(response.Content) && !HasDSMLToolCalls(response.ReasoningContent) {
                return response // No DSML markers found
        }

        // Try parsing from content first
        if HasDSMLToolCalls(response.Content) {
                toolCalls, remaining, err := ParseDSMLToolCalls(response.Content)
                if err != nil {
                        log.Printf("openai_compat: DSML parse warning: %v", err)
                }
                if len(toolCalls) > 0 {
                        response.ToolCalls = toolCalls
                        response.Content = remaining
                        if response.FinishReason == "stop" {
                                response.FinishReason = "tool_calls"
                        }
                        log.Printf("openai_compat: parsed %d tool calls from DSML format", len(toolCalls))
                        return response
                }
        }

        // If DSML markers found in reasoning_content but not successfully parsed from content,
        // try parsing from reasoning_content (edge case for local inference where reasoning
        // contains tool call planning that leaked into DSML format).
        if HasDSMLToolCalls(response.ReasoningContent) {
                toolCalls, remaining, err := ParseDSMLToolCalls(response.ReasoningContent)
                if err != nil {
                        log.Printf("openai_compat: DSML parse warning (reasoning): %v", err)
                }
                if len(toolCalls) > 0 {
                        response.ToolCalls = toolCalls
                        response.ReasoningContent = remaining
                        if response.FinishReason == "stop" {
                                response.FinishReason = "tool_calls"
                        }
                        log.Printf("openai_compat: parsed %d tool calls from DSML in reasoning_content", len(toolCalls))
                }
        }

        return response
}

// ChatStream implements streaming via OpenAI-compatible SSE (stream: true).
// onChunk receives the accumulated text so far on each text delta.
func (p *Provider) ChatStream(
        ctx context.Context,
        messages []Message,
        tools []ToolDefinition,
        model string,
        options map[string]any,
        onChunk func(accumulated string),
) (*LLMResponse, error) {
        if p.apiBase == "" {
                return nil, fmt.Errorf("API base not configured")
        }

        requestBody := p.buildRequestBody(messages, tools, model, options)
        requestBody["stream"] = true

        // WS 3.2: Request include_usage in streaming mode for accurate token counts.
        // DeepSeek V4 and OpenAI support stream_options.include_usage to return
        // prompt_tokens, completion_tokens, and prompt_cache_hit_tokens in the
        // final streaming chunk. This replaces heuristic token estimation.
        if includeUsage, _ := options["stream_include_usage"].(bool); includeUsage {
                requestBody["stream_options"] = map[string]any{
                        "include_usage": true,
                }
        }

        jsonData, err := json.Marshal(requestBody)
        if err != nil {
                return nil, fmt.Errorf("failed to marshal request: %w", err)
        }

        req, err := http.NewRequestWithContext(ctx, "POST", p.effectiveAPIBase(options)+"/chat/completions", bytes.NewReader(jsonData))
        if err != nil {
                return nil, fmt.Errorf("failed to create request: %w", err)
        }

        req.Header.Set("Content-Type", "application/json")
        req.Header.Set("Accept", "text/event-stream")
        if p.userAgent != "" {
                req.Header.Set("User-Agent", p.userAgent)
        }
        if p.apiKey != "" {
                req.Header.Set("Authorization", "Bearer "+p.apiKey)
        }
        p.applyCustomHeaders(req)

        // Use a client without Timeout for streaming — the http.Client.Timeout covers
        // the entire request lifecycle including body reads, which would kill long streams.
        // Context cancellation still provides the safety net.
        streamClient := &http.Client{Transport: p.httpClient.Transport}
        resp, err := streamClient.Do(req)
        if err != nil {
                return nil, fmt.Errorf("failed to send request: %w", err)
        }
        defer resp.Body.Close()

        if resp.StatusCode != http.StatusOK {
                return nil, common.HandleErrorResponse(resp, p.apiBase)
        }

        llmResp, err := parseStreamResponse(ctx, resp.Body, onChunk)
        if err != nil {
                return nil, err
        }

        // WS 5.1: Apply DSML parsing for streaming responses too.
        // Local inference engines (vLLM, Ollama) may return DSML-formatted
        // tool calls even in streaming mode.
        return maybeParseDSMLResponse(llmResp), nil
}

// parseStreamResponse parses an OpenAI-compatible SSE stream.
func parseStreamResponse(
        ctx context.Context,
        reader io.Reader,
        onChunk func(accumulated string),
) (*LLMResponse, error) {
        var textContent strings.Builder
        var reasoningContent strings.Builder
        var finishReason string
        var usage *UsageInfo

        // Tool call assembly: OpenAI streams tool calls as incremental deltas
        type toolAccum struct {
                id       string
                name     string
                argsJSON strings.Builder
        }
        activeTools := map[int]*toolAccum{}

        scanner := bufio.NewScanner(reader)
        scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024) // 1MB initial, 10MB max
        for scanner.Scan() {
                // Check for context cancellation between chunks
                if err := ctx.Err(); err != nil {
                        return nil, err
                }

                line := scanner.Text()

                if !strings.HasPrefix(line, "data: ") {
                        continue
                }
                data := strings.TrimPrefix(line, "data: ")
                if data == "[DONE]" {
                        break
                }

                var chunk struct {
                        Choices []struct {
                                Delta struct {
                                        Content          string `json:"content"`
                                        ReasoningContent string `json:"reasoning_content"`
                                        ToolCalls []struct {
                                                Index    int    `json:"index"`
                                                ID       string `json:"id"`
                                                Function *struct {
                                                        Name      string `json:"name"`
                                                        Arguments string `json:"arguments"`
                                                } `json:"function"`
                                        } `json:"tool_calls"`
                                } `json:"delta"`
                                FinishReason *string `json:"finish_reason"`
                        } `json:"choices"`
                        Usage *UsageInfo `json:"usage"`
                }

                if err := json.Unmarshal([]byte(data), &chunk); err != nil {
                        continue // skip malformed chunks
                }

                if chunk.Usage != nil {
                        usage = chunk.Usage
                }

                if len(chunk.Choices) == 0 {
                        continue
                }

                choice := chunk.Choices[0]

                // Accumulate text content
                if choice.Delta.Content != "" {
                        textContent.WriteString(choice.Delta.Content)
                        if onChunk != nil {
                                onChunk(textContent.String())
                        }
                }

                // Accumulate reasoning content (DeepSeek V4 thinking mode)
                if choice.Delta.ReasoningContent != "" {
                        reasoningContent.WriteString(choice.Delta.ReasoningContent)
                }

                // Accumulate tool call deltas
                for _, tc := range choice.Delta.ToolCalls {
                        acc, ok := activeTools[tc.Index]
                        if !ok {
                                acc = &toolAccum{}
                                activeTools[tc.Index] = acc
                        }
                        if tc.ID != "" {
                                acc.id = tc.ID
                        }
                        if tc.Function != nil {
                                if tc.Function.Name != "" {
                                        acc.name = tc.Function.Name
                                }
                                if tc.Function.Arguments != "" {
                                        acc.argsJSON.WriteString(tc.Function.Arguments)
                                }
                        }
                }

                if choice.FinishReason != nil {
                        finishReason = *choice.FinishReason
                }
        }

        if err := scanner.Err(); err != nil {
                return nil, fmt.Errorf("streaming read error: %w", err)
        }

        // Assemble tool calls from accumulated deltas
        var toolCalls []ToolCall
        for i := 0; i < len(activeTools); i++ {
                acc, ok := activeTools[i]
                if !ok {
                        continue
                }
                args := make(map[string]any)
                raw := acc.argsJSON.String()
                if raw != "" {
                        if err := json.Unmarshal([]byte(raw), &args); err != nil {
                                log.Printf("openai_compat stream: failed to decode tool call arguments for %q: %v", acc.name, err)
                                args["raw"] = raw
                        }
                }
                toolCalls = append(toolCalls, ToolCall{
                        ID:        acc.id,
                        Name:      acc.name,
                        Arguments: args,
                })
        }

        if finishReason == "" {
                finishReason = "stop"
        }

        return &LLMResponse{
                Content:          textContent.String(),
                ReasoningContent: reasoningContent.String(),
                ToolCalls:        toolCalls,
                FinishReason:     finishReason,
                Usage:            usage,
        }, nil
}

func normalizeModel(model, apiBase string) string {
        before, after, ok := strings.Cut(model, "/")
        if !ok {
                return model
        }

        if strings.Contains(strings.ToLower(apiBase), "openrouter.ai") {
                return model
        }

        prefix := strings.ToLower(before)
        if _, ok := stripModelPrefixProviders[prefix]; ok {
                return after
        }

        return model
}

func buildToolsList(tools []ToolDefinition, nativeSearch bool) []any {
        result := make([]any, 0, len(tools)+1)
        for _, t := range tools {
                if nativeSearch && strings.EqualFold(t.Function.Name, "web_search") {
                        continue
                }
                result = append(result, t)
        }
        if nativeSearch {
                result = append(result, map[string]any{"type": "web_search_preview"})
        }
        return result
}

// buildStrictToolsList creates a tools list with strict: true on each function
// definition for DeepSeek V4 beta API. When strict mode is enabled, the API
// validates that tool call outputs conform exactly to the JSON Schema.
// Schemas that don't conform to strict mode requirements (missing required
// array, missing additionalProperties: false) are automatically sanitized
// using SanitizeSchemaForStrictMode. Validation warnings are logged.
func buildStrictToolsList(tools []ToolDefinition) []any {
        result := make([]any, 0, len(tools))
        for _, t := range tools {
                // Validate the schema for strict mode compliance
                var parameters any = t.Function.Parameters
                if t.Function.Parameters != nil && len(t.Function.Parameters) > 0 {
                        validationResult := ValidateStrictSchema(t.Function.Parameters, t.Function.Name)
                        if !validationResult.Valid {
                                // Auto-sanitize the schema to fix common issues
                                parameters = SanitizeSchemaForStrictMode(t.Function.Parameters)
                                if len(validationResult.Errors) > 0 {
                                        log.Printf("openai_compat: strict mode schema auto-fixed for function %q: %s",
                                                t.Function.Name, FormatValidationResult(validationResult))
                                }
                        }
                }

                // Create a copy with strict: true added
                fn := map[string]any{
                        "name":        t.Function.Name,
                        "strict":      true,
                        "description": t.Function.Description,
                }
                if parameters != nil {
                        fn["parameters"] = parameters
                }
                result = append(result, map[string]any{
                        "type":     "function",
                        "function": fn,
                })
        }
        return result
}

func (p *Provider) SupportsNativeSearch() bool {
        return isNativeSearchHost(p.apiBase)
}

// isNativeOpenAIOrAzureEndpoint reports whether the given API base points to
// OpenAI's own API or an Azure OpenAI deployment.
func isNativeOpenAIOrAzureEndpoint(apiBase string) bool {
        u, err := url.Parse(apiBase)
        if err != nil {
                return false
        }
        host := u.Hostname()
        return host == "api.openai.com" || strings.HasSuffix(host, ".openai.azure.com")
}

func isNativeSearchHost(apiBase string) bool {
        return isNativeOpenAIOrAzureEndpoint(apiBase)
}

// supportsPromptCacheKey reports whether the given API base is known to
// support the prompt_cache_key request field. Currently only OpenAI's own
// API and Azure OpenAI support this. All other OpenAI-compatible providers
// (Mistral, Gemini, DeepSeek, Groq, etc.) reject unknown fields with 422 errors.
// DeepSeek V4 uses automatic prefix caching (no explicit cache key needed).
func supportsPromptCacheKey(apiBase string) bool {
        return isNativeOpenAIOrAzureEndpoint(apiBase)
}

// isDeepSeekV4ModelName reports whether the model identifier refers to a
// DeepSeek V4 model. V4 models support 1M context, reasoning modes,
// and automatic prefix caching.
// hasToolInteractions reports whether any message in the history involves
// tool interactions (tool role messages or assistant messages with tool calls).
// This is used for DeepSeek V4 interleaved thinking: when tools have been
// used in the conversation, all reasoning content must be preserved.
func hasToolInteractions(messages []Message) bool {
        for _, msg := range messages {
                if msg.Role == "tool" || (msg.Role == "assistant" && len(msg.ToolCalls) > 0) {
                        return true
                }
        }
        return false
}

// filterDeepSeekV4ReasoningMessagesDropThinking implements the V4 "general
// conversational" behavior: reasoning content from previous turns is discarded
// when a new user message arrives. Only the most recent assistant turn retains
// its reasoning_content. This is the drop_thinking=True behavior for V4 models
// when no tools are present.
func filterDeepSeekV4ReasoningMessagesDropThinking(messages []Message) []Message {
        // Find the index of the last user message
        lastUserIdx := -1
        for i := len(messages) - 1; i >= 0; i-- {
                if messages[i].Role == "user" {
                        lastUserIdx = i
                        break
                }
        }

        out := make([]Message, 0, len(messages))
        for i, msg := range messages {
                if messageutil.IsTransientAssistantThoughtMessage(msg) {
                        continue
                }

                cloned := msg
                // Strip reasoning_content from assistant messages before the last user message
                if cloned.Role == "assistant" && strings.TrimSpace(cloned.ReasoningContent) != "" && i < lastUserIdx {
                        cloned.ReasoningContent = ""
                }
                if assistantMessageEmpty(cloned) {
                        continue
                }
                out = append(out, cloned)
        }
        return out
}

func isDeepSeekV4ModelName(model string) bool {
        lower := strings.ToLower(model)
        return strings.Contains(lower, "deepseek-v4") ||
                strings.Contains(lower, "deepseek_v4") ||
                strings.HasPrefix(lower, "v4-flash") ||
                strings.HasPrefix(lower, "v4-pro")
}
