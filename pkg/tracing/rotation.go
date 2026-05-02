package tracing

import (
	"context"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// StartPruner starts a background goroutine that periodically prunes expired records.
func (ts *TraceStore) StartPruner(ctx context.Context) {
	interval := time.Duration(ts.config.PruneIntervalMinutes) * time.Minute
	if interval < time.Minute {
		interval = time.Minute
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		logger.InfoCF("tracing", "Pruner started", map[string]any{
			"interval_minutes": ts.config.PruneIntervalMinutes,
		})

		for {
			select {
			case <-ctx.Done():
				logger.InfoCF("tracing", "Pruner stopped", nil)
				return
			case <-ticker.C:
				ts.pruneExpiredRecords()
				ts.enforceMaxSize()
			}
		}
	}()
}

// pruneExpiredRecords deletes records past their retention period for each table.
func (ts *TraceStore) pruneExpiredRecords() {
	type pruneSpec struct {
		query   string
		days    int
		table   string
	}

	specs := []pruneSpec{
		{sqlPruneEvents, ts.config.EventRetentionDays, "events"},
		{sqlPruneLLMCalls, ts.config.LLMCallRetentionDays, "llm_calls"},
		{sqlPruneContextSnapshots, ts.config.ContextRetentionDays, "context_snapshots"},
		{sqlPruneSessions, ts.config.SessionRetentionDays, "sessions"},
	}

	for _, spec := range specs {
		result, err := ts.db.Exec(spec.query, spec.days)
		if err != nil {
			logger.ErrorCF("tracing", "Failed to prune table", map[string]any{
				"table": spec.table,
				"error": err.Error(),
			})
			continue
		}
		affected, _ := result.RowsAffected()
		if affected > 0 {
			logger.InfoCF("tracing", "Pruned expired records", map[string]any{
				"table":  spec.table,
				"count":  affected,
				"days":   spec.days,
			})
		}
	}

	// Reclaim space after pruning
	if _, err := ts.db.Exec("PRAGMA incremental_vacuum"); err != nil {
		logger.WarnCF("tracing", "Incremental vacuum failed", map[string]any{"error": err.Error()})
	}
}

// enforceMaxSize aggressively prunes oldest records when the database exceeds the size limit.
func (ts *TraceStore) enforceMaxSize() {
	maxBytes := int64(ts.config.MaxDBSizeMB) * 1024 * 1024
	size, err := ts.DBSize()
	if err != nil {
		logger.ErrorCF("tracing", "Failed to query DB size", map[string]any{"error": err.Error()})
		return
	}

	ts.metrics.DBSizeBytes.Store(size)

	if size <= maxBytes {
		return
	}

	logger.WarnCF("tracing", "Database size exceeds limit, aggressively pruning", map[string]any{
		"size_mb":    size / (1024 * 1024),
		"limit_mb":   ts.config.MaxDBSizeMB,
	})

	// Prune oldest events first (most voluminous), then context snapshots
	pruneTargets := []struct {
		query string
		table string
		limit int
	}{
		{sqlPruneOldestEvents, "events", 1000},
		{sqlPruneOldestContextSnapshots, "context_snapshots", 500},
		{sqlPruneOldestLLMCalls, "llm_calls", 200},
	}

	for _, target := range pruneTargets {
		result, err := ts.db.Exec(target.query, target.limit)
		if err != nil {
			logger.ErrorCF("tracing", "Aggressive prune failed", map[string]any{
				"table": target.table,
				"error": err.Error(),
			})
			continue
		}
		affected, _ := result.RowsAffected()
		if affected > 0 {
			logger.InfoCF("tracing", "Aggressively pruned records", map[string]any{
				"table": target.table,
				"count": affected,
			})
		}
	}

	// Reclaim space
	if _, err := ts.db.Exec("PRAGMA incremental_vacuum"); err != nil {
		logger.WarnCF("tracing", "Incremental vacuum after aggressive prune failed", map[string]any{"error": err.Error()})
	}

	// Update metrics
	if newSize, err := ts.DBSize(); err == nil {
		ts.metrics.DBSizeBytes.Store(newSize)
	}
}
