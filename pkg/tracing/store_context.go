package tracing

import (
	"fmt"
	"time"
)

// ContextSnapshot represents a point-in-time capture of context window state.
type ContextSnapshot struct {
	SessionKey            string
	AgentID               string
	TurnID                string
	Iteration             int
	Trigger               string // "post_llm", "pre_compress", "manual", "post_inject"
	Timestamp             time.Time
	UsedTokens            int
	TotalTokens           int
	CompressAtTokens      int
	UsedPercent           int
	CacheHitTokens        int
	CacheMissTokens       int
	SystemPromptTokens    int
	HistoryTokens         int
	InjectedContextTokens int
	ToolDefTokens         int
	ReasoningTokens       int
	OutputTokens          int
	PartitionConfig       string // JSON
}

// RecordContextSnapshot inserts a context snapshot into the database.
func (ts *TraceStore) RecordContextSnapshot(snap ContextSnapshot) error {
	if ts == nil || ts.db == nil {
		return nil
	}

	tsStr := snap.Timestamp.UTC().Format(time.RFC3339Nano)
	if snap.Timestamp.IsZero() {
		tsStr = time.Now().UTC().Format(time.RFC3339Nano)
	}

	_, err := ts.db.Exec(sqlInsertContextSnapshot,
		snap.SessionKey, snap.AgentID, snap.TurnID, snap.Iteration,
		snap.Trigger, tsStr,
		snap.UsedTokens, snap.TotalTokens, snap.CompressAtTokens, snap.UsedPercent,
		snap.CacheHitTokens, snap.CacheMissTokens,
		snap.SystemPromptTokens, snap.HistoryTokens, snap.InjectedContextTokens,
		snap.ToolDefTokens, snap.ReasoningTokens, snap.OutputTokens,
		snap.PartitionConfig,
	)
	if err != nil {
		return fmt.Errorf("tracing: context snapshot insert failed: %w", err)
	}

	return nil
}

// ContextSnapshotRow represents a row from the context_snapshots table.
type ContextSnapshotRow struct {
	ID                    int64
	SessionKey            string
	AgentID               string
	TurnID                string
	Iteration             int
	Trigger               string
	Timestamp             string
	UsedTokens            int
	TotalTokens           int
	CompressAtTokens      int
	UsedPercent           int
	CacheHitTokens        int
	CacheMissTokens       int
	SystemPromptTokens    int
	HistoryTokens         int
	InjectedContextTokens int
	ToolDefTokens         int
	ReasoningTokens       int
	OutputTokens          int
	PartitionConfig       string
	CreatedAt             string
}

// CacheHitRecord represents a cache hit rate data point.
type CacheHitRecord struct {
	Timestamp       string
	CacheHitTokens  int
	CacheMissTokens int
	TotalTokens     int
}

// CompressionEvent represents a compression event from the history.
type CompressionEvent struct {
	Timestamp       string
	Trigger         string
	UsedTokens      int
	TotalTokens     int
	UsedPercent     int
	CacheHitTokens  int
	CacheMissTokens int
}

// GetContextTrend returns context snapshots for a session over time.
func (ts *TraceStore) GetContextTrend(sessionKey string, since time.Time) ([]ContextSnapshotRow, error) {
	if ts == nil || ts.db == nil {
		return nil, nil
	}

	sinceStr := since.UTC().Format(time.RFC3339Nano)
	rows, err := ts.db.Query(sqlQueryContextTrend, sessionKey, sinceStr)
	if err != nil {
		return nil, fmt.Errorf("tracing: context trend query failed: %w", err)
	}
	defer rows.Close()

	var result []ContextSnapshotRow
	for rows.Next() {
		var r ContextSnapshotRow
		if err := rows.Scan(
			&r.ID, &r.SessionKey, &r.AgentID, &r.TurnID, &r.Iteration,
			&r.Trigger, &r.Timestamp,
			&r.UsedTokens, &r.TotalTokens, &r.CompressAtTokens, &r.UsedPercent,
			&r.CacheHitTokens, &r.CacheMissTokens,
			&r.SystemPromptTokens, &r.HistoryTokens, &r.InjectedContextTokens,
			&r.ToolDefTokens, &r.ReasoningTokens, &r.OutputTokens,
			&r.PartitionConfig, &r.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("tracing: context trend scan failed: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// GetCacheHitRateTrend returns cache hit rate data points for a session.
func (ts *TraceStore) GetCacheHitRateTrend(sessionKey string, since time.Time) ([]CacheHitRecord, error) {
	if ts == nil || ts.db == nil {
		return nil, nil
	}

	sinceStr := since.UTC().Format(time.RFC3339Nano)
	rows, err := ts.db.Query(sqlQueryCacheHitRate, sessionKey, sinceStr)
	if err != nil {
		return nil, fmt.Errorf("tracing: cache hit rate query failed: %w", err)
	}
	defer rows.Close()

	var result []CacheHitRecord
	for rows.Next() {
		var r CacheHitRecord
		if err := rows.Scan(&r.Timestamp, &r.CacheHitTokens, &r.CacheMissTokens, &r.TotalTokens); err != nil {
			return nil, fmt.Errorf("tracing: cache hit rate scan failed: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// GetCompressionHistory returns compression events for a session.
func (ts *TraceStore) GetCompressionHistory(sessionKey string) ([]CompressionEvent, error) {
	if ts == nil || ts.db == nil {
		return nil, nil
	}

	rows, err := ts.db.Query(sqlQueryCompressionHistory, sessionKey)
	if err != nil {
		return nil, fmt.Errorf("tracing: compression history query failed: %w", err)
	}
	defer rows.Close()

	var result []CompressionEvent
	for rows.Next() {
		var r CompressionEvent
		if err := rows.Scan(&r.Timestamp, &r.Trigger, &r.UsedTokens, &r.TotalTokens, &r.UsedPercent, &r.CacheHitTokens, &r.CacheMissTokens); err != nil {
			return nil, fmt.Errorf("tracing: compression history scan failed: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
