package agent

import (
        "fmt"
        "path/filepath"
        "strings"
        "sync"

        "github.com/sipeed/picoclaw/pkg/logger"
        "github.com/sipeed/picoclaw/pkg/tools"
)

// InjectedContextItem mirrors tools.InjectedContextItem for the agent package.
// The store implements the tools.InjectedContextStore interface.
type InjectedContextItem = tools.InjectedContextItem

const maxInjectedItems = 100

// InjectedContextStore is a thread-safe store for injected context items.
// It implements the tools.InjectedContextStore interface.
type InjectedContextStore struct {
        mu    sync.RWMutex
        items []InjectedContextItem
}

// Verify InjectedContextStore implements tools.InjectedContextStore at compile time.
var _ tools.InjectedContextStore = (*InjectedContextStore)(nil)

// NewInjectedContextStore creates a new empty InjectedContextStore.
func NewInjectedContextStore() *InjectedContextStore {
        return &InjectedContextStore{
                items: make([]InjectedContextItem, 0),
        }
}

// Inject adds a context item to the store, respecting the total budget.
// The budget is the maximum total tokens allowed (not remaining tokens).
// If adding the item would exceed the budget or the max item count, it is skipped.
func (s *InjectedContextStore) Inject(item InjectedContextItem, budgetTokens int) {
        s.mu.Lock()
        defer s.mu.Unlock()

        if budgetTokens > 0 && s.totalTokensLocked()+item.TokenCount > budgetTokens {
                logger.DebugCF("context", "Skipping context injection (over budget)",
                        map[string]any{"id": item.ID, "tokens": item.TokenCount, "budget": budgetTokens, "current": s.totalTokensLocked()})
                return
        }

        if len(s.items) >= maxInjectedItems {
                logger.WarnCF("context", "Max injected items reached, skipping",
                        map[string]any{"id": item.ID, "max": maxInjectedItems})
                return
        }

        s.items = append(s.items, item)
        logger.DebugCF("context", "Injected context item",
                map[string]any{"id": item.ID, "tokens": item.TokenCount, "source": item.Source})
}

// List returns a defensive copy of the current items.
func (s *InjectedContextStore) List() []InjectedContextItem {
        s.mu.RLock()
        defer s.mu.RUnlock()

        result := make([]InjectedContextItem, len(s.items))
        copy(result, s.items)
        return result
}

// Clear removes items matching a glob pattern on their ID.
// If pattern is empty, all items are removed.
// Returns the number of items removed.
func (s *InjectedContextStore) Clear(pattern string) int {
        s.mu.Lock()
        defer s.mu.Unlock()

        if pattern == "" {
                count := len(s.items)
                s.items = s.items[:0]
                logger.InfoCF("context", "Cleared all context items",
                        map[string]any{"count": count})
                return count
        }

        var remaining []InjectedContextItem
        removed := 0
        for _, item := range s.items {
                matched, _ := filepath.Match(pattern, item.ID)
                if matched {
                        removed++
                } else {
                        remaining = append(remaining, item)
                }
        }

        s.items = remaining
        logger.InfoCF("context", "Cleared context items by pattern",
                map[string]any{"pattern": pattern, "removed": removed})
        return removed
}

// TotalTokens returns the sum of all item token counts.
func (s *InjectedContextStore) TotalTokens() int {
        s.mu.RLock()
        defer s.mu.RUnlock()
        return s.totalTokensLocked()
}

// Content returns a formatted string with all items, using "--- {id} ---" headers.
func (s *InjectedContextStore) Content() string {
        s.mu.RLock()
        defer s.mu.RUnlock()

        if len(s.items) == 0 {
                return ""
        }

        var sb strings.Builder
        for _, item := range s.items {
                sb.WriteString(fmt.Sprintf("--- %s ---\n", item.ID))
                sb.WriteString(item.Content)
                if !strings.HasSuffix(item.Content, "\n") {
                        sb.WriteString("\n")
                }
        }
        return sb.String()
}

// totalTokensLocked computes total tokens without acquiring the lock.
// Caller must hold s.mu (or at least a read lock).
func (s *InjectedContextStore) totalTokensLocked() int {
        total := 0
        for _, item := range s.items {
                total += item.TokenCount
        }
        return total
}

// estimateTokens estimates token count using chars * 2 / 5 heuristic.
func estimateTokens(s string) int {
        return len(s) * 2 / 5
}
