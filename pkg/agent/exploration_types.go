package agent

import (
	"fmt"
	"strings"
)

// ExplorationPlan is the LLM's answer to "what do you need to read?".
// It is produced by a dedicated LLM call before the main agent loop starts.
type ExplorationPlan struct {
	// FilesToRead lists file paths the LLM wants to examine.
	FilesToRead []string `json:"files_to_read"`

	// SymbolsToSearch lists symbol names the LLM wants to find references for.
	SymbolsToSearch []string `json:"symbols_to_search"`

	// PackagesToTrace lists import paths the LLM wants to trace dependencies for.
	PackagesToTrace []string `json:"packages_to_trace"`

	// Reasoning explains why these items are needed (for debugging/observability).
	Reasoning string `json:"reasoning"`
}

// ExplorationResult contains the gathered context from executing an ExplorationPlan.
type ExplorationResult struct {
	// FilesRead maps file path to content summary (first N lines or full content).
	FilesRead map[string]string `json:"files_read"`

	// SymbolsSearched maps symbol name to list of files that reference it.
	SymbolsSearched map[string][]string `json:"symbols_searched"`

	// ImportGraph maps package to list of imported packages.
	ImportGraph map[string][]string `json:"import_graph"`

	// Stats for observability.
	FilesReadCount   int `json:"files_read_count"`
	SearchesRunCount int `json:"searches_run_count"`
	ImportHopsCount  int `json:"import_hops_count"`
	TotalTokensUsed  int `json:"total_tokens_used"`
}

// Summary produces a formatted string suitable for injection into InjectedContext.
func (r *ExplorationResult) Summary() string {
	var sb strings.Builder

	sb.WriteString("EXPLORATION RESULTS (auto-gathered before editing):\n\n")

	if len(r.FilesRead) > 0 {
		sb.WriteString("Files examined:\n")
		for path, content := range r.FilesRead {
			sb.WriteString(fmt.Sprintf("--- %s ---\n%s\n", path, content))
		}
		sb.WriteString("\n")
	}

	if len(r.SymbolsSearched) > 0 {
		sb.WriteString("Symbol references found:\n")
		for sym, files := range r.SymbolsSearched {
			sb.WriteString(fmt.Sprintf("  %s -> %s\n", sym, strings.Join(files, ", ")))
		}
		sb.WriteString("\n")
	}

	if len(r.ImportGraph) > 0 {
		sb.WriteString("Import graph:\n")
		for pkg, imports := range r.ImportGraph {
			sb.WriteString(fmt.Sprintf("  %s -> %s\n", pkg, strings.Join(imports, ", ")))
		}
	}

	return sb.String()
}

// VerificationResult contains the outcome of the verification phase.
type VerificationResult struct {
	// BuildPassed is true if the build succeeded.
	BuildPassed bool `json:"build_passed"`

	// BuildOutput contains the build command output (truncated if too long).
	BuildOutput string `json:"build_output,omitempty"`

	// TestPassed is true if tests passed (only checked if build passed).
	TestPassed bool `json:"test_passed"`

	// TestOutput contains the test command output (truncated if too long).
	TestOutput string `json:"test_output,omitempty"`

	// RetriesUsed is the number of verification->fix loops that occurred.
	RetriesUsed int `json:"retries_used"`

	// ProjectType is the detected project type (go, node, python, rust, make).
	ProjectType string `json:"project_type"`
}
