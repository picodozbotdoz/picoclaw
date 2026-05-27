# Tracing Analysis — 2026-05-27

> **Database**: `/home/zkian/.picoclaw/tracing/tracing.db`
> **Period analyzed**: 2026-05-27T00:00:00Z onward (post-P1+P2 deploy at 2026-05-26T16:35Z)
> **Reference design docs**: `docs/CONTEXT_WINDOW_MGT_P3.md`, `docs/CONTEXT_WINDOW_MAP.md`

---

## 1. Executive Summary

| Metric | Pre P1+P2 (May 26 overall) | Post P1+P2 (May 27) | Change |
|---|---|---|---|
| Total LLM calls | 167 | 77 | — |
| Total exec calls | 151 | 73 | — |
| Exec error rate | 15.2% (23/151) | **8.2%** (6/73) | **↓ 46%** |
| Cache hit rate | 98.6% (turn-5) | 97.8% (turn-2) | Stable |
| Max messages | 121 | **264** | **↑ 118%** ⚠️ |
| Max prompt tokens | 49,186 | **96,996** | **↑ 97%** ⚠️ |

**P1 (batching) and P2 (error detection) are working** — error rate nearly halved and
commands show `&&` chaining. But a new problem emerged: **context window accumulation**
across session restarts has ballooned to 264 messages / 97K tokens.

---

## 2. Turn-by-Turn Analysis (May 27 only)

| Turn | Iterations | Prompt Tokens | Cache Hit | Errors | Latency |
|---|---|---|---|---|---|
| turn-2 | 50 | 3.77M | 97.8% | 6 | 401s |
| turn-4 | 8 | 423K | 86.9% | 0 | 52s |
| turn-6 | 19 | 1.09M | 94.7% | 0 | 161s |

**Turn-2 is the heavyweight** — 50 iterations across ~6 hours (06:05-07:26 UTC).
This is a long-running debugging session that resumed from yesterday's context.

---

## 3. P1+P2 Impact Assessment

### 3.1 P1: Exec Batching — Effective ✅

Commands now show `&&` chaining patterns that were absent before:

```
# Before P1 (separate calls)
iter 30: chmod 775 data/
iter 35: chown ximimi:fuzzpot log

# After P1 (batched)
sudo rm -rf __pycache__ && sudo systemctl daemon-reload && sudo systemctl start svc
sudo cat cursor.json && echo "---" && sudo journalctl ...
ls -la /var/log/svc.log && sudo -u ximimi python3 -c "..."
```

**Evidence from tracing**: 73 exec calls today vs 151 yesterday — 52% fewer calls
for similar deployment/debugging work. Batching is reducing tool-call roundtrips.

### 3.2 P2: Error Detection — Partially Effective ⚠️

Error rate dropped from 15.2% to 8.2%. But 6 errors still occurred:

| Error | Root Cause | Avoidable? |
|---|---|---|
| `grep -n "HealthHandler" scheduler.py` | grep returned nothing (no match) — treated as error | Yes — LLM should know grep returns 1 on no match |
| `sqlite3 ... << 'SQL'` with complex query | SQL syntax issue in heredoc | Yes — could use `-cmd` flag |
| `git clone ...` with token in URL | Auth failure (credential in URL leaked) | Yes — should use credential-free URL |
| `git remote -v; git log` on non-git dir | `cd` to wrong directory first | Yes — verify dir exists first |
| `python3 << 'PYEOF'` with truncated import | Truncated Python heredoc (likely token limit) | Yes — write_file + exec instead |
| `python3 << 'PYEOF'` — incomplete code | Same truncation issue | Yes |

**P2 not fully effective on**: heredoc truncation (token limit on tool args cuts Python code),
grep exit code confusion, directory-not-found. These are limitations of prompt-level
rules — they can't prevent all error classes.

### 3.3 Remaining Anti-Patterns

Despite P1+P2, these patterns persist:

**A. Same file read 10+ times**: `.cursor_state.json` is read in at least 8 separate
   exec calls. Each time it's `cat` or `stat` on the same file. The LLM doesn't
   cache the knowledge that "the file hasn't changed since last read."

**B. `sudo cat file.py` instead of `read_file`**: The LLM uses `sudo cat` to read
   Python source files (`scheduler.py`, `cursor.py`, `__main__.py`) instead of the
   native `read_file` tool. This pollutes exec call counts and bypasses the
   `read_file`-specific optimizations.

**C. Inline heredoc scripts**: Multi-line Python scripts are embedded directly in
   exec calls using `python3 << 'PYEOF'`. These get truncated at the tool arg limit,
   causing silent errors. The better pattern (already in P1 rules): `write_file` +
   `exec` the script.

**D. Service debug loop**: The stop→fix→start→sleep→check pattern still appears
   across 5+ iterations for sec-intel-exporter debugging.

---

## 4. Context Window Analysis

### 4.1 Growth Pattern (Turn-2, worst case)

| Iteration | Messages | Prompt Tokens | Cache Hit |
|---|---|---|---|
| 1 | 163 | 57,040 | **0%** ⚠️ |
| 10 | 183 | 63,260 | 98.9% |
| 20 | 203 | 69,922 | 99.8% |
| 30 | 223 | 77,452 | 99.8% |
| 40 | 243 | 86,052 | 99.3% |
| 50 | 264 | **96,996** | 95.2% |

**Critical finding**: Turn-2 *starts* at 163 messages — it carries over the entire
conversation history from yesterday's turn-5 and turn-7. This means:

- The LLM pays for 57K tokens of history on iteration 1 (cold cache = 0% hit)
- Each tool execution adds 2 messages (assistant tool_call + tool result)
- By iteration 50, 99K tokens are in the window (~39% of 256K budget)

### 4.2 What's in Those 264 Messages?

Approximate breakdown (from `request_snippet` sampling):

| Component | Messages | Tokens (est.) | % |
|---|---|---|---|
| System prompt (stable + volatile) | 2 | ~2,200 | 2.3% |
| Context summary | 2 | ~1,100 | 1.1% |
| Yesterday's turn-5/7 history | ~120 | ~50,000 | 51.5% |
| Today's turn-2 tool calls/results | ~100 | ~35,000 | 36.1% |
| Today's turn-4/6 activity | ~38 | ~8,700 | 9.0% |
| **Total** | **264** | **~97,000** | |

**51.5% of the context window is yesterday's conversation.** This is both good
(DeepSeek V4 can cache the prefix) and bad (massive cold-start penalty on first
iteration of each new turn).

### 4.3 Cache Hit Pattern by Turn

```
Turn-2, iter 1:  cache_hit = 0       (new user message breaks cache)
Turn-2, iter 2+: cache_hit = 97-99%  (prefix rebuild established)
Turn-4, iter 1:  cache_hit = 2,048   (only system prompt cached)
Turn-4, iter 2+: cache_hit = 98-99%
Turn-6, iter 1:  cache_hit = 2,048
Turn-6, iter 2+: cache_hit = 97-99%
```

The 0% cache hit on turn-2's first iteration (vs 2,048 for turns 4 and 6) is
notable. This means the ENTIRE prefix changed — likely because yesterday's
context summary was different from what existed in the session before.

### 4.4 Compression Status

**Zero compressions** across the entire session. `context_snapshots` table has 0
rows. The `CompressionStrategy` is "eager" (from `instance.go:341`) but the
thresholds may be set too high:

- `SummarizeMessageThreshold`: 20 messages (config `summarize_message_threshold`)
- `SummarizeTokenPercent`: 75% of context window (config `summarize_token_percent`)

At 264 messages and 97K tokens, we're well past both thresholds. The summarizer
should have triggered. Check `pipeline_finalize.go:90` for the trigger logic.

---

## 5. Tool Call Analysis

### 5.1 Today's Tool Distribution

| Tool | Calls | Errors | Error % | Avg Duration | Avg Output |
|---|---|---|---|---|---|
| exec | 73 | 6 | 8.2% | 2,597ms | 1,376 chars |
| read_file | 1 | 0 | 0% | 3ms | 5,561 chars |
| edit_file | 2 | 0 | 0% | 8ms | 73 chars |
| cron | 1 | 0 | 0% | 8ms | 34 chars |

**Only 1 read_file call today** (vs 15 yesterday). The LLM switched to using
`sudo cat` via exec instead of the native `read_file` tool. This is likely because:
1. The files are in `/opt/` (outside workspace) requiring sudo
2. `read_file` is workspace-restricted
3. The LLM learned that `sudo cat` works and `read_file` doesn't for these paths

**Proposal**: Add `/opt/fuzz-sec-intel-v8/` and `/opt/sec-intel-exporter/` to
`allow_read_paths` so `read_file` can access them natively. This would:
- Reduce exec calls by ~10-15 per session
- Enable `read_file`-specific optimizations (P3 file dedup, hash-based caching)
- Reduce context window bloat (read_file outputs are already tracked)

### 5.2 Re-read File Pattern

`.cursor_state.json` is read via exec at least **8 times** today:

```
iter 3:  sudo cat cursor_state.json
iter 7:  sudo cat cursor_state.json && sudo ls -la cursor_state.json
iter 14: sudo stat cursor_state.json && sudo stat data/
iter 16: sudo cat cursor_state.json && echo "---" && journalctl
iter 17: sleep 10 && sudo cat cursor_state.json && tail log
iter 22: sudo cat cursor_state.json && echo "---" && ps aux
iter 26: sleep 15 && sudo cat cursor_state.json && journalctl
iter 51: echo "=== Cursor State ===" && sudo cat cursor_state.json
```

Each read costs one exec call and adds ~100-200 chars of JSON to the context
window. With P3 block dedup, reads 2-8 would be replaced with `[reference: hash=abc,
read at iter 3, 200 chars. Use read_file to retrieve if needed.]` — saving
~1,200 chars and 7 exec calls.

### 5.3 Inline Script Truncation

The heredoc pattern `python3 << 'PYEOF'` fails when the code exceeds the tool argument
length limit. The P1 rules recommend `write_file + exec` but the LLM still uses
heredoc for quick tests. This is a tool design issue — the exec tool could
auto-detect heredoc and suggest write_file when truncation occurs.

---

## 6. Recommendations (Priority Order)

### 🔴 P0: Enable read_file for external paths

**Problem**: LLM uses `sudo cat` via exec to read files in `/opt/` because
`read_file` is workspace-restricted. This wastes exec calls and prevents file
dedup optimizations.

**Fix**: Add to zkian's config.json:
```json
"allow_read_paths": [
  "/opt/fuzz-sec-intel-v8/.*",
  "/opt/sec-intel-exporter/.*",
  "/var/log/sec-intel-exporter.log"
]
```

**Expected savings**: 10-15 fewer exec calls per session, ~2,000 fewer tokens in
context from deduplication.

### 🔴 P1: Trigger summarization earlier

**Problem**: 264 messages, 97K tokens, zero compressions. The eager strategy
isn't triggering.

**Fix**: Check compression thresholds and trigger logic. Consider lowering
`summarize_message_threshold` to 30 or adding a token-based trigger at 50K.

**Reference**: `context.go:912` (summary injection), `pipeline_finalize.go:90`
(compact trigger), `instance.go:332` (SummarizeMessageThreshold).

### 🟡 P2: Implement P3 File Dedup (Phase 1 — Rules Engine)

**Problem**: `.cursor_state.json` read 8+ times. Each re-read costs an exec
call and bloats the context.

**Fix**: Implement the rules engine from `docs/CONTEXT_WINDOW_MGT_P3.md`:
- Track file reads with hash in `ContextBlock`
- On re-read of same hash → replace with reference block
- Apply in `sanitizeHistoryForProvider` (where tool truncation already runs)

**Expected savings**: 5-7 fewer exec calls, ~1,200 chars saved from dedup.

### 🟡 P3: Fix heredoc truncation in exec tool

**Problem**: `python3 << 'PYEOF'` gets truncated at exec arg limit, causing
silent failures (2 of today's 6 errors).

**Fix**: In the exec tool, detect heredoc patterns and either:
a) Warn the LLM when output is truncated with "heredoc may be incomplete"
b) Auto-escape the content
c) Suggest write_file pattern in the error message

### 🟢 P4: Add grafana/prometheus for tracing visibility

**Problem**: We're analyzing tracing data via sqlite3 queries. A dashboard would
make patterns immediately visible (context growth, cache hit rates, tool errors).

**Fix**: The `/metrics` endpoint already exists at port 18881. Add a grafana
dashboard or a simple HTML page that queries the tracing DB.

---

## 7. Before/After Comparison

### Before P1+P2 (turn-5, May 26):
```
Tool calls: 151 exec, 15 read_file, 3 write_file, 3 edit_file
Exec errors: 23 (15.2%)
Pattern: one-command-per-exec, retry loops
Max context: 121 messages, 49K tokens
```

### After P1+P2 (turn-2, May 27):
```
Tool calls: 73 exec, 1 read_file, 2 edit_file, 1 cron
Exec errors: 6 (8.2%)
Pattern: && chaining visible, fewer retries
Max context: 264 messages, 97K tokens ← new problem: accumulation
```

### Net Assessment

P1+P2 are **working as designed**:
- Exec calls reduced 52% (151 → 73)
- Error rate reduced 46% (15.2% → 8.2%)
- Command batching visible in tracing

But they exposed a **new bottleneck**: context window accumulation across
session restarts. When the session carries over 163 messages from yesterday,
the first iteration of today's turn pays 57K tokens with 0% cache hit.
The fix for this is **P1 (earlier summarization)** and **P3 (file dedup)** from
the context window optimization roadmap.

---

## 8. Tracing Data Queries (Reproducible)

All findings above can be reproduced with these queries against the tracing DB:

```sql
-- Overall stats
SELECT tbl, COUNT(*) AS rows FROM (
  SELECT 'llm_calls' AS tbl FROM llm_calls
  UNION ALL SELECT 'events' FROM events
  UNION ALL SELECT 'sessions' FROM sessions
);

-- Tool distribution (today)
SELECT json_extract(payload, '$.Tool') AS tool, COUNT(*) AS calls,
  SUM(json_extract(payload, '$.IsError')) AS errors
FROM events WHERE event_kind = 'tool_exec_end'
  AND timestamp > '2026-05-27T00:00:00Z'
GROUP BY tool ORDER BY calls DESC;

-- Context window growth (turn-2)
SELECT iteration, messages_count, prompt_tokens, cache_hit_tokens,
  ROUND(100.0 * cache_hit_tokens / prompt_tokens, 1) AS hit_pct
FROM llm_calls WHERE turn_id = 'default-turn-2'
  AND request_time > '2026-05-27T00:00:00Z'
ORDER BY iteration;

-- Cache hit pattern per turn
SELECT turn_id, COUNT(*) AS iters,
  ROUND(100.0 * SUM(cache_hit_tokens) / SUM(prompt_tokens), 1) AS hit_pct
FROM llm_calls WHERE request_time > '2026-05-27T00:00:00Z'
GROUP BY turn_id;
```
