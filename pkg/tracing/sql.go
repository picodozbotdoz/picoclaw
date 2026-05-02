package tracing

// All SQL constants for the TraceStore schema, queries, and pruning.

const sqlSchema = `
-- Core event log: every EventBus event is persisted here
CREATE TABLE IF NOT EXISTS events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp   TEXT NOT NULL,
    event_kind  TEXT NOT NULL,
    agent_id    TEXT NOT NULL DEFAULT '',
    session_key TEXT NOT NULL DEFAULT '',
    turn_id     TEXT NOT NULL DEFAULT '',
    iteration   INTEGER NOT NULL DEFAULT 0,
    trace_path  TEXT NOT NULL DEFAULT '',
    payload     TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_events_session ON events(session_key, timestamp);
CREATE INDEX IF NOT EXISTS idx_events_kind ON events(event_kind, timestamp);
CREATE INDEX IF NOT EXISTS idx_events_agent ON events(agent_id, timestamp);

-- LLM call tracing: full request/response capture
CREATE TABLE IF NOT EXISTS llm_calls (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    trace_id        TEXT NOT NULL UNIQUE,
    session_key     TEXT NOT NULL DEFAULT '',
    agent_id        TEXT NOT NULL DEFAULT '',
    turn_id         TEXT NOT NULL DEFAULT '',
    iteration       INTEGER NOT NULL DEFAULT 0,
    provider        TEXT NOT NULL,
    model           TEXT NOT NULL,
    -- Request
    request_time    TEXT NOT NULL,
    messages_count  INTEGER NOT NULL DEFAULT 0,
    tools_count     INTEGER NOT NULL DEFAULT 0,
    max_tokens      INTEGER NOT NULL DEFAULT 0,
    temperature     REAL NOT NULL DEFAULT 0,
    thinking_mode   TEXT NOT NULL DEFAULT '',
    is_streaming    INTEGER NOT NULL DEFAULT 0,
    request_snippet TEXT NOT NULL DEFAULT '',
    -- Response
    response_time    TEXT NOT NULL DEFAULT '',
    latency_ms       INTEGER NOT NULL DEFAULT 0,
    status_code      INTEGER NOT NULL DEFAULT 0,
    content_len      INTEGER NOT NULL DEFAULT 0,
    tool_calls_count INTEGER NOT NULL DEFAULT 0,
    has_reasoning    INTEGER NOT NULL DEFAULT 0,
    response_snippet TEXT NOT NULL DEFAULT '',
    -- Token usage
    prompt_tokens       INTEGER NOT NULL DEFAULT 0,
    completion_tokens   INTEGER NOT NULL DEFAULT 0,
    total_tokens        INTEGER NOT NULL DEFAULT 0,
    cache_hit_tokens    INTEGER NOT NULL DEFAULT 0,
    cache_miss_tokens   INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens    INTEGER NOT NULL DEFAULT 0,
    -- Fallback chain
    is_fallback         INTEGER NOT NULL DEFAULT 0,
    fallback_attempt    INTEGER NOT NULL DEFAULT 0,
    fallback_reason     TEXT NOT NULL DEFAULT '',
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
    trigger             TEXT NOT NULL,
    timestamp           TEXT NOT NULL,
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
    -- Partition budget
    partition_config        TEXT NOT NULL DEFAULT '',
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
`

const sqlInsertEvent = `
INSERT INTO events (timestamp, event_kind, agent_id, session_key, turn_id, iteration, trace_path, payload)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
`

const sqlInsertLLMCall = `
INSERT INTO llm_calls (
    trace_id, session_key, agent_id, turn_id, iteration,
    provider, model,
    request_time, messages_count, tools_count, max_tokens, temperature,
    thinking_mode, is_streaming, request_snippet
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`

const sqlUpdateLLMCallResponse = `
UPDATE llm_calls SET
    response_time = ?, latency_ms = ?, status_code = ?,
    content_len = ?, tool_calls_count = ?, has_reasoning = ?,
    response_snippet = ?,
    prompt_tokens = ?, completion_tokens = ?, total_tokens = ?,
    cache_hit_tokens = ?, cache_miss_tokens = ?, reasoning_tokens = ?,
    is_fallback = ?, fallback_attempt = ?, fallback_reason = ?,
    error = ''
WHERE trace_id = ?
`

const sqlUpdateLLMCallError = `
UPDATE llm_calls SET
    response_time = ?, latency_ms = ?,
    error = ?
WHERE trace_id = ?
`

const sqlInsertContextSnapshot = `
INSERT INTO context_snapshots (
    session_key, agent_id, turn_id, iteration, trigger, timestamp,
    used_tokens, total_tokens, compress_at_tokens, used_percent,
    cache_hit_tokens, cache_miss_tokens,
    system_prompt_tokens, history_tokens, injected_context_tokens,
    tool_def_tokens, reasoning_tokens, output_tokens,
    partition_config
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`

const sqlUpsertSession = `
INSERT INTO sessions (
    session_key, agent_id, channel, model, context_window,
    total_input_tokens, total_output_tokens, total_cache_hits, total_cost_usd,
    first_event_at, last_event_at,
    llm_call_count, tool_call_count, turn_count, compress_count,
    budget_usd, budget_exceeded
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_key) DO UPDATE SET
    agent_id = excluded.agent_id,
    model = CASE WHEN excluded.model != '' THEN excluded.model ELSE sessions.model END,
    context_window = CASE WHEN excluded.context_window > 0 THEN excluded.context_window ELSE sessions.context_window END,
    total_input_tokens = sessions.total_input_tokens + excluded.total_input_tokens,
    total_output_tokens = sessions.total_output_tokens + excluded.total_output_tokens,
    total_cache_hits = sessions.total_cache_hits + excluded.total_cache_hits,
    total_cost_usd = sessions.total_cost_usd + excluded.total_cost_usd,
    last_event_at = excluded.last_event_at,
    llm_call_count = sessions.llm_call_count + excluded.llm_call_count,
    tool_call_count = sessions.tool_call_count + excluded.tool_call_count,
    turn_count = sessions.turn_count + excluded.turn_count,
    compress_count = sessions.compress_count + excluded.compress_count,
    budget_usd = CASE WHEN excluded.budget_usd > 0 THEN excluded.budget_usd ELSE sessions.budget_usd END,
    budget_exceeded = CASE WHEN excluded.budget_exceeded > 0 THEN excluded.budget_exceeded ELSE sessions.budget_exceeded END
`

const sqlUpdateSessionCost = `
UPDATE sessions SET
    total_cost_usd = ?,
    budget_usd = ?,
    budget_exceeded = ?
WHERE session_key = ?
`

// Pruning DELETE statements
const sqlPruneEvents = `DELETE FROM events WHERE created_at < datetime('now', ?||' days')`
const sqlPruneLLMCalls = `DELETE FROM llm_calls WHERE created_at < datetime('now', ?||' days')`
const sqlPruneContextSnapshots = `DELETE FROM context_snapshots WHERE created_at < datetime('now', ?||' days')`
const sqlPruneSessions = `DELETE FROM sessions WHERE created_at < datetime('now', ?||' days')`

// Aggressive pruning: delete oldest records to shrink DB
const sqlPruneOldestEvents = `DELETE FROM events WHERE rowid IN (SELECT rowid FROM events ORDER BY rowid ASC LIMIT ?)`
const sqlPruneOldestLLMCalls = `DELETE FROM llm_calls WHERE rowid IN (SELECT rowid FROM llm_calls ORDER BY rowid ASC LIMIT ?)`
const sqlPruneOldestContextSnapshots = `DELETE FROM context_snapshots WHERE rowid IN (SELECT rowid FROM context_snapshots ORDER BY rowid ASC LIMIT ?)`

// SELECT query statements
const sqlQueryEventsBySession = `
SELECT id, timestamp, event_kind, agent_id, session_key, turn_id, iteration, trace_path, payload, created_at
FROM events WHERE session_key = ? AND timestamp >= ? ORDER BY timestamp DESC LIMIT ?
`

const sqlQueryLLMCallsBySession = `
SELECT id, trace_id, session_key, agent_id, turn_id, iteration,
    provider, model,
    request_time, messages_count, tools_count, max_tokens, temperature,
    thinking_mode, is_streaming, request_snippet,
    response_time, latency_ms, status_code,
    content_len, tool_calls_count, has_reasoning, response_snippet,
    prompt_tokens, completion_tokens, total_tokens,
    cache_hit_tokens, cache_miss_tokens, reasoning_tokens,
    is_fallback, fallback_attempt, fallback_reason,
    error, created_at
FROM llm_calls WHERE session_key = ? AND request_time >= ? ORDER BY request_time DESC LIMIT ?
`

const sqlQuerySessionCost = `
SELECT session_key, agent_id, channel, model, context_window,
    total_input_tokens, total_output_tokens, total_cache_hits, total_cost_usd,
    first_event_at, last_event_at, llm_call_count, tool_call_count, turn_count,
    compress_count, budget_usd, budget_exceeded, created_at
FROM sessions WHERE session_key = ?
`

const sqlQuerySessions = `
SELECT session_key, agent_id, channel, model, context_window,
    total_input_tokens, total_output_tokens, total_cache_hits, total_cost_usd,
    first_event_at, last_event_at, llm_call_count, tool_call_count, turn_count,
    compress_count, budget_usd, budget_exceeded, created_at
FROM sessions WHERE last_event_at >= ? ORDER BY last_event_at DESC LIMIT ?
`

const sqlQueryContextTrend = `
SELECT id, session_key, agent_id, turn_id, iteration, trigger, timestamp,
    used_tokens, total_tokens, compress_at_tokens, used_percent,
    cache_hit_tokens, cache_miss_tokens,
    system_prompt_tokens, history_tokens, injected_context_tokens,
    tool_def_tokens, reasoning_tokens, output_tokens,
    partition_config, created_at
FROM context_snapshots WHERE session_key = ? AND timestamp >= ? ORDER BY timestamp ASC
`

const sqlQueryCacheHitRate = `
SELECT timestamp, cache_hit_tokens, cache_miss_tokens, total_tokens
FROM context_snapshots WHERE session_key = ? AND timestamp >= ? ORDER BY timestamp ASC
`

const sqlQueryCompressionHistory = `
SELECT timestamp, trigger, used_tokens, total_tokens, used_percent, cache_hit_tokens, cache_miss_tokens
FROM context_snapshots WHERE session_key = ? AND trigger = 'pre_compress' ORDER BY timestamp ASC
`
