package config

// ExplorationConfig controls the structured exploration phase.
// When enabled, the agent proactively gathers context before the main LLM→Tools loop.
type ExplorationConfig struct {
	// Enabled is the master switch. When false, the exploration phase is skipped entirely.
	Enabled bool `json:"enabled" yaml:"enabled" env:"PICOCLAW_AGENTS_DEFAULTS_EXPLORATION_ENABLED"`

	// MaxFiles limits how many files the exploration plan can request to read.
	// Prevents the LLM from requesting an unbounded number of file reads.
	MaxFiles int `json:"max_files,omitempty" yaml:"max_files,omitempty" env:"PICOCLAW_AGENTS_DEFAULTS_EXPLORATION_MAX_FILES"`

	// MaxSearches limits how many symbol searches (grep/rg) the exploration can perform.
	MaxSearches int `json:"max_searches,omitempty" yaml:"max_searches,omitempty" env:"PICOCLAW_AGENTS_DEFAULTS_EXPLORATION_MAX_SEARCHES"`

	// MaxImportHops limits the depth of import graph traversal.
	// 0 = no import tracing, 1 = direct imports only, 2 = transitive imports.
	MaxImportHops int `json:"max_import_hops,omitempty" yaml:"max_import_hops,omitempty" env:"PICOCLAW_AGENTS_DEFAULTS_EXPLORATION_MAX_IMPORT_HOPS"`

	// TokenBudgetPercent is the percentage of the context window to allocate
	// for exploration results. Default: 15 (15% of context window).
	TokenBudgetPercent int `json:"token_budget_percent,omitempty" yaml:"token_budget_percent,omitempty" env:"PICOCLAW_AGENTS_DEFAULTS_EXPLORATION_TOKEN_BUDGET_PERCENT"`

	// Strategy controls when exploration runs:
	//   "auto"        — run exploration when the user message looks like a code task (default)
	//   "always"      — run exploration for every turn
	//   "complex_only" — run exploration only when the task involves multiple files
	Strategy string `json:"strategy,omitempty" yaml:"strategy,omitempty" env:"PICOCLAW_AGENTS_DEFAULTS_EXPLORATION_STRATEGY"`
}

// Effective returns a normalized copy with defaults filled in for zero fields.
// If the config is nil or disabled, returns a disabled config.
func (c *ExplorationConfig) Effective() *ExplorationConfig {
	if c == nil {
		return &ExplorationConfig{Enabled: false}
	}
	clone := *c
	if clone.MaxFiles == 0 {
		clone.MaxFiles = 10
	}
	if clone.MaxSearches == 0 {
		clone.MaxSearches = 5
	}
	if clone.MaxImportHops == 0 {
		clone.MaxImportHops = 2
	}
	if clone.TokenBudgetPercent == 0 {
		clone.TokenBudgetPercent = 15
	}
	if clone.Strategy == "" {
		clone.Strategy = "auto"
	}
	return &clone
}

// VerificationConfig controls the post-edit verification phase.
// When enabled, the agent runs build and test commands after edits and
// injects failures as steering so the LLM can fix them.
type VerificationConfig struct {
	// Enabled is the master switch. When false, the verification phase is skipped entirely.
	Enabled bool `json:"enabled" yaml:"enabled" env:"PICOCLAW_AGENTS_DEFAULTS_VERIFICATION_ENABLED"`

	// Build controls whether to run the build command after edits.
	Build bool `json:"build,omitempty" yaml:"build,omitempty" env:"PICOCLAW_AGENTS_DEFAULTS_VERIFICATION_BUILD"`

	// Test controls whether to run tests after a successful build.
	Test bool `json:"test,omitempty" yaml:"test,omitempty" env:"PICOCLAW_AGENTS_DEFAULTS_VERIFICATION_TEST"`

	// MaxRetries is the maximum number of verification→fix loops.
	// If the build/test fails, the error is injected as steering and the loop continues.
	// After MaxRetries failures, the turn finalizes with the last error.
	MaxRetries int `json:"max_retries,omitempty" yaml:"max_retries,omitempty" env:"PICOCLAW_AGENTS_DEFAULTS_VERIFICATION_MAX_RETRIES"`

	// TimeoutSeconds is the per-command timeout for build and test commands.
	TimeoutSeconds int `json:"timeout_seconds,omitempty" yaml:"timeout_seconds,omitempty" env:"PICOCLAW_AGENTS_DEFAULTS_VERIFICATION_TIMEOUT_SECONDS"`

	// BuildCommand overrides the auto-detected build command. If empty, auto-detect.
	BuildCommand string `json:"build_command,omitempty" yaml:"build_command,omitempty" env:"PICOCLAW_AGENTS_DEFAULTS_VERIFICATION_BUILD_COMMAND"`

	// TestCommand overrides the auto-detected test command. If empty, auto-detect.
	TestCommand string `json:"test_command,omitempty" yaml:"test_command,omitempty" env:"PICOCLAW_AGENTS_DEFAULTS_VERIFICATION_TEST_COMMAND"`
}

// Effective returns a normalized copy with defaults filled in for zero fields.
// If the config is nil or disabled, returns a disabled config.
func (c *VerificationConfig) Effective() *VerificationConfig {
	if c == nil {
		return &VerificationConfig{Enabled: false}
	}
	clone := *c
	if clone.MaxRetries == 0 {
		clone.MaxRetries = 2
	}
	if clone.TimeoutSeconds == 0 {
		clone.TimeoutSeconds = 60
	}
	return &clone
}
