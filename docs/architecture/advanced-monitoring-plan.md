# Advanced Monitoring & Observability Plan for PicoClaw

## Document Metadata

| Field | Value |
|-------|-------|
| Status | Draft |
| Branch | `wip/deepseekv4_advanced_monitoring` |
| Author | AI-assisted |
| Created | 2026-05-02 |
| Last Updated | 2026-05-02 |
| Depends On | `wip/deepseekv4_optimized_phase5` (Phases 1-5 of DeepSeek V4 optimization) |

---

## 1. Executive Summary

PicoClaw currently has a solid foundation for real-time observability through its zerolog-based structured logging and EventBus pub/sub system with 18 event kinds. However, all observability data is **ephemeral** — cost tracking, cooldown state, rate limiter state, and context usage metrics exist only in memory and are lost on restart. There is no persistence, no queryability, no time-series analysis, and no way to retroactively inspect what the agent did in a past session beyond reading raw log files. Provider-level HTTP request/response logging is missing entirely, and no distributed tracing exists.

This plan proposes a **layered observability architecture** that preserves the existing lightweight footprint while adding persistent, queryable observability through three tiers:

1. **Tier 1 — Structured Event Persistence** (SQLite): Capture all EventBus events to a local SQLite database for queryable, replayable session history
2. **Tier 2 — LLM Request/Response Tracing** (SQLite + enhanced logging): Full bidirectional capture of LLM API calls with timing, token counts, model routing, and fallback chain decisions
3. **Tier 3 — Context Window Telemetry** (SQLite + metrics API): Continuous tracking of context budget utilization, partition breakdowns, compression events, and cache hit rates with a `/metrics` HTTP endpoint

The persistence layer uses **SQLite** via `modernc.org/sqlite` (pure-Go, already a project dependency) with automatic rotation and pruning. No external infrastructure (Prometheus, Jaeger, Grafana) is required, though the design is compatible with future OpenTelemetry export.

---

## 2. Current State Assessment

### 2.1 What Works Well

| Component | Status | Detail |
|-----------|--------|--------|
| Structured logging | Strong | Zerolog with component-based fields, console + file, level control |
| EventBus | Strong | 18 event kinds, pub/sub, auto-logging, deep-cloned payloads, per-kind drop counters |
| Agent pipeline logging | Good | LLM request/response, tool execution, fallback chain, retry, compression all logged at DEBUG/INFO |
| Cost tracking | Good (ephemeral) | Per-model token/cost tracking, budget alerts, cache hit rate — but in-memory only |
| Context usage | Good (on-demand) | Heuristic + API-based estimation, partition breakdown — but not stored |
| Session history | Good | JSONL files, crash-safe append, per-session isolation |

### 2.2 Critical Gaps

| Gap | Impact | Severity |
|-----|--------|----------|
| **No event persistence** | All events are logged but evaporate; no way to query "what happened in session X 3 hours ago?" | Critical |
| **No LLM request/response capture** | Provider layer uses raw `log.Printf`; no structured logging of HTTP payloads, latency, headers, or status codes | Critical |
| **No context window history** | `computeContextUsage()` runs on-demand but results are never stored; no way to see context budget trends over time | High |
| **Cost data lost on restart** | `SessionCostTracker` is in-memory only; cumulative spend resets on every gateway restart | High |
| **No metrics endpoint** | Health server has no `/metrics`; no Prometheus-compatible scraping; no real-time dashboard data | High |
| **No distributed tracing** | OpenTelemetry is an indirect dependency but unused; `TracePath` is logical only, not propagated across service boundaries | Medium |
| **Raw `log.Printf` in providers** | 7+ files use Go's standard `log` instead of the project's structured logger; these bypass component tagging, level filtering, and file output | Medium |
| **No test verification of logging** | Zero tests verify that correct log messages, event payloads, or context usage values are emitted | Medium |

### 2.3 Current Architecture (Data Flow)

```
User Message → Agent Pipeline → LLM Call → Provider (HTTP) → LLM API
                    ↓                ↓              ↓
              EventBus.Emit()   logger.DebugCF()  log.Printf()  ← NOT structured
                    ↓                ↓              ↓
           Auto-log to zerolog   zerolog file    /dev/null (missed)
                    ↓
              In-memory only (ephemeral)
```

### 2.4 Target Architecture (Data Flow)

```
User Message → Agent Pipeline → LLM Call → Provider (HTTP) → LLM API
                    ↓                ↓              ↓
              EventBus.Emit()   logger.DebugCF()  logger.InfoCF()  ← structured
                    ↓                ↓              ↓
           ┌─────────────────────────────────────────────┐
           │          TraceStore (SQLite)                 │
           │  ┌──────────┐ ┌──────────┐ ┌─────────────┐ │
           │  │ events   │ │ llm_calls│ │ ctx_snapshots│ │
           │  └──────────┘ ┌──────────┐ └─────────────┘ │
           │                │ sessions │                  │
           │                └──────────┘                  │
           │  + Auto-rotation + Pruning + WAL mode       │
           └──────────────┬──────────────────────────────┘
                          ↓
              ┌────────────────────────┐
              │  /metrics HTTP endpoint │
              │  /api/traces/* REST API │
              └────────────────────────┘
```

---

## 3. Persistence Layer Design

### 3.1 Why SQLite

| Criterion | SQLite | BoltDB | BadgerDB | JSONL files | External (Postgres) |
|-----------|--------|--------|----------|-------------|-------------------|
| Already in project | Yes | No | No | Yes (but query-poor) | No |
| Pure-Go (no CGO) | Yes (`modernc.org/sqlite`) | Yes | Yes | Yes | N/A |
| Queryable (SQL) | Yes | No (key-value) | No (key-value) | No (linear scan) | Yes |
| Time-series aggregation | Yes | Manual | Manual | Manual | Yes |
| Concurrent reads | Yes (WAL mode) | Yes | Yes | Yes | Yes |
| Single-file deployment | Yes | Yes | Yes (directory) | Yes (per session) | No |
| Zero infrastructure | Yes | Yes | Yes | Yes | No |
| Proven in codebase | Yes (4 packages) | No | No | Yes | No |
| Crash safety | WAL + fsync | Yes | Yes | fsync per append | WAL |

**Recommendation**: SQLite via `modernc.org/sqlite` with WAL mode enabled. This is the natural choice given it is already a project dependency, proven in production (dashboard auth, seahorse, matrix, whatsapp), requires zero additional infrastructure, and provides full SQL queryability for time-series analysis of events, LLM calls, and context metrics.

### 3.2 TraceStore Package Design

**New package**: `pkg/tracing/`

```
pkg/tracing/
├── store.go              # TraceStore core: init, migrate, close
├── store_events.go       # Event recording from EventBus
├── store_llm.go          # LLM call recording
├── store_context.go      # Context snapshot recording
├── store_sessions.go     # Session lifecycle and cost tracking
├── queries.go            # Pre-built query functions
├── metrics.go            # In-memory metrics counters for /metrics endpoint
├── subscriber.go         # EventBus subscriber that auto-persists events
├── rotation.go           # Auto-rotation and pruning
├── provider_logger.go    # Structured logger adapter for provider layer
└── store_test.go         # Comprehensive tests
```

### 3.3 Database Schema

```sql
-- Core event log: every EventBus event is persisted here
CREATE TABLE IF NOT EXISTS events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp   TEXT NOT NULL,          -- ISO 8601 UTC
    event_kind  TEXT NOT NULL,          -- "llm_request", "llm_response", "tool_exec_end", etc.
    agent_id    TEXT NOT NULL DEFAULT '',
    session_key TEXT NOT NULL DEFAULT '',
    turn_id     TEXT NOT NULL DEFAULT '',
    iteration   INTEGER NOT NULL DEFAULT 0,
    trace_path  TEXT NOT NULL DEFAULT '',
    payload     TEXT NOT NULL,          -- JSON-encoded payload
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_events_session ON events(session_key, timestamp);
CREATE INDEX IF NOT EXISTS idx_events_kind ON events(event_kind, timestamp);
CREATE INDEX IF NOT EXISTS idx_events_agent ON events(agent_id, timestamp);

-- LLM call tracing: full request/response capture
CREATE TABLE IF NOT EXISTS llm_calls (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    trace_id        TEXT NOT NULL UNIQUE, -- correlation ID for the entire call chain
    session_key     TEXT NOT NULL DEFAULT '',
    agent_id        TEXT NOT NULL DEFAULT '',
    turn_id         TEXT NOT NULL DEFAULT '',
    iteration       INTEGER NOT NULL DEFAULT 0,
    provider        TEXT NOT NULL,        -- "deepseek", "openai", "anthropic"
    model           TEXT NOT NULL,        -- "deepseek-v4-flash", "gpt-4o", etc.
    -- Request
    request_time    TEXT NOT NULL,        -- ISO 8601 UTC
    messages_count  INTEGER NOT NULL DEFAULT 0,
    tools_count     INTEGER NOT NULL DEFAULT 0,
    max_tokens      INTEGER NOT NULL DEFAULT 0,
    temperature     REAL NOT NULL DEFAULT 0,
    thinking_mode   TEXT NOT NULL DEFAULT '',   -- "disabled", "high", "max"
    is_streaming    INTEGER NOT NULL DEFAULT 0, -- boolean
    request_snippet TEXT NOT NULL DEFAULT '',    -- first N chars of request messages JSON
    -- Response
    response_time   TEXT NOT NULL DEFAULT '',    -- ISO 8601 UTC
    latency_ms      INTEGER NOT NULL DEFAULT 0,  -- response_time - request_time
    status_code     INTEGER NOT NULL DEFAULT 0,  -- HTTP status code
    content_len     INTEGER NOT NULL DEFAULT 0,
    tool_calls_count INTEGER NOT NULL DEFAULT 0,
    has_reasoning   INTEGER NOT NULL DEFAULT 0,
    response_snippet TEXT NOT NULL DEFAULT '',   -- first N chars of response content
    -- Token usage
    prompt_tokens       INTEGER NOT NULL DEFAULT 0,
    completion_tokens   INTEGER NOT NULL DEFAULT 0,
    total_tokens        INTEGER NOT NULL DEFAULT 0,
    cache_hit_tokens    INTEGER NOT NULL DEFAULT 0,
    cache_miss_tokens   INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens    INTEGER NOT NULL DEFAULT 0,
    -- Fallback chain
    is_fallback         INTEGER NOT NULL DEFAULT 0, -- was this a fallback attempt?
    fallback_attempt    INTEGER NOT NULL DEFAULT 0, -- which attempt number (0=primary, 1=first fallback)
    fallback_reason     TEXT NOT NULL DEFAULT '',    -- "rate_limit", "timeout", "context_overflow"
    -- Error
    error           TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_llm_calls_session ON llm_calls(session_key, request_time);
CREATE INDEX IF NOT EXISTS idx_llm_calls_model ON llm_calls(model, request_time);
CREATE INDEX IF NOT EXISTS idx_llm_calls_trace ON llm_calls(trace_id);
CREATE INDEX IF NOT EXISTS idx_llm_calls_provider ON llm_calls(provider, request_time);

-- Context window snapshots: periodic + event-driven captures
CREATE TABLE IF NOT EXISTS context_snapshots (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    session_key         TEXT NOT NULL DEFAULT '',
    agent_id            TEXT NOT NULL DEFAULT '',
    turn_id             TEXT NOT NULL DEFAULT '',
    iteration           INTEGER NOT NULL DEFAULT 0,
    trigger             TEXT NOT NULL,         -- "post_llm", "pre_compress", "manual"
    timestamp           TEXT NOT NULL,         -- ISO 8601 UTC
    -- Budget
    used_tokens         INTEGER NOT NULL DEFAULT 0,
    total_tokens        INTEGER NOT NULL DEFAULT 0,
    compress_at_tokens  INTEGER NOT NULL DEFAULT 0,
    used_percent        INTEGER NOT NULL DEFAULT 0,
    -- DeepSeek V4 cache
    cache_hit_tokens    INTEGER NOT NULL DEFAULT 0,
    cache_miss_tokens   INTEGER NOT NULL DEFAULT 0,
    -- Token breakdown
    system_prompt_tokens    INTEGER NOT NULL DEFAULT 0,
    history_tokens          INTEGER NOT NULL DEFAULT 0,
    injected_context_tokens INTEGER NOT NULL DEFAULT 0,
    tool_def_tokens         INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens        INTEGER NOT NULL DEFAULT 0,
    output_tokens           INTEGER NOT NULL DEFAULT 0,
    -- Partition budget (when configured)
    partition_config        TEXT NOT NULL DEFAULT '', -- JSON of ContextPartition config
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_ctx_snap_session ON context_snapshots(session_key, timestamp);

-- Session lifecycle and cumulative cost
CREATE TABLE IF NOT EXISTS sessions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    session_key     TEXT NOT NULL UNIQUE,
    agent_id        TEXT NOT NULL DEFAULT '',
    channel         TEXT NOT NULL DEFAULT '',
    model           TEXT NOT NULL DEFAULT '',
    context_window  INTEGER NOT NULL DEFAULT 0,
    -- Cumulative cost
    total_input_tokens  INTEGER NOT NULL DEFAULT 0,
    total_output_tokens INTEGER NOT NULL DEFAULT 0,
    total_cache_hits    INTEGER NOT NULL DEFAULT 0,
    total_cost_usd      REAL NOT NULL DEFAULT 0,
    -- Lifecycle
    first_event_at  TEXT NOT NULL DEFAULT '',
    last_event_at   TEXT NOT NULL DEFAULT '',
    llm_call_count  INTEGER NOT NULL DEFAULT 0,
    tool_call_count INTEGER NOT NULL DEFAULT 0,
    turn_count      INTEGER NOT NULL DEFAULT 0,
    -- Compression stats
    compress_count  INTEGER NOT NULL DEFAULT 0,
    -- Budget
    budget_usd      REAL NOT NULL DEFAULT 0,
    budget_exceeded INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_sessions_channel ON sessions(channel, last_event_at);
```

### 3.4 Rotation and Pruning

The TraceStore implements automatic data lifecycle management:

| Policy | Default | Configurable |
|--------|---------|-------------|
| Max database size | 500 MB | `tracing.max_db_size_mb` |
| Event retention | 7 days | `tracing.event_retention_days` |
| LLM call retention | 30 days | `tracing.llm_call_retention_days` |
| Context snapshot retention | 7 days | `tracing.context_retention_days` |
| Session retention | 90 days | `tracing.session_retention_days` |
| Pruning interval | Every 1 hour | `tracing.prune_interval_minutes` |
| Request/response snippet size | 2048 chars | `tracing.snippet_max_chars` |

**Rotation mechanism**:
1. A background goroutine runs the pruning interval timer
2. On each tick, `DELETE FROM events WHERE created_at < datetime('now', ?retention)` for each table
3. After deletion, `PRAGMA incremental_vacuum` reclaims space (auto-vacuum mode)
4. If database size exceeds `max_db_size_mb`, aggressively prune oldest records until under limit
5. All pruning is logged at INFO level with counts

### 3.5 Configuration

```json
{
  "tracing": {
    "enabled": true,
    "db_path": "",
    "wal_mode": true,
    "max_db_size_mb": 500,
    "event_retention_days": 7,
    "llm_call_retention_days": 30,
    "context_retention_days": 7,
    "session_retention_days": 90,
    "prune_interval_minutes": 60,
    "snippet_max_chars": 2048,
    "capture_request_messages": false,
    "capture_response_content": false,
    "sanitize_secrets": true
  }
}
```

Key flags:
- `capture_request_messages`: When true, store the full messages JSON (not just a snippet). Off by default to save space.
- `capture_response_content`: When true, store the full LLM response content. Off by default.
- `sanitize_secrets`: Redact API keys, tokens, and other sensitive values from stored data (default: true).

---

## 4. Workstream Design

### Workstream M.1: TraceStore Core Infrastructure

**Goal**: Create the `pkg/tracing/` package with SQLite-backed storage, migration, and lifecycle management.

**Changes**:

1. **Create `pkg/tracing/store.go`**:
   - `TraceStore` struct wrapping `*sql.DB`
   - `NewTraceStore(config TraceConfig) (*TraceStore, error)` — opens SQLite, enables WAL mode, runs migrations
   - `Close() error` — checkpoint WAL, close DB
   - `Migrate() error` — creates all tables and indexes if not exist
   - `Pragma()` — sets `journal_mode=WAL`, `synchronous=NORMAL`, `busy_timeout=5000`, `auto_vacuum=INCREMENTAL`

2. **Create `pkg/tracing/rotation.go`**:
   - `StartPruner(ctx context.Context)` — background goroutine for periodic pruning
   - `pruneExpiredRecords()` — deletes records past retention per table
   - `enforceMaxSize()` — aggressive pruning when DB exceeds size limit
   - `DBSize() (int64, error)` — queries `page_count * page_size` from `PRAGMA`

3. **Create `pkg/tracing/metrics.go`**:
   - In-memory atomic counters for real-time `/metrics` exposure (avoid DB queries on every scrape)
   - `MetricsCounters` struct with fields: `TotalLLMCalls`, `TotalToolCalls`, `TotalTokensIn`, `TotalTokensOut`, `TotalCacheHits`, `TotalErrors`, `ActiveSessions`, `LLMCallsByProvider`, `LLMCallsByModel`
   - Updated on every write operation
   - `Snapshot() MetricsSnapshot` — thread-safe snapshot for `/metrics` endpoint

4. **Add tracing config to `pkg/config/gateway.go`**:
   - Add `Tracing TraceConfig` field to `GatewayConfig`
   - `TraceConfig` struct with all fields from Section 3.5
   - Default values applied when `Tracing.Enabled` is true but sub-fields are zero

5. **Initialize TraceStore in gateway startup**:
   - In `cmd/gateway/main.go` or `pkg/gateway/gateway.go`:
     - Create `TraceStore` after config load
     - Pass to `AgentInstance` or `AgentLoop` for event subscription
     - Register `TraceStore.Close()` in shutdown hook

**Acceptance criteria**:
- TraceStore opens, migrates, and closes cleanly
- WAL mode is active; concurrent reads don't block writes
- Pruner runs on schedule and removes expired records
- Database size is enforced with aggressive pruning
- All operations are logged at appropriate levels

---

### Workstream M.2: EventBus Subscriber — Event Persistence

**Goal**: Subscribe to the EventBus and persist every event to the `events` table.

**Changes**:

1. **Create `pkg/tracing/subscriber.go`**:
   - `EventSubscriber` struct wrapping `*TraceStore` and a subscription ID
   - `Start(al *AgentLoop, buffer int) error` — subscribes to EventBus with specified buffer
   - `Stop()` — unsubscribes from EventBus
   - `handleEvent(evt Event)` — converts Event to SQL INSERT, executes asynchronously

2. **Event-to-row mapping**:
   - Map `Event.Kind` → `event_kind` (use existing `EventKind.String()`)
   - Map `Event.Meta.AgentID` → `agent_id`
   - Map `Event.Meta.SessionKey` → `session_key`
   - Map `Event.Meta.TurnID` → `turn_id`
   - Map `Event.Meta.Iteration` → `iteration`
   - Map `Event.Meta.TracePath` → `trace_path`
   - JSON-marshal `Event.Payload` → `payload`

3. **Async write with backpressure**:
   - Events are written to a buffered channel (default: 256 entries)
   - A goroutine drains the channel and batches INSERTs (max 50 per transaction)
   - If the channel is full, events are counted in `Dropped` counter (never blocks the agent loop)
   - Batch commit interval: 100ms or when batch is full, whichever comes first

4. **Update `pkg/agent/agent_event.go`**:
   - After `eventBus.Emit(evt)`, if TraceStore is configured, no additional code needed here — the subscriber is independent
   - The subscriber is a clean separation: no changes to the EventBus or emission code

5. **Session upsert on events**:
   - On `EventKindTurnStart`: upsert `sessions` row with `first_event_at`, increment `turn_count`
   - On `EventKindLLMResponse`: update `sessions.total_input_tokens`, `total_output_tokens`, `total_cache_hits`, `total_cost_usd`, `llm_call_count`
   - On `EventKindToolExecEnd`: update `sessions.tool_call_count`
   - On `EventKindContextCompress`: update `sessions.compress_count`

**Acceptance criteria**:
- All 18 event kinds are persisted to `events` table
- No agent loop performance impact (async write with backpressure)
- Dropped events are counted and reported via metrics
- Session rows are kept up-to-date with cumulative statistics
- Subscriber starts/stops cleanly with gateway lifecycle

---

### Workstream M.3: LLM Request/Response Tracing

**Goal**: Capture the full lifecycle of every LLM API call with request parameters, response data, timing, token usage, and fallback chain decisions.

**Changes**:

1. **Create `pkg/tracing/store_llm.go`**:
   - `LLMCallStart(traceID, sessionKey, agentID, turnID string, iteration int, req LLMCallRequest) error` — records the request half
   - `LLMCallEnd(traceID string, resp LLMCallResponse) error` — updates with response half
   - `LLMCallError(traceID string, err error) error` — records error

   ```go
   type LLMCallRequest struct {
       Provider      string
       Model         string
       MessagesCount int
       ToolsCount    int
       MaxTokens     int
       Temperature   float64
       ThinkingMode  string  // "disabled", "high", "max"
       IsStreaming    bool
       RequestSnippet string // first N chars of messages JSON
   }

   type LLMCallResponse struct {
       LatencyMs        int64
       StatusCode       int
       ContentLen       int
       ToolCallsCount   int
       HasReasoning     bool
       ResponseSnippet  string
       PromptTokens     int
       CompletionTokens int
       TotalTokens      int
       CacheHitTokens   int
       CacheMissTokens  int
       ReasoningTokens  int
       IsFallback       bool
       FallbackAttempt  int
       FallbackReason   string
   }
   ```

2. **Instrument `pkg/agent/pipeline_llm.go`**:
   - Before each LLM call: generate a `traceID` (UUID), call `TraceStore.LLMCallStart()`
   - After each LLM call (success or failure): call `TraceStore.LLMCallEnd()` or `TraceStore.LLMCallError()`
   - For fallback chain: each candidate gets its own `traceID`; the primary call and all fallback attempts are linked by `turn_id` + `iteration`
   - For streaming: capture `traceID` at stream start, record final usage when stream completes

3. **Create `pkg/tracing/provider_logger.go`**:
   - `ProviderLogger` struct that wraps the project's `logger` package for provider-level logging
   - Replaces all raw `log.Printf` calls in provider packages with `logger.InfoCF("provider", ...)` calls
   - Adds structured logging for:
     - HTTP request start: method, URL, headers (sanitized), body size
     - HTTP response: status code, latency, body size, content type
     - Connection errors, timeouts, retries
   - `NewProviderLogger(providerName string) *ProviderLogger`

4. **Replace `log.Printf` in provider packages**:
   - `pkg/providers/openai_compat/provider.go`: Replace ~3 `log.Printf` calls with structured logger
   - `pkg/providers/anthropic/provider.go`: Replace ~1 `log.Printf` call
   - `pkg/providers/common/common.go`: Replace ~2 `log.Printf` calls
   - Other provider files as discovered

5. **Secret sanitization**:
   - Before persisting any request/response data, apply `SanitizeSecrets(data string) string`
   - Redacts patterns: `Bearer ...`, `sk-...`, API keys in headers, `Authorization` header values
   - Applied to `request_snippet` and `response_snippet` fields
   - Controlled by `tracing.sanitize_secrets` config (default: true)

**Acceptance criteria**:
- Every LLM call is recorded with full request/response lifecycle
- Latency is captured in milliseconds
- Fallback chain decisions are traced (primary vs. fallback, reason for fallback)
- Streaming calls are traced from start to final usage
- Provider-level logging uses structured logger (not raw `log.Printf`)
- Secrets are sanitized before persistence
- No performance impact on the LLM call path (async writes)

---

### Workstream M.4: Context Window Telemetry

**Goal**: Continuously track and persist context budget utilization, partition breakdowns, compression events, and cache statistics.

**Changes**:

1. **Create `pkg/tracing/store_context.go`**:
   - `RecordContextSnapshot(snap ContextSnapshot) error` — inserts a row into `context_snapshots`

   ```go
   type ContextSnapshot struct {
       SessionKey           string
       AgentID              string
       TurnID               string
       Iteration            int
       Trigger              string  // "post_llm", "pre_compress", "manual", "post_inject"
       Timestamp            time.Time
       UsedTokens           int
       TotalTokens          int
       CompressAtTokens     int
       UsedPercent          int
       CacheHitTokens       int
       CacheMissTokens      int
       SystemPromptTokens   int
       HistoryTokens        int
       InjectedContextTokens int
       ToolDefTokens        int
       ReasoningTokens      int
       OutputTokens         int
       PartitionConfig      string  // JSON
   }
   ```

2. **Instrument context usage computation**:
   - In `pkg/agent/context_usage.go`: After `computeContextUsage()`, if TraceStore is configured, call `RecordContextSnapshot()` with trigger `"post_llm"`
   - In `pkg/agent/pipeline_finalize.go`: Before compression, call `RecordContextSnapshot()` with trigger `"pre_compress"`
   - In `pkg/agent/context_budget.go`: After `isOverContextBudget()` returns true, record snapshot with trigger `"budget_exceeded"`
   - After context injection (from Phase 4 Workstream 4.1), record snapshot with trigger `"post_inject"`

3. **Persist cost tracking**:
   - On `EventKindTurnEnd`: persist the current `SessionCostTracker.GetSpend()` to the `sessions` table
   - This makes cost data survive restarts
   - On gateway startup: restore cost data from `sessions` table into `SessionCostTracker` for active sessions

4. **Add context trend query API**:
   - `GetContextTrend(sessionKey string, since time.Time) ([]ContextSnapshot, error)` — time-series for graphing
   - `GetCacheHitRateTrend(sessionKey string, since time.Time) ([]CacheHitRecord, error)` — cache efficiency over time
   - `GetCompressionHistory(sessionKey string) ([]CompressionEvent, error)` — when/why compressions occurred

**Acceptance criteria**:
- Context snapshots are recorded after every LLM call and before every compression
- Cost data persists across gateway restarts
- Cache hit rate trends are queryable
- Context partition breakdowns are tracked over time
- No significant performance impact (single INSERT per snapshot, async)

---

### Workstream M.5: Metrics HTTP Endpoint

**Goal**: Expose real-time metrics via a Prometheus-compatible `/metrics` endpoint and add a REST API for trace queries.

**Changes**:

1. **Add `/metrics` endpoint to health server** (`pkg/health/server.go`):
   - Mount `GET /metrics` on the health server's mux via `RegisterOnMux()`
   - Output format: Prometheus text exposition format
   - Metrics exposed:

   ```
   # LLM metrics
   picoclaw_llm_calls_total{provider, model} counter
   picoclaw_llm_call_duration_seconds{provider, model} histogram
   picoclaw_llm_tokens_input_total{provider, model} counter
   picoclaw_llm_tokens_output_total{provider, model} counter
   picoclaw_llm_tokens_cache_hit_total{provider, model} counter
   picoclaw_llm_errors_total{provider, model, status_code} counter
   picoclaw_llm_fallback_total{provider, reason} counter
   picoclaw_llm_streaming_calls_total{provider, model} counter

   # Context metrics
   picoclaw_context_used_percent{session_key} gauge
   picoclaw_context_used_tokens{session_key} gauge
   picoclaw_context_total_tokens{session_key} gauge
   picoclaw_context_cache_hit_rate{session_key} gauge
   picoclaw_context_compress_total{session_key, reason} counter

   # Tool metrics
   picoclaw_tool_calls_total{tool} counter
   picoclaw_tool_call_duration_seconds{tool} histogram
   picoclaw_tool_errors_total{tool} counter

   # Session metrics
   picoclaw_sessions_active gauge
   picoclaw_session_cost_usd{session_key} gauge
   picoclaw_session_budget_remaining_usd{session_key} gauge

   # EventBus metrics
   picoclaw_events_emitted_total{kind} counter
   picoclaw_events_dropped_total{kind} counter

   # System metrics
   picoclaw_tracing_db_size_bytes gauge
   picoclaw_tracing_events_pending gauge
   ```

2. **Add trace query REST API** (`pkg/tracing/api.go`):
   - Mount on the health server's mux or a dedicated API mux
   - `GET /api/traces/events?session=X&kind=Y&since=Z&limit=N` — query events
   - `GET /api/traces/llm-calls?session=X&model=Y&since=Z&limit=N` — query LLM calls
   - `GET /api/traces/context?session=X&since=Z` — query context snapshots
   - `GET /api/traces/sessions?channel=X&since=Z` — list sessions
   - `GET /api/traces/sessions/:key/cost` — session cost breakdown
   - All endpoints return JSON, support pagination, and are bearer-token protected (reuse health server auth)

3. **Metrics collection integration**:
   - `TraceStore.MetricsCounters` is updated on every write operation (atomic increments)
   - `/metrics` endpoint reads from `MetricsCounters.Snapshot()` — no DB queries
   - `/api/traces/*` endpoints query SQLite directly — suitable for dashboards and debugging

4. **Health check integration**:
   - Register a tracing health check: `health.RegisterCheck("tracing", func() (bool, string) { ... })`
   - Check: DB is accessible, pruner is running, pending events < threshold

**Acceptance criteria**:
- `/metrics` endpoint serves Prometheus-compatible metrics with zero DB queries
- `/api/traces/*` endpoints provide queryable access to all persisted data
- Metrics are accurate and update in real-time
- Health check reflects TraceStore status
- All endpoints are auth-protected

---

### Workstream M.6: Provider-Level HTTP Instrumentation

**Goal**: Add structured HTTP request/response logging and timing at the provider transport layer.

**Changes**:

1. **Create `pkg/providers/common/http_transport.go`**:
   - `TracingTransport` struct implementing `http.RoundTripper`
   - Wraps `http.DefaultTransport` with before/after hooks
   - Before: record request method, URL, content-length, timestamp
   - After: record response status, content-length, latency, error
   - Emits structured log via `logger.InfoCF("provider", "HTTP request", fields)` and `logger.InfoCF("provider", "HTTP response", fields)`
   - Optionally records to TraceStore if configured

   ```go
   type TracingTransport struct {
       inner    http.RoundTripper
       provider string
       store    *tracing.TraceStore  // nil if tracing disabled
   }
   ```

2. **Apply to all provider HTTP clients**:
   - `pkg/providers/openai_compat/provider.go`: Wrap client transport
   - `pkg/providers/anthropic/provider.go`: Wrap client transport
   - Other providers as needed
   - The transport is opt-in via config; when `tracing.enabled` is false, uses default transport

3. **Request/response body capture**:
   - For request: wrap `req.Body` in a `bytes.TeeReader` to capture first N bytes
   - For response: wrap `resp.Body` in a `bytes.TeeReader` to capture first N bytes
   - Captured bytes are stored as `request_snippet` / `response_snippet` in `llm_calls` table
   - Full body capture is available via config flag but off by default (memory/space concerns)

4. **Latency breakdown**:
   - Record `dns_lookup_ms`, `tls_handshake_ms`, `server_processing_ms`, `content_transfer_ms` if available from `httptrace`
   - Store in a `timing` JSON field in `llm_calls` table (or a separate `llm_call_timing` table)

**Acceptance criteria**:
- All LLM HTTP calls are logged with structured fields
- Latency is captured with sub-millisecond precision
- Request/response snippets are captured (configurable size)
- No performance impact when tracing is disabled (zero-overhead passthrough)
- Secret headers are sanitized in logs and persistence

---

## 5. Implementation Order and Dependencies

```
Workstream M.1: TraceStore Core Infrastructure ────── [no deps, foundational]
├── M.2: EventBus Subscriber (Event Persistence) ──── [depends on M.1]
├── M.3: LLM Request/Response Tracing ────────────── [depends on M.1]
│   └── M.6: Provider HTTP Instrumentation ────────── [depends on M.3]
├── M.4: Context Window Telemetry ─────────────────── [depends on M.1, M.2]
└── M.5: Metrics HTTP Endpoint ────────────────────── [depends on M.1, M.2, M.3, M.4]
```

**Recommended implementation order**:
1. M.1 (TraceStore Core) — 2-3 days
2. M.2 (Event Subscriber) — 1-2 days (can start once M.1 is ready)
3. M.3 (LLM Tracing) — 2-3 days (can start once M.1 is ready, parallel with M.2)
4. M.4 (Context Telemetry) — 1-2 days (after M.1+M.2)
5. M.6 (Provider HTTP Instrumentation) — 1-2 days (after M.3)
6. M.5 (Metrics Endpoint) — 2-3 days (after M.2+M.3+M.4)

**Total estimated effort**: 9-15 days

---

## 6. Persistence Layer Comparison — Deep Dive

### 6.1 SQLite (Recommended)

**Strengths**:
- Already a project dependency (`modernc.org/sqlite v1.48.2`, pure-Go, no CGO)
- Proven in 4 existing packages (dashboardauth, seahorse, matrix, whatsapp)
- Full SQL queryability — enables complex time-series analysis, aggregations, joins
- WAL mode provides concurrent reads without blocking writes
- Single-file deployment — easy backup, copy, and inspection
- Built-in full-text search via FTS5 extension
- `PRAGMA` allows tuning for write-heavy workloads
- Zero infrastructure — no separate process, no network, no config

**Weaknesses**:
- Single-writer constraint (writes are serialized) — mitigated by async batch writes
- Not suitable for distributed deployments (single-node only) — acceptable for PicoClaw's single-gateway architecture
- No built-in compression — mitigated by snippet truncation and retention pruning
- Schema migrations are manual — mitigated by simple versioned migration in `Migrate()`

**Mitigations for write-heavy workload**:
- Async batch writes (accumulate 50 INSERTs per transaction)
- WAL mode for concurrent reads during writes
- `PRAGMA synchronous=NORMAL` (safe with WAL, faster than FULL)
- `PRAGMA busy_timeout=5000` (wait up to 5s for lock)
- Background pruner keeps DB size bounded

### 6.2 Alternative: JSONL Append-Only Files

**Strengths**:
- Already used for session history (`pkg/memory/jsonl.go`)
- Simplest possible format — one JSON object per line
- Append-only — minimal write amplification
- Crash-safe with `f.Sync()` per append
- Easy to parse with any tool (jq, grep, Python)

**Weaknesses**:
- No queryability — must linearly scan entire file for any query
- No aggregation — can't do `SELECT model, AVG(latency_ms) FROM llm_calls GROUP BY model`
- No indexing — finding events for a specific session requires scanning all events
- No deletion — can't prune old data without rewriting the entire file
- Multiple files needed for different data types — complex management
- No concurrent access — file-level locking is coarse

**Verdict**: JSONL is already used for conversation history where append-only access is the primary pattern. It is not suitable for the queryable monitoring data store where time-range queries, aggregations, and filtering are essential.

### 6.3 Alternative: BoltDB / BBolt

**Strengths**:
- Pure Go, no CGO
- Key-value with buckets — simple data model
- Good for read-heavy workloads
- Single file, easy backup

**Weaknesses**:
- No query language — must write Go code for every query pattern
- No time-series support — range scans over timestamps are manual
- No aggregation — must load all records and aggregate in Go
- Not already in the project — adds a new dependency
- Read transactions block writes (and vice versa without careful management)

**Verdict**: BoltDB is too low-level for this use case. The monitoring store needs rich query capabilities (time-range filtering, model grouping, session correlation) that SQL provides naturally.

### 6.4 Alternative: Prometheus Remote Write

**Strengths**:
- Industry-standard time-series monitoring
- Rich query language (PromQL)
- Built-in retention and aggregation
- Grafana integration out of the box

**Weaknesses**:
- Requires external infrastructure (Prometheus server)
- Not suitable for high-cardinality labels (per-session metrics)
- No support for event/log storage — only numeric metrics
- No support for full request/response tracing data
- Overkill for single-node PicoClaw deployment

**Verdict**: Prometheus is excellent for numeric metrics but cannot store event payloads, LLM request/response data, or context snapshots. The `/metrics` endpoint we build (Workstream M.5) is Prometheus-compatible, so users who want Prometheus can scrape it. The primary store should be SQLite for self-contained operation.

### 6.5 Alternative: OpenTelemetry Traces + Jaeger

**Strengths**:
- Industry-standard distributed tracing
- Rich span/trace model
- Jaeger UI for visualization
- Export to multiple backends

**Weaknesses**:
- Requires external infrastructure (Jaeger/Tempo collector + storage)
- OTel Go SDK adds significant dependency weight
- Span model is not ideal for event-style data (events are more like structured logs than spans)
- Not self-contained — can't run without a collector

**Verdict**: OpenTelemetry is the right choice for distributed tracing in microservice architectures. PicoClaw is a single-process gateway; the overhead and complexity of an external collector is not justified. However, the TraceStore design is compatible with future OTel export — the `llm_calls` table already has `trace_id` and `span_id` columns that map to OTel trace/span IDs. A future workstream can add an OTLP exporter that reads from `llm_calls` and exports to a collector.

### 6.6 Summary Recommendation

| Use Case | Recommended Storage | Rationale |
|----------|-------------------|-----------|
| **Event persistence** | SQLite | Rich queries, time-range filtering, aggregation |
| **LLM call tracing** | SQLite | Structured data, correlation, latency analysis |
| **Context telemetry** | SQLite | Time-series snapshots, trend analysis |
| **Session cost tracking** | SQLite | Persistent across restarts, cumulative aggregation |
| **Conversation history** | JSONL (existing) | Append-only access pattern, proven |
| **Real-time metrics** | In-memory counters (existing pattern) | Zero-overhead, exposed via `/metrics` |
| **External monitoring** | Prometheus scrape (future) | `/metrics` is already Prometheus-compatible |

---

## 7. Data Volume Estimation

### 7.1 Per-Session Write Volume

| Event Type | Frequency | Row Size (est.) | Volume per Session (50 turns) |
|-----------|-----------|-----------------|-------------------------------|
| Events (all kinds) | ~5-10 per turn | ~500 bytes | 250-500 KB |
| LLM calls | ~2-3 per turn (with fallback) | ~800 bytes | 80-120 KB |
| Context snapshots | ~1 per turn | ~400 bytes | 20 KB |
| Session upserts | ~10 per session | ~500 bytes | 5 KB |
| **Total** | | | **~355-645 KB per session** |

### 7.2 Storage Growth

| Sessions/Day | Daily Growth | Monthly Growth | 30-Day Retention |
|-------------|-------------|----------------|-----------------|
| 10 | ~6 MB | ~180 MB | ~180 MB |
| 50 | ~30 MB | ~900 MB | ~900 MB |
| 100 | ~60 MB | ~1.8 GB | ~1.8 GB |
| 500 | ~300 MB | ~9 GB | ~9 GB |

### 7.3 Mitigation

- Default 500 MB max DB size is adequate for ~80 sessions at 30-day retention
- Aggressive pruning for events (7-day) keeps the most voluminous table small
- LLM call data (30-day) is the most valuable for debugging and cost analysis
- Snippet truncation (2048 chars) prevents large payloads from bloating the DB
- `capture_request_messages` and `capture_response_content` are off by default

---

## 8. Security Considerations

### 8.1 Secret Sanitization

All data persisted to TraceStore is sanitized to prevent credential leakage:

- `Authorization: Bearer sk-...` → `Authorization: Bearer [REDACTED]`
- `api-key: sk-...` → `api-key: [REDACTED]`
- OpenAI-style keys matching `sk-[a-zA-Z0-9]{20,}` are redacted
- Anthropic-style keys matching `sk-ant-[a-zA-Z0-9]{20,}` are redacted
- Telegram bot tokens are redacted (existing pattern from `maskSecrets()`)
- Custom regex patterns configurable via `tracing.sanitize_patterns`

### 8.2 Access Control

- `/metrics` endpoint is unauthenticated (standard Prometheus scraping pattern)
- `/api/traces/*` endpoints are bearer-token protected (reuse health server auth token)
- SQLite database file permissions: 0600 (owner read/write only)
- No data is sent to external services (unless user configures Prometheus scraping)

### 8.3 Data Retention

- Default retention periods are conservative (7 days for events, 30 days for LLM calls)
- Users can adjust retention or disable tracing entirely via config
- `PRAGMA secure_delete = ON` — zeroed-out deleted data (prevents forensic recovery)

---

## 9. Testing Strategy

### 9.1 Unit Tests

- `pkg/tracing/store_test.go`: Test all CRUD operations, migration, rotation, pruning
- `pkg/tracing/subscriber_test.go`: Test event persistence with mock EventBus
- `pkg/tracing/provider_logger_test.go`: Test structured logging replacement
- `pkg/tracing/api_test.go`: Test REST API endpoints with mock TraceStore

### 9.2 Integration Tests

- Full pipeline test: send a message through the agent, verify all events, LLM calls, and context snapshots are persisted correctly
- Cost persistence test: verify cost data survives a simulated restart (close + reopen TraceStore)
- Fallback chain test: verify all fallback attempts are traced with correct reasons

### 9.3 Performance Tests

- Benchmark: 1000 events/second sustained write rate with WAL mode
- Benchmark: `/metrics` endpoint response time under load
- Benchmark: Pruning performance with 100K rows per table
- Verify: No measurable latency impact on LLM call path when tracing is enabled
- Verify: Zero overhead when tracing is disabled (all code paths short-circuit on `store == nil`)

---

## 10. Future Extensions

These are explicitly **out of scope** for the initial implementation but designed to be compatible:

| Extension | Description | Compatibility |
|-----------|-------------|--------------|
| OpenTelemetry Export | Export `llm_calls` as OTLP spans to a collector | `trace_id`/`span_id` columns already in schema |
| Grafana Dashboard | Pre-built Grafana dashboard for PicoClaw metrics | `/metrics` is Prometheus-compatible |
| Web UI Trace Viewer | Interactive trace viewer in the PicoClaw web UI | `/api/traces/*` provides the data API |
| Distributed Tracing | Propagate `trace_id` across channels/tools | `trace_id` in all tables, W3C TraceContext format |
| Alert Rules | Configurable alerts on cost, latency, error rate | In-memory counters enable real-time checks |
| SQLite → Postgres Migration | For high-volume deployments needing separate DB | Schema is standard SQL, easily portable |
