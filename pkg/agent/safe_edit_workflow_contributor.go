package agent

import (
	"context"
)

const (
	// PromptSourceSafeEditWorkflow is the prompt source ID for the safe-edit
	// workflow rules. These are "Level 1" prompt-based rules that instruct the
	// LLM to follow systematic exploration before editing. The "Level 2" hook
	// (hook_safe_edit.go) enforces the most critical rules mechanically.
	PromptSourceSafeEditWorkflow PromptSourceID = "hook:safe_edit_workflow"
)

// safeEditWorkflowContributor injects mandatory code editing workflow rules
// into the system prompt. This is the "Level 1" layer — prompt instructions
// that the LLM should follow. The "Level 2" hook enforces the critical rules
// (read-before-edit, search-before-modify) mechanically when the hook is enabled.
//
// Even without the hook enabled, these rules improve LLM behavior by explicitly
// stating the expected workflow. With the hook enabled, violations of the most
// critical rules are blocked.
type safeEditWorkflowContributor struct{}

func (c safeEditWorkflowContributor) PromptSource() PromptSourceDescriptor {
	return PromptSourceDescriptor{
		ID:              PromptSourceSafeEditWorkflow,
		Owner:           "hooks",
		Description:     "Safe edit workflow rules — systematic exploration before code modification",
		Allowed:         []PromptPlacement{{Layer: PromptLayerCapability, Slot: PromptSlotTooling}},
		StableByDefault: true,
	}
}

func (c safeEditWorkflowContributor) ContributePrompt(
	_ context.Context,
	_ PromptBuildRequest,
) ([]PromptPart, error) {
	return []PromptPart{
		{
			ID:      "capability.safe_edit_workflow",
			Layer:   PromptLayerCapability,
			Slot:    PromptSlotTooling,
			Source:  PromptSource{ID: PromptSourceSafeEditWorkflow, Name: "hook:safe_edit_workflow"},
			Title:   "safe edit workflow",
			Content: safeEditWorkflowRules,
			Stable:  true,
			Cache:   PromptCacheEphemeral,
		},
	}, nil
}

const safeEditWorkflowRules = `MANDATORY CODE EDITING WORKFLOW — follow these rules for every code modification:

1. READ BEFORE EDIT: Before editing any file, you MUST read it in full using read_file. Never edit a file you haven't examined. Partial reads or assumptions about file contents lead to incorrect edits.

2. SEARCH BEFORE MODIFY: Before modifying a function, type, or variable, search for all references using exec grep/rg. Understanding who calls or uses the symbol is essential to avoid breaking callers you haven't considered.

3. BUILD AFTER EDIT: After modifying source code files, you MUST run the project's build command to verify compilation. Catch type errors, missing imports, and broken references immediately — don't let them cascade.

4. TEST AFTER BUILD: After a successful build, run relevant tests. Focus on the packages you modified, but also run integration tests if your changes affect cross-package interfaces.

5. ONE CHANGE AT A TIME: Make focused, incremental edits. Don't refactor multiple unrelated functions in a single edit. Each edit should have a clear, single purpose that can be verified independently.

6. TRACE IMPORTS: When adding a new import or package dependency, verify the package exists and compiles. Circular imports are a common failure mode — check the dependency graph before adding imports.

7. PRESERVE CALLERS: When changing a function signature, type definition, or interface, update ALL callers. A search (rule 2) should have already identified them; now update each one.

VIOLATION OF THESE RULES IS AN ERROR. The safe-edit hook may block edits that violate rules 1 and 2.`
