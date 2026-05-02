package agent

import (
        "strings"
        "testing"
        "time"

        "github.com/sipeed/picoclaw/pkg/tools"
)

// ── InjectedContextStore construction ────────────────────────────────────────

func TestNewInjectedContextStore(t *testing.T) {
        store := NewInjectedContextStore()
        if store == nil {
                t.Fatal("store should not be nil")
        }
        if len(store.items) != 0 {
                t.Error("new store should be empty")
        }
}

func TestInjectedContextStore_ImplementsInterface(t *testing.T) {
        var _ tools.InjectedContextStore = (*InjectedContextStore)(nil)
}

// ── Inject ──────────────────────────────────────────────────────────────────

func TestInjectedContextStore_Inject_Single(t *testing.T) {
        store := NewInjectedContextStore()
        store.Inject(InjectedContextItem{
                ID: "test.go", Content: "package main", TokenCount: 10, Source: "test",
        }, 100)
        items := store.List()
        if len(items) != 1 {
                t.Fatalf("expected 1 item, got %d", len(items))
        }
        if items[0].ID != "test.go" {
                t.Errorf("ID: got %q, want %q", items[0].ID, "test.go")
        }
}

func TestInjectedContextStore_Inject_Multiple(t *testing.T) {
        store := NewInjectedContextStore()
        store.Inject(InjectedContextItem{ID: "a.go", Content: "a", TokenCount: 10, Source: "test"}, 100)
        store.Inject(InjectedContextItem{ID: "b.go", Content: "b", TokenCount: 20, Source: "test"}, 100)
        items := store.List()
        if len(items) != 2 {
                t.Fatalf("expected 2 items, got %d", len(items))
        }
}

func TestInjectedContextStore_Inject_RespectsBudget(t *testing.T) {
        store := NewInjectedContextStore()
        // Budget is 50 tokens
        store.Inject(InjectedContextItem{ID: "a.go", Content: "a", TokenCount: 30, Source: "test"}, 50)
        store.Inject(InjectedContextItem{ID: "b.go", Content: "b", TokenCount: 30, Source: "test"}, 50)
        items := store.List()
        if len(items) != 1 {
                t.Errorf("second item should be skipped (over budget), got %d items", len(items))
        }
}

func TestInjectedContextStore_Inject_ZeroBudget(t *testing.T) {
        store := NewInjectedContextStore()
        // Zero budget means unlimited
        store.Inject(InjectedContextItem{ID: "a.go", Content: "a", TokenCount: 1000, Source: "test"}, 0)
        items := store.List()
        if len(items) != 1 {
                t.Errorf("zero budget should allow injection, got %d items", len(items))
        }
}

func TestInjectedContextStore_Inject_ExactBudget(t *testing.T) {
        store := NewInjectedContextStore()
        // Exact budget match
        store.Inject(InjectedContextItem{ID: "a.go", Content: "a", TokenCount: 50, Source: "test"}, 50)
        items := store.List()
        if len(items) != 1 {
                t.Errorf("exact budget should allow injection, got %d items", len(items))
        }
}

func TestInjectedContextStore_Inject_MaxItems(t *testing.T) {
        store := NewInjectedContextStore()
        // Fill up to max
        for i := 0; i < maxInjectedItems; i++ {
                store.Inject(InjectedContextItem{
                        ID: "file" + string(rune('a'+i%26)) + ".go",
                        Content: "x",
                        TokenCount: 1,
                        Source: "test",
                }, 0)
        }
        // Next one should be dropped
        store.Inject(InjectedContextItem{ID: "overflow.go", Content: "x", TokenCount: 1, Source: "test"}, 0)
        items := store.List()
        if len(items) != maxInjectedItems {
                t.Errorf("should be capped at %d, got %d", maxInjectedItems, len(items))
        }
}

// ── List ────────────────────────────────────────────────────────────────────

func TestInjectedContextStore_List_DefensiveCopy(t *testing.T) {
        store := NewInjectedContextStore()
        store.Inject(InjectedContextItem{ID: "a.go", Content: "a", TokenCount: 10, Source: "test"}, 100)
        items := store.List()
        items[0] = InjectedContextItem{ID: "modified"} // modify copy
        original := store.List()
        if original[0].ID == "modified" {
                t.Error("List should return a defensive copy")
        }
}

func TestInjectedContextStore_List_Empty(t *testing.T) {
        store := NewInjectedContextStore()
        items := store.List()
        if items == nil || len(items) != 0 {
                t.Error("empty store should return empty slice")
        }
}

// ── Clear ───────────────────────────────────────────────────────────────────

func TestInjectedContextStore_Clear_All(t *testing.T) {
        store := NewInjectedContextStore()
        store.Inject(InjectedContextItem{ID: "a.go", Content: "a", TokenCount: 10, Source: "test"}, 100)
        store.Inject(InjectedContextItem{ID: "b.go", Content: "b", TokenCount: 20, Source: "test"}, 100)
        removed := store.Clear("")
        if removed != 2 {
                t.Errorf("removed: got %d, want 2", removed)
        }
        if len(store.List()) != 0 {
                t.Error("store should be empty after clear")
        }
}

func TestInjectedContextStore_Clear_ByPattern(t *testing.T) {
        store := NewInjectedContextStore()
        store.Inject(InjectedContextItem{ID: "a.go", Content: "a", TokenCount: 10, Source: "test"}, 100)
        store.Inject(InjectedContextItem{ID: "b.py", Content: "b", TokenCount: 20, Source: "test"}, 100)
        removed := store.Clear("*.go")
        if removed != 1 {
                t.Errorf("removed: got %d, want 1", removed)
        }
        items := store.List()
        if len(items) != 1 || items[0].ID != "b.py" {
                t.Error("should only have b.py remaining")
        }
}

func TestInjectedContextStore_Clear_NoMatch(t *testing.T) {
        store := NewInjectedContextStore()
        store.Inject(InjectedContextItem{ID: "a.go", Content: "a", TokenCount: 10, Source: "test"}, 100)
        removed := store.Clear("*.py")
        if removed != 0 {
                t.Errorf("removed: got %d, want 0", removed)
        }
        if len(store.List()) != 1 {
                t.Error("item should remain when pattern doesn't match")
        }
}

func TestInjectedContextStore_Clear_EmptyStore(t *testing.T) {
        store := NewInjectedContextStore()
        removed := store.Clear("")
        if removed != 0 {
                t.Errorf("removing from empty store: got %d, want 0", removed)
        }
}

// ── TotalTokens ─────────────────────────────────────────────────────────────

func TestInjectedContextStore_TotalTokens_Empty(t *testing.T) {
        store := NewInjectedContextStore()
        if store.TotalTokens() != 0 {
                t.Error("empty store should have zero tokens")
        }
}

func TestInjectedContextStore_TotalTokens_WithItems(t *testing.T) {
        store := NewInjectedContextStore()
        store.Inject(InjectedContextItem{ID: "a.go", Content: "a", TokenCount: 10, Source: "test"}, 100)
        store.Inject(InjectedContextItem{ID: "b.go", Content: "b", TokenCount: 20, Source: "test"}, 100)
        if store.TotalTokens() != 30 {
                t.Errorf("total tokens: got %d, want 30", store.TotalTokens())
        }
}

func TestInjectedContextStore_TotalTokens_AfterClear(t *testing.T) {
        store := NewInjectedContextStore()
        store.Inject(InjectedContextItem{ID: "a.go", Content: "a", TokenCount: 10, Source: "test"}, 100)
        store.Clear("")
        if store.TotalTokens() != 0 {
                t.Error("tokens should be zero after clear")
        }
}

// ── Content ─────────────────────────────────────────────────────────────────

func TestInjectedContextStore_Content_Empty(t *testing.T) {
        store := NewInjectedContextStore()
        if store.Content() != "" {
                t.Error("empty store should return empty content")
        }
}

func TestInjectedContextStore_Content_WithItems(t *testing.T) {
        store := NewInjectedContextStore()
        store.Inject(InjectedContextItem{
                ID: "test.go", Content: "package main", TokenCount: 10, Source: "test", InjectedAt: time.Now(),
        }, 100)
        content := store.Content()
        if !strings.Contains(content, "--- test.go ---") {
                t.Error("content should have file header")
        }
        if !strings.Contains(content, "package main") {
                t.Error("content should contain item content")
        }
}

func TestInjectedContextStore_Content_MultipleItems(t *testing.T) {
        store := NewInjectedContextStore()
        store.Inject(InjectedContextItem{
                ID: "a.go", Content: "package a", TokenCount: 10, Source: "test", InjectedAt: time.Now(),
        }, 100)
        store.Inject(InjectedContextItem{
                ID: "b.go", Content: "package b", TokenCount: 10, Source: "test", InjectedAt: time.Now(),
        }, 100)
        content := store.Content()
        if !strings.Contains(content, "--- a.go ---") {
                t.Error("should have a.go header")
        }
        if !strings.Contains(content, "--- b.go ---") {
                t.Error("should have b.go header")
        }
}

// ── estimateTokens helper ───────────────────────────────────────────────────

func TestEstimateTokens(t *testing.T) {
        result := estimateTokens("hello world")
        // len("hello world") = 11, 11 * 2 / 5 = 4
        if result != 4 {
                t.Errorf("estimateTokens: got %d, want 4", result)
        }
}

// ── Concurrent access ───────────────────────────────────────────────────────

func TestInjectedContextStore_ConcurrentAccess(t *testing.T) {
        store := NewInjectedContextStore()
        done := make(chan bool)

        for i := 0; i < 10; i++ {
                go func(n int) {
                        store.Inject(InjectedContextItem{
                                ID:         "file" + string(rune('a'+n)) + ".go",
                                Content:    "content",
                                TokenCount: 10,
                                Source:     "test",
                        }, 0)
                        _ = store.List()
                        _ = store.TotalTokens()
                        _ = store.Content()
                        done <- true
                }(i)
        }

        for i := 0; i < 10; i++ {
                <-done
        }

        items := store.List()
        if len(items) != 10 {
                t.Errorf("concurrent inject: got %d items, want 10", len(items))
        }
}

func TestInjectedContextStore_ConcurrentInjectAndClear(t *testing.T) {
        store := NewInjectedContextStore()
        done := make(chan bool)

        for i := 0; i < 5; i++ {
                go func() {
                        store.Inject(InjectedContextItem{ID: "x.go", Content: "x", TokenCount: 1, Source: "test"}, 0)
                        done <- true
                }()
                go func() {
                        store.Clear("")
                        done <- true
                }()
        }

        for i := 0; i < 10; i++ {
                <-done
        }
        // No assertion on final count — just verify no deadlock/panic
}
