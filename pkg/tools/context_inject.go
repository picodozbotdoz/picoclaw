package tools

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// InjectedContextStore is the interface for managing injected context items.
// The agent package provides the concrete implementation.
type InjectedContextStore interface {
	Inject(item InjectedContextItem, budgetTokens int)
	List() []InjectedContextItem
	Clear(pattern string) int
	TotalTokens() int
	Content() string
}

// InjectedContextItem represents a single piece of injected context.
type InjectedContextItem struct {
	ID         string
	Content    string
	TokenCount int
	Source     string
	InjectedAt time.Time
}

// Context value keys for injected context dependencies.

type injectedContextStoreKey struct{}
type injectedContextBudgetKey struct{}
type injectedContextWorkspaceKey struct{}

// WithInjectedContextStore returns a context with the InjectedContextStore.
func WithInjectedContextStore(ctx context.Context, store InjectedContextStore) context.Context {
	return context.WithValue(ctx, injectedContextStoreKey{}, store)
}

// InjectedContextStoreFromCtx extracts the InjectedContextStore from context.
func InjectedContextStoreFromCtx(ctx context.Context) InjectedContextStore {
	store, _ := ctx.Value(injectedContextStoreKey{}).(InjectedContextStore)
	return store
}

// WithInjectedContextBudget returns a context with the token budget.
func WithInjectedContextBudget(ctx context.Context, budget int) context.Context {
	return context.WithValue(ctx, injectedContextBudgetKey{}, budget)
}

// InjectedContextBudgetFromCtx extracts the token budget from context.
func InjectedContextBudgetFromCtx(ctx context.Context) int {
	budget, _ := ctx.Value(injectedContextBudgetKey{}).(int)
	return budget
}

// WithInjectedContextWorkspace returns a context with the workspace path.
func WithInjectedContextWorkspace(ctx context.Context, workspace string) context.Context {
	return context.WithValue(ctx, injectedContextWorkspaceKey{}, workspace)
}

// InjectedContextWorkspaceFromCtx extracts the workspace path from context.
func InjectedContextWorkspaceFromCtx(ctx context.Context) string {
	workspace, _ := ctx.Value(injectedContextWorkspaceKey{}).(string)
	return workspace
}

// ── context_inject tool ─────────────────────────────────────────────────────

// ContextInjectTool injects file/directory content into the LLM context.
type ContextInjectTool struct{}

func NewContextInjectTool() *ContextInjectTool {
	return &ContextInjectTool{}
}

func (t *ContextInjectTool) Name() string { return "context_inject" }
func (t *ContextInjectTool) Description() string {
	return "Inject file or directory content into the LLM context window. Reads files, respects token budget, skips binary/node_modules/.git/vendor."
}

func (t *ContextInjectTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to file or directory to inject into context",
			},
			"max_tokens": map[string]any{
				"type":        "integer",
				"description": "Maximum tokens to inject (optional, defaults to context budget)",
			},
			"pattern": map[string]any{
				"type":        "string",
				"description": "Glob pattern to filter files (optional)",
			},
		},
		"required": []string{"path"},
	}
}

func (t *ContextInjectTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	pathVal, ok := args["path"].(string)
	if !ok || pathVal == "" {
		return ErrorResult("Missing or invalid 'path' argument")
	}

	store := InjectedContextStoreFromCtx(ctx)
	if store == nil {
		return ErrorResult("Context injection not available: no store configured")
	}

	budget := InjectedContextBudgetFromCtx(ctx)
	if maxTokens, ok := args["max_tokens"].(float64); ok && int(maxTokens) > 0 {
		budget = int(maxTokens)
	}
	if budget <= 0 {
		budget = 10000 // reasonable default
	}

	workspace := InjectedContextWorkspaceFromCtx(ctx)
	resolvedPath := resolveContextPath(workspace, pathVal)

	var pattern string
	if p, ok := args["pattern"].(string); ok {
		pattern = p
	}

	info, err := os.Stat(resolvedPath)
	if err != nil {
		return ErrorResult(fmt.Sprintf("Path not found: %s", pathVal))
	}

	var files []fileEntry

	if info.IsDir() {
		files, err = scanDirectory(resolvedPath, pattern)
		if err != nil {
			return ErrorResult(fmt.Sprintf("Error scanning directory: %v", err))
		}
	} else {
		content, err := readFileForContext(resolvedPath)
		if err != nil {
			return ErrorResult(fmt.Sprintf("Error reading file: %v", err))
		}
		tokenCount := estimateTokenCount(content)
		files = []fileEntry{{path: resolvedPath, content: content, tokenCount: tokenCount, modTime: info.ModTime()}}
	}

	// Sort by mtime (most recent first)
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.After(files[j].modTime)
	})

	// Inject files respecting budget
	totalInjected := 0
	totalTokens := 0
	remaining := budget - store.TotalTokens()

	for _, f := range files {
		if f.tokenCount > remaining {
			logger.DebugCF("context", "Skipping file (over budget)",
				map[string]any{"file": f.path, "tokens": f.tokenCount, "remaining": remaining})
			continue
		}

		item := InjectedContextItem{
			ID:         f.path,
			Content:    fmt.Sprintf("--- %s ---\n%s", f.path, f.content),
			TokenCount: f.tokenCount,
			Source:     "context_inject",
			InjectedAt: time.Now(),
		}

		store.Inject(item, budget)
		remaining -= f.tokenCount
		totalTokens += f.tokenCount
		totalInjected++
	}

	logger.InfoCF("context", "Context injection completed",
		map[string]any{"files": totalInjected, "tokens": totalTokens, "budget": budget})

	return SilentResult(fmt.Sprintf("Injected %d files (%d tokens) into context. Budget: %d tokens remaining.", totalInjected, totalTokens, remaining))
}

// ── context_list tool ────────────────────────────────────────────────────────

// ContextListTool lists injected context items.
type ContextListTool struct{}

func NewContextListTool() *ContextListTool { return &ContextListTool{} }

func (t *ContextListTool) Name() string { return "context_list" }
func (t *ContextListTool) Description() string {
	return "List injected context items with token counts and remaining budget."
}

func (t *ContextListTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t *ContextListTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	store := InjectedContextStoreFromCtx(ctx)
	if store == nil {
		return ErrorResult("Context injection not available: no store configured")
	}

	items := store.List()
	if len(items) == 0 {
		return SilentResult("No context items injected.")
	}

	budget := InjectedContextBudgetFromCtx(ctx)
	totalTokens := store.TotalTokens()
	var remaining int
	if budget > 0 {
		remaining = budget - totalTokens
		if remaining < 0 {
			remaining = 0
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Injected context (%d items, %d tokens):\n", len(items), totalTokens))
	for _, item := range items {
		sb.WriteString(fmt.Sprintf("  - %s: %d tokens (source: %s)\n", item.ID, item.TokenCount, item.Source))
	}
	if budget > 0 {
		sb.WriteString(fmt.Sprintf("Budget: %d / %d tokens (%d remaining)\n", totalTokens, budget, remaining))
	}

	return SilentResult(sb.String())
}

// ── context_clear tool ───────────────────────────────────────────────────────

// ContextClearTool clears injected context items.
type ContextClearTool struct{}

func NewContextClearTool() *ContextClearTool { return &ContextClearTool{} }

func (t *ContextClearTool) Name() string { return "context_clear" }
func (t *ContextClearTool) Description() string {
	return "Clear all or pattern-matching injected context items."
}

func (t *ContextClearTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "Glob pattern to match item IDs for clearing (optional, clears all if omitted)",
			},
		},
	}
}

func (t *ContextClearTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	store := InjectedContextStoreFromCtx(ctx)
	if store == nil {
		return ErrorResult("Context injection not available: no store configured")
	}

	pattern, _ := args["pattern"].(string)
	removed := store.Clear(pattern)

	if pattern == "" {
		return SilentResult(fmt.Sprintf("Cleared all %d context items.", removed))
	}
	return SilentResult(fmt.Sprintf("Cleared %d context items matching pattern %q.", removed, pattern))
}

// ── Helper functions ────────────────────────────────────────────────────────

type fileEntry struct {
	path       string
	content    string
	tokenCount int
	modTime    time.Time
}

// resolveContextPath resolves a path relative to the workspace.
func resolveContextPath(workspace, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	if workspace != "" {
		return filepath.Join(workspace, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

// skipDirs lists directory names that should be skipped during scanning.
var skipDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
	"vendor":       true,
	"__pycache__":  true,
	".svn":         true,
	".hg":          true,
}

// scanDirectory recursively scans a directory, collecting file entries.
func scanDirectory(root, pattern string) ([]fileEntry, error) {
	var entries []fileEntry

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip errors
		}

		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip hidden files
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}

		// Apply glob pattern if specified
		if pattern != "" && !matchesAnyGlob(d.Name(), pattern) {
			// Also try matching against relative path
			relPath, _ := filepath.Rel(root, path)
			if !matchesAnyGlob(relPath, pattern) {
				return nil
			}
		}

		content, err := readFileForContext(path)
		if err != nil {
			return nil // skip unreadable files
		}
		if content == "" {
			return nil // binary or empty
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		entries = append(entries, fileEntry{
			path:       path,
			content:    content,
			tokenCount: estimateTokenCount(content),
			modTime:    info.ModTime(),
		})
		return nil
	})

	return entries, err
}

// matchesAnyGlob checks if a name matches a glob pattern.
func matchesAnyGlob(name, pattern string) bool {
	matched, _ := filepath.Match(pattern, name)
	if matched {
		return true
	}
	// Try matching against the base name
	matched, _ = filepath.Match(pattern, filepath.Base(name))
	return matched
}

// readFileForContext reads a file and returns its content as a string.
// Returns empty string for binary files.
func readFileForContext(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	// Binary detection: check for null bytes in first 8KB
	checkSize := 8192
	if len(data) < checkSize {
		checkSize = len(data)
	}
	if bytes.Contains(data[:checkSize], []byte{0}) {
		return "", nil // binary file
	}

	return string(data), nil
}

// estimateTokenCount estimates the number of tokens in a string.
// Uses the same heuristic as tokenizer: chars * 2 / 5
func estimateTokenCount(s string) int {
	return len(s) * 2 / 5
}
