// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
        "context"
        "encoding/json"
        "errors"
        "fmt"
        "strings"
        "time"

        "github.com/sipeed/picoclaw/pkg/constants"
        "github.com/sipeed/picoclaw/pkg/logger"
        "github.com/sipeed/picoclaw/pkg/providers"
        "github.com/sipeed/picoclaw/pkg/providers/openai_compat"
        "github.com/sipeed/picoclaw/pkg/tokenizer"
)

// CallLLM performs an LLM call with fallback support, hook invocation, and retry logic.
// It handles PreLLM setup, the actual LLM invocation with retry, and AfterLLM processing.
// Returns Control indicating what the coordinator should do next.
func (p *Pipeline) CallLLM(
        ctx context.Context,
        turnCtx context.Context,
        ts *turnState,
        exec *turnExecution,
        iteration int,
) (Control, error) {
        al := p.al
        maxMediaSize := p.Cfg.Agents.Defaults.GetMaxMediaSize()

        // PreLLM: resolve media refs (except on iteration 1 where user media is already resolved)
        if iteration > 1 {
                exec.messages = resolveMediaRefs(exec.messages, p.MediaStore, maxMediaSize)
        }

        // PreLLM: graceful terminal handling
        exec.gracefulTerminal, _ = ts.gracefulInterruptRequested()
        exec.providerToolDefs = ts.agent.Tools.ToProviderDefs()

        // Native web search support
        webSearchEnabled := al.cfg.Tools.IsToolEnabled("web")
        exec.useNativeSearch = webSearchEnabled && al.cfg.Tools.Web.PreferNative &&
                func() bool {
                        if ns, ok := ts.agent.Provider.(providers.NativeSearchCapable); ok {
                                return ns.SupportsNativeSearch()
                        }
                        return false
                }()

        if exec.useNativeSearch {
                filtered := make([]providers.ToolDefinition, 0, len(exec.providerToolDefs))
                for _, td := range exec.providerToolDefs {
                        if td.Function.Name != "web_search" {
                                filtered = append(filtered, td)
                        }
                }
                exec.providerToolDefs = filtered
        }

        exec.callMessages = exec.messages
        if exec.gracefulTerminal {
                exec.callMessages = append(append([]providers.Message(nil), exec.messages...), ts.interruptHintMessage())
                exec.providerToolDefs = nil
                ts.markGracefulTerminalUsed()
        }

        // WS 5.4: When JSON output mode is active, ensure the system/user messages
        // include JSON formatting instructions. DeepSeek V4 and OpenAI require this
        // when response_format is json_object; without it, the API returns an error
        // or produces non-JSON output.
        if ts.agent.ResponseFormat == "json_object" {
                exec.callMessages = ensureJSONInstructions(exec.callMessages)
        }

        exec.llmOpts = map[string]any{
                "max_tokens":       ts.agent.MaxTokens,
                "temperature":      ts.agent.Temperature,
                "prompt_cache_key": ts.agent.ID,
                "user_id":          ts.agent.ID, // DeepSeek V4 KV cache isolation per user_id
        }
        if exec.useNativeSearch {
                exec.llmOpts["native_search"] = true
        }

        // WS 3.1: Dynamic thinking level — resolve per-iteration based on context.
        // The configured ThinkingLevel is the baseline; dynamic mode can switch to
        // non-think after tool execution for cost/latency optimization.
        // The complexity score enables three-tier dynamic thinking: complex tasks
        // get boosted to think-max/xhigh, while simple post-tool iterations use
        // non-think mode for speed.
        dynamicMode := parseDynamicThinkingMode(ts.agent.DynamicThinkingMode)
        steeringResumed := iteration > 1 && len(exec.pendingMessages) > 0 && !exec.postToolCall
        complexity := 0.0
        if ts.agent.Router != nil {
                _, _, complexity = ts.agent.Router.SelectModel(ts.userMessage, exec.history, ts.agent.Model)
        }
        effectiveLevel, reason := resolveThinkingLevelForIteration(
                ts.agent.ThinkingLevel,
                dynamicMode,
                iteration,
                exec.postToolCall,
                steeringResumed,
                exec.isRetry,
                complexity,
        )
        exec.dynamicThinkingLevel = effectiveLevel
        exec.thinkingModeStats = append(exec.thinkingModeStats, ThinkingModeStat{
                Iteration:     iteration,
                ThinkingLevel: effectiveLevel,
                Reason:        reason,
        })

        if effectiveLevel != ThinkingOff {
                if tc, ok := ts.agent.Provider.(providers.ThinkingCapable); ok && tc.SupportsThinking() {
                        exec.llmOpts["thinking_level"] = string(effectiveLevel)
                } else {
                        logger.WarnCF("agent", "thinking_level is set but current provider does not support it, ignoring",
                                map[string]any{"agent_id": ts.agent.ID, "thinking_level": string(effectiveLevel)})
                }
        }
        // DeepSeek V4 strict mode for tool calls (Beta).
        // Validate tool schemas for strict mode compliance before sending.
        // The provider layer also validates (in buildStrictToolsList), but this
        // agent-layer check provides an earlier warning and can disable strict
        // mode entirely if schemas are non-compliant, avoiding 400 API errors.
        if ts.agent.StrictToolCalls {
                if len(exec.providerToolDefs) > 0 {
                        invalidCount := 0
                        for _, td := range exec.providerToolDefs {
                                if td.Function.Parameters != nil {
                                        if schemaMap, ok := td.Function.Parameters.(map[string]any); ok {
                                                if result := openai_compat.ValidateStrictSchema(schemaMap, td.Function.Name); !result.Valid {
                                                        invalidCount++
                                                        if len(result.Errors) > 0 {
                                                                logger.WarnCF("agent", "Strict mode schema validation error",
                                                                        map[string]any{
                                                                                "function": td.Function.Name,
                                                                                "errors":   strings.Join(result.Errors, "; "),
                                                                        })
                                                        }
                                                }
                                        }
                                }
                        }
                        if invalidCount > 0 {
                                logger.WarnCF("agent", "Strict mode: some tool schemas invalid, provider will auto-sanitize",
                                        map[string]any{"invalid_count": invalidCount, "total_tools": len(exec.providerToolDefs)})
                        }
                }
                exec.llmOpts["strict_tool_calls"] = true
        }
        // DeepSeek V4 response format (json_object for guaranteed JSON output).
        if ts.agent.ResponseFormat != "" {
                exec.llmOpts["response_format"] = ts.agent.ResponseFormat
        }
        // DeepSeek V4 Chat Prefix Completion (Beta).
        if ts.agent.PrefixCompletion != "" {
                exec.llmOpts["prefix_completion"] = ts.agent.PrefixCompletion
                if ts.agent.ReasoningPrefix != "" {
                        exec.llmOpts["reasoning_prefix"] = ts.agent.ReasoningPrefix
                }
        }

        exec.llmModel = exec.activeModel

        // BeforeLLM hook
        if p.Hooks != nil {
                llmReq, decision := p.Hooks.BeforeLLM(turnCtx, &LLMHookRequest{
                        Meta:             ts.eventMeta("runTurn", "turn.llm.request"),
                        Context:          cloneTurnContext(ts.turnCtx),
                        Model:            exec.llmModel,
                        Messages:         exec.callMessages,
                        Tools:            exec.providerToolDefs,
                        Options:          exec.llmOpts,
                        GracefulTerminal: exec.gracefulTerminal,
                })
                switch decision.normalizedAction() {
                case HookActionContinue, HookActionModify:
                        if llmReq != nil {
                                exec.llmModel = llmReq.Model
                                exec.callMessages = llmReq.Messages
                                exec.providerToolDefs = llmReq.Tools
                                exec.llmOpts = llmReq.Options
                        }
                case HookActionAbortTurn:
                        exec.abortedByHook = true
                        return ControlBreak, nil
                case HookActionHardAbort:
                        _ = ts.requestHardAbort()
                        exec.abortedByHardAbort = true
                        return ControlBreak, nil
                }
        }

        al.emitEvent(
                EventKindLLMRequest,
                ts.eventMeta("runTurn", "turn.llm.request"),
                LLMRequestPayload{
                        Model:         exec.llmModel,
                        MessagesCount: len(exec.callMessages),
                        ToolsCount:    len(exec.providerToolDefs),
                        MaxTokens:     ts.agent.MaxTokens,
                        Temperature:   ts.agent.Temperature,
                },
        )

        logger.DebugCF("agent", "LLM request",
                map[string]any{
                        "agent_id":          ts.agent.ID,
                        "iteration":         iteration,
                        "model":             exec.llmModel,
                        "messages_count":    len(exec.callMessages),
                        "tools_count":       len(exec.providerToolDefs),
                        "max_tokens":        ts.agent.MaxTokens,
                        "temperature":       ts.agent.Temperature,
                        "system_prompt_len": len(exec.callMessages[0].Content),
                })
        logger.DebugCF("agent", "Full LLM request",
                map[string]any{
                        "iteration":     iteration,
                        "messages_json": formatMessagesForLog(exec.callMessages),
                        "tools_json":    formatToolsForLog(exec.providerToolDefs),
                })

        // LLM call closure with fallback support
        callLLM := func(messagesForCall []providers.Message, toolDefsForCall []providers.ToolDefinition) (*providers.LLMResponse, error) {
                providerCtx, providerCancel := context.WithCancel(turnCtx)
                ts.setProviderCancel(providerCancel)
                defer func() {
                        providerCancel()
                        ts.clearProviderCancel(providerCancel)
                }()

                al.activeRequests.Add(1)
                defer al.activeRequests.Done()

                // WS 3.2: Streaming integration — check if streaming should be used
                // based on StreamingMode config and provider capability.
                useStreaming := shouldUseStreaming(ts.agent, exec.activeProvider)

                // chatWithFallback invokes a single candidate provider, using streaming
                // when available. If streaming fails, the fallback chain will naturally
                // try the next candidate — we do NOT fall through to non-streaming Chat
                // on the same candidate to avoid double-retrying.
                chatWithFallback := func(ctx context.Context, provider, model string) (*providers.LLMResponse, error) {
                        candidateProvider := exec.activeProvider
                        if cp, ok := ts.agent.CandidateProviders[providers.ModelKey(provider, model)]; ok {
                                candidateProvider = cp
                        }
                        // Use streaming if enabled and the candidate supports it.
                        if useStreaming {
                                if sp, ok := candidateProvider.(providers.StreamingProvider); ok {
                                        return callWithStreaming(al, sp, exec, ctx, messagesForCall, toolDefsForCall, ts, model)
                                }
                        }
                        return candidateProvider.Chat(ctx, messagesForCall, toolDefsForCall, model, exec.llmOpts)
                }

                if len(exec.activeCandidates) > 1 && p.Fallback != nil {
                        fbResult, fbErr := p.Fallback.Execute(
                                providerCtx,
                                exec.activeCandidates,
                                chatWithFallback,
                        )
                        if fbErr != nil {
                                return nil, fbErr
                        }
                        if fbResult.Provider != "" && len(fbResult.Attempts) > 0 {
                                logger.InfoCF(
                                        "agent",
                                        fmt.Sprintf("Fallback: succeeded with %s/%s after %d attempts",
                                                fbResult.Provider, fbResult.Model, len(fbResult.Attempts)+1),
                                        map[string]any{"agent_id": ts.agent.ID, "iteration": iteration},
                                )
                        }
                        return fbResult.Response, nil
                }

                // No fallback candidates — single provider path.
                if useStreaming {
                        if sp, ok := exec.activeProvider.(providers.StreamingProvider); ok {
                                return callWithStreaming(al, sp, exec, providerCtx, messagesForCall, toolDefsForCall, ts, exec.llmModel)
                        }
                }
                return exec.activeProvider.Chat(providerCtx, messagesForCall, toolDefsForCall, exec.llmModel, exec.llmOpts)
        }

        // Retry loop
        var err error
        maxRetries := 2
        for retry := 0; retry <= maxRetries; retry++ {
                exec.response, err = callLLM(exec.callMessages, exec.providerToolDefs)
                if err == nil {
                        break
                }
                if ts.hardAbortRequested() && errors.Is(err, context.Canceled) {
                        _ = ts.requestHardAbort()
                        exec.abortedByHardAbort = true
                        return ControlBreak, nil
                }

                // Retry without media if vision is unsupported
                if hasMediaRefs(exec.callMessages) && isVisionUnsupportedError(err) && retry < maxRetries {
                        al.emitEvent(
                                EventKindLLMRetry,
                                ts.eventMeta("runTurn", "turn.llm.retry"),
                                LLMRetryPayload{
                                        Attempt:    retry + 1,
                                        MaxRetries: maxRetries,
                                        Reason:     "vision_unsupported",
                                        Error:      err.Error(),
                                        Backoff:    0,
                                },
                        )
                        logger.WarnCF("agent", "Vision unsupported, retrying without media", map[string]any{
                                "error": err.Error(),
                                "retry": retry,
                        })
                        exec.callMessages = stripMessageMedia(exec.callMessages)
                        if !ts.opts.NoHistory {
                                exec.history = stripMessageMedia(exec.history)
                                ts.agent.Sessions.SetHistory(ts.sessionKey, exec.history)
                                for i := range ts.persistedMessages {
                                        ts.persistedMessages[i].Media = nil
                                }
                                ts.refreshRestorePointFromSession(ts.agent)
                        }
                        continue
                }

                errMsg := strings.ToLower(err.Error())
                isTimeoutError := errors.Is(err, context.DeadlineExceeded) ||
                        strings.Contains(errMsg, "deadline exceeded") ||
                        strings.Contains(errMsg, "client.timeout") ||
                        strings.Contains(errMsg, "timed out") ||
                        strings.Contains(errMsg, "timeout exceeded")

                isContextError := !isTimeoutError && (strings.Contains(errMsg, "context_length_exceeded") ||
                        strings.Contains(errMsg, "context window") ||
                        strings.Contains(errMsg, "context_window") ||
                        strings.Contains(errMsg, "maximum context length") ||
                        strings.Contains(errMsg, "token limit") ||
                        strings.Contains(errMsg, "too many tokens") ||
                        strings.Contains(errMsg, "max_tokens") ||
                        strings.Contains(errMsg, "invalidparameter") ||
                        strings.Contains(errMsg, "prompt is too long") ||
                        strings.Contains(errMsg, "request too large"))

                if isTimeoutError && retry < maxRetries {
                        backoff := time.Duration(retry+1) * 5 * time.Second
                        al.emitEvent(
                                EventKindLLMRetry,
                                ts.eventMeta("runTurn", "turn.llm.retry"),
                                LLMRetryPayload{
                                        Attempt:    retry + 1,
                                        MaxRetries: maxRetries,
                                        Reason:     "timeout",
                                        Error:      err.Error(),
                                        Backoff:    backoff,
                                },
                        )
                        logger.WarnCF("agent", "Timeout error, retrying after backoff", map[string]any{
                                "error":   err.Error(),
                                "retry":   retry,
                                "backoff": backoff.String(),
                        })
                        if sleepErr := sleepWithContext(turnCtx, backoff); sleepErr != nil {
                                if ts.hardAbortRequested() {
                                        _ = ts.requestHardAbort()
                                        return ControlBreak, nil
                                }
                                err = sleepErr
                                break
                        }
                        continue
                }

                if isContextError && retry < maxRetries && !ts.opts.NoHistory {
                        al.emitEvent(
                                EventKindLLMRetry,
                                ts.eventMeta("runTurn", "turn.llm.retry"),
                                LLMRetryPayload{
                                        Attempt:    retry + 1,
                                        MaxRetries: maxRetries,
                                        Reason:     "context_limit",
                                        Error:      err.Error(),
                                },
                        )
                        logger.WarnCF(
                                "agent",
                                "Context window error detected, attempting compression",
                                map[string]any{
                                        "error": err.Error(),
                                        "retry": retry,
                                },
                        )

                        if retry == 0 && !constants.IsInternalChannel(ts.channel) {
                                al.bus.PublishOutbound(ctx, outboundMessageForTurn(
                                        ts,
                                        "Context window exceeded. Compressing history and retrying...",
                                ))
                        }

                        if compactErr := p.ContextManager.Compact(ctx, &CompactRequest{
                                SessionKey: ts.sessionKey,
                                Reason:     ContextCompressReasonRetry,
                                Budget:     ts.agent.ContextWindow,
                        }); compactErr != nil {
                                logger.WarnCF("agent", "Context overflow compact failed", map[string]any{
                                        "session_key": ts.sessionKey,
                                        "error":       compactErr.Error(),
                                })
                        }
                        ts.refreshRestorePointFromSession(ts.agent)
                        if asmResp, asmErr := p.ContextManager.Assemble(ctx, &AssembleRequest{
                                SessionKey: ts.sessionKey,
                                Budget:     ts.agent.ContextWindow,
                                MaxTokens:  ts.agent.MaxTokens,
                        }); asmErr == nil && asmResp != nil {
                                exec.history = asmResp.History
                                exec.summary = asmResp.Summary
                        }
                        exec.messages = ts.agent.ContextBuilder.BuildMessagesFromPrompt(
                                promptBuildRequestForTurn(ts, exec.history, exec.summary, "", nil),
                        )
                        exec.callMessages = exec.messages
                        if exec.gracefulTerminal {
                                msgs := append([]providers.Message(nil), exec.messages...)
                                exec.callMessages = append(msgs, ts.interruptHintMessage())
                        }
                        continue
                }
                break
        }

        if err != nil {
                al.emitEvent(
                        EventKindError,
                        ts.eventMeta("runTurn", "turn.error"),
                        ErrorPayload{
                                Stage:   "llm",
                                Message: err.Error(),
                        },
                )
                logger.ErrorCF("agent", "LLM call failed",
                        map[string]any{
                                "agent_id":  ts.agent.ID,
                                "iteration": iteration,
                                "model":     exec.llmModel,
                                "error":     err.Error(),
                        })
                return ControlBreak, fmt.Errorf("LLM call failed after retries: %w", err)
        }

        // AfterLLM hook
        if p.Hooks != nil {
                llmResp, decision := p.Hooks.AfterLLM(turnCtx, &LLMHookResponse{
                        Meta:     ts.eventMeta("runTurn", "turn.llm.response"),
                        Context:  cloneTurnContext(ts.turnCtx),
                        Model:    exec.llmModel,
                        Response: exec.response,
                })
                switch decision.normalizedAction() {
                case HookActionContinue, HookActionModify:
                        if llmResp != nil && llmResp.Response != nil {
                                exec.response = llmResp.Response
                        }
                case HookActionAbortTurn:
                        exec.abortedByHook = true
                        return ControlBreak, nil
                case HookActionHardAbort:
                        _ = ts.requestHardAbort()
                        exec.abortedByHardAbort = true
                        return ControlBreak, nil
                }
        }

        // Save finishReason to turnState for SubTurn truncation detection
        if innerTS := turnStateFromContext(ctx); innerTS != nil {
                innerTS.SetLastFinishReason(exec.response.FinishReason)
                if exec.response.Usage != nil {
                        innerTS.SetLastUsage(exec.response.Usage)
                }
        }

        reasoningContent := responseReasoningContent(exec.response)
        shouldPublishPicoToolCallInterim := ts.channel == "pico" && len(exec.response.ToolCalls) > 0
        if shouldPublishPicoToolCallInterim {
                // Pico tool-call turns publish their reasoning/content/tool summary as a
                // structured sequence after the tool-call payload is normalized below.
        } else if ts.channel == "pico" {
                go al.publishPicoReasoning(turnCtx, reasoningContent, ts.chatID)
        } else {
                go al.handleReasoning(
                        turnCtx,
                        reasoningContent,
                        ts.channel,
                        al.targetReasoningChannelID(ts.channel),
                )
        }
        al.emitEvent(
                EventKindLLMResponse,
                ts.eventMeta("runTurn", "turn.llm.response"),
                LLMResponsePayload{
                        ContentLen:   len(exec.response.Content),
                        ToolCalls:    len(exec.response.ToolCalls),
                        HasReasoning: exec.response.Reasoning != "" || exec.response.ReasoningContent != "",
                },
        )

        llmResponseFields := map[string]any{
                "agent_id":       ts.agent.ID,
                "iteration":      iteration,
                "content_chars":  len(exec.response.Content),
                "tool_calls":     len(exec.response.ToolCalls),
                "reasoning":      exec.response.Reasoning,
                "target_channel": al.targetReasoningChannelID(ts.channel),
                "channel":        ts.channel,
        }
        if exec.response.Usage != nil {
                llmResponseFields["prompt_tokens"] = exec.response.Usage.PromptTokens
                llmResponseFields["completion_tokens"] = exec.response.Usage.CompletionTokens
                llmResponseFields["total_tokens"] = exec.response.Usage.TotalTokens
        }
        logger.DebugCF("agent", "LLM response", llmResponseFields)

        // WS 4.3: Propagate API usage data to ContextUsage for accurate tracking.
        // This replaces heuristic estimates with actual counts from the provider.
        if exec.response.Usage != nil {
                usage := computeContextUsage(ts.agent, ts.sessionKey)
                if usage != nil {
                        UpdateUsageFromAPI(usage, exec.response.Usage)
                        ts.setContextUsage(usage)
                }
                // Record usage in the session cost tracker for /cost reporting.
                if ts.agent.CostTracker != nil {
                        ts.agent.CostTracker.RecordLLMUsage(exec.llmModel, exec.response.Usage)
                }
                // Token estimator feedback loop: refine the per-model tokens-per-character
                // ratio from actual API usage. This improves the accuracy of future
                // EstimateMessageTokens calls when API usage data isn't available yet,
                // reducing the 20-40% error rate of the generic chars*2/5 heuristic.
                if exec.response.Usage.PromptTokens > 0 && len(exec.callMessages) > 0 {
                        totalInputChars := 0
                        for _, m := range exec.callMessages {
                                totalInputChars += len(m.Content) + len(m.ReasoningContent)
                        }
                        if totalInputChars > 0 {
                                ratio := float64(exec.response.Usage.PromptTokens) / float64(totalInputChars)
                                tokenizer.UpdateModelTokenRate(exec.llmModel, ratio)
                        }
                }
        }

        // No-tool-call path: steering check and direct response
        if len(exec.response.ToolCalls) == 0 || exec.gracefulTerminal {
                responseContent := exec.response.Content
                if responseContent == "" && exec.response.ReasoningContent != "" && ts.channel != "pico" {
                        responseContent = exec.response.ReasoningContent
                }
                // WS 5.4: Validate JSON response when response_format is json_object
                if ts.agent.ResponseFormat == "json_object" && !validateJSONResponse(responseContent) {
                        logger.WarnCF("agent", "response_format=json_object but response is not valid JSON",
                                map[string]any{
                                        "agent_id":       ts.agent.ID,
                                        "iteration":      iteration,
                                        "content_length": len(responseContent),
                                })
                }
                if steerMsgs := al.dequeueSteeringMessagesForScope(ts.sessionKey); len(steerMsgs) > 0 {
                        logger.InfoCF("agent", "Steering arrived after direct LLM response; continuing turn",
                                map[string]any{
                                        "agent_id":       ts.agent.ID,
                                        "iteration":      iteration,
                                        "steering_count": len(steerMsgs),
                                })
                        exec.pendingMessages = append(exec.pendingMessages, steerMsgs...)
                        return ControlContinue, nil
                }
                exec.finalContent = responseContent
                logger.InfoCF("agent", "LLM response without tool calls (direct answer)",
                        map[string]any{
                                "agent_id":      ts.agent.ID,
                                "iteration":     iteration,
                                "content_chars": len(exec.finalContent),
                        })
                return ControlBreak, nil
        }

        // Tool-call path: normalize and prepare for tool execution
        exec.normalizedToolCalls = make([]providers.ToolCall, 0, len(exec.response.ToolCalls))
        for _, tc := range exec.response.ToolCalls {
                exec.normalizedToolCalls = append(exec.normalizedToolCalls, providers.NormalizeToolCall(tc))
        }

        toolNames := make([]string, 0, len(exec.normalizedToolCalls))
        for _, tc := range exec.normalizedToolCalls {
                toolNames = append(toolNames, tc.Name)
        }
        logger.InfoCF("agent", "LLM requested tool calls",
                map[string]any{
                        "agent_id":  ts.agent.ID,
                        "tools":     toolNames,
                        "count":     len(exec.normalizedToolCalls),
                        "iteration": iteration,
                })

        exec.allResponsesHandled = len(exec.normalizedToolCalls) > 0
        assistantMsg := providers.Message{
                Role:             "assistant",
                Content:          exec.response.Content,
                ReasoningContent: reasoningContent,
        }
        for _, tc := range exec.normalizedToolCalls {
                argumentsJSON, _ := json.Marshal(tc.Arguments)
                toolFeedbackExplanation := toolFeedbackExplanationForToolCall(
                        exec.response,
                        tc,
                        exec.messages,
                )
                extraContent := tc.ExtraContent
                if strings.TrimSpace(toolFeedbackExplanation) != "" {
                        if extraContent == nil {
                                extraContent = &providers.ExtraContent{}
                        }
                        extraContent.ToolFeedbackExplanation = toolFeedbackExplanation
                }
                thoughtSignature := ""
                if tc.Function != nil {
                        thoughtSignature = tc.Function.ThoughtSignature
                }
                assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, providers.ToolCall{
                        ID:   tc.ID,
                        Type: "function",
                        Name: tc.Name,
                        Function: &providers.FunctionCall{
                                Name:             tc.Name,
                                Arguments:        string(argumentsJSON),
                                ThoughtSignature: thoughtSignature,
                        },
                        ExtraContent:     extraContent,
                        ThoughtSignature: thoughtSignature,
                })
        }
        exec.messages = append(exec.messages, assistantMsg)
        if !ts.opts.NoHistory {
                ts.agent.Sessions.AddFullMessage(ts.sessionKey, assistantMsg)
                ts.recordPersistedMessage(assistantMsg)
                ts.ingestMessage(turnCtx, al, assistantMsg)
        }
        if shouldPublishPicoToolCallInterim {
                al.publishPicoToolCallInterim(
                        turnCtx,
                        ts,
                        reasoningContent,
                        exec.response.Content,
                        assistantMsg.ToolCalls,
                )
        }

        return ControlToolLoop, nil
}

// shouldUseStreaming determines whether streaming should be used for an LLM call
// based on the agent's StreamingMode configuration and provider capability.
// Returns true when streaming should be used, false otherwise.
func shouldUseStreaming(agent *AgentInstance, provider providers.LLMProvider) bool {
        mode := agent.StreamingMode
        switch strings.ToLower(strings.TrimSpace(mode)) {
        case "always":
                // Force streaming — will fail at call time if provider doesn't support it
                _, ok := provider.(providers.StreamingProvider)
                return ok
        case "never":
                return false
        default: // "auto" or empty
                // Auto: use streaming if provider supports it
                _, ok := provider.(providers.StreamingProvider)
                return ok
        }
}

// callWithStreaming executes a streaming LLM call with onChunk callback.
// The onChunk callback is used to emit partial content events for real-time
// display in channels that support it. The final LLMResponse is returned
// with the same structure as a non-streaming call for compatibility.
// The model parameter specifies which model to request; when called from the
// fallback path it differs from exec.llmModel (which is the primary model).
//
// If the stream fails after partial content has been received, the function
// returns a best-effort response with the accumulated text and a heuristic
// usage estimate, rather than propagating the error. This prevents total
// loss of long-running streaming responses when the connection is interrupted
// after the model has already generated substantial content.
func callWithStreaming(
        al *AgentLoop,
        sp providers.StreamingProvider,
        exec *turnExecution,
        ctx context.Context,
        messages []providers.Message,
        tools []providers.ToolDefinition,
        ts *turnState,
        model string,
) (*providers.LLMResponse, error) {
        // Request include_usage in streaming mode for accurate token counts.
        // This is passed via llmOpts so the provider can add stream_options.
        opts := exec.llmOpts
        if opts == nil {
                opts = make(map[string]any)
        }
        opts["stream_include_usage"] = true

        // Track accumulated content for graceful fallback on stream interruption.
        var accumulatedText string

        // onChunk receives accumulated text and can be used for real-time
        // partial content display in channels that support it.
        onChunk := func(accumulated string) {
                accumulatedText = accumulated
                // Emit partial content for real-time display.
                // Channels like Telegram and Pico support partial updates.
                // This is a lightweight callback — no heavy processing here.
                if ts.channel == "pico" || ts.channel == "telegram" {
                        al.emitEvent(
                                EventKindLLMDelta,
                                ts.eventMeta("runTurn", "turn.llm.delta"),
                                LLMDeltaPayload{
                                        ContentDeltaLen: len(accumulated),
                                },
                        )
                }
        }

        resp, err := sp.ChatStream(ctx, messages, tools, model, opts, onChunk)
        if err != nil {
                // Graceful fallback: if the stream was interrupted after generating
                // substantial content, return a partial response rather than an error.
                // This handles common failure modes like connection drops, timeouts,
                // and provider-side interruptions during long streaming responses.
                if accumulatedText != "" {
                        logger.WarnCF("agent", "Stream failed, returning partial response",
                                map[string]any{
                                        "error":       err.Error(),
                                        "partial_len": len(accumulatedText),
                                        "model":       model,
                                })
                        // Estimate prompt tokens from the input messages
                        promptEstimate := 0
                        for _, m := range messages {
                                promptEstimate += tokenizer.EstimateMessageTokens(m)
                        }
                        return &providers.LLMResponse{
                                Content:      accumulatedText,
                                FinishReason: "interrupted",
                                Usage: &providers.UsageInfo{
                                        PromptTokens:     promptEstimate,
                                        CompletionTokens: tokenizer.EstimateMessageTokens(providers.Message{Content: accumulatedText}),
                                },
                        }, nil
                }
                return nil, err
        }

        // Log streaming-specific usage data if available
        if resp.Usage != nil {
                logger.DebugCF("agent", "Streaming LLM response usage",
                        map[string]any{
                                "agent_id":         ts.agent.ID,
                                "prompt_tokens":    resp.Usage.PromptTokens,
                                "completion_tokens": resp.Usage.CompletionTokens,
                                "total_tokens":     resp.Usage.TotalTokens,
                                "cache_hit_tokens": resp.Usage.PromptCacheHitTokens,
                        })
        }

        return resp, nil
}

// ensureJSONInstructions checks if the messages already contain JSON formatting
// instructions. If not, it appends a JSON instruction note to the system message
// or adds a system message with JSON instructions. This is required by DeepSeek V4
// and OpenAI when response_format is json_object.
const jsonInstructionSuffix = "\n\nIMPORTANT: You must respond with valid JSON only. Do not include any text outside the JSON structure."

func ensureJSONInstructions(messages []providers.Message) []providers.Message {
        if len(messages) == 0 {
                return messages
        }

        // Check if any message already contains JSON instructions
        for _, msg := range messages {
                if strings.Contains(strings.ToLower(msg.Content), "json") &&
                        (strings.Contains(strings.ToLower(msg.Content), "respond") ||
                                strings.Contains(strings.ToLower(msg.Content), "output") ||
                                strings.Contains(strings.ToLower(msg.Content), "format") ||
                                strings.Contains(strings.ToLower(msg.Content), "return")) {
                        return messages // Already has JSON instructions
                }
        }

        // Append JSON instruction to the first system message if present
        for i, msg := range messages {
                if msg.Role == "system" {
                        cloned := msg
                        cloned.Content = cloned.Content + jsonInstructionSuffix
                        result := make([]providers.Message, len(messages))
                        copy(result, messages)
                        result[i] = cloned
                        return result
                }
        }

        // No system message found; prepend one with JSON instructions
        jsonSystemMsg := providers.Message{
                Role:    "system",
                Content: "You must respond with valid JSON only. Do not include any text outside the JSON structure.",
        }
        result := make([]providers.Message, 0, len(messages)+1)
        result = append(result, jsonSystemMsg)
        result = append(result, messages...)
        return result
}

// validateJSONResponse checks if the LLM response content is valid JSON when
// response_format is json_object. If validation fails, it logs a warning and
// returns false. The caller can decide how to handle invalid JSON responses.
func validateJSONResponse(content string) bool {
        content = strings.TrimSpace(content)
        if content == "" {
                return false
        }
        // Try to parse as JSON — must be an object or array at the top level
        var parsed any
        if err := json.Unmarshal([]byte(content), &parsed); err != nil {
                return false
        }
        // Valid JSON: accept both objects and arrays
        return true
}
