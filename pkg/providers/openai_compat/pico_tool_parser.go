package openai_compat

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

// picoToolUseRe matches the [tool_use: NAME, args: {...}] pattern that
// DeepSeek V4 sometimes outputs as text content instead of structured
// tool_calls. This is the picoclaw internal tool call format leaking
// into the LLM's text output — typically when the model sees [tool_use:]
// patterns in conversation history and mimics them.
//
// Format: [tool_use: tool_name, args: {"key": "value", ...}]
//
// The regex captures:
//   1. tool name (e.g., "exec", "read_file")
//   2. args JSON body (everything between "args: {" and the final "}]")
var picoToolUseRe = regexp.MustCompile(`\[tool_use:\s*(\w+),\s*args:\s*(\{.*?\})\s*\]`)

// HasPicoToolCalls reports whether content contains [tool_use: ...] patterns.
func HasPicoToolCalls(content string) bool {
	return picoToolUseRe.MatchString(content)
}

// ParsePicoToolCalls extracts tool calls from [tool_use: ...] formatted text.
// Returns parsed ToolCall(s) and the remaining text with tool_use blocks removed.
// Falls back gracefully — malformed blocks are skipped, remaining text is preserved.
func ParsePicoToolCalls(content string) ([]protocoltypes.ToolCall, string, error) {
	if !HasPicoToolCalls(content) {
		return nil, content, nil
	}

	var toolCalls []protocoltypes.ToolCall
	var errs []string

	remainingContent := picoToolUseRe.ReplaceAllStringFunc(content, func(match string) string {
		submatch := picoToolUseRe.FindStringSubmatch(match)
		if len(submatch) < 3 {
			errs = append(errs, "malformed [tool_use:] block")
			return match // keep in content if can't parse
		}

		toolName := submatch[1]
		argsJSON := submatch[2]

		// Parse JSON args
		var args map[string]any
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			errs = append(errs, fmt.Sprintf("tool_use %q: invalid args JSON: %v", toolName, err))
			return match // keep in content if JSON is broken
		}

		// Convert args to a clean JSON string for FunctionCall.Arguments
		cleanJSON, err := json.Marshal(args)
		if err != nil {
			errs = append(errs, fmt.Sprintf("tool_use %q: marshal args: %v", toolName, err))
			return match
		}

		tc := protocoltypes.ToolCall{
			ID:   fmt.Sprintf("picotool_%s_%d", toolName, len(toolCalls)),
			Type: "function",
			Function: &protocoltypes.FunctionCall{
				Name:      toolName,
				Arguments: string(cleanJSON),
			},
			Name:      toolName,
			Arguments: args,
		}
		toolCalls = append(toolCalls, tc)

		return "" // Remove from remaining content
	})

	remainingContent = strings.TrimSpace(remainingContent)

	if len(toolCalls) > 0 && len(errs) > 0 {
		logger.WarnCF("provider", "Pico tool_use parser: some blocks could not be parsed",
			map[string]any{"parsed": len(toolCalls), "errors": strings.Join(errs, "; ")})
	}

	var err error
	if len(errs) > 0 && len(toolCalls) == 0 {
		err = fmt.Errorf("pico tool_use parse errors: %s", strings.Join(errs, "; "))
	}

	return toolCalls, remainingContent, err
}
