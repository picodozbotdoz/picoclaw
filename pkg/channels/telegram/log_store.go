package telegram

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	
	_ "modernc.org/sqlite"
)

// TGLogStore records raw Telegram messages into SQLite for debugging.
type TGLogStore struct {
	db     *sql.DB
	enabled bool
	mu     sync.RWMutex
}

// NewTGLogStore opens (or creates) the SQLite database at dbPath.
func NewTGLogStore(dbPath string) (*TGLogStore, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath+"?_journal=WAL&_busy_timeout=2000")
	if err != nil {
		return nil, fmt.Errorf("open log db: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping log db: %w", err)
	}

	store := &TGLogStore{db: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}

	return store, nil
}

func (s *TGLogStore) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS tg_messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			direction TEXT NOT NULL CHECK(direction IN ('in','out')),
			chat_id TEXT NOT NULL,
			message_id TEXT NOT NULL,
			thread_id TEXT NOT NULL DEFAULT '',
			text_preview TEXT NOT NULL DEFAULT '',
			raw_json TEXT NOT NULL,
			session_key TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_tg_created ON tg_messages(created_at);
	`)
	return err
}

// SetEnabled toggles logging on/off.
func (s *TGLogStore) SetEnabled(v bool) {
	s.mu.Lock()
	s.enabled = v
	s.mu.Unlock()
}

// IsEnabled reports whether logging is active.
func (s *TGLogStore) IsEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabled
}

// LogIncoming records a raw inbound Telegram message.
func (s *TGLogStore) LogIncoming(chatID, messageID, threadID, textPreview string, rawJSON []byte, sessionKey string) {
	if !s.IsEnabled() {
		return
	}
	s.insert("in", chatID, messageID, threadID, textPreview, rawJSON, sessionKey)
}

// LogOutgoing records a raw outbound Telegram API response.
func (s *TGLogStore) LogOutgoing(chatID, messageID, threadID, textPreview string, rawJSON []byte) {
	if !s.IsEnabled() {
		return
	}
	s.insert("out", chatID, messageID, threadID, textPreview, rawJSON, "")
}

func (s *TGLogStore) insert(direction, chatID, messageID, threadID, textPreview string, rawJSON []byte, sessionKey string) {
	s.mu.RLock()
	db := s.db
	s.mu.RUnlock()
	if db == nil {
		return
	}
	_, _ = db.Exec(
		"INSERT INTO tg_messages (direction, chat_id, message_id, thread_id, text_preview, raw_json, session_key) VALUES (?,?,?,?,?,?,?)",
		direction, chatID, messageID, threadID, textPreview, string(rawJSON), sessionKey,
	)
}

// Close releases the database connection.
func (s *TGLogStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// buildRawJSON serializes v to compact JSON for storage.
func buildRawJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(fmt.Sprintf("{\"error\":%q}", err.Error()))
	}
	return b
}
