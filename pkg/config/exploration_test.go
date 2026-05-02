package config

import "testing"

func TestExplorationConfig_Effective_Nil(t *testing.T) {
	var cfg *ExplorationConfig
	effective := cfg.Effective()
	if effective.Enabled {
		t.Error("nil ExplorationConfig should produce disabled Effective()")
	}
}

func TestExplorationConfig_Effective_Defaults(t *testing.T) {
	cfg := &ExplorationConfig{Enabled: true}
	effective := cfg.Effective()
	if !effective.Enabled {
		t.Error("Enabled should be preserved")
	}
	if effective.MaxFiles != 10 {
		t.Errorf("MaxFiles = %d, want 10", effective.MaxFiles)
	}
	if effective.MaxSearches != 5 {
		t.Errorf("MaxSearches = %d, want 5", effective.MaxSearches)
	}
	if effective.MaxImportHops != 2 {
		t.Errorf("MaxImportHops = %d, want 2", effective.MaxImportHops)
	}
	if effective.TokenBudgetPercent != 15 {
		t.Errorf("TokenBudgetPercent = %d, want 15", effective.TokenBudgetPercent)
	}
	if effective.Strategy != "auto" {
		t.Errorf("Strategy = %q, want %q", effective.Strategy, "auto")
	}
}

func TestExplorationConfig_Effective_Custom(t *testing.T) {
	cfg := &ExplorationConfig{
		Enabled:           true,
		MaxFiles:          20,
		MaxSearches:       10,
		MaxImportHops:     3,
		TokenBudgetPercent: 25,
		Strategy:          "always",
	}
	effective := cfg.Effective()
	if effective.MaxFiles != 20 {
		t.Errorf("MaxFiles = %d, want 20 (custom preserved)", effective.MaxFiles)
	}
	if effective.Strategy != "always" {
		t.Errorf("Strategy = %q, want %q", effective.Strategy, "always")
	}
}

func TestExplorationConfig_Effective_DoesNotMutateOriginal(t *testing.T) {
	cfg := &ExplorationConfig{Enabled: true}
	effective := cfg.Effective()
	_ = effective
	if cfg.MaxFiles != 0 {
		t.Error("Effective() should not mutate the original config")
	}
	if cfg.Strategy != "" {
		t.Error("Effective() should not mutate the original config Strategy")
	}
}

func TestVerificationConfig_Effective_Nil(t *testing.T) {
	var cfg *VerificationConfig
	effective := cfg.Effective()
	if effective.Enabled {
		t.Error("nil VerificationConfig should produce disabled Effective()")
	}
}

func TestVerificationConfig_Effective_Defaults(t *testing.T) {
	cfg := &VerificationConfig{Enabled: true}
	effective := cfg.Effective()
	if !effective.Enabled {
		t.Error("Enabled should be preserved")
	}
	if effective.MaxRetries != 2 {
		t.Errorf("MaxRetries = %d, want 2", effective.MaxRetries)
	}
	if effective.TimeoutSeconds != 60 {
		t.Errorf("TimeoutSeconds = %d, want 60", effective.TimeoutSeconds)
	}
}

func TestVerificationConfig_Effective_Custom(t *testing.T) {
	cfg := &VerificationConfig{
		Enabled:        true,
		Build:          true,
		Test:           true,
		MaxRetries:     5,
		TimeoutSeconds: 120,
		BuildCommand:   "make build",
		TestCommand:    "make test",
	}
	effective := cfg.Effective()
	if effective.MaxRetries != 5 {
		t.Errorf("MaxRetries = %d, want 5 (custom preserved)", effective.MaxRetries)
	}
	if effective.BuildCommand != "make build" {
		t.Errorf("BuildCommand = %q, want %q", effective.BuildCommand, "make build")
	}
	if effective.TestCommand != "make test" {
		t.Errorf("TestCommand = %q, want %q", effective.TestCommand, "make test")
	}
}

func TestVerificationConfig_Effective_DoesNotMutateOriginal(t *testing.T) {
	cfg := &VerificationConfig{Enabled: true}
	effective := cfg.Effective()
	_ = effective
	if cfg.MaxRetries != 0 {
		t.Error("Effective() should not mutate the original config")
	}
	if cfg.TimeoutSeconds != 0 {
		t.Error("Effective() should not mutate the original config TimeoutSeconds")
	}
}
