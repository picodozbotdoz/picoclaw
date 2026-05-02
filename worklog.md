---
Task ID: 1
Agent: main
Task: Review unit tests and fix failures on wip/deepseekv4_optimized_phase4 branch

Work Log:
- Checked current git state: on branch wip/deepseekv4_optimized_phase4
- Ran unit tests across pkg/providers, pkg/config, pkg/routing — all passed
- Ran agent package tests — identified 3 failing tests:
  - TestProcessMessage_SwitchModelRoutesSubsequentRequestsToSelectedProvider
  - TestProcessMessage_ModelRoutingUsesLightProvider
  - TestProcessMessage_FallbackUsesPerCandidateProvider
  - TestProcessMessage_FallbackUsesActiveProviderWhenCandidateNotRegistered
- Root cause 1: WS 3.2 streaming integration (Phase 3) enabled automatic streaming for openai_compat providers via shouldUseStreaming(), but test mock servers only returned regular JSON responses, not SSE format. ChatStream sent stream:true, got non-SSE response, parseStreamResponse returned empty content.
- Root cause 2: Streaming path bypassed fallback chain — callWithStreaming returned directly without going through p.Fallback.Execute(), breaking model fallback when primary returned 429.
- Root cause 3: callWithStreaming always used exec.llmModel (primary model) even when called from fallback chain with a different model name.

Fixes applied:
1. pkg/agent/agent_test.go:
   - Added writeSSEChatCompletionResponse() helper that writes proper SSE format (data: chunks + [DONE] sentinel)
   - Updated newChatCompletionTestServer() to detect stream:true and respond with SSE
   - Updated newStrictChatCompletionTestServer() to detect stream:true and respond with SSE
   - Updated inline test server in TestProcessMessage_FallbackUsesActiveProviderWhenCandidateNotRegistered to support SSE
2. pkg/agent/pipeline_llm.go:
   - Restructured callLLM closure to integrate streaming with fallback chain
   - Added chatWithFallback closure that uses streaming when available per-candidate
   - Added model parameter to callWithStreaming() so fallback candidates use correct model name
   - Single-provider path still uses streaming when available (no fallback needed)

Stage Summary:
- All 3+ previously failing tests now pass
- Full pkg/agent test suite passes (31.9s)
- All other package tests pass (providers, config, routing, utils, session, seahorse, commands, tools, etc.)
- Key insight: streaming integration must be compatible with fallback chain for production use
