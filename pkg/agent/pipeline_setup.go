// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
        "context"
        "strings"

        "github.com/sipeed/picoclaw/pkg/logger"
        "github.com/sipeed/picoclaw/pkg/providers"
)

// SetupTurn extracts the one-time initialization phase, returning a
// turnExecution populated with history, messages, and candidate selection.
// It replaces lines 56-145 of the original runTurn.
func (p *Pipeline) SetupTurn(ctx context.Context, ts *turnState) (*turnExecution, error) {
        cfg := p.Cfg
        maxMediaSize := cfg.Agents.Defaults.GetMaxMediaSize()

        var history []providers.Message
        var summary string
        if !ts.opts.NoHistory {
                if resp, err := p.ContextManager.Assemble(ctx, &AssembleRequest{
                        SessionKey: ts.sessionKey,
                        Budget:     ts.agent.ContextWindow,
                        MaxTokens:  ts.agent.MaxTokens,
                }); err == nil && resp != nil {
                        history = resp.History
                        summary = resp.Summary
                }
        }
        ts.captureRestorePoint(history, summary)

        messages := ts.agent.ContextBuilder.BuildMessagesFromPrompt(
                promptBuildRequestForTurn(ts, history, summary, ts.userMessage, ts.media),
        )

        messages = resolveMediaRefs(messages, p.MediaStore, maxMediaSize)

        if !ts.opts.NoHistory {
                toolDefs := ts.agent.Tools.ToProviderDefs()
                var needCompress bool
                var compressReason string

                // Resolve the active model for model-aware token estimation.
                // The model name is used to look up per-model tokens-per-character
                // ratios learned from prior API usage, reducing the 20-40% error
                // rate of the generic chars*2/5 heuristic.
                activeModel := ts.agent.Model

                // WS 3.3: Use partition-based budget check when ContextPartition is configured.
                // This replaces the flat isOverContextBudget with per-partition enforcement,
                // enabling targeted compression when a specific partition (e.g., history) overflows.
                if ts.agent.ContextPartition != nil {
                        historyTokens := 0
                        for _, m := range messages {
                                historyTokens += EstimateMessageTokensForModel(m, activeModel)
                        }
                        systemEstimate := 0
                        if ts.agent.ContextBuilder != nil {
                                systemEstimate = ts.agent.ContextBuilder.EstimateSystemTokens(
                                        ts.agent.Sessions.GetSummary(ts.sessionKey),
                                        nil,
                                )
                        }
                        toolTokens := EstimateToolDefsTokensForModel(toolDefs, activeModel)
                        injectedTokens := 0
                        if ts.agent.InjectedContext != nil {
                                injectedTokens = ts.agent.InjectedContext.TotalTokens()
                        }
                        overflow, partition := isOverPartitionBudget(
                                ts.agent.ContextPartition,
                                ts.agent.ContextWindow,
                                historyTokens,
                                systemEstimate,
                                injectedTokens,
                                toolTokens,
                                ts.agent.MaxTokens,
                                activeModel,
                        )
                        if overflow {
                                needCompress = true
                                compressReason = "partition overflow: " + partition
                        }
                } else {
                        needCompress = isOverContextBudget(ts.agent.ContextWindow, messages, toolDefs, ts.agent.MaxTokens, activeModel)
                        if needCompress {
                                compressReason = "flat budget exceeded"
                        }
                }

                if needCompress {
                        logger.WarnCF("agent", "Proactive compression: "+compressReason,
                                map[string]any{"session_key": ts.sessionKey})
                        if err := p.ContextManager.Compact(ctx, &CompactRequest{
                                SessionKey: ts.sessionKey,
                                Reason:     ContextCompressReasonProactive,
                                Budget:     ts.agent.ContextWindow,
                                Partition:  compressReason,
                                Model:      activeModel,
                        }); err != nil {
                                logger.WarnCF("agent", "Proactive compact failed", map[string]any{
                                        "session_key": ts.sessionKey,
                                        "error":       err.Error(),
                                })
                        }
                        ts.refreshRestorePointFromSession(ts.agent)
                        if resp, err := p.ContextManager.Assemble(ctx, &AssembleRequest{
                                SessionKey: ts.sessionKey,
                                Budget:     ts.agent.ContextWindow,
                                MaxTokens:  ts.agent.MaxTokens,
                        }); err == nil && resp != nil {
                                history = resp.History
                                summary = resp.Summary
                        }
                        messages = ts.agent.ContextBuilder.BuildMessagesFromPrompt(
                                promptBuildRequestForTurn(ts, history, summary, ts.userMessage, ts.media),
                        )
                        messages = resolveMediaRefs(messages, p.MediaStore, maxMediaSize)
                }
        }

        if !ts.opts.NoHistory && (strings.TrimSpace(ts.userMessage) != "" || len(ts.media) > 0) {
                rootMsg := userPromptMessage(ts.userMessage, ts.media)
                if len(rootMsg.Media) > 0 {
                        ts.agent.Sessions.AddFullMessage(ts.sessionKey, rootMsg)
                } else {
                        ts.agent.Sessions.AddMessage(ts.sessionKey, rootMsg.Role, rootMsg.Content)
                }
                ts.recordPersistedMessage(rootMsg)
                ts.ingestMessage(ctx, p.al, rootMsg)
        }

        activeCandidates, activeModel, usedLight := p.al.selectCandidates(ts.agent, ts.userMessage, messages)
        activeProvider := ts.agent.Provider
        if usedLight && ts.agent.LightProvider != nil {
                activeProvider = ts.agent.LightProvider
        }

        exec := newTurnExecution(
                ts.agent,
                ts.opts,
                history,
                summary,
                messages,
        )
        exec.activeCandidates = activeCandidates
        exec.activeModel = activeModel
        exec.activeProvider = activeProvider
        exec.usedLight = usedLight

        return exec, nil
}
