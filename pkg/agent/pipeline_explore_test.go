package agent

import (
        "encoding/json"
        "strings"
        "testing"
)

// --- looksLikeCodeTask tests ---

func TestLooksLikeCodeTask_Implement(t *testing.T) {
        if !looksLikeCodeTask("implement the auth module") {
                t.Error("expected 'implement' to match")
        }
}

func TestLooksLikeCodeTask_Refactor(t *testing.T) {
        if !looksLikeCodeTask("refactor the database layer") {
                t.Error("expected 'refactor' to match")
        }
}

func TestLooksLikeCodeTask_Fix(t *testing.T) {
        if !looksLikeCodeTask("fix the null pointer error") {
                t.Error("expected 'fix' to match")
        }
}

func TestLooksLikeCodeTask_Add(t *testing.T) {
        if !looksLikeCodeTask("add a new endpoint") {
                t.Error("expected 'add' to match")
        }
}

func TestLooksLikeCodeTask_Write(t *testing.T) {
        if !looksLikeCodeTask("write a test for the handler") {
                t.Error("expected 'write' to match")
        }
}

func TestLooksLikeCodeTask_Edit(t *testing.T) {
        if !looksLikeCodeTask("edit the config file") {
                t.Error("expected 'edit' to match")
        }
}

func TestLooksLikeCodeTask_Struct(t *testing.T) {
        if !looksLikeCodeTask("define a struct for the response") {
                t.Error("expected 'struct' to match")
        }
}

func TestLooksLikeCodeTask_Function(t *testing.T) {
        if !looksLikeCodeTask("create a function that validates input") {
                t.Error("expected 'function' to match")
        }
}

func TestLooksLikeCodeTask_Greeting(t *testing.T) {
        if looksLikeCodeTask("hello, how are you?") {
                t.Error("expected greeting not to match")
        }
}

func TestLooksLikeCodeTask_Question(t *testing.T) {
        if looksLikeCodeTask("what is the capital of France?") {
                t.Error("expected general question not to match")
        }
}

func TestLooksLikeCodeTask_Thanks(t *testing.T) {
        if looksLikeCodeTask("thanks for your help!") {
                t.Error("expected thanks not to match")
        }
}

func TestLooksLikeCodeTask_CaseInsensitive(t *testing.T) {
        if !looksLikeCodeTask("IMPLEMENT THE AUTH MODULE") {
                t.Error("expected case-insensitive match for 'IMPLEMENT'")
        }
        if !looksLikeCodeTask("Fix The Bug") {
                t.Error("expected case-insensitive match for 'Fix'")
        }
}

// --- looksLikeComplexTask tests ---

func TestLooksLikeComplexTask_Refactor(t *testing.T) {
        if !looksLikeComplexTask("refactor the entire codebase") {
                t.Error("expected 'refactor' to be complex")
        }
}

func TestLooksLikeComplexTask_MultipleFiles(t *testing.T) {
        if !looksLikeComplexTask("update across multiple files") {
                t.Error("expected 'multiple files' to be complex")
        }
}

func TestLooksLikeComplexTask_Migrate(t *testing.T) {
        if !looksLikeComplexTask("migrate the database schema and update all files") {
                t.Error("expected 'migrate' to be complex")
        }
}

func TestLooksLikeComplexTask_SimpleFixNotComplex(t *testing.T) {
        if looksLikeComplexTask("fix the typo in readme") {
                t.Error("expected simple fix not to be complex")
        }
}

func TestLooksLikeComplexTask_NonCodeNotComplex(t *testing.T) {
        // 'refactor' is itself a code indicator, so looksLikeCodeTask returns true.
        // But looksLikeComplexTask only returns true if there's also a complexity indicator.
        // "refactor X" IS both a code task and complex, so this should return true.
        if !looksLikeComplexTask("refactor the entire module") {
                t.Error("expected 'refactor' to be complex since it is both a code and complexity indicator")
        }
        // A truly non-code message should not be complex
        if looksLikeComplexTask("hello, what is the weather today?") {
                t.Error("expected non-code message not to be complex")
        }
}

func TestLooksLikeComplexTask_Across(t *testing.T) {
        if !looksLikeComplexTask("change the API across all services") {
                t.Error("expected 'across' to be complex")
        }
}

func TestLooksLikeComplexTask_Restructure(t *testing.T) {
        if !looksLikeComplexTask("restructure the project layout") {
                t.Error("expected 'restructure' to be complex")
        }
}

// --- stripCodeFences tests ---

func TestStripCodeFences_JSON(t *testing.T) {
        input := "```json\n{\"key\": \"value\"}\n```"
        want := "{\"key\": \"value\"}"
        got := stripCodeFences(input)
        if got != want {
                t.Errorf("stripCodeFences(%q) = %q, want %q", input, got, want)
        }
}

func TestStripCodeFences_Plain(t *testing.T) {
        input := "```\nsome code\n```"
        want := "some code"
        got := stripCodeFences(input)
        if got != want {
                t.Errorf("stripCodeFences(%q) = %q, want %q", input, got, want)
        }
}

func TestStripCodeFences_NoFences(t *testing.T) {
        input := `{"files_to_read": []}`
        got := stripCodeFences(input)
        if got != input {
                t.Errorf("stripCodeFences(%q) = %q, want unchanged", input, got)
        }
}

func TestStripCodeFences_LeadingTrailing(t *testing.T) {
        input := "  ```json\n{\"a\":1}\n```  "
        want := `{"a":1}`
        got := stripCodeFences(input)
        if got != want {
                t.Errorf("stripCodeFences(%q) = %q, want %q", input, got, want)
        }
}

// --- truncateContent tests ---

func TestTruncateContent_UnderLimit(t *testing.T) {
        content := "short"
        got := truncateContent(content, 100)
        if got != content {
                t.Errorf("truncateContent(short, 100) = %q, want %q", got, content)
        }
}

func TestTruncateContent_OverLimit(t *testing.T) {
        content := "a very long string that exceeds the limit"
        got := truncateContent(content, 10)
        want := content[:10] + "\n... (truncated)"
        if got != want {
                t.Errorf("truncateContent(long, 10) = %q, want %q", got, want)
        }
}

func TestTruncateContent_ExactLimit(t *testing.T) {
        content := "exactly10!"
        got := truncateContent(content, 10)
        if got != content {
                t.Errorf("truncateContent(exact, 10) = %q, want %q", got, content)
        }
}

// --- parseGrepOutput tests ---

func TestParseGrepOutput_SingleMatch(t *testing.T) {
        output := "main.go:42:func main() {"
        files := parseGrepOutput(output)
        if len(files) != 1 || files[0] != "main.go" {
                t.Errorf("parseGrepOutput(%q) = %v, want [main.go]", output, files)
        }
}

func TestParseGrepOutput_MultipleMatches(t *testing.T) {
        output := "main.go:42:func main() {\nutil.go:10:func helper()"
        files := parseGrepOutput(output)
        if len(files) != 2 {
                t.Fatalf("parseGrepOutput returned %d files, want 2", len(files))
        }
        if files[0] != "main.go" {
                t.Errorf("files[0] = %q, want main.go", files[0])
        }
        if files[1] != "util.go" {
                t.Errorf("files[1] = %q, want util.go", files[1])
        }
}

func TestParseGrepOutput_Deduplicates(t *testing.T) {
        output := "main.go:42:func main()\nmain.go:50:func helper()"
        files := parseGrepOutput(output)
        if len(files) != 1 || files[0] != "main.go" {
                t.Errorf("parseGrepOutput should deduplicate, got %v", files)
        }
}

func TestParseGrepOutput_EmptyInput(t *testing.T) {
        files := parseGrepOutput("")
        if len(files) != 0 {
                t.Errorf("parseGrepOutput('') = %v, want empty", files)
        }
}

func TestParseGrepOutput_BlankLines(t *testing.T) {
        output := "main.go:1:hello\n\nutil.go:5:world\n"
        files := parseGrepOutput(output)
        if len(files) != 2 {
                t.Errorf("parseGrepOutput with blank lines = %v, want 2 files", files)
        }
}

func TestParseGrepOutput_NoColon(t *testing.T) {
        output := "this line has no colon separator"
        files := parseGrepOutput(output)
        if len(files) != 0 {
                t.Errorf("parseGrepOutput with no colon = %v, want empty", files)
        }
}

func TestParseGrepOutput_ColonAtStart(t *testing.T) {
        output := ":starts_with_colon"
        files := parseGrepOutput(output)
        if len(files) != 0 {
                t.Errorf("parseGrepOutput with colon at index 0 = %v, want empty", files)
        }
}

// --- searchIncludesForProject tests ---

func TestSearchIncludesForProject_Go(t *testing.T) {
        got := searchIncludesForProject(ProjectTypeGo)
        want := "--include='*.go'"
        if got != want {
                t.Errorf("searchIncludesForProject(Go) = %q, want %q", got, want)
        }
}

func TestSearchIncludesForProject_Node(t *testing.T) {
        got := searchIncludesForProject(ProjectTypeNode)
        if got == "" {
                t.Error("searchIncludesForProject(Node) should not be empty")
        }
        if !strings.Contains(got, "*.ts") || !strings.Contains(got, "*.js") {
                t.Errorf("searchIncludesForProject(Node) = %q, want ts and js includes", got)
        }
}

func TestSearchIncludesForProject_Python(t *testing.T) {
        got := searchIncludesForProject(ProjectTypePython)
        if !strings.Contains(got, "*.py") {
                t.Errorf("searchIncludesForProject(Python) = %q, want py include", got)
        }
}

func TestSearchIncludesForProject_Rust(t *testing.T) {
        got := searchIncludesForProject(ProjectTypeRust)
        if !strings.Contains(got, "*.rs") {
                t.Errorf("searchIncludesForProject(Rust) = %q, want rs include", got)
        }
}

func TestSearchIncludesForProject_Unknown(t *testing.T) {
        got := searchIncludesForProject(ProjectTypeUnknown)
        if got == "" {
                t.Error("searchIncludesForProject(Unknown) should include multiple types")
        }
}

// --- ExplorationResult.Summary tests ---

func TestExplorationResult_Summary_FilesRead(t *testing.T) {
        r := &ExplorationResult{
                FilesRead: map[string]string{
                        "main.go": "package main\nfunc main() {}",
                },
                FilesReadCount: 1,
        }
        s := r.Summary()
        if !strings.Contains(s, "EXPLORATION RESULTS") {
                t.Error("Summary should contain header")
        }
        if !strings.Contains(s, "main.go") {
                t.Error("Summary should contain file path")
        }
        if !strings.Contains(s, "package main") {
                t.Error("Summary should contain file content")
        }
}

func TestExplorationResult_Summary_SymbolsSearched(t *testing.T) {
        r := &ExplorationResult{
                SymbolsSearched: map[string][]string{
                        "HandleRequest": {"handler.go", "middleware.go"},
                },
                SearchesRunCount: 1,
        }
        s := r.Summary()
        if !strings.Contains(s, "HandleRequest") {
                t.Error("Summary should contain symbol name")
        }
        if !strings.Contains(s, "handler.go") {
                t.Error("Summary should contain reference file")
        }
}

func TestExplorationResult_Summary_ImportGraph(t *testing.T) {
        r := &ExplorationResult{
                ImportGraph: map[string][]string{
                        "github.com/example/pkg": {"fmt", "os"},
                },
                ImportHopsCount: 1,
        }
        s := r.Summary()
        if !strings.Contains(s, "Import graph") {
                t.Error("Summary should contain import graph section")
        }
        if !strings.Contains(s, "github.com/example/pkg") {
                t.Error("Summary should contain package path")
        }
}

func TestExplorationResult_Summary_Empty(t *testing.T) {
        r := &ExplorationResult{
                FilesRead:       map[string]string{},
                SymbolsSearched: map[string][]string{},
                ImportGraph:     map[string][]string{},
        }
        s := r.Summary()
        if !strings.Contains(s, "EXPLORATION RESULTS") {
                t.Error("Even empty summary should contain header")
        }
}

// --- ExplorationPlan JSON parsing ---

func TestExplorationPlan_JSONParsing(t *testing.T) {
        jsonStr := `{"files_to_read":["a.go","b.go"],"symbols_to_search":["Func"],"packages_to_trace":["pkg"],"reasoning":"test"}`
        var plan ExplorationPlan
        if err := json.Unmarshal([]byte(jsonStr), &plan); err != nil {
                t.Fatalf("Failed to parse ExplorationPlan JSON: %v", err)
        }
        if len(plan.FilesToRead) != 2 {
                t.Errorf("FilesToRead = %d, want 2", len(plan.FilesToRead))
        }
        if plan.FilesToRead[0] != "a.go" {
                t.Errorf("FilesToRead[0] = %q, want 'a.go'", plan.FilesToRead[0])
        }
        if len(plan.SymbolsToSearch) != 1 {
                t.Errorf("SymbolsToSearch = %d, want 1", len(plan.SymbolsToSearch))
        }
        if plan.Reasoning != "test" {
                t.Errorf("Reasoning = %q, want 'test'", plan.Reasoning)
        }
}

func TestExplorationPlan_EmptyJSON(t *testing.T) {
        jsonStr := `{"files_to_read":[],"symbols_to_search":[],"packages_to_trace":[],"reasoning":"no code changes needed"}`
        var plan ExplorationPlan
        if err := json.Unmarshal([]byte(jsonStr), &plan); err != nil {
                t.Fatalf("Failed to parse empty ExplorationPlan JSON: %v", err)
        }
        if len(plan.FilesToRead) != 0 {
                t.Errorf("FilesToRead = %d, want 0", len(plan.FilesToRead))
        }
        if plan.Reasoning != "no code changes needed" {
                t.Errorf("Reasoning = %q, want 'no code changes needed'", plan.Reasoning)
        }
}
