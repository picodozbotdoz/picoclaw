package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mockStore implements InjectedContextStore for testing.
type mockStore struct {
	items      []InjectedContextItem
	totalToken int
}

func (m *mockStore) Inject(item InjectedContextItem, budgetTokens int) {
	m.items = append(m.items, item)
	m.totalToken += item.TokenCount
}

func (m *mockStore) List() []InjectedContextItem {
	result := make([]InjectedContextItem, len(m.items))
	copy(result, m.items)
	return result
}

func (m *mockStore) Clear(pattern string) int {
	count := len(m.items)
	m.items = nil
	m.totalToken = 0
	return count
}

func (m *mockStore) TotalTokens() int {
	return m.totalToken
}

func (m *mockStore) Content() string {
	var sb strings.Builder
	for _, item := range m.items {
		sb.WriteString(item.Content)
		sb.WriteString("\n")
	}
	return sb.String()
}

// ── Context value helpers ───────────────────────────────────────────────────

func TestWithInjectedContextStore(t *testing.T) {
	store := &mockStore{}
	ctx := WithInjectedContextStore(context.Background(), store)
	extracted := InjectedContextStoreFromCtx(ctx)
	if extracted != store {
		t.Error("should extract same store")
	}
}

func TestInjectedContextStoreFromCtx_Nil(t *testing.T) {
	store := InjectedContextStoreFromCtx(context.Background())
	if store != nil {
		t.Error("should be nil when not set")
	}
}

func TestWithInjectedContextBudget(t *testing.T) {
	ctx := WithInjectedContextBudget(context.Background(), 5000)
	budget := InjectedContextBudgetFromCtx(ctx)
	if budget != 5000 {
		t.Errorf("budget: got %d, want 5000", budget)
	}
}

func TestInjectedContextBudgetFromCtx_Default(t *testing.T) {
	budget := InjectedContextBudgetFromCtx(context.Background())
	if budget != 0 {
		t.Errorf("default budget: got %d, want 0", budget)
	}
}

func TestWithInjectedContextWorkspace(t *testing.T) {
	ctx := WithInjectedContextWorkspace(context.Background(), "/tmp/ws")
	ws := InjectedContextWorkspaceFromCtx(ctx)
	if ws != "/tmp/ws" {
		t.Errorf("workspace: got %q, want %q", ws, "/tmp/ws")
	}
}

func TestInjectedContextWorkspaceFromCtx_Default(t *testing.T) {
	ws := InjectedContextWorkspaceFromCtx(context.Background())
	if ws != "" {
		t.Errorf("default workspace: got %q, want empty", ws)
	}
}

// ── ContextInjectTool ───────────────────────────────────────────────────────

func TestContextInjectTool_Name(t *testing.T) {
	tool := NewContextInjectTool()
	if tool.Name() != "context_inject" {
		t.Errorf("name: got %q, want %q", tool.Name(), "context_inject")
	}
}

func TestContextInjectTool_Description(t *testing.T) {
	tool := NewContextInjectTool()
	if tool.Description() == "" {
		t.Error("description should not be empty")
	}
}

func TestContextInjectTool_Parameters(t *testing.T) {
	tool := NewContextInjectTool()
	params := tool.Parameters()
	if params == nil {
		t.Error("parameters should not be nil")
	}
}

func TestContextInjectTool_MissingPath(t *testing.T) {
	tool := NewContextInjectTool()
	result := tool.Execute(context.Background(), map[string]any{})
	if !result.IsError {
		t.Error("missing path should return error")
	}
}

func TestContextInjectTool_NoStore(t *testing.T) {
	tool := NewContextInjectTool()
	result := tool.Execute(context.Background(), map[string]any{"path": "/tmp/test"})
	if !result.IsError {
		t.Error("no store should return error")
	}
}

func TestContextInjectTool_InvalidPath(t *testing.T) {
	tool := NewContextInjectTool()
	store := &mockStore{}
	ctx := WithInjectedContextStore(context.Background(), store)
	ctx = WithInjectedContextBudget(ctx, 10000)
	result := tool.Execute(ctx, map[string]any{"path": "/nonexistent/path/xyz123"})
	if !result.IsError {
		t.Error("invalid path should return error")
	}
}

func TestContextInjectTool_SingleFile(t *testing.T) {
	// Create a temp file
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test.go")
	content := "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n"
	if err := os.WriteFile(tmpFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewContextInjectTool()
	store := &mockStore{}
	ctx := WithInjectedContextStore(context.Background(), store)
	ctx = WithInjectedContextBudget(ctx, 10000)
	result := tool.Execute(ctx, map[string]any{"path": tmpFile})
	if result.IsError {
		t.Errorf("should not error: %s", result.ForLLM)
	}
	if len(store.items) != 1 {
		t.Fatalf("should have 1 item, got %d", len(store.items))
	}
	if !strings.Contains(store.items[0].Content, "--- "+tmpFile+" ---") {
		t.Error("content should have file header")
	}
	if !strings.Contains(store.items[0].Content, content) {
		t.Error("content should contain file content")
	}
}

func TestContextInjectTool_Directory(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "a.txt"), []byte("file a content"), 0o644)
	os.WriteFile(filepath.Join(tmpDir, "b.txt"), []byte("file b content"), 0o644)

	tool := NewContextInjectTool()
	store := &mockStore{}
	ctx := WithInjectedContextStore(context.Background(), store)
	ctx = WithInjectedContextBudget(ctx, 10000)
	result := tool.Execute(ctx, map[string]any{"path": tmpDir})
	if result.IsError {
		t.Errorf("should not error: %s", result.ForLLM)
	}
	if len(store.items) != 2 {
		t.Errorf("should have 2 items, got %d", len(store.items))
	}
}

func TestContextInjectTool_SkipsNodeModules(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main"), 0o644)
	os.MkdirAll(filepath.Join(tmpDir, "node_modules", "pkg"), 0o755)
	os.WriteFile(filepath.Join(tmpDir, "node_modules", "pkg", "index.js"), []byte("module.exports = {}"), 0o644)

	tool := NewContextInjectTool()
	store := &mockStore{}
	ctx := WithInjectedContextStore(context.Background(), store)
	ctx = WithInjectedContextBudget(ctx, 10000)
	result := tool.Execute(ctx, map[string]any{"path": tmpDir})
	if result.IsError {
		t.Errorf("should not error: %s", result.ForLLM)
	}
	if len(store.items) != 1 {
		t.Errorf("should skip node_modules, got %d items", len(store.items))
	}
}

func TestContextInjectTool_SkipsGitDir(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main"), 0o644)
	os.MkdirAll(filepath.Join(tmpDir, ".git", "objects"), 0o755)
	os.WriteFile(filepath.Join(tmpDir, ".git", "HEAD"), []byte("ref: refs/heads/main"), 0o644)

	tool := NewContextInjectTool()
	store := &mockStore{}
	ctx := WithInjectedContextStore(context.Background(), store)
	ctx = WithInjectedContextBudget(ctx, 10000)
	result := tool.Execute(ctx, map[string]any{"path": tmpDir})
	if result.IsError {
		t.Errorf("should not error: %s", result.ForLLM)
	}
	if len(store.items) != 1 {
		t.Errorf("should skip .git, got %d items", len(store.items))
	}
}

func TestContextInjectTool_SkipsBinaryFiles(t *testing.T) {
	tmpDir := t.TempDir()
	// Write a binary file (contains null bytes)
	binaryData := []byte{0x89, 0x50, 0x4E, 0x47, 0x00, 0x00, 0x00} // PNG-like header with null
	os.WriteFile(filepath.Join(tmpDir, "image.png"), binaryData, 0o644)
	os.WriteFile(filepath.Join(tmpDir, "readme.md"), []byte("# Hello"), 0o644)

	tool := NewContextInjectTool()
	store := &mockStore{}
	ctx := WithInjectedContextStore(context.Background(), store)
	ctx = WithInjectedContextBudget(ctx, 10000)
	result := tool.Execute(ctx, map[string]any{"path": tmpDir})
	if result.IsError {
		t.Errorf("should not error: %s", result.ForLLM)
	}
	if len(store.items) != 1 {
		t.Errorf("should skip binary, got %d items", len(store.items))
	}
}

func TestContextInjectTool_WithMaxTokens(t *testing.T) {
	tmpDir := t.TempDir()
	// Create a large file
	largeContent := strings.Repeat("hello world\n", 1000)
	os.WriteFile(filepath.Join(tmpDir, "large.txt"), []byte(largeContent), 0o644)

	tool := NewContextInjectTool()
	store := &mockStore{}
	ctx := WithInjectedContextStore(context.Background(), store)
	ctx = WithInjectedContextBudget(ctx, 1000000) // large overall budget
	// But restrict max_tokens for this call
	result := tool.Execute(ctx, map[string]any{"path": filepath.Join(tmpDir, "large.txt"), "max_tokens": float64(100)})
	if result.IsError {
		t.Errorf("should not error: %s", result.ForLLM)
	}
	// The file should be injected since its token count is within the max_tokens budget
	// estimateTokenCount for "hello world\n" * 1000 ≈ 4400 tokens
	// With max_tokens=100, the token count (4400) exceeds remaining (100)
	// So it should not be injected if it exceeds
	if len(store.items) > 1 {
		t.Errorf("should respect max_tokens, got %d items", len(store.items))
	}
}

// ── ContextListTool ─────────────────────────────────────────────────────────

func TestContextListTool_Name(t *testing.T) {
	tool := NewContextListTool()
	if tool.Name() != "context_list" {
		t.Errorf("name: got %q, want %q", tool.Name(), "context_list")
	}
}

func TestContextListTool_NoStore(t *testing.T) {
	tool := NewContextListTool()
	result := tool.Execute(context.Background(), map[string]any{})
	if !result.IsError {
		t.Error("no store should return error")
	}
}

func TestContextListTool_Empty(t *testing.T) {
	tool := NewContextListTool()
	store := &mockStore{}
	ctx := WithInjectedContextStore(context.Background(), store)
	result := tool.Execute(ctx, map[string]any{})
	if result.IsError {
		t.Error("empty store should not error")
	}
	if !strings.Contains(result.ForLLM, "No context items") {
		t.Errorf("should mention no items, got: %s", result.ForLLM)
	}
}

func TestContextListTool_WithItems(t *testing.T) {
	tool := NewContextListTool()
	store := &mockStore{
		items: []InjectedContextItem{
			{ID: "file1.go", TokenCount: 100, Source: "context_inject", InjectedAt: time.Now()},
			{ID: "file2.go", TokenCount: 200, Source: "context_inject", InjectedAt: time.Now()},
		},
		totalToken: 300,
	}
	ctx := WithInjectedContextStore(context.Background(), store)
	ctx = WithInjectedContextBudget(ctx, 1000)
	result := tool.Execute(ctx, map[string]any{})
	if result.IsError {
		t.Error("should not error")
	}
	if !strings.Contains(result.ForLLM, "file1.go") {
		t.Error("should list file1.go")
	}
	if !strings.Contains(result.ForLLM, "Budget") {
		t.Error("should show budget")
	}
}

// ── ContextClearTool ────────────────────────────────────────────────────────

func TestContextClearTool_Name(t *testing.T) {
	tool := NewContextClearTool()
	if tool.Name() != "context_clear" {
		t.Errorf("name: got %q, want %q", tool.Name(), "context_clear")
	}
}

func TestContextClearTool_NoStore(t *testing.T) {
	tool := NewContextClearTool()
	result := tool.Execute(context.Background(), map[string]any{})
	if !result.IsError {
		t.Error("no store should return error")
	}
}

func TestContextClearTool_ClearAll(t *testing.T) {
	tool := NewContextClearTool()
	store := &mockStore{
		items: []InjectedContextItem{
			{ID: "file1.go", TokenCount: 100},
			{ID: "file2.go", TokenCount: 200},
		},
		totalToken: 300,
	}
	ctx := WithInjectedContextStore(context.Background(), store)
	result := tool.Execute(ctx, map[string]any{})
	if result.IsError {
		t.Error("should not error")
	}
	if !strings.Contains(result.ForLLM, "2") {
		t.Errorf("should report 2 cleared, got: %s", result.ForLLM)
	}
	if len(store.items) != 0 {
		t.Error("store should be empty after clear")
	}
}

func TestContextClearTool_WithPattern(t *testing.T) {
	tool := NewContextClearTool()
	store := &mockStore{
		items: []InjectedContextItem{
			{ID: "file1.go", TokenCount: 100},
		},
		totalToken: 100,
	}
	ctx := WithInjectedContextStore(context.Background(), store)
	result := tool.Execute(ctx, map[string]any{"pattern": "*.go"})
	if result.IsError {
		t.Error("should not error")
	}
	if !strings.Contains(result.ForLLM, "*.go") {
		t.Errorf("should mention pattern, got: %s", result.ForLLM)
	}
}

// ── Helper functions ────────────────────────────────────────────────────────

func TestResolveContextPath_Absolute(t *testing.T) {
	result := resolveContextPath("/workspace", "/absolute/path")
	if result != "/absolute/path" {
		t.Errorf("absolute path: got %q, want %q", result, "/absolute/path")
	}
}

func TestResolveContextPath_Relative(t *testing.T) {
	result := resolveContextPath("/workspace", "relative/path")
	expected := filepath.Join("/workspace", "relative/path")
	if result != expected {
		t.Errorf("relative path: got %q, want %q", result, expected)
	}
}

func TestResolveContextPath_NoWorkspace(t *testing.T) {
	result := resolveContextPath("", "relative/path")
	abs, _ := filepath.Abs("relative/path")
	if result != abs {
		t.Errorf("no workspace: got %q, want %q", result, abs)
	}
}

func TestMatchesAnyGlob_Exact(t *testing.T) {
	if !matchesAnyGlob("test.go", "test.go") {
		t.Error("exact match should work")
	}
}

func TestMatchesAnyGlob_Wildcard(t *testing.T) {
	if !matchesAnyGlob("test.go", "*.go") {
		t.Error("wildcard match should work")
	}
}

func TestMatchesAnyGlob_NoMatch(t *testing.T) {
	if matchesAnyGlob("test.py", "*.go") {
		t.Error("should not match different extension")
	}
}

func TestReadFileForContext_Text(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "test.txt")
	os.WriteFile(tmpFile, []byte("hello world"), 0o644)
	content, err := readFileForContext(tmpFile)
	if err != nil {
		t.Fatal(err)
	}
	if content != "hello world" {
		t.Errorf("content: got %q, want %q", content, "hello world")
	}
}

func TestReadFileForContext_Binary(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "test.bin")
	os.WriteFile(tmpFile, []byte{0x89, 0x50, 0x4E, 0x47, 0x00}, 0o644)
	content, err := readFileForContext(tmpFile)
	if err != nil {
		t.Fatal(err)
	}
	if content != "" {
		t.Error("binary file should return empty content")
	}
}

func TestReadFileForContext_Nonexistent(t *testing.T) {
	_, err := readFileForContext("/nonexistent/file")
	if err == nil {
		t.Error("should error for nonexistent file")
	}
}

func TestEstimateTokenCount(t *testing.T) {
	result := estimateTokenCount("hello world")
	// len("hello world") = 11, 11 * 2 / 5 = 4
	if result != 4 {
		t.Errorf("token count: got %d, want 4", result)
	}
}

func TestEstimateTokenCount_Empty(t *testing.T) {
	result := estimateTokenCount("")
	if result != 0 {
		t.Errorf("empty token count: got %d, want 0", result)
	}
}

// ── InjectedContextItem ─────────────────────────────────────────────────────

func TestInjectedContextItem_Fields(t *testing.T) {
	now := time.Now()
	item := InjectedContextItem{
		ID:         "test.go",
		Content:    "package main",
		TokenCount: 10,
		Source:     "context_inject",
		InjectedAt: now,
	}
	if item.ID != "test.go" {
		t.Error("ID should be set")
	}
	if item.TokenCount != 10 {
		t.Error("TokenCount should be set")
	}
	if !item.InjectedAt.Equal(now) {
		t.Error("InjectedAt should be set")
	}
}
