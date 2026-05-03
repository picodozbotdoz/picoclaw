// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
        "context"
        "time"

        "github.com/sipeed/picoclaw/pkg/bus"
        "github.com/sipeed/picoclaw/pkg/logger"
        "github.com/sipeed/picoclaw/pkg/providers"
)

// Finalize handles turn finalization, either:
// - Early return when allResponsesHandled=true (ExecuteTools already finalized)
// - Normal finalization for allResponsesHandled=false (sets finalContent, saves session, compact)
func (p *Pipeline) Finalize(
        ctx context.Context,
        turnCtx context.Context,
        ts *turnState,
        exec *turnExecution,
        turnStatus TurnEndStatus,
        finalContent string,
) (turnResult, error) {
        al := p.al

        // When allResponsesHandled=true, ExecuteTools already finalized
        // (added handledToolResponseSummary, saved session, set phase to Completed).
        // But still check for hard abort - if requested, abort the turn.
        if exec.allResponsesHandled {
                if ts.hardAbortRequested() {
                        return al.abortTurn(ts)
                }
                ts.setPhase(TurnPhaseCompleted)
                return turnResult{
                        finalContent: finalContent,
                        status:       turnStatus,
                        followUps:    append([]bus.InboundMessage(nil), ts.followUps...),
                }, nil
        }

        finalizeStart := time.Now()

        ts.setPhase(TurnPhaseFinalizing)
        ts.setFinalContent(finalContent)

        logger.InfoCF("agent", "Turn finalizing",
                map[string]any{
                        "session_key":   ts.sessionKey,
                        "agent_id":      ts.agent.ID,
                        "turn_status":   string(turnStatus),
                        "content_len":   len(finalContent),
                        "has_reasoning": responseReasoningContent(exec.response) != "",
                        "all_handled":   exec.allResponsesHandled,
                })

        if !ts.opts.NoHistory {
                finalMsg := providers.Message{
                        Role:             "assistant",
                        Content:          finalContent,
                        ReasoningContent: responseReasoningContent(exec.response),
                }
                ts.agent.Sessions.AddFullMessage(ts.sessionKey, finalMsg)
                ts.recordPersistedMessage(finalMsg)
                ts.ingestMessage(turnCtx, al, finalMsg)
                if err := ts.agent.Sessions.Save(ts.sessionKey); err != nil {
                        al.emitEvent(
                                EventKindError,
                                ts.eventMeta("runTurn", "turn.error"),
                                ErrorPayload{
                                        Stage:   "session_save",
                                        Message: err.Error(),
                                },
                        )
                        logger.ErrorCF("agent", "Session save failed during finalization",
                                map[string]any{
                                        "session_key": ts.sessionKey,
                                        "agent_id":    ts.agent.ID,
                                        "error":       err.Error(),
                                })
                        return turnResult{status: TurnEndStatusError}, err
                }
                logger.DebugCF("agent", "Session saved after finalization",
                        map[string]any{
                                "session_key": ts.sessionKey,
                        })
        }

        if ts.opts.EnableSummary {
                logger.InfoCF("agent", "Triggering summary/compact after turn",
                        map[string]any{
                                "session_key":    ts.sessionKey,
                                "agent_id":       ts.agent.ID,
                                "context_window": ts.agent.ContextWindow,
                        })
                al.contextManager.Compact(
                        turnCtx,
                        &CompactRequest{
                                SessionKey: ts.sessionKey,
                                Reason:     ContextCompressReasonSummarize,
                                Budget:     ts.agent.ContextWindow,
                        },
                )
        }

        ts.setPhase(TurnPhaseCompleted)

        finalizeDuration := time.Since(finalizeStart)
        logger.InfoCF("agent", "Turn completed",
                map[string]any{
                        "session_key":      ts.sessionKey,
                        "agent_id":         ts.agent.ID,
                        "turn_status":      string(turnStatus),
                        "finalize_ms":      finalizeDuration.Milliseconds(),
                        "content_len":      len(finalContent),
                        "follow_ups":       len(ts.followUps),
                        "summary_enabled":  ts.opts.EnableSummary,
                })

        return turnResult{
                finalContent: finalContent,
                status:       turnStatus,
                followUps:    append([]bus.InboundMessage(nil), ts.followUps...),
        }, nil
}
