# DeepSeek V4 Optimization Plan for PicoClaw

## Document Metadata

| Field | Value |
|-------|-------|
| Status | Draft |
| Branch | `wip/deepseekv4_optimized` |
| Author | AI-assisted |
| Created | 2026-05-01 |
| Last Updated | 2026-05-01 |

---

## 1. Executive Summary

DeepSeek V4 introduces a **1,048,576-token context window** (1M), **automatic prefix caching** with 10x cost reduction on cache hits, **384K max output tokens**, and **three reasoning modes** (non-think, think-high, think-max). These capabilities fundamentally change how PicoClaw should manage context, prompt construction, token budgets, and the agent loop lifecycle.

The current PicoClaw architecture was designed for models with 32K-128K context windows. It uses aggressive compression, heuristic token estimation, and treats all providers uniformly through an OpenAI-compatible abstraction. DeepSeek V4's scale demands a targeted optimization strategy that preserves the existing abstraction while exploiting V4's unique features: massive context, automatic caching, and reasoning mode control.

This plan proposes **8 workstreams** across **4 phases**, estimated at **6-8 weeks** of effort. Each workstream is independently mergeable with no breaking changes to existing provider integrations.

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
```

Workstreams 1.1 and 1.2 can be developed in parallel. Within Phase 2, workstreams 2.1 and 2.2 are also parallelizable given their Phase 1 dependencies are met.

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

---

## 7. Testing Strategy

### Unit Tests

- DeepSeek V4 model detection and configuration
- Thinking level mapping (PicoClaw → DeepSeek API parameters)
- Adaptive compression threshold calculation
- Context partition budget enforcement
- Message ordering for cache stability

### Integration Tests

- End-to-end DeepSeek V4 API calls (non-think, think-high, think-max)
- Multi-turn conversation with `reasoning_content` preservation
- Streaming with tool calls
- Prefix cache hit rate measurement (requires sequential calls)
- Context injection and budget management

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
      "thinking_level": "high"
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

---

## 9. Success Metrics

| Metric | Current Baseline | Target (After Phase 4) |
|--------|-----------------|----------------------|
| Max effective context on DeepSeek V4 | ~128K (heuristic limit) | 1M (full window) |
| Prefix cache hit rate | N/A (not measured) | > 60% for sequential calls |
| Compression trigger (1M context) | ~20 messages | ~375 messages (adaptive) |
| Reasoning content preservation | Stripped | Fully preserved |
| Thinking mode control | Fixed per-session | Dynamic per-iteration |
| Token estimation accuracy | ~60-80% (heuristic) | > 95% (API-reported) |
| Time-to-first-token | Full response wait | < 2 seconds (streaming) |
| Cost per 10-turn session (V4-Flash) | ~$0.14 (no caching) | ~$0.03 (with caching + adaptive thinking) |

---

## 10. Out of Scope

The following are explicitly excluded from this plan:

- **RAG replacement**: Full-context loading reduces but does not eliminate the need for RAG. Very large codebases (> 1M tokens) still require retrieval.
- **DeepSeek V4 fine-tuning**: This plan focuses on API optimization, not model customization.
- **Multi-model consensus**: Running V4-Flash and V4-Pro in parallel for the same query is not planned.
- **Batch processing**: DeepSeek does not offer a batch API; implementing a custom batch queue is out of scope.
- **On-premise deployment**: The plan targets the DeepSeek V4 cloud API. Local deployment optimizations are a separate concern.
- **Web UI changes**: The web frontend (`web/`) does not require changes for V4 optimization. Context usage display will use existing `ContextUsage` fields.
