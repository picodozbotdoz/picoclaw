package agent

import (
	"context"
)

// Prompt source IDs for exec efficiency contributors.
const (
	// PromptSourceExecBatching encourages the LLM to batch sequential shell
	// commands into single exec calls using &&/; or multi-line scripts.
	PromptSourceExecBatching PromptSourceID = "tools:exec_batching"

	// PromptSourceExecErrorDetection instructs the LLM to read error output
	// before retrying failed exec calls and prohibits retrying the same command.
	PromptSourceExecErrorDetection PromptSourceID = "tools:exec_error_detection"
)

// execBatchingContributor injects rules encouraging the LLM to combine
// related shell commands into batched exec calls instead of making one
// exec call per command. This reduces the number of LLM iterations and
// tool-call roundtrips, especially during deployment/setup workflows.
type execBatchingContributor struct{}

func (c execBatchingContributor) PromptSource() PromptSourceDescriptor {
	return PromptSourceDescriptor{
		ID:              PromptSourceExecBatching,
		Owner:           "tools",
		Description:     "Exec batching rules — combine related shell commands",
		Allowed:         []PromptPlacement{{Layer: PromptLayerCapability, Slot: PromptSlotTooling}},
		StableByDefault: true,
	}
}

func (c execBatchingContributor) ContributePrompt(
	_ context.Context,
	_ PromptBuildRequest,
) ([]PromptPart, error) {
	return []PromptPart{
		{
			ID:      "capability.exec_batching",
			Layer:   PromptLayerCapability,
			Slot:    PromptSlotTooling,
			Source:  PromptSource{ID: PromptSourceExecBatching, Name: "tools:exec_batching"},
			Title:   "exec batching rules",
			Content: execBatchingRules,
			Stable:  true,
			Cache:   PromptCacheEphemeral,
		},
	}, nil
}

// execErrorDetectionContributor injects rules for handling exec failures:
// reading the error output before retrying and never retrying the exact
// same command without changes. This prevents retry-without-change loops
// that waste LLM iterations and latency.
type execErrorDetectionContributor struct{}

func (c execErrorDetectionContributor) PromptSource() PromptSourceDescriptor {
	return PromptSourceDescriptor{
		ID:              PromptSourceExecErrorDetection,
		Owner:           "tools",
		Description:     "Exec error detection rules — read errors, avoid retry loops",
		Allowed:         []PromptPlacement{{Layer: PromptLayerCapability, Slot: PromptSlotTooling}},
		StableByDefault: true,
	}
}

func (c execErrorDetectionContributor) ContributePrompt(
	_ context.Context,
	_ PromptBuildRequest,
) ([]PromptPart, error) {
	return []PromptPart{
		{
			ID:      "capability.exec_error_detection",
			Layer:   PromptLayerCapability,
			Slot:    PromptSlotTooling,
			Source:  PromptSource{ID: PromptSourceExecErrorDetection, Name: "tools:exec_error_detection"},
			Title:   "exec error handling rules",
			Content: execErrorDetectionRules,
			Stable:  true,
			Cache:   PromptCacheEphemeral,
		},
	}, nil
}

const execBatchingRules = `EXEC BATCHING RULES — combine related shell commands to reduce tool calls:

1. BATCH RELATED COMMANDS: When you need to run multiple shell commands in sequence (e.g., cd + install + configure + verify), combine them into a single exec call using && or ;. Each exec call costs one LLM iteration.

2. PERMISSION FIXES: When fixing file/directory permissions, do all chmod/chown/setfacl operations in ONE call:
   exec("sudo chmod 775 dir/ && sudo chgrp group dir/ && sudo chown user:group file")

3. SETUP SEQUENCES: Git clone, venv setup, pip install — combine into a script or chained command:
   exec("git clone ... && cd dir && python3 -m venv .venv && .venv/bin/pip install -r requirements.txt")

4. DIAGNOSTIC CHECKS: Instead of running ls, then find, then cat separately, combine:
   exec("ls -la dir/ && echo '---' && find dir/ -name '*.db' && echo '---' && cat config.json")

5. SERVICE MANAGEMENT: Start service, wait, check status — do it in one call:
   exec("sudo systemctl start svc && sleep 3 && sudo systemctl status svc --no-pager -l")

6. SCRIPT FOR COMPLEX WORKFLOWS: For multi-step workflows (>3 commands), write a bash script with write_file and execute it once:
   write_file(path="setup.sh", content="...")
   exec("bash setup.sh && rm setup.sh")

VIOLATING THESE RULES wastes iterations and latency. Each unnecessary exec call adds 3-10 seconds.

REMEMBER: 151 exec calls in one session is a sign of poor batching. Aim for <30.`

const execErrorDetectionRules = `EXEC ERROR HANDLING — never retry blindly:

1. READ THE ERROR: When an exec call fails (returns an error), the error text IS part of the tool output. Read it carefully before deciding what to do next. The first few lines of stderr usually contain the root cause.

2. NEVER RETRY THE SAME COMMAND: If "exec('python3 -m broken_module')" fails, do NOT call the exact same command again. The second call will fail identically. Instead, diagnose the error and change the command.

3. DIAGNOSE, THEN FIX: Follow this pattern on failure:
   a) Read the error output (already in the tool result)
   b) Identify the root cause (missing file? permission denied? syntax error?)
   c) Fix the root cause with a DIFFERENT command
   d) Then retry the original operation

4. LONG-RUNNING COMMANDS: Don't use "sleep 120" to wait for a service. Check the service status or use a timeout-aware approach:
   exec("timeout 30 bash -c 'while ! systemctl is-active --quiet svc; do sleep 2; done' && echo READY || echo TIMEOUT")

5. TIMEOUT AWARENESS: Commands have a ~30 second timeout. If an operation takes longer, consider using cron scheduling or breaking it into async steps.

RETRYING WITHOUT CHANGES IS THE #1 CAUSE OF WASTED ITERATIONS. Diagnose first, act second.`
