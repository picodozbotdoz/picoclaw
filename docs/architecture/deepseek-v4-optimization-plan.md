# DeepSeek V4 Optimization Plan for PicoClaw

## Document Metadata

| Field | Value |
|-------|-------|
| Status | Draft (Revised after V4 PDF review) |
| Branch | `wip/deepseekv4_optimized` |
| Author | AI-assisted |
| Created | 2026-05-01 |
| Last Updated | 2026-05-01 (v2 — added DSML, Interleaved Thinking, Chat Prefix Completion, Strict Mode, Quick Instruction, response_format, reasoning_effort max system prompt) |

---

## 1. Executive Summary

DeepSeek V4 introduces a **1,048,576-token context window** (1M), **automatic prefix caching** with 50x cost reduction on cache hits, **384K max output tokens**, **three reasoning modes** (non-think, think-high, think-max), **DSML (DeepSeek Markup Language)** for XML-based tool calls, **interleaved thinking** that preserves reasoning across tool-call boundaries, **Chat Prefix Completion** for guided generation, **strict mode** for deterministic tool output, and **Quick Instruction** tokens for auxiliary tasks. These capabilities fundamentally change how PicoClaw should manage context, prompt construction, token budgets, and the agent loop lifecycle.

The current PicoClaw architecture was designed for models with 32K-128K context windows. It uses aggressive compression, heuristic token estimation, and treats all providers uniformly through an OpenAI-compatible abstraction. DeepSeek V4's scale demands a targeted optimization strategy that preserves the existing abstraction while exploiting V4's unique features: massive context, automatic caching, reasoning mode control, DSML-based tool calling, and interleaved thinking.

This plan proposes **12 workstreams** across **5 phases**, estimated at **8-10 weeks** of effort. Each workstream is independently mergeable with no breaking changes to existing provider integrations.

---

## 2. DeepSeek V4 Technical Reference

### 2.1 Model Variants

| Model | Context Window | Max Output | Active Params | Total Params |
|-------|---------------|------------|---------------|--------------|
| `deepseek-v4-flash` | 1,048,576 | 384,000 | 13B | 284B |
| `deepseek-v4-pro` | 1,048,576 | 384,000 | 49B | 1.6T |

### 2.2 Pricing (per 1M tokens)

| Token Type | V4-Flash | V4-Pro |
|-----------|----------|--------|
| Input (cache hit) | $0.0028 | $0.003625 |
| Input (cache miss) | $0.14 | $0.435 |
| Output | $0.28 | $0.87 |

Cache hit pricing is **1/50th of cache miss** on Flash. This makes prefix caching the single highest-impact optimization.

### 2.3 Reasoning Modes

| Mode | API Parameter | Use Case |
|------|--------------|----------|
| Non-think | `thinking.type: "disabled"` | Fast tool calls, simple chat, summarization |
| Think High (default) | `thinking.type: "enabled", reasoning_effort: "high"` | General reasoning, multi-step tasks |
| Think Max | `thinking.type: "enabled", reasoning_effort: "max"` | Complex agent reasoning, code generation, deep analysis |

Think Max requires >= 384K context window allocation. Responses include `reasoning_content` alongside `content`.

### 2.4 Automatic Prefix Caching

- **Mechanism**: "Context Caching on Disk" — automatic, no opt-in or headers required
- **Scope**: Overlapping **prefixes** in the messages array are matched
- **Isolation**: KVCache isolated at `user_id` parameter level
- **Cost impact**: Cache hit = 1/50 of cache miss on Flash
- **Critical rule**: Messages must maintain identical ordering and content in the prefix portion to get cache hits

### 2.5 API Compatibility

- **Protocol**: OpenAI ChatCompletions compatible
- **Base URL**: `https://api.deepseek.com`
- **Endpoint**: `POST /chat/completions`
- **`max_tokens` field**: Uses `max_tokens` (not `max_completion_tokens`)
- **Tool calling**: OpenAI-style function calling, max 128 functions, parallel tool calls supported
- **Streaming**: SSE with `stream_options.include_usage: true` for token usage stats
- **Deprecated**: `frequency_penalty` and `presence_penalty` have no effect
- **JSON Output**: `response_format: { "type": "json_object" }` forces valid JSON output
- **Anthropic API compatibility**: DeepSeek also offers an Anthropic-compatible API endpoint for migration ease

### 2.6 DSML (DeepSeek Markup Language)

DeepSeek V4 introduces a new XML-based tool-call schema called **DSML** that replaces the previous JSON-only format. While the DeepSeek API accepts OpenAI-compatible `tools`/`tool_choice` parameters and returns structured `tool_calls` in the response, the model internally uses DSML encoding.

**DSML tool-call format** (internal model representation):
```xml
<|DSML|tool_calls>
<|DSML|invoke name="$TOOL_NAME">
<|DSML|parameter name="$PARAMETER_NAME" string="true|false">$PARAMETER_VALUE</|DSML|parameter>
...
</|DSML|invoke>
</|DSML|tool_calls>
```

Key rules:
- `string="true"`: Parameter value is a raw string
- `string="false"`: Parameter value is JSON (number, boolean, array, object)
- Tool results are wrapped in `<tool_result>` tags within user messages
- When thinking mode is enabled, reasoning MUST appear inside `<think...</think` BEFORE any DSML tool calls
- DSML effectively mitigates escaping failures and reduces tool-call errors vs. JSON-based formats

**Implication for PicoClaw**: The API translates between DSML and OpenAI-compatible function calling format, so PicoClaw can continue using OpenAI-style tool definitions. However, when working with raw model outputs (e.g., local inference, vLLM), PicoClaw must parse DSML-formatted responses directly.

### 2.7 Interleaved Thinking

DeepSeek V4 refines the thinking management strategy from V3.2 with two distinct behaviors:

1. **Tool-Calling Scenarios**: ALL reasoning content is fully preserved throughout the entire conversation, including across user message boundaries. Unlike V3.2 which discarded thinking traces upon each new user turn, V4 retains the complete reasoning history across all rounds when tools are present. This allows the model to maintain a coherent, cumulative chain of thought over long-horizon agent tasks.

2. **General Conversational Scenarios**: Reasoning content from previous turns is discarded when a new user message arrives, keeping the context concise for settings where persistent reasoning traces provide limited benefit.

The `drop_thinking` encoding parameter controls this behavior:
- `drop_thinking=True` (default for non-tool conversations): Strip reasoning from earlier turns
- `drop_thinking=False` (automatic when tools are present): Preserve all reasoning content

**Implication for PicoClaw**: When tools are defined on the system message, PicoClaw MUST NOT strip `reasoning_content` from any turn. The current `filterDeepSeekReasoningMessages()` function must be aware of whether tools are present in the conversation context.

### 2.8 Chat Prefix Completion (Beta)

Allows pre-seeding the assistant's response to guide generation. Uses the `prefix: true` parameter on an assistant message to force the model to complete from that prefix.

- **Beta endpoint**: `base_url="https://api.deepseek.com/beta"`
- **Use cases**: Force code output format (e.g., prefix with `` ```python\n ``), guide structured responses, implement constrained generation
- **Thinking mode integration**: The `reasoning_content` field on the last assistant message can be used as a CoT prefix input
- **Stop sequences**: Combine with `stop` parameter for precise output control

### 2.9 Strict Mode for Tool Calls (Beta)

When `strict: true` is set on a function definition, the API validates that tool call outputs conform exactly to the function's JSON Schema. This eliminates malformed tool invocations.

- **Beta endpoint**: `base_url="https://api.deepseek.com/beta"`
- **Requirements**: All properties must be in `required`, `additionalProperties: false` on every object
- **Supported schema types**: `object`, `string`, `number`, `integer`, `boolean`, `array`, `enum`, `anyOf`
- **Works in both**: Thinking and non-thinking modes
- **Validation**: Server-side schema validation rejects invalid schemas with error messages

### 2.10 Quick Instruction Tokens

Special tokens appended to messages for auxiliary classification and generation tasks. These tokens leverage the already-computed KV cache, avoiding redundant prefilling and reducing TTFT.

| Token | Purpose | Format |
|-------|---------|--------|
| `<\|action\|>` | Route: web search vs. direct answer | `...<\|User\|>{prompt}<\|Assistant\|><think<\|action\|>` |
| `<\|title\|>` | Generate conversation title | `...<\|Assistant\|>{response}<\|end_of_sentence\|><\|title\|>` |
| `<\|query\|>` | Generate search queries | `...<\|User\|>{prompt}<\|query\|>` |
| `<\|authority\|>` | Classify source authoritativeness | `...<\|User\|>{prompt}<\|authority\|>` |
| `<\|domain\|>` | Identify prompt domain | `...<\|User\|>{prompt}<\|domain\|>` |
| `<\|extracted_url\|><\|read_url\|>` | URL fetch decision | `...<\|User\|>{prompt}<\|extracted_url\|>{url}<\|read_url\|>` |

### 2.11 Reasoning Effort: Max Mode System Prompt Injection

When `reasoning_effort: "max"` is set, the API automatically prepends a special system prompt instruction at the very beginning of the conversation (before the user's system prompt):

```
Reasoning Effort: Absolute maximum with no shortcuts permitted.
You MUST be very thorough in your thinking and comprehensively decompose the
problem to resolve the root cause, rigorously stress-testing your logic against all
potential paths, edge cases, and adversarial scenarios.
Explicitly write out your entire deliberation process, documenting every intermediate
step, considered alternative, and rejected hypothesis to ensure absolutely no
assumption is left unchecked.
```

**Implication for PicoClaw**: This prefix is managed server-side by the API. PicoClaw should NOT inject this text manually. However, PicoClaw must account for the extra tokens this prefix adds to the prompt when calculating context budgets.

---

## 3. Current Architecture Gap Analysis

### 3.1 Context Window Defaults Are Too Conservative

**Current**: `ContextWindow = MaxTokens * 4 = 32768 * 4 = 131,072 tokens`

**Impact**: DeepSeek V4 offers 1M tokens — the default heuristic allocates only ~13% of available context. This causes premature compression, unnecessary summarization, and loss of conversation history that could otherwise be retained in full.

**Files affected**: `pkg/config/defaults.go`, `pkg/agent/instance.go`

### 3.2 No DeepSeek-Specific Tokenizer

**Current**: `EstimateMessageTokens()` uses `chars * 2 / 5` heuristic (~2.5 chars/token)

**Impact**: This heuristic was calibrated for Latin-script text on GPT-class tokenizers. For DeepSeek's tokenizer (which uses different BPE merges), estimation error can reach 20-40%. Over-estimation triggers unnecessary compression; under-estimation risks context overflow errors.

**Files affected**: `pkg/agent/context.go`, `pkg/agent/context_usage.go`

### 3.3 Prompt Caching Not Exploited for DeepSeek

**Current**: `supportsPromptCacheKey()` only returns true for OpenAI/Azure. DeepSeek's automatic prefix caching is not leveraged at the application level.

**Impact**: Every LLM call pays full cache-miss pricing because the message ordering and system prompt construction may break the prefix invariant. Even though DeepSeek caches automatically, application-level message reordering between calls defeats the cache.

**Files affected**: `pkg/agent/prompt.go`, `pkg/agent/prompt_turn.go`, `pkg/providers/openai_compat/provider.go`

### 3.4 No Reasoning Mode Control

**Current**: PicoClaw supports `ThinkingLevel` in config, but it is only routed to Anthropic-style `thinking` blocks. DeepSeek V4 uses `thinking.type` + `reasoning_effort` which is a different API shape.

**Impact**: The agent cannot switch between non-think (fast tool calls) and think-max (complex reasoning) within the same session, even though this would be the optimal strategy for multi-step agent workflows.

**Files affected**: `pkg/providers/openai_compat/provider.go`, `pkg/config/config_struct.go`, `pkg/agent/pipeline_llm.go`

### 3.5 Reasoning Content Not Preserved in Multi-turn

**Current**: `filterDeepSeekReasoningMessages()` strips `reasoning_content` from non-tool-interaction turns.

**Impact**: DeepSeek V4 API docs explicitly state: "When concatenating subsequent multi-round messages, include the reasoning_content field to maintain the context of the model's reasoning." Stripping it causes the model to lose its reasoning chain, degrading response quality in multi-turn conversations.

**Files affected**: `pkg/providers/openai_compat/provider.go`, `pkg/bus/types.go`, `pkg/memory/jsonl.go`

### 3.6 No Adaptive Compression Strategy

**Current**: Compression triggers at `compressAt = contextWindow - maxTokens`. With a 1M context window and 32K max_tokens, this means compression only triggers at ~970K tokens of input — but the `SummarizeMessageThreshold` (20 messages) and `SummarizeTokenPercent` (75%) fire much earlier.

**Impact**: With 1M context, early summarization at 20 messages or 75% of context is wasteful. A 50-turn conversation might easily fit within 200K tokens, but the current thresholds would compress it after just 20 messages.

**Files affected**: `pkg/config/defaults.go`, `pkg/agent/pipeline_setup.go`, `pkg/agent/pipeline_finalize.go`

### 3.7 Streaming Not Used in Agent Loop

**Current**: The agent loop calls `Chat()` (non-streaming). `ChatStream()` exists in the provider interface but is not used in the pipeline.

**Impact**: For DeepSeek V4 with up to 384K output tokens, a non-streaming call means the user waits potentially minutes before seeing any response. Streaming also provides `include_usage` token counts that could replace heuristic estimation.

**Files affected**: `pkg/agent/pipeline_llm.go`, `pkg/providers/openai_compat/provider.go`

### 3.8 No Context Window Partitioning Strategy

**Current**: The context window is treated as a flat budget with history consuming whatever space remains after system prompt + tool defs + max_tokens reserve.

**Impact**: There is no deliberate allocation strategy for the 1M window. When working with large documents or codebases, there's no mechanism to reserve space for "retrieved context" vs "conversation history" vs "working memory".

**Files affected**: `pkg/agent/context.go`, `pkg/agent/context_usage.go`

### 3.9 No DSML Tool-Call Parsing

**Current**: PicoClaw assumes all providers return tool calls in OpenAI-compatible JSON format (`tool_calls` array with `function.name` and `function.arguments`). DeepSeek V4's internal DSML format is not handled.

**Impact**: When using local inference (vLLM, Ollama) with DeepSeek V4 models, the model output may contain DSML-formatted tool calls (`<|DSML|tool_calls>...`) instead of structured JSON. PicoClaw cannot parse these, breaking the agent loop for local deployments. Even when using the cloud API, understanding DSML is important for debugging raw model outputs and for parsing `reasoning_content` that may contain partial DSML fragments.

**Files affected**: `pkg/providers/openai_compat/provider.go`, `pkg/providers/openai_compat/dsml_parser.go` (new)

### 3.10 Interleaved Thinking Not Properly Handled

**Current**: `filterDeepSeekReasoningMessages()` strips `reasoning_content` based on whether the turn involves tool interactions, but it does not consider the V4 distinction between tool-call scenarios (preserve all reasoning) and general chat scenarios (drop earlier reasoning).

**Impact**: In V4, when tools are present, ALL reasoning content across ALL turns (including across user message boundaries) must be preserved. The current code may still strip reasoning inappropriately, breaking V4's interleaved thinking mechanism. This degrades the model's ability to maintain coherent reasoning over multi-step agent tasks.

**Files affected**: `pkg/providers/openai_compat/provider.go`, `pkg/agent/pipeline_llm.go`

### 3.11 No Chat Prefix Completion Support

**Current**: PicoClaw has no mechanism to pre-seed assistant responses using the `prefix: true` parameter.

**Impact**: Chat Prefix Completion enables guided generation patterns (e.g., forcing code output format, constraining JSON structure) that reduce prompt engineering complexity and improve output reliability. Without it, PicoClaw relies solely on prompt instructions for format control, which is less reliable.

**Files affected**: `pkg/providers/openai_compat/provider.go`, `pkg/agent/pipeline_llm.go`

### 3.12 No Strict Mode for Tool Calls

**Current**: Tool function definitions are sent without the `strict: true` parameter, meaning the model's tool call outputs are not validated against the JSON Schema.

**Impact**: Without strict mode, tool calls may occasionally produce malformed JSON arguments (missing fields, wrong types), causing runtime errors in tool execution. Strict mode would eliminate this class of errors entirely, which is critical for autonomous agent workflows where a single malformed tool call can derail the entire task.

**Files affected**: `pkg/providers/openai_compat/provider.go`, `pkg/tools/registry.go`

### 3.13 No JSON Output Mode Support

**Current**: PicoClaw does not send `response_format: { "type": "json_object" }` in API requests.

**Impact**: Some PicoClaw features (structured data extraction, tool argument parsing) would benefit from guaranteed JSON output. Without this, the model may generate freeform text when JSON is expected, requiring additional parsing and error handling.

**Files affected**: `pkg/providers/openai_compat/provider.go`, `pkg/agent/pipeline_llm.go`

---

## 4. Optimization Plan

### Phase 1: Foundation (Weeks 1-2)

These changes establish the infrastructure for DeepSeek V4 optimization without altering existing behavior for other providers.

---

#### Workstream 1.1: DeepSeek V4 Provider Profile

**Goal**: Create a dedicated DeepSeek V4 provider configuration that correctly handles V4-specific API parameters.

**Changes**:

1. **Add DeepSeek V4 model auto-detection** in `pkg/providers/openai_compat/provider.go`:
   - Detect `deepseek-v4-flash` and `deepseek-v4-pro` model names
   - Auto-set `ContextWindow = 1048576` when these models are detected
   - Use `max_tokens` field (not `max_completion_tokens`) for V4 models
   - Do NOT send `prompt_cache_key` (V4 doesn't support it; causes 422 errors)

2. **Add reasoning mode parameters** to the request builder:
   - Map PicoClaw's `ThinkingLevel` to DeepSeek V4's `thinking` + `reasoning_effort`:
     - `ThinkingLevelNone` → `thinking.type: "disabled"`
     - `ThinkingLevelMedium` → `thinking.type: "enabled", reasoning_effort: "high"`
     - `ThinkingLevelHigh` → `thinking.type: "enabled", reasoning_effort: "max"`
   - Add `thinking` and `reasoning_effort` to the request JSON body for DeepSeek V4 models

3. **Preserve `reasoning_content` in multi-turn**:
   - Remove or conditionally bypass `filterDeepSeekReasoningMessages()` for V4 models
   - Store `reasoning_content` in `providers.Message.ReasoningContent` field (already exists)
   - Ensure `reasoning_content` is included in assistant messages sent back to the API
   - **Interleaved thinking**: When tools are present in the system message, preserve ALL `reasoning_content` across ALL turns (including across user message boundaries). This implements V4's "tool-calling scenario" behavior where the model maintains a cumulative chain of thought
   - When no tools are present, apply the `drop_thinking` behavior: strip reasoning from turns before the last user message, preserving only the most recent assistant turn's reasoning

4. **Add `user_id` parameter support**:
   - Pass `user_id` in the request body for KV cache isolation in multi-tenant scenarios
   - Derive from session key or channel+chatID combination

**Acceptance criteria**:
- DeepSeek V4 models are correctly identified and configured
- Thinking mode parameters are sent in API requests
- `reasoning_content` is preserved across multi-turn conversations
- Existing provider behavior is unchanged for non-DeepSeek models

---

#### Workstream 1.2: Adaptive Context Window Configuration

**Goal**: Make context window defaults model-aware rather than using a fixed multiplier.

**Changes**:

1. **Add `ModelContextWindow` field** to `ModelConfig` in `pkg/config/config_struct.go`:
   - Optional field that overrides the `ContextWindow = MaxTokens * 4` heuristic
   - When set, takes precedence over both the heuristic and `AgentDefaults.ContextWindow`
   - When unset, falls back to current behavior (backwards compatible)

2. **Add model-specific default registry** in `pkg/config/defaults.go`:
   ```
   deepseek-v4-flash  → context_window: 1048576, max_tokens: 16384
   deepseek-v4-pro    → context_window: 1048576, max_tokens: 16384
   ```
   - This registry maps model name prefixes to recommended defaults
   - Applied when a model is first configured, before user overrides

3. **Update `AgentInstance` initialization** in `pkg/agent/instance.go`:
   - When resolving context window, check `ModelConfig.ModelContextWindow` first
   - Log a warning if `ContextWindow < 100000` for a model that supports > 100K (likely misconfiguration)

4. **Add `MaxOutputTokens` field** to `ModelConfig`:
   - DeepSeek V4 supports up to 384K output tokens, but the default should be conservative (16K)
   - This is separate from `MaxTokens` which controls the output generation limit per call
   - Explicit setting prevents accidentally requesting 384K output at $0.28/M tokens

**Acceptance criteria**:
- DeepSeek V4 models automatically get 1M context window
- Existing models retain current behavior
- Users can override context window per-model in config
- Warning logged for likely misconfigurations

---

### Phase 2: Caching & Compression (Weeks 3-4)

These changes optimize the prompt construction and compression pipeline for DeepSeek V4's caching and large context.

---

#### Workstream 2.1: Cache-Aware Prompt Construction

**Goal**: Structure the messages array to maximize DeepSeek V4 prefix cache hits across sequential LLM calls within the same session.

**Changes**:

1. **Stabilize system prompt ordering** in `pkg/agent/prompt.go`:
   - The current `PromptRegistry` sorts by layer priority, then slot order. This is already deterministic.
   - Verify that no dynamic content (timestamps, runtime info) is embedded in the cached portion
   - Move ALL dynamic content (time, runtime, channel/chatID, sender info) to AFTER the static system prompt content
   - Structure: `[static_system_prompt | dynamic_system_prompt | summary | history | user_message]`
   - The static system prompt (identity, workspace, skill catalog, tool discovery) should be identical across all calls in a session

2. **Implement `CacheBoundary` in `BuildMessagesFromPrompt()`**:
   - Add a `CacheBoundary` marker in the message construction that indicates where the "stable prefix" ends
   - For DeepSeek V4, this boundary tells the provider adapter that everything before this point is cacheable
   - For Anthropic, this maps to `CacheControl: ephemeral` on the last stable content block
   - For OpenAI, this maps to `prompt_cache_key` (already supported)

3. **Freeze tool definitions ordering**:
   - `ToolRegistry` already sorts tool names deterministically (alphabetical)
   - Ensure this ordering is preserved in the request body, not just the prompt text
   - Tool definitions should be included in the stable prefix portion of the request

4. **Add session-level cache stats tracking**:
   - Track cache hit/miss ratios per session using `usage` data from API responses
   - Expose via `ContextUsage` in `OutboundMessage` for monitoring
   - Log cache hit rates to help users optimize their configurations

**Acceptance criteria**:
- System prompt prefix is identical across sequential calls in the same session
- Dynamic content is always appended after static content
- Cache hit rate is measurable and logged
- No change to prompt content, only ordering and structure

---

#### Workstream 2.2: Adaptive Compression Strategy for Large Context

**Goal**: Replace the fixed-threshold compression with a context-aware strategy that delays compression appropriately for 1M-token models.

**Changes**:

1. **Add `CompressionStrategy` to config** in `pkg/config/config_struct.go`:
   - `eager` (current behavior): Compress early based on message count and token percentage
   - `adaptive` (new): Scale thresholds proportionally to context window size
   - `conservative` (new): Only compress on proactive budget check, never on message count

2. **Implement adaptive threshold calculation** in `pkg/agent/pipeline_finalize.go`:
   ```
   adaptive_threshold = max(
       SummarizeMessageThreshold,
       context_window / average_turn_tokens * target_fill_percent
   )
   ```
   - For 128K context with ~2K tokens/turn: threshold ≈ 48 messages at 75% fill
   - For 1M context with ~2K tokens/turn: threshold ≈ 375 messages at 75% fill
   - This prevents premature compression on large-context models while still triggering on small-context models

3. **Update `SummarizeTokenPercent` for large contexts**:
   - Current: 75% of context window triggers summarization
   - New: For context windows > 512K, raise to 85% (more room before compression needed)
   - For context windows > 128K but <= 512K, raise to 80%
   - For context windows <= 128K, keep 75% (current behavior)

4. **Add `full_context_mode` option** for DeepSeek V4:
   - When enabled, disables summarization entirely and retains all conversation history
   - Only triggers emergency compression on context overflow
   - Suitable for sessions where the full 1M context is expected to be used (e.g., codebase analysis)

5. **Preserve `reasoning_content` through compression**:
   - When compressing turns that include `reasoning_content`, include a compressed version of the reasoning in the summary
   - This ensures the model retains its reasoning chain even after older turns are summarized away

**Acceptance criteria**:
- Compression thresholds scale with context window size
- No premature compression on 1M-context models
- Users can opt into full-context mode for DeepSeek V4
- Existing compression behavior unchanged for models with <= 128K context

---

### Phase 3: Pipeline Optimization (Weeks 5-6)

These changes optimize the agent loop pipeline for DeepSeek V4's specific capabilities.

---

#### Workstream 3.1: Reasoning Mode Switching Within a Turn

**Goal**: Allow the agent to switch between non-think and think-max modes within a single turn's iteration loop, optimizing cost and latency.

**Changes**:

1. **Add `DynamicThinkingLevel` to `processOptions`**:
   - Track the current thinking level per-iteration within a turn
   - Default: Use the configured `ThinkingLevel` for the first LLM call
   - After tool execution: Switch to non-think mode for the next LLM call (tool results don't need deep reasoning)
   - After a tool-call-free iteration: Switch back to the configured thinking level

2. **Implement thinking level routing in `CallLLM()`**:
   - Before each LLM call, determine the appropriate thinking level based on:
     - Is this the first call in a turn? → Use configured level
     - Is this a post-tool-call iteration? → Use non-think (fast)
     - Is this a steering-resume iteration? → Use configured level
     - Is this a retry after compression? → Use configured level
   - Pass the resolved thinking level to the provider adapter

3. **Add thinking mode statistics to `TurnState`**:
   - Track which thinking levels were used per iteration
   - Include in turn completion events for monitoring and cost analysis

**Cost impact analysis** (V4-Flash):
- Non-think: No reasoning tokens, faster inference
- Think-high: Moderate reasoning tokens, standard pricing
- Think-max: Heavy reasoning tokens, requires >= 384K context
- Strategy: Use non-think for 60-80% of agent iterations (tool result processing), think for 20-40% (initial reasoning, complex decisions)
- Estimated savings: 40-60% reduction in output token costs per session

**Acceptance criteria**:
- Thinking level switches automatically between iterations
- Tool result processing uses non-think mode by default
- Users can override with a fixed thinking level in config
- Thinking mode statistics are logged

---

#### Workstream 3.2: Streaming Integration in Agent Loop

**Goal**: Use streaming LLM calls in the agent pipeline to reduce time-to-first-token and provide real-time token usage data.

**Changes**:

1. **Add `StreamingMode` to `AgentDefaults`**:
   - `auto` (default): Use streaming for models that support it, non-streaming otherwise
   - `always`: Force streaming for all providers
   - `never`: Force non-streaming (current behavior)

2. **Implement streaming in `CallLLM()`** in `pkg/agent/pipeline_llm.go`:
   - Replace `provider.Chat()` with `provider.ChatStream()` when streaming is enabled
   - Process SSE chunks incrementally:
     - Accumulate text content for the final assistant message
     - Accumulate tool call deltas into complete tool calls
     - Track `reasoning_content` chunks for thinking mode
   - Emit partial content events for real-time display in channels that support it
   - Use `stream_options.include_usage: true` to get accurate token counts from the API

3. **Replace heuristic token estimation with API-reported usage**:
   - When streaming with `include_usage`, the final chunk contains `prompt_tokens`, `completion_tokens`, and `prompt_cache_hit_tokens`
   - Use these values to update `ContextUsage` instead of the heuristic estimate
   - Fall back to heuristic estimation when API usage data is not available

4. **Handle keep-alive and timeout**:
   - DeepSeek V4 sends SSE keep-alive comments during long inference
   - Implement a 10-minute timeout as per DeepSeek's documented limit
   - On timeout, treat as a transient error and retry with exponential backoff

**Acceptance criteria**:
- Streaming works end-to-end for DeepSeek V4
- Token usage is accurately tracked from API response
- Time-to-first-token is reduced for long responses
- Non-streaming fallback works for providers that don't support it
- Existing behavior unchanged when streaming is disabled

---

#### Workstream 3.3: Context Window Partitioning

**Goal**: Implement deliberate context budget allocation for the 1M-token window, enabling "full codebase in context" workflows.

**Changes**:

1. **Add `ContextPartition` config to `ModelConfig`**:
   ```go
   type ContextPartition struct {
       SystemPromptPct   float64  // % of context for system prompt (default: 2%)
       WorkingMemoryPct  float64  // % for working memory/scratchpad (default: 3%)
       RetrievedContextPct float64  // % for injected documents/code (default: 60%)
       HistoryPct        float64  // % for conversation history (default: 30%)
       OutputPct         float64  // % reserved for output (default: 5%)
   }
   ```
   - Percentages must sum to 100%
   - Defaults are calibrated for DeepSeek V4's 1M context

2. **Implement budget enforcement in `BuildMessagesFromPrompt()`**:
   - Before assembling messages, calculate token budgets per partition
   - If system prompt exceeds its budget, log a warning but allow overflow into retrieved context
   - If history exceeds its budget, trigger targeted compression of the oldest turns only
   - If retrieved context exceeds its budget, truncate with smart chunking (keep the most relevant sections)

3. **Add `InjectContext()` API to `ContextManager`**:
   - New method for injecting large documents or codebases into the "retrieved context" partition
   - Supports: file contents, code snippets, web page content, PDF extractions
   - Automatically manages the retrieved context budget, evicting older injections when new ones arrive
   - Marked as cacheable content (placed in the stable prefix for DeepSeek V4 caching)

4. **Implement `ContextBudget` telemetry**:
   - After each LLM call, report how each partition was utilized
   - Track: actual vs. budgeted tokens per partition
   - Expose via `ContextUsage` in outbound messages
   - Log warnings when any partition consistently exceeds its budget

**Acceptance criteria**:
- Context window is partitioned with configurable budgets
- Large documents can be injected without overflowing the history partition
- Budget utilization is tracked and reported
- Default partition sizes work well for DeepSeek V4 at 1M context

---

### Phase 4: Advanced Features (Weeks 7-8)

These changes add DeepSeek V4-specific features that go beyond basic optimization.

---

#### Workstream 4.1: Full-Context Codebase Loading

**Goal**: Enable loading entire codebases or large documents into context for DeepSeek V4, replacing RAG chunking for many use cases.

**Changes**:

1. **Add `context_inject` tool to `pkg/tools/`**:
   - New tool that reads files/directories and injects them into the retrieved context partition
   - Parameters: `path` (file or directory), `max_tokens` (budget limit), `pattern` (glob for directory scanning)
   - For directories: reads all matching files, sorted by relevance (modified time, name match)
   - Respects the `RetrievedContextPct` budget; truncates or skips files that would overflow

2. **Add `context_list` tool**:
   - Lists currently injected context items with their token counts
   - Shows remaining budget in the retrieved context partition

3. **Add `context_clear` tool**:
   - Clears injected context to free budget
   - Optional `pattern` parameter to selectively clear matching items

4. **Implement smart file prioritization**:
   - When injecting a directory with more files than the budget allows:
     - Prioritize files modified more recently
     - Prioritize files matching the current task description
     - Skip binary files, `node_modules`, `.git`, vendor directories
     - Include file path as a header for each file's content

5. **Cache injected context**:
   - Injected context is placed in the stable prefix of the messages array
   - On subsequent LLM calls, the injected context is a cache hit (prefix match)
   - Cost of re-sending 500K of code is only $0.0014 per call on V4-Flash (cache hit pricing)

**Cost analysis** (V4-Flash):
- 500K tokens of injected code at cache miss: $0.07/call
- 500K tokens of injected code at cache hit: $0.0014/call
- Typical 10-call session: $0.014 total for full codebase context (vs $0.70 without caching)
- This is cheaper than RAG embedding + retrieval for most use cases

**Acceptance criteria**:
- Entire codebases can be loaded into context via tool calls
- Budget is enforced; overflow is handled gracefully
- Injected context is cache-friendly (stable prefix)
- Clearing and re-injecting works correctly

---

#### Workstream 4.2: Cost-Aware Model Routing

**Goal**: Implement intelligent routing between V4-Flash and V4-Pro based on task complexity, maximizing cost efficiency.

**Changes**:

1. **Extend `steering.go` with cost-aware routing**:
   - Add `CostAwareRouter` that considers:
     - Task complexity (simple tool call vs. complex reasoning)
     - Current session token usage (how much cached prefix would be lost by switching models)
     - Thinking level requirement (non-think always uses Flash; think-max may use Pro)
     - Rate limit status (if Flash is rate-limited, fall back to Pro)

2. **Implement cache-preserving model switches**:
   - When switching from V4-Flash to V4-Pro mid-session:
     - Both models share the same API base and prefix caching namespace
     - The system prompt and injected context will still be cache hits on the new model
     - Only the difference in model processing costs changes
   - When switching to a different provider entirely:
     - Prefix cache is lost; full cache-miss pricing applies
     - Router should prefer staying on the same provider family when possible

3. **Add `CostBudget` to session config**:
   - Optional per-session cost limit (in USD)
   - Router downgrades from Pro to Flash when approaching the limit
   - Tracks cumulative spend based on API-reported usage data

4. **Implement complexity scoring**:
   - Score each LLM call based on: number of tools available, conversation depth, input token count, task type
   - Low complexity (< 0.3): V4-Flash, non-think
   - Medium complexity (0.3 - 0.7): V4-Flash, think-high
   - High complexity (> 0.7): V4-Pro, think-high
   - Critical complexity (explicit user request): V4-Pro, think-max

**Acceptance criteria**:
- Model routing considers cost and caching implications
- Flash is preferred for simple tasks; Pro for complex ones
- Cache is preserved when switching within the DeepSeek family
- Users can set cost budgets per session

---

#### Workstream 4.3: Enhanced Token Usage Tracking and Reporting

**Goal**: Provide detailed token usage breakdown including cache hit rates, reasoning token costs, and per-partition utilization.

**Changes**:

1. **Extend `ContextUsage` in `pkg/bus/types.go`**:
   ```go
   type ContextUsage struct {
       UsedTokens          int
       TotalTokens         int
       CompressAtTokens    int
       UsedPercent         float64

       // DeepSeek V4 specific
       CacheHitTokens      int     // Tokens served from prefix cache
       CacheMissTokens     int     // Tokens computed fresh
       CacheHitRate        float64 // CacheHitTokens / (CacheHitTokens + CacheMissTokens)
       ReasoningTokens     int     // Tokens used for reasoning_content
       OutputTokens        int     // Tokens in the final response

       // Partition breakdown
       SystemPromptTokens  int
       HistoryTokens       int
       InjectedContextTokens int
       ToolDefTokens       int
   }
   ```

2. **Parse usage from API responses**:
   - DeepSeek V4 returns `usage.prompt_tokens`, `usage.completion_tokens`, and `usage.prompt_cache_hit_tokens`
   - Map these to the extended `ContextUsage` fields
   - When streaming, extract from the final `include_usage` chunk

3. **Add session-level cost tracking**:
   - Calculate cumulative cost per session based on token usage and pricing
   - Store in session metadata
   - Expose via API for dashboard display

4. **Add `/cost` command to CLI**:
   - Shows current session cost breakdown
   - Shows cache hit rate and savings from caching
   - Shows per-model cost allocation if using multiple models

**Acceptance criteria**:
- Token usage is accurately tracked including cache statistics
- Session costs are calculated and stored
- Cost breakdown is accessible via CLI command
- Existing `ContextUsage` fields remain backwards compatible

---

### Phase 5: DSML & Advanced V4 Features (Weeks 9-10)

These changes add DeepSeek V4-specific features discovered during PDF documentation review: DSML tool-call parsing, Chat Prefix Completion, strict mode for tool calls, and JSON output mode support.

---

#### Workstream 5.1: DSML Tool-Call Parser

**Goal**: Implement a parser for DeepSeek V4's DSML (DeepSeek Markup Language) tool-call format, enabling compatibility with both cloud API and local inference deployments.

**Changes**:

1. **Create `pkg/providers/openai_compat/dsml_parser.go`**:
   - Implement `ParseDSMLToolCalls(content string) ([]ToolCall, error)` that extracts tool calls from DSML-formatted text:
     ```
     <|DSML|tool_calls>
     <|DSML|invoke name="function_name">
     <|DSML|parameter name="param" string="true">string_value</|DSML|parameter>
     <|DSML|parameter name="count" string="false">5</|DSML|parameter>
     </|DSML|invoke>
     </|DSML|tool_calls>
     ```
   - Parse each `<|DSML|invoke>` block into a structured `ToolCall` with `name` and `arguments` (JSON object)
   - For `string="true"` parameters: wrap the raw value in JSON as a string
   - For `string="false"` parameters: parse the value as JSON directly
   - Handle multiple `<|DSML|invoke>` blocks within a single `<|DSML|tool_calls>` block

2. **Create `pkg/providers/openai_compat/dsml_parser_test.go`**:
   - Test single tool call with string and non-string parameters
   - Test multiple tool calls in a single DSML block
   - Test nested JSON parameters (arrays, objects)
   - Test malformed DSML (unclosed tags, missing attributes)
   - Test DSML intermixed with regular text content
   - Test DSML within `reasoning_content` (should be preserved as-is)

3. **Integrate DSML parser into response processing** in `pkg/providers/openai_compat/provider.go`:
   - After receiving a response, check if `content` contains `<|DSML|tool_calls>` markers
   - If DSML markers are found AND `tool_calls` array is empty, parse DSML to populate `tool_calls`
   - This handles the case where local inference engines return DSML instead of structured JSON
   - For cloud API responses, the `tool_calls` array is already populated; DSML parsing is a fallback

4. **Add DSML-aware debug logging**:
   - When DSML content is detected in a response, log at debug level with the parsed tool calls
   - This helps with debugging local inference deployments

**Acceptance criteria**:
- DSML-formatted tool calls are correctly parsed into OpenAI-compatible `ToolCall` structures
- Cloud API responses work unchanged (DSML parsing is fallback only)
- Local inference responses with DSML format are correctly handled
- Parser handles edge cases (malformed XML, mixed content, nested parameters)

---

#### Workstream 5.2: Chat Prefix Completion Support

**Goal**: Enable PicoClaw to use DeepSeek V4's Chat Prefix Completion feature for guided generation.

**Changes**:

1. **Add `PrefixCompletion` option to `ProviderOptions`** in `pkg/providers/openai_compat/provider.go`:
   - New option: `prefix_completion_content string` — content to use as the assistant prefix
   - When set, the last assistant message in the request will have `prefix: true`
   - Requires using the beta endpoint: `base_url="https://api.deepseek.com/beta"`

2. **Implement prefix completion in request builder**:
   - Append an assistant message with the prefix content and `prefix: true`
   - Optionally set `stop` sequences to control where generation ends
   - Example: Force Python code output by setting prefix to `` ```python\n `` and stop to `` ``` ``

3. **Add `reasoning_content` prefix support**:
   - The `reasoning_content` field on the last assistant message can serve as a CoT prefix
   - When `thinking_mode` is enabled, allow providing both `reasoning_content` and content prefix
   - This enables "guided reasoning" where the model continues from a partially-written reasoning chain

4. **Add beta endpoint detection**:
   - When prefix completion is requested, automatically switch to `https://api.deepseek.com/beta`
   - Log a warning that beta features are being used
   - Fall back gracefully if beta endpoint returns errors

**Acceptance criteria**:
- Prefix completion can be used to guide output format
- Beta endpoint is used automatically when needed
- Regular (non-prefix) completions are unaffected
- Works with both thinking and non-thinking modes

---

#### Workstream 5.3: Strict Mode for Tool Calls

**Goal**: Enable strict mode for DeepSeek V4 tool calls to guarantee schema-conformant output.

**Changes**:

1. **Add `StrictToolCalls` option to `ModelConfig`** in `pkg/config/config_struct.go`:
   - When `true`, all tool function definitions sent to DeepSeek V4 will include `strict: true`
   - Default: `false` (backward compatible)

2. **Implement strict mode in request builder** in `pkg/providers/openai_compat/provider.go`:
   - When `StrictToolCalls` is enabled for a DeepSeek V4 model:
     - Add `"strict": true` to each function definition in the `tools` array
     - Ensure all object schemas have `additionalProperties: false` and all properties in `required`
     - Validate schemas locally before sending; log warnings for non-compliant schemas
   - Use beta endpoint: `base_url="https://api.deepseek.com/beta"`

3. **Add schema validation helper** in `pkg/tools/schema_validator.go` (new):
   - Validate that tool parameter schemas conform to strict mode requirements
   - Check: all object properties are in `required`, `additionalProperties: false` on every object
   - Supported types: `object`, `string`, `number`, `integer`, `boolean`, `array`, `enum`, `anyOf`
   - Return validation errors with actionable messages (e.g., "property 'name' missing from required array")

4. **Handle strict mode validation errors**:
   - If the API returns a schema validation error, log the error with the specific function and parameter
   - Fall back to non-strict mode for that function and retry
   - This prevents a single non-compliant schema from blocking the entire request

**Acceptance criteria**:
- Strict mode guarantees schema-conformant tool call output
- Non-compliant schemas are detected and reported before API submission
- Fallback to non-strict mode works gracefully
- Existing tool definitions work unchanged when strict mode is disabled

---

#### Workstream 5.4: JSON Output Mode

**Goal**: Support DeepSeek V4's `response_format` parameter for guaranteed JSON output.

**Changes**:

1. **Add `ResponseFormat` option to `ProviderOptions`** in `pkg/providers/openai_compat/provider.go`:
   - New option: `response_format string` — `"text"` (default) or `"json_object"`
   - When `"json_object"`, add `response_format: { "type": "json_object" }` to the request body

2. **Integrate with agent pipeline**:
   - Certain tools or pipeline stages can request JSON output mode (e.g., structured data extraction)
   - When JSON output mode is active, ensure the system or user message includes JSON formatting instructions (required by the API)
   - Add validation that the response is valid JSON before processing

3. **Add `ResponseFormat` to `ModelConfig`**:
   - Optional field: `response_format string`
   - When set at config level, applies to all requests for that model
   - Can be overridden per-request in pipeline options

**Acceptance criteria**:
- JSON output mode forces valid JSON responses
- System prompt includes JSON instructions when mode is active
- Response validation catches non-JSON output gracefully
- Default behavior (text mode) is unchanged

---

## 5. Implementation Order and Dependencies

```
Phase 1: Foundation
├── 1.1 DeepSeek V4 Provider Profile ──────────── [no deps]
└── 1.2 Adaptive Context Window Config ────────── [no deps]

Phase 2: Caching & Compression
├── 2.1 Cache-Aware Prompt Construction ────────── [depends on 1.1]
└── 2.2 Adaptive Compression Strategy ──────────── [depends on 1.2]

Phase 3: Pipeline Optimization
├── 3.1 Reasoning Mode Switching ────────────────── [depends on 1.1]
├── 3.2 Streaming Integration ───────────────────── [depends on 1.1]
└── 3.3 Context Window Partitioning ─────────────── [depends on 1.2, 2.1]

Phase 4: Advanced Features
├── 4.1 Full-Context Codebase Loading ───────────── [depends on 3.3]
├── 4.2 Cost-Aware Model Routing ────────────────── [depends on 3.1, 4.3]
└── 4.3 Enhanced Token Usage Tracking ───────────── [depends on 3.2]

Phase 5: DSML & Advanced V4 Features
├── 5.1 DSML Tool-Call Parser ───────────────────── [depends on 1.1]
├── 5.2 Chat Prefix Completion ──────────────────── [depends on 1.1]
├── 5.3 Strict Mode for Tool Calls ──────────────── [depends on 1.1, 5.1]
└── 5.4 JSON Output Mode ────────────────────────── [depends on 1.1]
```

Workstreams 1.1 and 1.2 can be developed in parallel. Within Phase 2, workstreams 2.1 and 2.2 are also parallelizable given their Phase 1 dependencies are met. Phase 5 workstreams 5.1, 5.2, and 5.4 can be developed in parallel; 5.3 depends on 5.1 (DSML parser needed for strict mode integration testing).

---

## 6. Risk Assessment

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| DeepSeek V4 API changes before stable release | Medium | High | Pin to documented API spec; add integration tests against preview API |
| Prefix caching behavior differs from documentation | Low | Medium | Add cache hit rate monitoring; fall back to non-cached behavior if hit rate < 20% |
| 1M context causes memory pressure on PicoClaw server | Low | High | Set default `MaxTokens` conservatively (16K); add memory monitoring |
| Token estimation inaccuracy leads to context overflow | Medium | Medium | Use API-reported usage (Workstream 3.2) as primary; keep heuristic as fallback |
| Reasoning content bloats session storage | Medium | Low | Add optional `reasoning_content` compression; cap stored reasoning per turn |
| Cost overrun from uncontrolled output generation | Medium | High | Always set `max_tokens` explicitly; add per-session cost budgets (Workstream 4.2) |
| DSML parsing errors on malformed model output | Medium | Medium | Comprehensive test coverage; graceful fallback to raw text; log DSML parse failures |
| Beta API features (prefix completion, strict mode) may change | Medium | Medium | Feature-gate behind config flags; document as beta; provide fallback paths |
| Interleaved thinking increases token usage significantly | High | Medium | Track reasoning token costs; add budget limits for reasoning tokens per session |

---

## 7. Testing Strategy

### Unit Tests

- DeepSeek V4 model detection and configuration
- Thinking level mapping (PicoClaw → DeepSeek API parameters)
- Adaptive compression threshold calculation
- Context partition budget enforcement
- Message ordering for cache stability
- DSML tool-call parsing (single, multiple, malformed, mixed content)
- Strict mode schema validation
- JSON output mode request construction
- Chat prefix completion request construction
- Interleaved thinking: reasoning preservation when tools present vs. absent

### Integration Tests

- End-to-end DeepSeek V4 API calls (non-think, think-high, think-max)
- Multi-turn conversation with `reasoning_content` preservation
- Streaming with tool calls
- Prefix cache hit rate measurement (requires sequential calls)
- Context injection and budget management
- DSML-formatted response parsing from local inference
- Strict mode tool calls with schema validation
- Chat prefix completion with guided output
- JSON output mode with response validation

### Load Tests

- Sustained 1M-token context sessions (memory, latency)
- High-concurrency multi-tenant sessions with `user_id` isolation
- Cache hit rate under realistic conversation patterns

### Cost Validation Tests

- Compare estimated costs vs. actual API billing
- Verify cache hit pricing is applied correctly
- Test cost budget enforcement

---

## 8. Configuration Examples

### Minimal DeepSeek V4 Configuration

```json
{
  "models": [
    {
      "name": "deepseek-v4-flash",
      "model": "deepseek/deepseek-v4-flash",
      "api_base": "https://api.deepseek.com",
      "api_key": "${DEEPSEEK_API_KEY}",
      "max_tokens": 16384,
      "context_window": 1048576
    }
  ],
  "defaults": {
    "model": "deepseek-v4-flash",
    "max_tokens": 16384,
    "thinking_level": "medium",
    "compression_strategy": "adaptive"
  }
}
```

### Full DeepSeek V4 Configuration with Routing

```json
{
  "models": [
    {
      "name": "deepseek-v4-flash",
      "model": "deepseek/deepseek-v4-flash",
      "api_base": "https://api.deepseek.com",
      "api_key": "${DEEPSEEK_API_KEY}",
      "max_tokens": 16384,
      "context_window": 1048576,
      "thinking_level": "medium"
    },
    {
      "name": "deepseek-v4-pro",
      "model": "deepseek/deepseek-v4-pro",
      "api_base": "https://api.deepseek.com",
      "api_key": "${DEEPSEEK_API_KEY}",
      "max_tokens": 32768,
      "context_window": 1048576,
      "thinking_level": "high",
      "strict_tool_calls": true
    }
  ],
  "defaults": {
    "model": "deepseek-v4-flash",
    "max_tokens": 16384,
    "thinking_level": "medium",
    "compression_strategy": "adaptive",
    "streaming_mode": "auto",
    "context_partitions": {
      "system_prompt_pct": 2,
      "working_memory_pct": 3,
      "retrieved_context_pct": 60,
      "history_pct": 30,
      "output_pct": 5
    }
  },
  "router": {
    "light_candidates": ["deepseek-v4-flash"],
    "heavy_candidates": ["deepseek-v4-pro"],
    "cost_budget_usd": 1.0
  }
}
```

### Configuration with Advanced V4 Features

```json
{
  "models": [
    {
      "name": "deepseek-v4-pro-strict",
      "model": "deepseek/deepseek-v4-pro",
      "api_base": "https://api.deepseek.com/beta",
      "api_key": "${DEEPSEEK_API_KEY}",
      "max_tokens": 32768,
      "context_window": 1048576,
      "thinking_level": "high",
      "strict_tool_calls": true,
      "response_format": "json_object"
    }
  ]
}
```

---

## 9. Success Metrics

| Metric | Current Baseline | Target (After Phase 5) |
|--------|-----------------|----------------------|
| Max effective context on DeepSeek V4 | ~128K (heuristic limit) | 1M (full window) |
| Prefix cache hit rate | N/A (not measured) | > 60% for sequential calls |
| Compression trigger (1M context) | ~20 messages | ~375 messages (adaptive) |
| Reasoning content preservation | Stripped | Fully preserved (with interleaved thinking) |
| Thinking mode control | Fixed per-session | Dynamic per-iteration |
| Token estimation accuracy | ~60-80% (heuristic) | > 95% (API-reported) |
| Time-to-first-token | Full response wait | < 2 seconds (streaming) |
| Cost per 10-turn session (V4-Flash) | ~$0.14 (no caching) | ~$0.03 (with caching + adaptive thinking) |
| DSML tool-call parsing | Not supported | Full support (local + cloud) |
| Strict mode tool calls | Not supported | Schema-validated tool output |
| Chat prefix completion | Not supported | Guided generation for code/structured output |
| JSON output mode | Not supported | Guaranteed JSON responses |

---

## 10. Out of Scope

The following are explicitly excluded from this plan:

- **RAG replacement**: Full-context loading reduces but does not eliminate the need for RAG. Very large codebases (> 1M tokens) still require retrieval.
- **DeepSeek V4 fine-tuning**: This plan focuses on API optimization, not model customization.
- **Multi-model consensus**: Running V4-Flash and V4-Pro in parallel for the same query is not planned.
- **Batch processing**: DeepSeek does not offer a batch API; implementing a custom batch queue is out of scope.
- **On-premise deployment**: The plan targets the DeepSeek V4 cloud API. Local deployment optimizations are a separate concern.
- **Web UI changes**: The web frontend (`web/`) does not require changes for V4 optimization. Context usage display will use existing `ContextUsage` fields.
- **Quick Instruction tokens**: The `<|action|>`, `<|title|>`, `<|query|>`, `<|authority|>`, `<|domain|>`, `<|extracted_url|>`, and `<|read_url|>` tokens are DeepSeek's internal pipeline tokens for chatbot auxiliary tasks. They are documented in Section 2.10 for reference, but implementing them in PicoClaw is out of scope because PicoClaw does not run a chatbot UI with search/title generation features. If needed in the future, they would be a separate feature.
- **`developer` role**: The `developer` message role is used exclusively in DeepSeek's internal search agent pipeline. The official API does not accept messages with this role. PicoClaw should not send `developer`-role messages.
- **`latest_reminder` role**: The `latest_reminder` role injects date/locale context. PicoClaw handles this differently via its own system prompt construction, so the V4-specific role is not needed.
- **Anthropic API compatibility endpoint**: DeepSeek offers an Anthropic-compatible API, but PicoClaw already has a native Anthropic provider implementation, so using the DeepSeek Anthropic wrapper provides no benefit.
- **FIM (Fill-In-the-Middle) Completion**: This is a separate beta feature for code completion tasks, not chat-based coding assistance. May be reconsidered if PicoClaw adds inline code completion features.
