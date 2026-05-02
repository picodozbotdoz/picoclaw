package agent

import (
        "context"
        "testing"

        "github.com/sipeed/picoclaw/pkg/tools"
)

func TestSafeEditHook_ReadBeforeEdit_BlocksUnreadFile(t *testing.T) {
        hook := newSafeEditHook(defaultSafeEditConfig())

        // Attempt to edit a file that hasn't been read
        req := &ToolCallHookRequest{
                Tool: "edit_file",
                Arguments: map[string]any{
                        "path":       "pkg/agent/example.go",
                        "old_string": "func hello()",
                        "new_string": "func hello2()",
                },
        }

        result, decision, err := hook.BeforeTool(context.Background(), req)
        if err != nil {
                t.Fatalf("BeforeTool error: %v", err)
        }

        if decision.Action != HookActionRespond {
                t.Errorf("Expected HookActionRespond for unread file, got %s", decision.Action)
        }

        if result.HookResult == nil {
                t.Fatal("Expected HookResult to be set")
        }

        content := result.HookResult.ContentForLLM()
        if content == "" {
                t.Error("Expected non-empty error message in HookResult")
        }
        t.Logf("Block message: %s", content)
}

func TestSafeEditHook_ReadBeforeEdit_AllowsReadFile(t *testing.T) {
        hook := newSafeEditHook(defaultSafeEditConfig())

        // Read the file first
        readReq := &ToolCallHookRequest{
                Tool: "read_file",
                Arguments: map[string]any{
                        "path": "pkg/agent/example.go",
                },
        }
        _, _, err := hook.BeforeTool(context.Background(), readReq)
        if err != nil {
                t.Fatalf("BeforeTool error for read: %v", err)
        }

        // Now edit should be allowed (non-function edit so SearchBeforeModify won't block)
        editReq := &ToolCallHookRequest{
                Tool: "edit_file",
                Arguments: map[string]any{
                        "path":       "pkg/agent/example.go",
                        "old_string": "// old comment",
                        "new_string": "// new comment",
                },
        }

        _, decision, err := hook.BeforeTool(context.Background(), editReq)
        if err != nil {
                t.Fatalf("BeforeTool error for edit: %v", err)
        }

        if decision.Action != HookActionContinue {
                t.Errorf("Expected HookActionContinue after read, got %s", decision.Action)
        }
}

func TestSafeEditHook_ReadBeforeEdit_NormalizesPaths(t *testing.T) {
        hook := newSafeEditHook(defaultSafeEditConfig())

        // Read with ./ prefix
        readReq := &ToolCallHookRequest{
                Tool: "read_file",
                Arguments: map[string]any{
                        "path": "./pkg/agent/example.go",
                },
        }
        _, _, _ = hook.BeforeTool(context.Background(), readReq)

        // Edit without ./ prefix should still work
        editReq := &ToolCallHookRequest{
                Tool: "edit_file",
                Arguments: map[string]any{
                        "path": "pkg/agent/example.go",
                },
        }
        _, decision, _ := hook.BeforeTool(context.Background(), editReq)

        if decision.Action != HookActionContinue {
                t.Errorf("Expected path normalization to work, got %s", decision.Action)
        }
}

func TestSafeEditHook_ReadBeforeEdit_SkipsConfiguredPaths(t *testing.T) {
        cfg := defaultSafeEditConfig()
        cfg.SkipPaths = []string{"*.json", "go.sum"}
        hook := newSafeEditHook(cfg)

        // Edit a .json file without reading — should be allowed (skip path)
        editReq := &ToolCallHookRequest{
                Tool: "edit_file",
                Arguments: map[string]any{
                        "path": "config.json",
                },
        }
        _, decision, _ := hook.BeforeTool(context.Background(), editReq)

        if decision.Action != HookActionContinue {
                t.Errorf("Expected skip path to bypass read check, got %s", decision.Action)
        }
}

func TestSafeEditHook_ReadBeforeEdit_Disabled(t *testing.T) {
        cfg := defaultSafeEditConfig()
        cfg.ReadBeforeEdit = false
        hook := newSafeEditHook(cfg)

        // Edit without reading — should be allowed when disabled
        editReq := &ToolCallHookRequest{
                Tool: "edit_file",
                Arguments: map[string]any{
                        "path": "pkg/agent/example.go",
                },
        }
        _, decision, _ := hook.BeforeTool(context.Background(), editReq)

        if decision.Action != HookActionContinue {
                t.Errorf("Expected HookActionContinue when ReadBeforeEdit disabled, got %s", decision.Action)
        }
}

func TestSafeEditHook_SearchBeforeModify_BlocksUnsearchedSymbol(t *testing.T) {
        hook := newSafeEditHook(defaultSafeEditConfig())

        // Read the file first (so ReadBeforeEdit doesn't block)
        readReq := &ToolCallHookRequest{
                Tool: "read_file",
                Arguments: map[string]any{
                        "path": "pkg/agent/example.go",
                },
        }
        _, _, _ = hook.BeforeTool(context.Background(), readReq)

        // Edit a function without searching for callers
        editReq := &ToolCallHookRequest{
                Tool: "edit_file",
                Arguments: map[string]any{
                        "path":       "pkg/agent/example.go",
                        "old_string": "func EstimateMessageTokens(msg Message) int {",
                        "new_string": "func EstimateMessageTokensForModel(msg Message, model string) int {",
                },
        }

        result, decision, _ := hook.BeforeTool(context.Background(), editReq)

        if decision.Action != HookActionRespond {
                t.Errorf("Expected HookActionRespond for unsearched symbol, got %s", decision.Action)
        }

        if result.HookResult != nil {
                t.Logf("Block message: %s", result.HookResult.ContentForLLM())
        }
}

func TestSafeEditHook_SearchBeforeModify_AllowsAfterSearch(t *testing.T) {
        hook := newSafeEditHook(defaultSafeEditConfig())

        // Read the file first
        _, _, _ = hook.BeforeTool(context.Background(), &ToolCallHookRequest{
                Tool:      "read_file",
                Arguments: map[string]any{"path": "pkg/agent/example.go"},
        })

        // Search for the symbol
        _, _, _ = hook.BeforeTool(context.Background(), &ToolCallHookRequest{
                Tool:      "exec",
                Arguments: map[string]any{"command": "grep -rn EstimateMessageTokens ./pkg/"},
        })

        // Now edit should be allowed
        _, decision, _ := hook.BeforeTool(context.Background(), &ToolCallHookRequest{
                Tool: "edit_file",
                Arguments: map[string]any{
                        "path":       "pkg/agent/example.go",
                        "old_string": "func EstimateMessageTokens(msg Message) int {",
                        "new_string": "func EstimateMessageTokensForModel(msg Message, model string) int {",
                },
        })

        if decision.Action != HookActionContinue {
                t.Errorf("Expected HookActionContinue after search, got %s", decision.Action)
        }
}

func TestSafeEditHook_SearchBeforeModify_Disabled(t *testing.T) {
        cfg := defaultSafeEditConfig()
        cfg.SearchBeforeModify = false
        hook := newSafeEditHook(cfg)

        // Read the file first
        _, _, _ = hook.BeforeTool(context.Background(), &ToolCallHookRequest{
                Tool:      "read_file",
                Arguments: map[string]any{"path": "pkg/agent/example.go"},
        })

        // Edit a function without searching — should be allowed when disabled
        _, decision, _ := hook.BeforeTool(context.Background(), &ToolCallHookRequest{
                Tool: "edit_file",
                Arguments: map[string]any{
                        "path":       "pkg/agent/example.go",
                        "old_string": "func EstimateMessageTokens(msg Message) int {",
                        "new_string": "func EstimateMessageTokensForModel(msg Message, model string) int {",
                },
        })

        if decision.Action != HookActionContinue {
                t.Errorf("Expected HookActionContinue when SearchBeforeModify disabled, got %s", decision.Action)
        }
}

func TestSafeEditHook_BuildAfterEdit_LogsAfterEdit(t *testing.T) {
        hook := newSafeEditHook(defaultSafeEditConfig())

        // After edit_file, should log build reminder
        resp := &ToolResultHookResponse{
                Tool: "edit_file",
                Arguments: map[string]any{
                        "path": "pkg/agent/example.go",
                },
        }

        _, decision, err := hook.AfterTool(context.Background(), resp)
        if err != nil {
                t.Fatalf("AfterTool error: %v", err)
        }

        if decision.Action != HookActionContinue {
                t.Errorf("Expected HookActionContinue for AfterTool, got %s", decision.Action)
        }

        if hook.editsMade != 1 {
                t.Errorf("Expected editsMade=1, got %d", hook.editsMade)
        }
}

func TestSafeEditHook_Reset(t *testing.T) {
        hook := newSafeEditHook(defaultSafeEditConfig())

        // Read a file
        _, _, _ = hook.BeforeTool(context.Background(), &ToolCallHookRequest{
                Tool:      "read_file",
                Arguments: map[string]any{"path": "example.go"},
        })

        // Search a symbol
        _, _, _ = hook.BeforeTool(context.Background(), &ToolCallHookRequest{
                Tool:      "exec",
                Arguments: map[string]any{"command": "grep -rn MyFunc ./"},
        })

        // Edit
        _, _, _ = hook.AfterTool(context.Background(), &ToolResultHookResponse{
                Tool:      "edit_file",
                Arguments: map[string]any{"path": "example.go"},
        })

        // Verify state
        if len(hook.filesRead) == 0 || len(hook.symbolsSearched) == 0 || hook.editsMade == 0 {
                t.Fatal("Expected state to be populated before reset")
        }

        // Reset
        hook.Reset()

        if len(hook.filesRead) != 0 || len(hook.symbolsSearched) != 0 || hook.editsMade != 0 {
                t.Error("Expected state to be cleared after reset")
        }
}

func TestSafeEditHook_WriteFileAlsoBlocked(t *testing.T) {
        hook := newSafeEditHook(defaultSafeEditConfig())

        // write_file should also be blocked without reading first
        req := &ToolCallHookRequest{
                Tool: "write_file",
                Arguments: map[string]any{
                        "path":    "pkg/agent/new_file.go",
                        "content": "package agent",
                },
        }

        _, decision, _ := hook.BeforeTool(context.Background(), req)

        if decision.Action != HookActionRespond {
                t.Errorf("Expected write_file to be blocked for unread file, got %s", decision.Action)
        }
}

func TestSafeEditHook_NonEditToolsPassThrough(t *testing.T) {
        hook := newSafeEditHook(defaultSafeEditConfig())

        // Tools that aren't edit-related should pass through
        for _, tool := range []string{"read_file", "list_dir", "exec", "context_inject", "message"} {
                req := &ToolCallHookRequest{
                        Tool:      tool,
                        Arguments: map[string]any{},
                }
                _, decision, _ := hook.BeforeTool(context.Background(), req)
                if decision.Action != HookActionContinue {
                        t.Errorf("Expected %s to pass through, got %s", tool, decision.Action)
                }
        }
}

// ---- Helper tests ----

func TestExtractTopLevelIdentifier(t *testing.T) {
        tests := []struct {
                input string
                want  string
        }{
                {"func EstimateMessageTokens(msg Message) int {", "EstimateMessageTokens"},
                {"func (r *Router) SelectModel(msg string) bool {", "SelectModel"},
                {"type Router struct {", "Router"},
                {"var modelTokenRates = map[string]float64{}", "modelTokenRates"},
                {"const defaultThreshold = 0.35", "defaultThreshold"},
                {"// just a comment", ""},
                {"x := 42", ""},
                {"func (m *legacyContextManager) Compact(ctx context.Context) error {", "Compact"},
        }

        for _, tt := range tests {
                got := extractTopLevelIdentifier(tt.input)
                if got != tt.want {
                        t.Errorf("extractTopLevelIdentifier(%q) = %q, want %q", tt.input, got, tt.want)
                }
        }
}

func TestExtractSearchedSymbols(t *testing.T) {
        tests := []struct {
                command string
                want    []string
        }{
                {"grep -rn EstimateMessageTokens ./pkg/", []string{"EstimateMessageTokens"}},
                {"rg 'func.*Compact' ./pkg/agent/", []string{"'func.*Compact'"}},
                {"grep -e EstimateMessageTokens -e GetModelTokenRate .", []string{"EstimateMessageTokens", "GetModelTokenRate"}},
                {"ls -la", nil},
                {"go test ./...", nil},
        }

        for _, tt := range tests {
                got := extractSearchedSymbols(tt.command)
                if len(got) != len(tt.want) {
                        t.Errorf("extractSearchedSymbols(%q) = %v, want %v", tt.command, got, tt.want)
                        continue
                }
                for i, v := range got {
                        if v != tt.want[i] {
                                t.Errorf("extractSearchedSymbols(%q)[%d] = %q, want %q", tt.command, i, v, tt.want[i])
                        }
                }
        }
}

func TestNormalizePath(t *testing.T) {
        tests := []struct {
                input string
                want  string
        }{
                {"./pkg/agent/example.go", "pkg/agent/example.go"},
                {"pkg/agent/example.go", "pkg/agent/example.go"},
                {"/tmp/test.go", "/tmp/test.go"},
                {"./test.go", "test.go"},
        }

        for _, tt := range tests {
                got := normalizePath(tt.input)
                if got != tt.want {
                        t.Errorf("normalizePath(%q) = %q, want %q", tt.input, got, tt.want)
                }
        }
}

// Verify the hook implements ToolInterceptor
func TestSafeEditHook_ImplementsToolInterceptor(t *testing.T) {
        var _ ToolInterceptor = newSafeEditHook(defaultSafeEditConfig())
}

// Verify the builtin hook factory is registered
func TestSafeEditBuiltinHookRegistered(t *testing.T) {
        _, ok := lookupBuiltinHook("safe_edit")
        if !ok {
                t.Error("safe_edit builtin hook not registered")
        }
}

// Verify the prompt contributor implements PromptContributor
func TestSafeEditWorkflowContributor_ImplementsPromptContributor(t *testing.T) {
        var _ PromptContributor = safeEditWorkflowContributor{}
}

// Verify the prompt contributor produces content
func TestSafeEditWorkflowContributor_ContributesPrompt(t *testing.T) {
        c := safeEditWorkflowContributor{}
        parts, err := c.ContributePrompt(context.Background(), PromptBuildRequest{})
        if err != nil {
                t.Fatalf("ContributePrompt error: %v", err)
        }
        if len(parts) != 1 {
                t.Fatalf("Expected 1 PromptPart, got %d", len(parts))
        }
        part := parts[0]
        if part.Content == "" {
                t.Error("Expected non-empty content")
        }
        if part.Layer != PromptLayerCapability {
                t.Errorf("Expected PromptLayerCapability, got %s", part.Layer)
        }
        if part.Slot != PromptSlotTooling {
                t.Errorf("Expected PromptSlotTooling, got %s", part.Slot)
        }
        t.Logf("Contributed prompt title: %s", part.Title)
        t.Logf("Content length: %d chars", len(part.Content))
}

// Test the ErrorResult helper from the tools package
func TestSafeEditHook_ErrorResultFormat(t *testing.T) {
        result := tools.ErrorResult("test error")
        if result == nil {
                t.Fatal("ErrorResult returned nil")
        }
        content := result.ContentForLLM()
        if content == "" {
                t.Error("ErrorResult ContentForLLM is empty")
        }
}
