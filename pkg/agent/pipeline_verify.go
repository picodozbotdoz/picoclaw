package agent

import (
        "context"
        "fmt"
        "strings"
        "time"

        "github.com/sipeed/picoclaw/pkg/bus"
        "github.com/sipeed/picoclaw/pkg/logger"
        "github.com/sipeed/picoclaw/pkg/providers"
)

const (
        // maxVerificationOutput truncates build/test output to prevent context blowout.
        maxVerificationOutput = 5000

        // verificationContextID is the InjectedContext item ID used for verification results.
        verificationContextID = "verification"
)

// Verify runs the post-edit verification phase. It:
//  1. Detects the project type and determines build/test commands
//  2. Runs the build command
//  3. If build fails, injects the error as steering and returns false
//  4. If build passes, runs the test command
//  5. If tests fail, injects the failure as steering and returns false
//
// Returns true if verification passed (or was skipped), false if it failed.
// When false is returned, the failure is already injected as steering so the
// coordinator can continue the loop for the LLM to fix the errors.
func (p *Pipeline) Verify(
        ctx context.Context,
        turnCtx context.Context,
        ts *turnState,
        exec *turnExecution,
) bool {
        cfg := ts.agent.VerificationConfig
        if cfg == nil || !cfg.Enabled {
                return true
        }

        // Only verify if edits were actually made in this turn.
        if !p.hasEditsInTurn(ts) {
                logger.DebugCF("agent", "Verification: no edits detected, skipping",
                        map[string]any{"session_key": ts.sessionKey})
                return true
        }

        ts.setPhase(TurnPhaseVerifying)

        projectInfo := DetectProject(ts.agent.Workspace)

        // Determine build and test commands (config overrides take precedence).
        buildCmd := cfg.BuildCommand
        if buildCmd == "" {
                buildCmd = projectInfo.BuildCmd
        }
        testCmd := cfg.TestCommand
        if testCmd == "" {
                testCmd = projectInfo.TestCmd
        }

        p.al.emitEvent(
                EventKindVerifyStart,
                ts.eventMeta("verify", "turn.verify.start"),
                VerifyStartPayload{
                        ProjectType: string(projectInfo.Type),
                        BuildCmd:    buildCmd,
                        TestCmd:     testCmd,
                },
        )

        // Run build.
        if cfg.Build && buildCmd != "" {
                buildOutput, buildErr := p.runVerificationCommand(ctx, turnCtx, ts, buildCmd, cfg.TimeoutSeconds)
                buildFailed := buildErr != nil || !isBuildSuccess(buildOutput)

                if buildFailed {
                        p.injectVerificationFailure(ts, "BUILD FAILED", buildCmd, buildOutput)

                        p.al.emitEvent(
                                EventKindVerifyFail,
                                ts.eventMeta("verify", "turn.verify.fail"),
                                VerifyFailPayload{
                                        Stage:   "build",
                                        Command: buildCmd,
                                        Output:  truncateContent(buildOutput, 500),
                                },
                        )

                        return false
                }
        }

        // Run tests.
        if cfg.Test && testCmd != "" {
                testOutput, testErr := p.runVerificationCommand(ctx, turnCtx, ts, testCmd, cfg.TimeoutSeconds)
                testFailed := testErr != nil || !isTestSuccess(testOutput)

                if testFailed {
                        p.injectVerificationFailure(ts, "TESTS FAILED", testCmd, testOutput)

                        p.al.emitEvent(
                                EventKindVerifyFail,
                                ts.eventMeta("verify", "turn.verify.fail"),
                                VerifyFailPayload{
                                        Stage:   "test",
                                        Command: testCmd,
                                        Output:  truncateContent(testOutput, 500),
                                },
                        )

                        return false
                }
        }

        logger.InfoCF("agent", "Verification phase passed",
                map[string]any{"project_type": string(projectInfo.Type)})

        p.al.emitEvent(
                EventKindVerifyEnd,
                ts.eventMeta("verify", "turn.verify.end"),
                VerifyEndPayload{
                        BuildPassed: true,
                        TestPassed:  true,
                },
        )

        return true
}

// hasEditsInTurn checks whether any edit_file or write_file operations
// occurred in the current turn by examining the session history for
// tool calls to those tools.
func (p *Pipeline) hasEditsInTurn(ts *turnState) bool {
        if ts.agent.Sessions == nil {
                return false
        }

        history := ts.agent.Sessions.GetHistory(ts.sessionKey)
        for i := len(history) - 1; i >= 0; i-- {
                msg := history[i]
                if msg.Role != "assistant" {
                        continue
                }
                for _, tc := range msg.ToolCalls {
                        if tc.Function != nil {
                                name := tc.Function.Name
                                if name == "edit_file" || name == "write_file" || name == "append_file" {
                                        return true
                                }
                        }
                }
        }
        return false
}

// runVerificationCommand executes a build or test command via the exec tool.
func (p *Pipeline) runVerificationCommand(
        ctx context.Context,
        turnCtx context.Context,
        ts *turnState,
        command string,
        timeoutSeconds int,
) (string, error) {
        if timeoutSeconds <= 0 {
                timeoutSeconds = 60
        }

        // Set a context deadline as a safety net alongside the exec tool's own timeout.
        cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
        defer cancel()

        output, err := p.executeToolForExploration(cmdCtx, turnCtx, ts, "exec", map[string]any{
                "command": command,
        })

        if err != nil {
                return fmt.Sprintf("Command failed: %v\nOutput: %s", err, truncateContent(output, maxVerificationOutput)), err
        }

        return truncateContent(output, maxVerificationOutput), nil
}

// injectVerificationFailure creates a steering message with the verification failure
// and adds it to the turn's followUps so the coordinator can inject it as steering.
func (p *Pipeline) injectVerificationFailure(ts *turnState, stage, command, output string) {
        content := fmt.Sprintf(
                "VERIFICATION FAILURE -- %s\nCommand: %s\nOutput:\n%s\n\nPlease fix the errors above and retry.",
                stage, command, output)

        // Create a providers.Message steering message and wrap it as a bus.InboundMessage.
        steerMsg := steeringPromptMessage(providers.Message{
                Role:    "user",
                Content: content,
        })

        followUp := bus.InboundMessage{
                Content:    content,
                Channel:    ts.channel,
                ChatID:     ts.chatID,
                SessionKey: ts.sessionKey,
        }

        // Store the steering message as a pendingMessage so the coordinator injects it.
        _ = steerMsg // The steering message is used for reference; the followUp carries the content.

        ts.mu.Lock()
        ts.followUps = append(ts.followUps, followUp)
        ts.mu.Unlock()

        logger.WarnCF("agent", "Verification failure injected as steering",
                map[string]any{
                        "stage":      stage,
                        "command":    command,
                        "output_len": len(output),
                })
}

// isBuildSuccess checks if build output indicates success by looking for
// common failure indicators. If none are found, we assume success.
func isBuildSuccess(output string) bool {
        lower := strings.ToLower(output)
        failureIndicators := []string{
                "error:", "fatal error:", "build failed",
                "compilation failed", "undefined:", "cannot find",
                "no such file or directory", "syntax error",
                "link error", "ld returned",
        }
        for _, indicator := range failureIndicators {
                if strings.Contains(lower, indicator) {
                        return false
                }
        }
        return true
}

// isTestSuccess checks if test output indicates success by looking for
// common failure indicators. If none are found, we assume success.
func isTestSuccess(output string) bool {
        lower := strings.ToLower(output)

        // Check for explicit success indicators first.
        successIndicators := []string{
                "ok  ", "pass", "passed", "no tests to run",
        }
        for _, indicator := range successIndicators {
                if strings.Contains(lower, indicator) {
                        return true
                }
        }

        // Check for explicit failure indicators.
        failureIndicators := []string{
                "fail", "failed", "panic",
                "assertion", "expected",
        }
        for _, indicator := range failureIndicators {
                if strings.Contains(lower, indicator) {
                        return false
                }
        }

        // No strong signal either way: assume success if the command didn't error.
        return true
}
