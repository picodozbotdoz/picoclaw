# Context Window Restructure — Maximizing DeepSeek V4 Prefix Cache

> **Status**: Design proposal
> **References**: `docs/CONTEXT_WINDOW_MAP.md`, `docs/CONTEXT_WINDOW_MGT_P3.md`
> **Current code**: `pkg/agent/context.go::BuildMessagesFromPrompt()`

---

## Problem

Current cache hit on first iteration of every new turn is only **2,048 tokens**
(just the stable system prompt). The rest (~55K tokens) is uncached because
the context summary and volatile system message sit between the stable prompt
and the conversation history, breaking the cacheable prefix.

DeepSeek V4's automatic prefix caching works on the longest matching prefix
from position 0. If the first element differs (or is in a different position),
the entire cache is invalidated. Every element between the stable prefix and
the conversation history that changes per-turn is a cache-break point.

---

## Current Structure (Problematic)

```
┌──────────────────────────────────────────────────────────────┐
│ POSITION 0 ────────────────────────────────────────────► N  │
├──────────┬─────────┬──────────┬──────────┬──────────────────┤
│ SYSTEM   │ SYSTEM  │ SUMMARY  │ SUMMARY  │ HISTORY          │
│ (stable) │ (vol)   │ (user)   │ (asst)   │ (160+ msgs)      │
│          │         │          │          │                  │
│ ~2048    │ ~75     │ ~500     │ ~50      │ ~50,000 tokens   │
│ tokens   │ tokens  │ tokens   │ tokens   │                  │
├──────────┴─────────┴──────────┴──────────┴──────────────────┤
│  ▲ CACHE HIT: 2,048          │  NOT CACHED: ~50,625 tokens  │
└──────────────────────────────────────────────────────────────┘
                            │
                    ┌───────┴───────┐
                    │ USER MSG      │
                    │ (current)     │
                    │ ~250 tokens   │
                    └───────────────┘

TOOLS (separate JSON field): 27 definitions, ~3,500 tokens
  → Positioned by DeepSeek internally (unknown, not effectively cached)
```

**Cache boundary**: The volatile system message (`messages[1]`) changes every
turn because `buildDynamicContext()` embeds `time.Now()` and session info.
The context summary (`messages[2-3]`) changes when the summarizer runs.
Everything after them — the entire conversation history — is uncached.

**Result**: On a 264-message, 97K-token session, only 2,048 tokens (2.1%)
hit the cache on first-iteration calls. The other 97.9% must be recomputed.

---

## Target Structure (Optimized)

```
┌──────────────────────────────────────────────────────────────┐
│ POSITION 0 ────────────────────────────────────────────► N  │
├──────────┬──────────────────┬──────────┬────────────────────┤
│ SYSTEM   │ HISTORY          │ VOLATILE │ USER MSG           │
│ (stable) │ (sanitized)      │ (runtime │ (current)          │
│          │                  │ +summary)│                    │
│ ~2048    │ ~50,000 tokens   │ ~625     │ ~250 tokens        │
│ tokens   │ (stable portion  │ tokens   │                    │
│          │  is cacheable)   │          │                    │
├──────────┴──────────────────┼──────────┼────────────────────┤
│  ▲ CACHE HIT: ~52,000       │ dynamic  │ dynamic            │
│    (system + old history)   │          │                    │
└─────────────────────────────┴──────────┴────────────────────┘

TOOLS (separate JSON field): 27 definitions, ~3,500 tokens
  → Positioned by DeepSeek internally after system prompt
  → Now within the cacheable prefix region
```

**Cache boundary**: The volatile message (`messages[N-2]`) becomes the
cache-break point. Everything before it — stable system prompt + all of
conversation history — forms the cacheable prefix.

**Result**: On a 264-message session, approximately **52,000 tokens (54%)**
hit the cache on first-iteration calls — up from 2,048 (2.1%). This saves
~50K tokens of recomputation per turn.

---

## Element Classification

### CONSTANT (always at front, fully cacheable)

| Element | Current Position | New Position | Why Static |
|---|---|---|---|
| **Model definition** | API `model` field | Same | Never changes for a session |
| **Tools definition** | API `tools` field | Same (after system, before history) | 27 functions, sorted, deterministic |
| **System prompt (stable)** | `messages[0]` | `messages[0]` | Identity, rules, bootstrap, skills catalog, memory |
| **Safe-edit workflow rules** | `messages[0]` (contributor) | `messages[0]` | PromptCacheEphemeral, static text |
| **Tool discovery rules** | `messages[0]` (contributor) | `messages[0]` | PromptCacheEphemeral, static text |
| **Conversation history** | `messages[4..N-2]` | `messages[1..N-3]` | Only the LAST turn pair changes; all prior history is stable. DeepSeek V4 can cache the shared prefix. |
| **Summarised chat history** | `messages[2-3]` (user/assistant pair) | Merged into history or appended at history end | Already part of history conceptually |

### DYNAMIC (at back, small, recomputed cheaply)

| Element | Current Position | New Position | Why Dynamic |
|---|---|---|---|
| **Runtime context** | `messages[1]` (volatile system) | `messages[N-2]` (volatile system) | `time.Now()`, session info |
| **Active skills** | `messages[1]` (volatile) | `messages[N-2]` (volatile) | Changes per request |
| **Injected context** | `messages[1]` (volatile) | `messages[N-2]` (volatile) | Changes during turn |
| **Context summary** | `messages[2-3]` (user/assistant pair) | Merged into `messages[N-2]` or as history prefix | Changes when summarizer runs |
| **Tool outputs** | Part of history | Remains in history position | Inevitably dynamic, but now at end of cacheable prefix |
| **Current user message** | `messages[N-1]` | `messages[N-1]` | Per-turn |
| **Thinking level** | API `thinking` field | Same | May change per-iteration |

---

## Implementation Changes

### 1. Reorder messages in `BuildMessagesFromPrompt()`

**File**: `pkg/agent/context.go::BuildMessagesFromPrompt()`

**Current order**:
```go
messages = append(messages, stableSystemMsg)     // [0]
messages = append(messages, volatileSystemMsg)   // [1]
if summary != "" {
    messages = append(messages, summaryUserMsg)   // [2]
    messages = append(messages, summaryAsstMsg)   // [3]
}
messages = append(messages, history...)           // [4..N-2]
messages = append(messages, currentUserMsg)       // [N-1]
```

**New order**:
```go
messages = append(messages, stableSystemMsg)     // [0] — cacheable prefix start
// History immediately follows stable system prompt
// so the entire stable prefix (system + old history) is cacheable
messages = append(messages, history...)           // [1..N-3] — mostly static

// Summary and volatile content at the back
// so they don't break the cacheable prefix
if summary != "" {
    messages = append(messages, summaryUserMsg)   // [N-2]
    messages = append(messages, summaryAsstMsg)   // [N-1]
}
messages = append(messages, volatileSystemMsg)   // [N-1 or N+1]
messages = append(messages, currentUserMsg)       // last
```

### 2. Move summary from user/assistant pair to system context

The context summary is currently injected as a user/assistant message pair:
```
messages[2]: user — "CONTEXT_SUMMARY: ..."
messages[3]: assistant — "Understood. I will use this context summary..."
```

This format was chosen to keep system messages cache-friendly. But it puts
the summary BEFORE the history, breaking the cache prefix. Moving it AFTER
the history preserves the cache while keeping the same semantic effect.

**Alternative**: Embed the summary into the volatile system message
(`messages[N-2]`) as a prefix, eliminating the separate message pair entirely.
The LLM would see it as part of the runtime context, which is conceptually
correct — the summary IS part of the current request's context.

### 3. Ensure history is sanitized before inclusion

The `sanitizeHistoryForProvider()` function already:
- Drops system messages from history
- Validates tool/assistant message ordering
- Deduplicates tool results
- Truncates old tool outputs

This function runs before history is added to messages. With the reorder,
it runs at the same point — just earlier in the message assembly.

### 4. DeepSeek V4 `user_id` for cache isolation

The code already sets `user_id` in `exec.llmOpts` (`pipeline_llm.go:83`):
```go
exec.llmOpts = map[string]any{
    "user_id": ts.agent.ID, // DeepSeek V4 KV cache isolation per user_id
}
```

This ensures the cache is scoped to the agent/user. Multiple users/agents
sharing the same API key won't pollute each other's cache. No changes needed.

---

## Expected Impact

### Cache Hit Improvement

| Scenario | Current Hit | New Hit | Savings |
|---|---|---|---|
| Turn-2, iter 1 (cold start) | 2,048 (2.1%) | ~52,000 (54%) | **~50K tokens** |
| Turn-4, iter 1 | 2,048 (4.8%) | ~40,000 (70%) | **~38K tokens** |
| Turn-6, iter 1 | 2,048 (3.1%) | ~55,000 (82%) | **~53K tokens** |
| Within-turn iters 2+ | 97-99% | 97-99% | No change (already optimal) |

### Latency Impact

First-iteration calls currently take 4-10 seconds (cold cache). With 50K more
tokens cached, latency should drop to 1-2 seconds — similar to within-turn
iteration latency.

### Cost Impact

DeepSeek V4 pricing: $0.28/M input tokens. Cache hits are **free** (DeepSeek
doesn't charge for cached tokens).

- Per cold-start turn: 50K uncached → 50K cached = $0.014 saved
- Per 10-turn session with 3 cold starts: $0.042 saved
- Per 100-turn session: ~$0.28 saved

---

## Risks and Mitigations

### Risk 1: Summary position breaks LLM understanding

**Risk**: Moving the summary to AFTER the history may confuse the LLM — it
sees history first, then a summary of "what happened before."

**Mitigation**: The summary already says "The following is an approximate
summary of prior conversation." The LLM naturally reads the summary as
context, not as chronological conversation. Position doesn't matter
semantically — the LLM processes the full prompt before generating.

### Risk 2: History grows unbounded in cacheable prefix

**Risk**: If history grows to 160+ messages, the cacheable prefix becomes
very large. DeepSeek V4 has a finite cache (likely shared across requests).

**Mitigation**: The summarizer already truncates history and replaces with
summary. With P1 (lowered threshold to 20 messages), history should stay
under 30-40 messages after compression. This is well within cache limits.

### Risk 3: Provider compatibility

**Risk**: Some providers (Anthropic, Codex) handle multiple system messages
differently. Moving the volatile system message to the back may break their
internal message assembly.

**Mitigation**: The code already handles provider-specific message preparation
in `prepareMessagesForRequest()` (`provider.go:288`). Non-OpenAI-compat
providers collect all system messages into a single top-level parameter.
The order of system messages doesn't affect this collection — they're
concatenated regardless of position.

### Risk 4: Tool outputs between summary and current message

**Risk**: In the reordered structure, tool outputs from history are at
position [1..N-3] (before the summary). The summary might be about content
that appears later, which could be confusing.

**Mitigation**: This is the same as the current behavior — the summary
describes what happened BEFORE this turn's conversation. The history after
it is the current turn's conversation. The LLM naturally distinguishes
"summary of past" from "current conversation."

---

## Code Locations to Modify

| File | Function | Change |
|---|---|---|
| `pkg/agent/context.go` | `BuildMessagesFromPrompt()` | Reorder message assembly: history immediately after stable system, volatile+summary at back |
| `pkg/agent/context.go` | `sanitizeHistoryForProvider()` | No changes (already handles dedup, truncation) |
| `pkg/agent/context.go` | `buildDynamicContext()` | No changes (already produces small volatile context) |
| `pkg/tools/registry.go` | `ToProviderDefs()` | No changes (already sorts tools deterministically) |

**No new files needed.** This is a pure reorder of existing message assembly logic.
The change is approximately 15-20 lines moved within `BuildMessagesFromPrompt()`.

---

## Verification

After implementation, verify via tracing:

```sql
-- Check cache hit on first-iteration calls (should be much higher)
SELECT turn_id, iteration, messages_count,
  prompt_tokens, cache_hit_tokens,
  ROUND(100.0 * cache_hit_tokens / prompt_tokens, 1) AS hit_pct
FROM llm_calls WHERE iteration = 1
ORDER BY request_time DESC LIMIT 10;
```

Expected: cache hit on iteration 1 should jump from ~3-17% to ~50-85%.
