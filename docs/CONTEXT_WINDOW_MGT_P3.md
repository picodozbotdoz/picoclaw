# P3: Context Window as Lego Blocks — Managed Tool Output Retention

## Problem Statement

From `llm_calls` tracing on zkian's picoclaw (turn-5, 80 iterations, 121 messages):

| File | Chars | Tokens (est.) | Times Read | Wasted |
|---|---|---|---|---|
| `DEPLOYMENT_INSTRUCTION.md` | 10,016 | ~2,500 | 1 | Medium — persists across remaining 79 iters |
| `scheduler.py` | 6,252 | ~1,560 | 1 | Medium |
| `api_client.py` | 4,673 | ~1,170 | **3 times** | High — same file, unchanged |
| `dex/SKILL.md` | 2,956 | ~740 | 1 | Low |
| `MEMORY.md` | 2,248 | ~560 | 1 | Low |

**Root cause**: Tool outputs (especially `read_file` results) persist in conversation
history at full length across all subsequent iterations. A 10,016-char deployment doc
read at iteration 3 is still occupying ~2,500 tokens at iteration 80 — even though
the LLM finished acting on it 77 iterations ago.

Current mitigation: `truncateToolResultContent` (P3 from earlier, `context.go:946`)
caps tool outputs at 500 chars when they enter history from a previous turn. But this
is a blunt instrument — it treats all tool outputs identically and loses semantic
signal.

---

## Design Philosophy: Context Window as Lego Blocks

Every piece of data that enters the LLM context window is a discrete **block** with
metadata. A **ContextManager** (rule engine or support LLM) assembles these blocks
according to retention policies before each turn.

### Block Schema

```go
type ContextBlock struct {
    ID          string            // unique: "file:/path/to/file.py" or "exec:iter-5-call-3"
    Kind        string            // "read_file", "exec", "web_fetch", "mcp", "user_msg", "assistant_msg"
    Content     string            // the actual text
    CharLen     int               // byte length
    TokenEst    int               // estimated tokens
    CreatedAt   time.Time         // when first added
    LastUsedAt  time.Time         // last iteration it was referenced
    Source      string            // which tool/iteration produced it
    FilePath    string            // for read_file blocks
    Hash        string            // content hash for dedup
    Relevance   float64           // 0.0–1.0, set by S1 or heuristic
    Retention   RetentionPolicy   // "keep_full", "summarize", "reference", "drop"
    Summary     string            // populated when retention=summarize
}
```

### Retention Policies

| Policy | Behavior | Applies To |
|---|---|---|
| `keep_full` | Content stays as-is | Current-turn tool results, recent user messages |
| `summarize` | Content replaced with LLM-generated summary | Old tool outputs, verbose exec results |
| `reference` | Content replaced with compact pointer + hash | Re-read files, duplicate outputs |
| `drop` | Block removed entirely | Transient errors, superseded content |

### Rules Engine (Fast Path)

Deterministic rules that run synchronously before every `BuildMessagesFromPrompt`:

```
RULE 1: CURRENT_TURN — blocks from current turn iteration = keep_full
RULE 2: FILE_DEDUP — same FilePath, same Hash → subsequent = reference
RULE 3: SIZE_CAP — Content > 2000 chars AND age > 1 turn → summarize
RULE 4: EXEC_ERROR — IsError=true AND age > 1 turn → drop (keep only most recent error)
RULE 5: STALE_REF — LastUsedAt < 5 turns ago → reference (LLM can re-fetch)
```

These rules are cheap (no API calls), run in microseconds, and handle 80% of cases.

### S1: Support LLM for Semantic Decisions (Slow Path)

For the remaining 20% — blocks where simple rules can't decide relevance — a
separate lightweight LLM (S1) makes semantic retention decisions.

**S1 Architecture:**

```
┌─────────────────────────────────────────────────┐
│                  Main Agent (A)                  │
│  Model: deepseek-v4-pro                         │
│  Task: Execute user requests                    │
│  Context: Assembled by ContextManager            │
└────────────────────┬────────────────────────────┘
                     │ context_assembly_request
                     ▼
┌─────────────────────────────────────────────────┐
│              ContextManager                      │
│  ┌──────────────┐    ┌──────────────────────┐   │
│  │ Rules Engine  │    │   S1 (Support LLM)   │   │
│  │ (sync, fast)  │    │   (async, semantic)  │   │
│  │               │    │   Model: flash/cheap  │   │
│  │ 80% decisions │    │   20% decisions       │   │
│  └──────────────┘    └──────────────────────┘   │
│                                                  │
│  Output: optimized []providers.Message           │
└─────────────────────────────────────────────────┘
```

**S1 Prompt (compact):**

```
You are a context window optimizer. Given a list of context blocks with metadata,
decide retention for each ambiguous block. Return JSON.

Rules:
- If the block content is still relevant to the current conversation, keep_full
- If it was useful but no longer critical, summarize (2-3 sentences)
- If it's a file that was read but not acted on, reference (pointer only)
- If it's obsolete or superseded, drop

Current user message: "{last_user_message}"
Last assistant response: "{last_assistant_response}"

Ambiguous blocks:
{JSON array of blocks with id, kind, content_preview, age, relevance}
```

**S1 runs:**
- Every N turns (configurable, default 5)
- When context window exceeds threshold (e.g., 70% of budget)
- Asynchronously — doesn't block the main agent's response

**S1 model choice:**
- `deepseek-v4-flash` (cheap, fast, $0.14/M input) vs A's `deepseek-v4-pro` ($0.28/M)
- S1 processes ~2-5K tokens per call → ~$0.001 per optimization run
- Runs every 5 turns → ~$0.02 per 100-turn session

---

## Implementation Plan

### Phase 1: Rules Engine (No S1)

1. **Define `ContextBlock` struct** in `pkg/agent/context_block.go`
2. **Add `ContextBlocks` to `ContextBuilder`** — accumulates blocks during turn execution
3. **Add block tracking in `pipeline_execute.go`** — when tool results are stored, also
   register them as blocks with metadata
4. **Implement Rules Engine** in `ContextManager.ApplyRetentionRules()`:
   - `ruleCurrentTurn`: keep_full for blocks from this turn
   - `ruleFileDedup`: replace re-read files with reference
   - `ruleSizeCap`: summarize blocks >2000 chars from past turns
   - `ruleExecError`: drop old errors
5. **Wire into `BuildMessagesFromPrompt`** — apply rules before building final message array
6. **Extend `truncateToolResultContent`** to use policies instead of fixed cap

### Phase 2: S1 Support LLM (Optional Enhancement)

1. **Add S1 model config** to agent configuration (model, api_base, api_key)
2. **Implement `S1Optimizer`** — calls S1 with ambiguous blocks, parses JSON response
3. **Add async optimization trigger** — goroutine that runs S1 periodically
4. **Add `Relevance` scoring** — S1 assigns relevance scores, stored on blocks
5. **Add `summarize` policy implementation** — store S1-generated summaries on blocks

### Phase 3: Observability

1. **Tracing events** for block retention decisions (what was kept/summarized/dropped)
2. **Metrics**: blocks_retained, blocks_summarized, blocks_dropped, s1_calls, s1_latency
3. **Dashboard**: context window composition over time

---

## Tradeoffs

| Approach | Latency | Cost | Accuracy | Complexity |
|---|---|---|---|---|
| **No management** (current) | 0ms | 0 | Low (waste tokens) | None |
| **Rules only** (Phase 1) | ~0.1ms | 0 | Medium (80% cases) | Low |
| **Rules + S1** (Phase 2) | ~500ms async | ~$0.001/run | High (95% cases) | Medium |

**Recommendation**: Start with Phase 1 (rules engine). Add S1 only if rules prove
insufficient for complex multi-file workflows where semantic relevance matters
(e.g., the LLM reads 5 files, acts on 2, the other 3 need intelligent summarization).

---

## Comparison: Rule-Based vs S1 for read_file

### Current behavior (turn-5, api_client.py read 3 times):
```
Iter 5:  read_file(api_client.py) → 4,673 chars in result
Iter 20: read_file(api_client.py) → 4,673 chars again (same hash)
Iter 40: read_file(api_client.py) → 4,673 chars again
Context at iter 80: 14,006 chars from just this one file × 3 reads
```

### With rules engine (Phase 1):
```
Iter 5:  read_file(api_client.py) → 4,673 chars [keep_full]
Iter 20: read_file(api_client.py) → [reference: hash=abc123, 4,673 chars, read at iter 5.
         Use read_file to retrieve if needed.]
Iter 40: same reference block
Context at iter 80: 4,673 chars (saved 9,333 chars)
```

### With S1 (Phase 2):
```
Iter 5:  read_file(api_client.py) → [keep_full: 4,673 chars]
S1 runs at iter 10:
  → summarizes: "api_client.py: HTTP client with retry logic, endpoints /submit,
     /healthz, /status. Config in config.py. 4,673 chars."
Context at iter 10+: ~200 chars summary
```

---

## Open Questions

1. **When does S1 run?** Per-turn is too expensive. Every N turns? On context threshold breach? Both?
2. **S1 failure mode:** If S1 call fails, fall back to rules engine.
3. **Block identity:** How to identify "same file read again"? Content hash + file path.
4. **Summary storage:** Where to store S1-generated summaries? On the block itself, in session store.
5. **Multi-agent coordination:** If S1 manages A's context, does S1 need its own context? Yes — S1 needs recent conversation state to judge relevance. Keep S1's window small (last 3 turns).
