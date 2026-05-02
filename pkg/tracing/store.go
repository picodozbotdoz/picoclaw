package tracing

import (
        "database/sql"
        "fmt"
        "sync"
        "sync/atomic"
        "time"

        _ "modernc.org/sqlite"

        "github.com/sipeed/picoclaw/pkg/logger"
)

// TraceConfig holds the configuration for the TraceStore.
type TraceConfig struct {
        Enabled               bool
        DBPath                string
        WALMode               bool
        MaxDBSizeMB           int
        EventRetentionDays    int
        LLMCallRetentionDays  int
        ContextRetentionDays  int
        SessionRetentionDays  int
        PruneIntervalMinutes  int
        SnippetMaxChars       int
        CaptureRequestMsgs    bool
        CaptureResponseContent bool
        SanitizeSecrets       bool
}

// DefaultTraceConfig returns a TraceConfig with sensible defaults.
func DefaultTraceConfig() TraceConfig {
        return TraceConfig{
                Enabled:               false,
                DBPath:                "",
                WALMode:               true,
                MaxDBSizeMB:           500,
                EventRetentionDays:    7,
                LLMCallRetentionDays:  30,
                ContextRetentionDays:  7,
                SessionRetentionDays:  90,
                PruneIntervalMinutes:  60,
                SnippetMaxChars:       2048,
                CaptureRequestMsgs:    false,
                CaptureResponseContent: false,
                SanitizeSecrets:       true,
        }
}

// ApplyDefaults fills zero-valued fields with defaults.
func (c *TraceConfig) ApplyDefaults() {
        d := DefaultTraceConfig()
        if c.DBPath == "" {
                c.DBPath = d.DBPath
        }
        if c.MaxDBSizeMB == 0 {
                c.MaxDBSizeMB = d.MaxDBSizeMB
        }
        if c.EventRetentionDays == 0 {
                c.EventRetentionDays = d.EventRetentionDays
        }
        if c.LLMCallRetentionDays == 0 {
                c.LLMCallRetentionDays = d.LLMCallRetentionDays
        }
        if c.ContextRetentionDays == 0 {
                c.ContextRetentionDays = d.ContextRetentionDays
        }
        if c.SessionRetentionDays == 0 {
                c.SessionRetentionDays = d.SessionRetentionDays
        }
        if c.PruneIntervalMinutes == 0 {
                c.PruneIntervalMinutes = d.PruneIntervalMinutes
        }
        if c.SnippetMaxChars == 0 {
                c.SnippetMaxChars = d.SnippetMaxChars
        }
}

// MetricsCounters holds in-memory atomic counters for real-time /metrics exposure.
type MetricsCounters struct {
        TotalLLMCalls   atomic.Int64
        TotalToolCalls  atomic.Int64
        TotalTokensIn   atomic.Int64
        TotalTokensOut  atomic.Int64
        TotalCacheHits  atomic.Int64
        TotalErrors     atomic.Int64
        ActiveSessions  atomic.Int64
        DBSizeBytes     atomic.Int64
        EventsPending   atomic.Int64

        LLMCallsByProvider sync.Map // map[string]*atomic.Int64
        LLMCallsByModel    sync.Map // map[string]*atomic.Int64
        EventsEmitted      sync.Map // map[string]*atomic.Int64
        EventsDropped      sync.Map // map[string]*atomic.Int64
}

// MetricsSnapshot is a point-in-time copy of all counters for serialization.
type MetricsSnapshot struct {
        TotalLLMCalls       int64                         `json:"total_llm_calls"`
        TotalToolCalls      int64                         `json:"total_tool_calls"`
        TotalTokensIn       int64                         `json:"total_tokens_in"`
        TotalTokensOut      int64                         `json:"total_tokens_out"`
        TotalCacheHits      int64                         `json:"total_cache_hits"`
        TotalErrors         int64                         `json:"total_errors"`
        ActiveSessions      int64                         `json:"active_sessions"`
        DBSizeBytes         int64                         `json:"db_size_bytes"`
        EventsPending       int64                         `json:"events_pending"`
        LLMCallsByProvider  map[string]int64              `json:"llm_calls_by_provider"`
        LLMCallsByModel     map[string]int64              `json:"llm_calls_by_model"`
        EventsEmitted       map[string]int64              `json:"events_emitted"`
        EventsDropped       map[string]int64              `json:"events_dropped"`
}

// Snapshot returns a point-in-time copy of all metrics counters.
func (m *MetricsCounters) Snapshot() MetricsSnapshot {
        s := MetricsSnapshot{
                TotalLLMCalls:      m.TotalLLMCalls.Load(),
                TotalToolCalls:     m.TotalToolCalls.Load(),
                TotalTokensIn:      m.TotalTokensIn.Load(),
                TotalTokensOut:     m.TotalTokensOut.Load(),
                TotalCacheHits:     m.TotalCacheHits.Load(),
                TotalErrors:        m.TotalErrors.Load(),
                ActiveSessions:     m.ActiveSessions.Load(),
                DBSizeBytes:        m.DBSizeBytes.Load(),
                EventsPending:      m.EventsPending.Load(),
                LLMCallsByProvider: make(map[string]int64),
                LLMCallsByModel:    make(map[string]int64),
                EventsEmitted:      make(map[string]int64),
                EventsDropped:      make(map[string]int64),
        }
        m.LLMCallsByProvider.Range(func(key, val any) bool {
                if ctr, ok := val.(*atomic.Int64); ok {
                        s.LLMCallsByProvider[key.(string)] = ctr.Load()
                }
                return true
        })
        m.LLMCallsByModel.Range(func(key, val any) bool {
                if ctr, ok := val.(*atomic.Int64); ok {
                        s.LLMCallsByModel[key.(string)] = ctr.Load()
                }
                return true
        })
        m.EventsEmitted.Range(func(key, val any) bool {
                if ctr, ok := val.(*atomic.Int64); ok {
                        s.EventsEmitted[key.(string)] = ctr.Load()
                }
                return true
        })
        m.EventsDropped.Range(func(key, val any) bool {
                if ctr, ok := val.(*atomic.Int64); ok {
                        s.EventsDropped[key.(string)] = ctr.Load()
                }
                return true
        })
        return s
}

// IncrProvider increments the LLM call counter for a specific provider.
func (m *MetricsCounters) IncrProvider(provider string) {
        val, _ := m.LLMCallsByProvider.LoadOrStore(provider, &atomic.Int64{})
        if ctr, ok := val.(*atomic.Int64); ok {
                ctr.Add(1)
        }
}

// IncrModel increments the LLM call counter for a specific model.
func (m *MetricsCounters) IncrModel(model string) {
        val, _ := m.LLMCallsByModel.LoadOrStore(model, &atomic.Int64{})
        if ctr, ok := val.(*atomic.Int64); ok {
                ctr.Add(1)
        }
}

// IncrEventEmitted increments the emitted event counter for a specific kind.
func (m *MetricsCounters) IncrEventEmitted(kind string) {
        val, _ := m.EventsEmitted.LoadOrStore(kind, &atomic.Int64{})
        if ctr, ok := val.(*atomic.Int64); ok {
                ctr.Add(1)
        }
}

// IncrEventDropped increments the dropped event counter for a specific kind.
func (m *MetricsCounters) IncrEventDropped(kind string) {
        val, _ := m.EventsDropped.LoadOrStore(kind, &atomic.Int64{})
        if ctr, ok := val.(*atomic.Int64); ok {
                ctr.Add(1)
        }
}

// eventWrite represents a single event INSERT to be processed asynchronously.
type eventWrite struct {
        timestamp   string
        eventKind   string
        agentID     string
        sessionKey  string
        turnID      string
        iteration   int
        tracePath   string
        payload     string
}

// batchItem represents a single parameterized SQL execution to be processed asynchronously.
type batchItem struct {
        query string
        args  []any
}

const (
        defaultEventChSize = 256
        defaultBatchChSize = 256
        batchMaxItems      = 50
        batchFlushInterval = 100 * time.Millisecond
)

// TraceStore is the core SQLite-backed observability store.
type TraceStore struct {
        db      *sql.DB
        config  TraceConfig
        metrics MetricsCounters

        eventCh  chan eventWrite
        batchCh  chan batchItem
        closeCh  chan struct{}
        closeOnce sync.Once
        wg       sync.WaitGroup
}

// NewTraceStore creates a new TraceStore. Returns nil if config is not enabled.
func NewTraceStore(config TraceConfig) (*TraceStore, error) {
        if !config.Enabled {
                return nil, nil
        }

        config.ApplyDefaults()

        if config.DBPath == "" {
                config.DBPath = "tracing.db"
        }

        // Build DSN with WAL pragmas in URI
        dsn := config.DBPath
        if config.WALMode {
                dsn = fmt.Sprintf("file:%s?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000", config.DBPath)
        }

        db, err := sql.Open("sqlite", dsn)
        if err != nil {
                return nil, fmt.Errorf("tracing: failed to open database: %w", err)
        }

        // Single-writer constraint for SQLite
        db.SetMaxOpenConns(1)

        ts := &TraceStore{
                db:      db,
                config:  config,
                eventCh: make(chan eventWrite, defaultEventChSize),
                batchCh: make(chan batchItem, defaultBatchChSize),
                closeCh: make(chan struct{}),
        }

        if err := ts.applyPragmas(); err != nil {
                db.Close()
                return nil, fmt.Errorf("tracing: failed to apply pragmas: %w", err)
        }

        if err := ts.migrate(); err != nil {
                db.Close()
                return nil, fmt.Errorf("tracing: failed to migrate: %w", err)
        }

        // Start async write goroutines
        ts.wg.Add(2)
        go ts.eventWriterLoop()
        go ts.batchWriterLoop()

        logger.InfoCF("tracing", "TraceStore initialized", map[string]any{
                "db_path":  config.DBPath,
                "wal_mode": config.WALMode,
        })

        return ts, nil
}

// Close shuts down the TraceStore, flushing pending writes and closing the database.
func (ts *TraceStore) Close() error {
        var err error
        ts.closeOnce.Do(func() {
                close(ts.closeCh)
                ts.wg.Wait()
                if ts.db != nil {
                        err = ts.db.Close()
                }
                logger.InfoCF("tracing", "TraceStore closed", nil)
        })
        return err
}

// DB returns the underlying database connection.
func (ts *TraceStore) DB() *sql.DB {
        return ts.db
}

// Metrics returns a pointer to the metrics counters.
func (ts *TraceStore) Metrics() *MetricsCounters {
        return &ts.metrics
}

// Config returns the active configuration.
func (ts *TraceStore) Config() TraceConfig {
        return ts.config
}

func (ts *TraceStore) applyPragmas() error {
        pragmas := []string{
                "PRAGMA journal_mode=WAL",
                "PRAGMA synchronous=NORMAL",
                "PRAGMA busy_timeout=5000",
                "PRAGMA auto_vacuum=INCREMENTAL",
                "PRAGMA secure_delete=ON",
        }
        for _, p := range pragmas {
                if _, err := ts.db.Exec(p); err != nil {
                        return fmt.Errorf("tracing: pragma %q failed: %w", p, err)
                }
        }
        return nil
}

func (ts *TraceStore) migrate() error {
        if _, err := ts.db.Exec(sqlSchema); err != nil {
                return fmt.Errorf("tracing: migration failed: %w", err)
        }
        logger.DebugCF("tracing", "Database migration completed", nil)
        return nil
}

// eventWriterLoop reads events from the eventCh, batches them, and commits.
func (ts *TraceStore) eventWriterLoop() {
        defer ts.wg.Done()

        batch := make([]eventWrite, 0, batchMaxItems)
        timer := time.NewTimer(batchFlushInterval)
        defer timer.Stop()

        commit := func() {
                if len(batch) == 0 {
                        return
                }
                tx, err := ts.db.Begin()
                if err != nil {
                        logger.ErrorCF("tracing", "Failed to begin event transaction", map[string]any{"error": err.Error()})
                        batch = batch[:0]
                        return
                }
                stmt, err := tx.Prepare(sqlInsertEvent)
                if err != nil {
                        logger.ErrorCF("tracing", "Failed to prepare event insert", map[string]any{"error": err.Error()})
                        tx.Rollback()
                        batch = batch[:0]
                        return
                }
                defer stmt.Close()

                for _, ev := range batch {
                        if _, err := stmt.Exec(ev.timestamp, ev.eventKind, ev.agentID, ev.sessionKey, ev.turnID, ev.iteration, ev.tracePath, ev.payload); err != nil {
                                logger.ErrorCF("tracing", "Failed to insert event", map[string]any{"error": err.Error()})
                        }
                }
                if err := tx.Commit(); err != nil {
                        logger.ErrorCF("tracing", "Failed to commit event batch", map[string]any{"error": err.Error()})
                }
                ts.metrics.EventsPending.Add(-int64(len(batch)))
                batch = batch[:0]
        }

        for {
                select {
                case ev, ok := <-ts.eventCh:
                        if !ok {
                                commit()
                                return
                        }
                        batch = append(batch, ev)
                        if len(batch) >= batchMaxItems {
                                commit()
                                if !timer.Stop() {
                                        select {
                                        case <-timer.C:
                                        default:
                                        }
                                }
                                timer.Reset(batchFlushInterval)
                        }
                case <-timer.C:
                        commit()
                        timer.Reset(batchFlushInterval)
                case <-ts.closeCh:
                        // Drain remaining events
                        for {
                                select {
                                case ev := <-ts.eventCh:
                                        batch = append(batch, ev)
                                default:
                                        commit()
                                        return
                                }
                        }
                }
        }
}

// batchWriterLoop reads batch items from the batchCh, batches them, and commits.
func (ts *TraceStore) batchWriterLoop() {
        defer ts.wg.Done()

        batch := make([]batchItem, 0, batchMaxItems)
        timer := time.NewTimer(batchFlushInterval)
        defer timer.Stop()

        commit := func() {
                if len(batch) == 0 {
                        return
                }
                tx, err := ts.db.Begin()
                if err != nil {
                        logger.ErrorCF("tracing", "Failed to begin batch transaction", map[string]any{"error": err.Error()})
                        batch = batch[:0]
                        return
                }

                for _, item := range batch {
                        if _, err := tx.Exec(item.query, item.args...); err != nil {
                                logger.ErrorCF("tracing", "Failed to execute batch item", map[string]any{
                                        "error": err.Error(),
                                        "query": item.query,
                                })
                        }
                }
                if err := tx.Commit(); err != nil {
                        logger.ErrorCF("tracing", "Failed to commit batch", map[string]any{"error": err.Error()})
                }
                batch = batch[:0]
        }

        for {
                select {
                case item, ok := <-ts.batchCh:
                        if !ok {
                                commit()
                                return
                        }
                        batch = append(batch, item)
                        if len(batch) >= batchMaxItems {
                                commit()
                                if !timer.Stop() {
                                        select {
                                        case <-timer.C:
                                        default:
                                        }
                                }
                                timer.Reset(batchFlushInterval)
                        }
                case <-timer.C:
                        commit()
                        timer.Reset(batchFlushInterval)
                case <-ts.closeCh:
                        for {
                                select {
                                case item := <-ts.batchCh:
                                        batch = append(batch, item)
                                default:
                                        commit()
                                        return
                                }
                        }
                }
        }
}

// enqueueEvent adds an event to the async write queue (non-blocking).
func (ts *TraceStore) enqueueEvent(ev eventWrite) {
        ts.metrics.EventsPending.Add(1)
        select {
        case ts.eventCh <- ev:
                ts.metrics.IncrEventEmitted(ev.eventKind)
        default:
                ts.metrics.EventsPending.Add(-1)
                ts.metrics.IncrEventDropped(ev.eventKind)
                logger.WarnCF("tracing", "Event channel full, dropping event", map[string]any{
                        "event_kind": ev.eventKind,
                })
        }
}

// enqueueBatch adds a batch item to the async write queue (non-blocking).
func (ts *TraceStore) enqueueBatch(item batchItem) {
        select {
        case ts.batchCh <- item:
        default:
                logger.WarnCF("tracing", "Batch channel full, dropping item", map[string]any{
                        "query": item.query,
                })
        }
}

// DBSize returns the current database size in bytes.
func (ts *TraceStore) DBSize() (int64, error) {
        var pageCount, pageSize int64
        if err := ts.db.QueryRow("PRAGMA page_count").Scan(&pageCount); err != nil {
                return 0, fmt.Errorf("tracing: failed to query page_count: %w", err)
        }
        if err := ts.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
                return 0, fmt.Errorf("tracing: failed to query page_size: %w", err)
        }
        return pageCount * pageSize, nil
}
