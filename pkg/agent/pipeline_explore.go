package agent

import (
        "context"
        "encoding/json"
        "fmt"
        "strings"

        "github.com/sipeed/picoclaw/pkg/config"
        "github.com/sipeed/picoclaw/pkg/logger"
        "github.com/sipeed/picoclaw/pkg/providers"
        "github.com/sipeed/picoclaw/pkg/tools"
)

const (
        // explorationPromptTemplate is the system-level instruction sent to the LLM
        // when requesting an exploration plan. The LLM returns a JSON object
        // describing which files to read, symbols to search, and packages to trace.
        explorationPromptTemplate = `You are an exploration planner. Given the following task, list the files you need to read, symbols you need to search for, and packages whose imports you need to trace.

IMPORTANT: Return ONLY a JSON object with this structure, no other text:
{
  "files_to_read": ["path/to/file1.go", "path/to/file2.go"],
  "symbols_to_search": ["FunctionName", "TypeName"],
  "packages_to_trace": ["github.com/example/pkg"],
  "reasoning": "Brief explanation of why these items are needed"
}

Rules:
- Maximum %d files to read
- Maximum %d symbols to search
- Maximum %d packages to trace
- Use relative paths from the project root
- Only include items you genuinely need to understand before making changes
- If this task doesn't require code exploration, return empty arrays

Task: %s`

        // explorationContextID is the InjectedContext item ID used for exploration results.
        explorationContextID = "exploration"

        // maxExplorationFileContent limits the content stored per file in the exploration result.
        maxExplorationFileContent = 5000
)

// Explore runs the structured exploration phase. It:
//  1. Asks the LLM to produce an exploration plan (what to read/search/trace)
//  2. Executes the plan automatically using the tool registry
//  3. Injects the results into InjectedContext
//
// Returns the ExplorationResult or nil if exploration is disabled/skipped.
// Errors are non-fatal: the turn continues without exploration.
func (p *Pipeline) Explore(
        ctx context.Context,
        turnCtx context.Context,
        ts *turnState,
        exec *turnExecution,
) (*ExplorationResult, error) {
        cfg := ts.agent.ExplorationConfig
        if cfg == nil || !cfg.Enabled {
                return nil, nil
        }

        // Decide whether to explore based on strategy
        if !p.shouldExplore(ts, cfg) {
                return nil, nil
        }

        // Set phase for observability
        ts.setPhase(TurnPhaseExploring)

        p.al.emitEvent(
                EventKindExploreStart,
                ts.eventMeta("explore", "turn.explore.start"),
                ExploreStartPayload{},
        )

        // Step 1: Request exploration plan from LLM
        plan, err := p.requestExplorationPlan(ctx, turnCtx, ts, exec, cfg)
        if err != nil {
                logger.WarnCF("agent", "Exploration plan request failed, skipping exploration",
                        map[string]any{"error": err.Error()})
                return nil, nil // Non-fatal: continue without exploration
        }

        if plan == nil || (len(plan.FilesToRead) == 0 && len(plan.SymbolsToSearch) == 0 && len(plan.PackagesToTrace) == 0) {
                logger.InfoCF("agent", "Exploration plan is empty, skipping",
                        map[string]any{"reasoning": plan.Reasoning})
                return nil, nil
        }

        // Step 2: Execute the exploration plan
        result := p.executeExplorationPlan(ctx, turnCtx, ts, plan, cfg)

        // Step 3: Clear any stale exploration context, then inject fresh results
        if ts.agent.InjectedContext != nil {
                ts.agent.InjectedContext.Clear(explorationContextID)
        }
        if result != nil && ts.agent.InjectedContext != nil {
                budgetTokens := ts.agent.ContextWindow * cfg.TokenBudgetPercent / 100
                ts.agent.InjectedContext.Inject(InjectedContextItem{
                        ID:         explorationContextID,
                        Content:    result.Summary(),
                        Source:     "pipeline_explore",
                        TokenCount: estimateTokens(result.Summary()),
                }, budgetTokens)
        }

        logger.InfoCF("agent", "Exploration phase completed",
                map[string]any{
                        "files_read":   result.FilesReadCount,
                        "searches_run": result.SearchesRunCount,
                        "import_hops":  result.ImportHopsCount,
                        "tokens_used":  result.TotalTokensUsed,
                })

        p.al.emitEvent(
                EventKindExploreEnd,
                ts.eventMeta("explore", "turn.explore.end"),
                ExploreEndPayload{
                        FilesRead:   result.FilesReadCount,
                        SearchesRun: result.SearchesRunCount,
                        ImportHops:  result.ImportHopsCount,
                        TokensUsed:  result.TotalTokensUsed,
                },
        )

        // Rebuild messages to include the new injected context.
        // This mirrors what SetupTurn does after compression.
        if !ts.opts.NoHistory {
                history := exec.history
                summary := exec.summary
                messages := ts.agent.ContextBuilder.BuildMessagesFromPrompt(
                        promptBuildRequestForTurn(ts, history, summary, ts.userMessage, ts.media),
                )
                maxMediaSize := p.Cfg.Agents.Defaults.GetMaxMediaSize()
                messages = resolveMediaRefs(messages, p.MediaStore, maxMediaSize)
                exec.messages = messages
        }

        return result, nil
}

// shouldExplore decides whether the exploration phase should run based on
// the configured strategy and the content of the user message.
func (p *Pipeline) shouldExplore(ts *turnState, cfg *config.ExplorationConfig) bool {
        switch cfg.Strategy {
        case "always":
                return true
        case "complex_only":
                return looksLikeComplexTask(ts.userMessage)
        case "auto":
                return looksLikeCodeTask(ts.userMessage)
        default:
                return false
        }
}

// looksLikeCodeTask uses simple heuristics to determine if a message
// is likely a code editing task (vs. a question, greeting, etc.)
func looksLikeCodeTask(msg string) bool {
        lower := strings.ToLower(msg)
        codeIndicators := []string{
                "implement", "refactor", "fix", "add ", "change ", "modify",
                "update ", "remove ", "delete ", "move ", "rename ",
                "create ", "build", "write ", "edit ", "patch",
                "bug", "feature", "issue", "pr ", "pull request",
                "function", "method", "class", "struct", "interface",
                "file", "module", "package", "import", "export",
                "test", "spec", "config",
        }
        for _, indicator := range codeIndicators {
                if strings.Contains(lower, indicator) {
                        return true
                }
        }
        return false
}

// looksLikeComplexTask returns true only for tasks that appear to involve
// changes across multiple files or significant restructuring.
func looksLikeComplexTask(msg string) bool {
        if !looksLikeCodeTask(msg) {
                return false
        }
        lower := strings.ToLower(msg)
        complexIndicators := []string{
                "refactor", "multiple files", "across", "throughout",
                "all ", "every ", "each ", "rename ", "move ",
                "restructure", "reorganize", "migrate", "rewrite",
        }
        for _, indicator := range complexIndicators {
                if strings.Contains(lower, indicator) {
                        return true
                }
        }
        return false
}

// requestExplorationPlan makes a dedicated LLM call to ask what files,
// symbols, and packages the agent should explore before editing.
func (p *Pipeline) requestExplorationPlan(
        ctx context.Context,
        turnCtx context.Context,
        ts *turnState,
        exec *turnExecution,
        cfg *config.ExplorationConfig,
) (*ExplorationPlan, error) {
        prompt := fmt.Sprintf(explorationPromptTemplate,
                cfg.MaxFiles, cfg.MaxSearches, cfg.MaxImportHops,
                ts.userMessage)

        provider := exec.activeProvider
        model := exec.activeModel
        if provider == nil {
                return nil, fmt.Errorf("no active provider for exploration plan")
        }

        // Build a minimal message list for the planning call.
        // We include a focused system prompt + the planning request.
        messages := []providers.Message{
                {Role: "system", Content: "You are a code exploration planner. Return only valid JSON."},
                {Role: "user", Content: prompt},
        }

        // Make the LLM call (no tools, just text response).
        opts := map[string]any{
                "max_tokens":  1024,
                "temperature": float64(0),
        }

        resp, err := provider.Chat(ctx, messages, nil, model, opts)
        if err != nil {
                return nil, fmt.Errorf("exploration plan LLM call failed: %w", err)
        }

        if resp == nil || resp.Content == "" {
                return nil, fmt.Errorf("exploration plan LLM returned empty response")
        }

        // Parse the JSON response.
        var plan ExplorationPlan
        content := strings.TrimSpace(resp.Content)
        content = stripCodeFences(content)
        if err := json.Unmarshal([]byte(content), &plan); err != nil {
                // Return a truncated view for debugging, not the whole content.
                preview := content
                if len(preview) > 200 {
                        preview = preview[:200] + "..."
                }
                return nil, fmt.Errorf("exploration plan JSON parse failed: %w (content: %s)", err, preview)
        }

        // Enforce limits.
        if len(plan.FilesToRead) > cfg.MaxFiles {
                plan.FilesToRead = plan.FilesToRead[:cfg.MaxFiles]
        }
        if len(plan.SymbolsToSearch) > cfg.MaxSearches {
                plan.SymbolsToSearch = plan.SymbolsToSearch[:cfg.MaxSearches]
        }
        if len(plan.PackagesToTrace) > cfg.MaxImportHops {
                plan.PackagesToTrace = plan.PackagesToTrace[:cfg.MaxImportHops]
        }

        return &plan, nil
}

// executeExplorationPlan runs the planned file reads, symbol searches,
// and import traces using the existing tool registry.
func (p *Pipeline) executeExplorationPlan(
        ctx context.Context,
        turnCtx context.Context,
        ts *turnState,
        plan *ExplorationPlan,
        cfg *config.ExplorationConfig,
) *ExplorationResult {
        result := &ExplorationResult{
                FilesRead:       make(map[string]string),
                SymbolsSearched: make(map[string][]string),
                ImportGraph:     make(map[string][]string),
        }

        // Execute file reads using the read_file tool.
        for _, filePath := range plan.FilesToRead {
                content, err := p.executeToolForExploration(ctx, turnCtx, ts, "read_file", map[string]any{
                        "path": filePath,
                })
                if err != nil {
                        logger.DebugCF("agent", "Exploration: failed to read file",
                                map[string]any{"path": filePath, "error": err.Error()})
                        continue
                }
                result.FilesRead[filePath] = truncateContent(content, maxExplorationFileContent)
                result.FilesReadCount++
        }

        // Execute symbol searches using the exec tool (grep/rg).
        projectInfo := DetectProject(ts.agent.Workspace)
        searchIncludes := searchIncludesForProject(projectInfo.Type)
        for _, symbol := range plan.SymbolsToSearch {
                searchCmd := fmt.Sprintf("grep -rn '%s' . %s", symbol, searchIncludes)
                output, err := p.executeToolForExploration(ctx, turnCtx, ts, "exec", map[string]any{
                        "command": searchCmd,
                })
                if err != nil {
                        logger.DebugCF("agent", "Exploration: failed to search symbol",
                                map[string]any{"symbol": symbol, "error": err.Error()})
                        continue
                }
                files := parseGrepOutput(output)
                result.SymbolsSearched[symbol] = files
                result.SearchesRunCount++
        }

        // Execute import tracing (Go-specific, extensible).
        for _, pkg := range plan.PackagesToTrace {
                imports := p.traceImports(ctx, turnCtx, ts, pkg)
                result.ImportGraph[pkg] = imports
                result.ImportHopsCount++
        }

        result.TotalTokensUsed = estimateTokens(result.Summary())

        return result
}

// executeToolForExploration runs a single tool and returns its ForLLM output.
// This reuses the existing ToolRegistry.ExecuteWithContext infrastructure.
func (p *Pipeline) executeToolForExploration(
        ctx context.Context,
        turnCtx context.Context,
        ts *turnState,
        toolName string,
        args map[string]any,
) (string, error) {
        execCtx := tools.WithInjectedContextStore(turnCtx, ts.agent.InjectedContext)
        execCtx = tools.WithInjectedContextWorkspace(execCtx, ts.agent.Workspace)

        result := ts.agent.Tools.ExecuteWithContext(execCtx, toolName, args, ts.channel, ts.chatID, nil)
        if result == nil {
                return "", fmt.Errorf("tool %q returned nil result", toolName)
        }
        if result.IsError {
                return "", fmt.Errorf("tool error: %s", result.ForLLM)
        }
        return result.ForLLM, nil
}

// traceImports traces the import graph for a Go package using go list.
func (p *Pipeline) traceImports(
        ctx context.Context,
        turnCtx context.Context,
        ts *turnState,
        pkg string,
) []string {
        cmd := fmt.Sprintf("go list -f '{{.Imports}}' %s 2>/dev/null || echo '[]'", pkg)
        output, err := p.executeToolForExploration(ctx, turnCtx, ts, "exec", map[string]any{
                "command": cmd,
        })
        if err != nil {
                return nil
        }
        // Parse the bracket-enclosed list.
        output = strings.TrimSpace(output)
        output = strings.Trim(output, "[]")
        if output == "" {
                return nil
        }
        imports := strings.Fields(output)
        return imports
}

// stripCodeFences removes ```json and ``` wrapping from LLM responses.
func stripCodeFences(s string) string {
        s = strings.TrimSpace(s)
        if strings.HasPrefix(s, "```json") {
                s = strings.TrimPrefix(s, "```json")
        } else if strings.HasPrefix(s, "```") {
                s = strings.TrimPrefix(s, "```")
        }
        if strings.HasSuffix(s, "```") {
                s = strings.TrimSuffix(s, "```")
        }
        return strings.TrimSpace(s)
}

// truncateContent truncates content to maxBytes, appending a truncation notice.
func truncateContent(content string, maxBytes int) string {
        if len(content) <= maxBytes {
                return content
        }
        return content[:maxBytes] + "\n... (truncated)"
}

// parseGrepOutput extracts file paths from grep output lines.
func parseGrepOutput(output string) []string {
        seen := make(map[string]bool)
        var files []string
        for _, line := range strings.Split(output, "\n") {
                line = strings.TrimSpace(line)
                if line == "" {
                        continue
                }
                if idx := strings.Index(line, ":"); idx > 0 {
                        file := line[:idx]
                        if !seen[file] {
                                seen[file] = true
                                files = append(files, file)
                        }
                }
        }
        return files
}

// searchIncludesForProject returns grep --include flags appropriate for the
// detected project type, so we don't search irrelevant file types.
func searchIncludesForProject(pType ProjectType) string {
        switch pType {
        case ProjectTypeGo:
                return "--include='*.go'"
        case ProjectTypeNode:
                return "--include='*.ts' --include='*.tsx' --include='*.js' --include='*.jsx'"
        case ProjectTypePython:
                return "--include='*.py'"
        case ProjectTypeRust:
                return "--include='*.rs'"
        default:
                return "--include='*.go' --include='*.ts' --include='*.js' --include='*.py' --include='*.rs'"
        }
}
