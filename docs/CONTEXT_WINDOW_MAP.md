# Context Window Map

This document traces exactly how the LLM context window is populated in picoclaw,
using code-level terms so every section can be traced back to its source in the codebase.

> **Model**: DeepSeek V4 (deepseek-v4-pro), automatic prefix caching
> **Reference**: `pkg/agent/context.go` (primary), `pkg/agent/pipeline_llm.go` (execution)

---

## 1. Call Chain (Entry Points)

```
Inbound message arrives at channel (e.g. Telegram)
  → Pipeline.RunTurn() or coordinator loop
    → Pipeline.CallLLM()                          [pkg/agent/pipeline_llm.go]
      → ContextBuilder.BuildMessagesFromPrompt()  [pkg/agent/context.go] ← CONTEXT ASSEMBLED HERE
        → provider.Chat(messages, toolDefs, ...)  [pkg/providers/openai_compat/provider.go]
```

The full context is assembled in **`BuildMessagesFromPrompt(req PromptBuildRequest)`** at `context.go:712`.
This function returns `[]providers.Message` which becomes `exec.callMessages` in `pipeline_llm.go:67`,
and tool definitions are separately computed as `exec.providerToolDefs` at `pipeline_llm.go:43`.

---

## 2. Message Order (exactly as sent to the LLM)

The final `messages` array is built in strict order. Each section maps to a code block in `BuildMessagesFromPrompt`:

| Index | Role | Content Source | Cacheable? | Code Reference |
|---|---|---|---|---|
| **messages[0]** | `system` | **Stable system prompt** | ✅ Fully cacheable across turns | `context.go:809` |
| **messages[1]** | `system` | **Volatile system context** | ❌ Changes per request | `context.go:877` |
| **messages[2]** | `user` | **Context summary** (if present) | ❌ Changes per turn | `context.go:886` |
| **messages[3]** | `assistant` | Summary acknowledgment | ❌ | `context.go:893` |
| **messages[4..N-2]** | mixed | **Conversation history** (sanitized) | ⚠️ Within-turn only | `context.go:912` |
| **messages[N-1]** | `user` | **Current user message** + media | ❌ | `context.go:918` |

Between `messages` and tool definitions, the OpenAI-compat provider sends `tools` as a **separate JSON field** in the API request body (`provider.go:604-608`). DeepSeek V4 positions tools internally (likely after system messages, before conversation in tokenized form).

---

## 3. messages[0] — Stable System Prompt

**Code**: `context.go:793-809`

Built from two sources merged together:

### 3a. Static Prompt (`BuildSystemPromptWithCache()`)

| PromptPart ID | Slot | Source | Content |
|---|---|---|---|
| `kernel.identity` | `PromptSlotIdentity` | `PromptSourceKernel` | `getIdentity()` — picoclaw name, workspace path, 4 hard rules |
| `instruction.workspace` | `PromptSlotWorkspace` | `PromptSourceWorkspace` | `LoadBootstrapFiles()` — AGENT.md / SOUL.md / USER.md / IDENTITY.md |
| `capability.skill_catalog` | `PromptSlotSkillCatalog` | `PromptSourceSkillCatalog` | `BuildSkillsSummary()` — installed skill names + descriptions |
| `context.memory` | `PromptSlotMemory` | `PromptSourceMemory` | `GetMemoryContext()` — MEMORY.md contents |
| `context.output_policy.split_on_marker` | `PromptSlotOutput` | `PromptSourceOutputPolicy` | (if `splitOnMarker` enabled) |

**Cache**: Locally cached in-memory (`cachedSystemPrompt`). Invalidated when tracked workspace files change (mtime check via `sourceFilesChangedLocked()`).

### 3b. Stable Contributors (PromptCacheEphemeral)

Collected via `promptRegistryOrDefault().Collect()` at `context.go:762`. These are prompt contributors whose `Cache` policy is `PromptCacheEphemeral`:

| Contributor | Source ID | Content |
|---|---|---|
| `safeEditWorkflowContributor` | `PromptSourceSafeEditWorkflow` | Mandatory code editing workflow rules (READ-SEARCH-BUILD-TEST) |
| `toolDiscoveryPromptContributor` | `PromptSourceToolDiscovery` | Tool discovery instructions (if BM25/regex tool search enabled) |

**Cache**: These are included in `messages[0]` because `PromptCacheEphemeral` → they are part of the DeepSeek V4 cacheable prefix.

### 3c. Cache Boundary

- `CacheBoundaryIndex` = number of `SystemParts` blocks in messages[0] (`context.go:819`)
- `PrefixHash` = FNV-1a hash of all stable content blocks (`context.go:827`)
- Logged as "Prefix cache break detected" when hash changes between calls

---

## 4. messages[1] — Volatile System Context

**Code**: `context.go:858-881`

### 4a. Volatile Parts (PromptCacheNone)

Parts from contributors with `Cache == PromptCacheNone`:

| Source | Content |
|---|---|
| `InjectedContext.Content()` | Injected context from `context_inject` tool calls |
| `buildActiveSkillsContext()` | Active skill instructions (full SKILL.md loaded) |

### 4b. Runtime Context (`buildDynamicContext()`)

**Code**: `context.go:636-655`

```go
## Current Time
{2006-01-02 15:04 (Monday)}      // time.Now()

## Runtime
{linux amd64, Go go1.X.Y}        // runtime.GOOS, runtime.GOARCH, runtime.Version()

## Current Session
Channel: {telegram/discord/pico}
Chat ID: {chatID}

## Current Sender
{senderDisplayName} (ID: {senderID})
```

All of this is per-request dynamic — **never** cacheable. Placed in `messages[1]` (not `messages[0]`) to keep the cacheable prefix clean.

---

## 5. messages[2-3] — Context Summary

**Code**: `context.go:885-894`

When `req.Summary != ""` (from `seahorse` short-term memory engine after compression):

```
messages[2]: user
  "CONTEXT_SUMMARY: The following is an approximate summary of prior
   conversation for reference only... \n\n{summary text}"

messages[3]: assistant
  "Understood. I will use this context summary as background reference."
```

Emitted as user/assistant pair (not system) to keep both system messages stable-pattern-friendly for prefix caching.

---

## 6. messages[4..N-2] — Conversation History

**Code**: `context.go:912` (`history := sanitizeHistoryForProvider(req.History)`)

### 6a. Source

`req.History` comes from the session store (`ts.agent.Sessions` → `SessionStore`). Messages are persisted via `AddFullMessage()` in:
- `pipeline_llm.go:574` — assistant messages
- `pipeline_execute.go:277` — tool result messages

### 6b. Sanitization (`sanitizeHistoryForProvider()`)

**Code**: `context.go:957`

Processing rules:
1. **Drop system messages** — `BuildMessagesFromPrompt` always constructs its own
2. **Validate tool messages** — Must follow an assistant message with matching `tool_calls`
3. **Validate assistant messages** — Tool-call assistants must precede tool results
4. **Deduplicate tool results** — Within a tool-result block, duplicate `tool_call_id` dropped
5. **Drop incomplete tool blocks** — Assistant with tool_calls but missing tool results is dropped
6. **TRUNCATE verbose tool outputs** — `truncateToolResultContent()` caps at `toolResultMaxHistoryChars` (500). See §6c.

### 6c. Tool Output Truncation

**Code**: `context.go:934-954`

```go
const toolResultMaxHistoryChars = 500

func truncateToolResultContent(msg providers.Message) providers.Message
```

Applied during sanitization pass 1 (line 993). Only affects tool messages **from saved history** — current-turn tool results in `exec.messages` are preserved at full length.

Truncation format:
```
{first 500 chars of output}

[...truncated from {N} chars in history; full output was available at execution time...]
```

This saves ~200-400 tokens per verbose tool result while keeping the signal for the LLM.

---

## 7. messages[N-1] — Current User Message

**Code**: `context.go:918-921`

```go
userPromptMessage(req.CurrentMessage, req.Media)
```

From `promptBuildRequestForTurn()` at `prompt_turn.go:10`:
- `CurrentMessage` = `ts.userMessage` (raw user input)
- `Media` = resolved media references from `al.mediaStore`

---

## 8. Tool Definitions (Separate from Messages)

**Code**: `pipeline_llm.go:43` → `registry.go:331`

### 8a. Source: `ToolRegistry.ToProviderDefs()`

**Code**: `registry.go:331-373`

- Iterates tools in **sorted order** via `sortedToolNames()` (`registry.go:391`)
- Sorting is critical for KV cache stability — non-deterministic map iteration would produce different tool definition JSON each call
- Each tool becomes a `providers.ToolDefinition{Type:"function", Function:{Name,Description,Parameters}}`

### 8b. API placement

**Code**: `provider.go:604-608`

```go
requestBody["tools"] = buildToolsList(tools, nativeSearch)
```

Tools are sent as a **separate JSON field** (`"tools"`) in the API request body, not embedded in messages. DeepSeek V4 positions them in its internal prompt format (exact position unknown, but cache hit data suggests they are after the system prompt prefix).

### 8c. Tool count impact

- 27 tools × ~130 chars each (name + description + schema) = ~3,500 tokens
- These are static across turns → cacheable within a turn's iterations (92-99% hit)
- NOT cacheable across turns (cache resets to 2,048 at each new turn's first iteration)

---

## 9. DeepSeek V4 Cache Behavior (Observed)

Based on tracing data from `llm_calls` table:

| Turn | Iteration | Cache Hit Tokens | Hit % | Explanation |
|---|---|---|---|---|
| turn-N | 1 | **2,048** | ~16% | Only system prompt cached (messages[0]) |
| turn-N | 2+ | **90-99%** | ~97% | Full prefix cached (system + history + tools) |

**2,048 tokens** ≈ system prompt (`getIdentity()` + bootstrap + skills + memory + safe-edit + tool-discovery).

The cache resets at each new turn's first iteration because the conversation history in `messages[4..]` has new messages appended, changing the full prompt. Only the system prompt prefix (messages[0]) is identical across turns.

---

## 10. Token Accounting (Approximate, Peak: 14,418 tokens)

| Section | Chars | Tokens (est.) | % | Cacheable? |
|---|---|---|---|---|
| System prompt (messages[0]) | ~8,200 | ~2,050 | 14% | ✅ Cross-turn |
| Volatile context (messages[1]) | ~300 | ~75 | 0.5% | ❌ |
| Context summary (messages[2-3]) | ~4,300 | ~1,075 | 7% | ❌ |
| Conversation history | ~20,000 | ~5,000 | 35% | ⚠️ Within-turn |
| Tool definitions (27 tools) | — | ~3,500 | 24% | ⚠️ Within-turn |
| Current user message | ~1,000 | ~250 | 2% | ❌ |
| Tool outputs in history (truncated) | ~2,500 | ~625 | 4% | ⚠️ Within-turn |
| **Total** | | **~14,500** | | |

---

## 11. Key Types Reference

| Type | File | Purpose |
|---|---|---|
| `ContextBuilder` | `context.go:26` | Builds system prompt, assembles messages |
| `PromptBuildRequest` | `prompt.go:105` | Request struct carrying history/summary/channel/skills |
| `PromptPart` | `prompt.go:94` | Single section of system prompt with Cache policy |
| `PromptCachePolicy` | `prompt.go:67` | `Ephemeral` (cacheable), `None` (volatile) |
| `PromptStack` | `prompt.go:329` | Ordered, validated collection of PromptParts |
| `turnExecution` | `turn_state.go:157` | Per-turn mutable state (messages, toolDefs, traceID) |
| `turnState` | `turn_state.go` | Immutable turn context (agent, channel, session, etc.) |
| `providers.Message` | `protocoltypes` | Role/Content/ToolCalls/ToolCallID |
| `providers.ToolDefinition` | `protocoltypes` | Type/Function{Name,Description,Parameters} |
| `ToolRegistry` | `registry.go` | Tool registration and ToProviderDefs() |
