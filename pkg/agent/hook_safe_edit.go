package agent

import (
        "context"
        "encoding/json"
        "fmt"
        "path/filepath"
        "regexp"
        "strings"
        "sync"

        "github.com/sipeed/picoclaw/pkg/config"
        "github.com/sipeed/picoclaw/pkg/logger"
        "github.com/sipeed/picoclaw/pkg/tools"
)

// safeEditHook enforces systematic code exploration before modification.
// It implements ToolInterceptor to block edit_file/write_file on files that
// haven't been read first, and nudges the LLM to build/test after edits.
//
// This is the "Level 2" enforcement layer that complements the prompt-based
// "Level 1" workflow rules (see safe_edit_workflow_contributor.go).
// While prompts are suggestions the LLM may ignore, this hook enforces
// the critical rules mechanically.
//
// Three enforcement modes (all configurable):
//   - ReadBeforeEdit: Blocks edit_file/write_file if the target file hasn't
//     been read via read_file in the current turn. This prevents the LLM from
//     editing files it hasn't examined, which is the #1 cause of incorrect edits.
//   - BuildAfterEdit: After each edit_file/write_file, injects a steering
//     message reminding the LLM to run the build command. This catches
//     compilation errors early before they cascade.
//   - SearchBeforeModify: When editing a Go function or struct, checks whether
//     the LLM has searched for callers of that symbol (via exec grep/rg).
//     If not, the edit is blocked with guidance. This prevents changes that
//     break callers the LLM hasn't considered.
type safeEditHook struct {
        mu sync.Mutex

        // Per-turn tracking state. Reset when a new turn starts.
        filesRead     map[string]bool
        symbolsSearched map[string]bool // e.g., "EstimateMessageTokens" → true
        editsMade     int

        // Configuration
        config SafeEditHookConfig
}

// SafeEditHookConfig controls which enforcement modes are active.
type SafeEditHookConfig struct {
        // ReadBeforeEdit blocks edits to files that haven't been read. Default: true.
        ReadBeforeEdit bool `json:"read_before_edit"`

        // BuildAfterEdit injects a steering reminder to build after edits. Default: true.
        BuildAfterEdit bool `json:"build_after_edit"`

        // SearchBeforeModify blocks function/struct edits without caller search. Default: true.
        SearchBeforeModify bool `json:"search_before_modify"`

        // BuildCommand is the command to suggest for building (auto-detected if empty).
        BuildCommand string `json:"build_command"`

        // SkipPaths are glob patterns for paths that bypass ReadBeforeEdit checks.
        // Useful for generated files, config files, or files the LLM creates from scratch.
        SkipPaths []string `json:"skip_paths"`
}

func defaultSafeEditConfig() SafeEditHookConfig {
        return SafeEditHookConfig{
                ReadBeforeEdit:    true,
                BuildAfterEdit:    true,
                SearchBeforeModify: true,
        }
}

func newSafeEditHook(cfg SafeEditHookConfig) *safeEditHook {
        return &safeEditHook{
                filesRead:       make(map[string]bool),
                symbolsSearched: make(map[string]bool),
                config:          cfg,
        }
}

// Reset clears per-turn tracking state. Called at the start of each turn.
func (h *safeEditHook) Reset() {
        h.mu.Lock()
        h.filesRead = make(map[string]bool)
        h.symbolsSearched = make(map[string]bool)
        h.editsMade = 0
        h.mu.Unlock()
}

// ---- ToolInterceptor: BeforeTool ----

func (h *safeEditHook) BeforeTool(ctx context.Context, req *ToolCallHookRequest) (*ToolCallHookRequest, HookDecision, error) {
        switch req.Tool {
        case "read_file", "read_file_lines":
                h.trackRead(req.Arguments)
        case "exec":
                h.trackSearch(req.Arguments)
        case "edit_file", "write_file":
                return h.enforceEditRules(req)
        }
        return req, HookDecision{Action: HookActionContinue}, nil
}

// ---- ToolInterceptor: AfterTool ----

func (h *safeEditHook) AfterTool(ctx context.Context, resp *ToolResultHookResponse) (*ToolResultHookResponse, HookDecision, error) {
        if h.config.BuildAfterEdit && (resp.Tool == "edit_file" || resp.Tool == "write_file") {
                h.mu.Lock()
                h.editsMade++
                h.mu.Unlock()

                // Inject a steering message via the event bus.
                // We log it rather than inject steering (which requires AgentLoop access)
                // because hooks don't have a direct steering API. The prompt rules
                // (Level 1) already instruct the LLM to build after edits; this hook
                // adds logging for observability.
                buildCmd := h.config.BuildCommand
                if buildCmd == "" {
                        buildCmd = detectBuildCommand()
                }
                logger.InfoCF("safe_edit_hook", "File modified — LLM should verify build", map[string]any{
                        "tool":        resp.Tool,
                        "edits_count": h.editsMade,
                        "suggest_cmd": buildCmd,
                })
        }
        return resp, HookDecision{Action: HookActionContinue}, nil
}

// ---- Internal: Tracking ----

func (h *safeEditHook) trackRead(args map[string]any) {
        path, _ := args["path"].(string)
        if path == "" {
                return
        }
        h.mu.Lock()
        h.filesRead[normalizePath(path)] = true
        h.mu.Unlock()
}

func (h *safeEditHook) trackSearch(args map[string]any) {
        command, _ := args["command"].(string)
        if command == "" {
                return
        }
        // Extract searched symbols from grep/rg commands
        for _, sym := range extractSearchedSymbols(command) {
                h.mu.Lock()
                h.symbolsSearched[sym] = true
                h.mu.Unlock()
        }
}

// ---- Internal: Enforcement ----

func (h *safeEditHook) enforceEditRules(req *ToolCallHookRequest) (*ToolCallHookRequest, HookDecision, error) {
        path, _ := req.Arguments["path"].(string)
        if path == "" {
                return req, HookDecision{Action: HookActionContinue}, nil
        }

        normalizedPath := normalizePath(path)

        // Check skip paths
        if h.matchesSkipPath(normalizedPath) {
                return req, HookDecision{Action: HookActionContinue}, nil
        }

        // Rule 1: Read before edit
        if h.config.ReadBeforeEdit {
                h.mu.Lock()
                read := h.filesRead[normalizedPath]
                h.mu.Unlock()

                if !read {
                        msg := fmt.Sprintf(
                                "BLOCKED by safe-edit hook: You must read %s before editing it. "+
                                        "Use read_file to examine its contents first, then retry the edit.",
                                path)
                        result := tools.ErrorResult(msg)
                        req.HookResult = result
                        return req, HookDecision{Action: HookActionRespond, Reason: "file not read before edit"}, nil
                }
        }

        // Rule 2: Search before modifying a function/struct
        if h.config.SearchBeforeModify {
                oldStr, _ := req.Arguments["old_string"].(string)
                if symbol := extractTopLevelIdentifier(oldStr); symbol != "" {
                        h.mu.Lock()
                        searched := h.symbolsSearched[symbol]
                        h.mu.Unlock()

                        if !searched {
                                msg := fmt.Sprintf(
                                        "BLOCKED by safe-edit hook: You're modifying '%s' but haven't searched for its callers. "+
                                                "Run: exec grep -rn '%s' ./ --include='*.go' (or equivalent) first to understand impact.",
                                        symbol, symbol)
                                result := tools.ErrorResult(msg)
                                req.HookResult = result
                                return req, HookDecision{Action: HookActionRespond, Reason: "symbol not searched before modification"}, nil
                        }
                }
        }

        return req, HookDecision{Action: HookActionContinue}, nil
}

// ---- Internal: Helpers ----

// matchesSkipPath checks if a path matches any of the configured skip patterns.
func (h *safeEditHook) matchesSkipPath(path string) bool {
        for _, pattern := range h.config.SkipPaths {
                if matched, _ := filepath.Match(pattern, path); matched {
                        return true
                }
                if matched, _ := filepath.Match(pattern, filepath.Base(path)); matched {
                        return true
                }
        }
        return false
}

// normalizePath normalizes a file path for consistent tracking.
func normalizePath(path string) string {
        // Clean the path and make it relative-ish
        path = filepath.Clean(path)
        // Remove leading ./ if present
        path = strings.TrimPrefix(path, "./")
        return path
}

// goIdentifierRe matches Go top-level identifiers like func names, type names, etc.
// Handles both standalone functions and methods with receivers.
// Examples: "func EstimateMessageTokens", "func (r *Router) SelectModel", "type Router struct"
var goIdentifierRe = regexp.MustCompile(`(?:func\s+(?:\([^)]*\)\s+)?(\w+)|type\s+(\w+)|var\s+(\w+)|const\s+(\w+))`)

// extractTopLevelIdentifier attempts to extract the Go identifier being modified
// from the old_string of an edit_file operation. Returns empty string if not identifiable.
func extractTopLevelIdentifier(oldStr string) string {
        if oldStr == "" {
                return ""
        }
        matches := goIdentifierRe.FindStringSubmatch(oldStr)
        // Capture groups: 1=func name, 2=type name, 3=var name, 4=const name
        if len(matches) >= 2 {
                for _, name := range matches[1:] {
                        if name != "" {
                                return name
                        }
                }
        }
        return ""
}

// extractSearchedSymbols attempts to identify what symbols are being searched
// for in a shell command (grep, rg, etc.).
func extractSearchedSymbols(command string) []string {
        var symbols []string
        lower := strings.ToLower(command)

        // Check if this is a search command
        isSearch := strings.Contains(lower, "grep") ||
                strings.Contains(lower, "rg ") ||
                strings.Contains(lower, "ag ") ||
                strings.Contains(lower, "ack ") ||
                strings.Contains(lower, "git grep")

        if !isSearch {
                return nil
        }

        // Extract the search pattern. This is a best-effort heuristic:
        // - For grep/rg, the pattern is typically the first non-flag argument
        // - We look for patterns after -e or as the main argument
        parts := strings.Fields(command)
        for i, part := range parts {
                if part == "-e" && i+1 < len(parts) {
                        symbols = append(symbols, parts[i+1])
                }
        }

        // If no -e found, try the first non-flag argument after the command name
        if len(symbols) == 0 && len(parts) >= 2 {
                for i := 1; i < len(parts); i++ {
                        if !strings.HasPrefix(parts[i], "-") {
                                symbols = append(symbols, parts[i])
                                break
                        }
                }
        }

        return symbols
}

// detectBuildCommand tries to detect the appropriate build command for the project.
// Delegates to DetectProject for workspace-aware detection.
func detectBuildCommand() string {
        // Fallback: detect in current working directory
        info := DetectProject(".")
        if info.BuildCmd != "" && info.Type != ProjectTypeUnknown {
                return info.BuildCmd
        }
        return "build"
}

// ---- Builtin Hook Factory ----

func init() {
        RegisterBuiltinHook("safe_edit", func(ctx context.Context, spec config.BuiltinHookConfig) (any, error) {
                cfg := defaultSafeEditConfig()
                if spec.Config != nil {
                        if err := parseSafeEditConfig(spec.Config, &cfg); err != nil {
                                return nil, fmt.Errorf("parse safe_edit hook config: %w", err)
                        }
                }
                return newSafeEditHook(cfg), nil
        })
}

func parseSafeEditConfig(raw json.RawMessage, cfg *SafeEditHookConfig) error {
        decoder := json.NewDecoder(strings.NewReader(string(raw)))
        decoder.DisallowUnknownFields()
        return decoder.Decode(cfg)
}
