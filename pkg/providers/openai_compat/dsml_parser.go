package openai_compat

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

// dsmlToolCallsRe matches the outer <|DSML|tool_calls>...</|DSML|tool_calls> block.
var dsmlToolCallsRe = regexp.MustCompile(`(?s)<\|DSML\|tool_calls>(.*?)</\|DSML\|tool_calls>`)

// dsmlInvokeRe matches each <|DSML|invoke name="...">...</|DSML|invoke> block within a tool_calls block.
var dsmlInvokeRe = regexp.MustCompile(`(?s)<\|DSML\|invoke name="([^"]+)">(.*?)</\|DSML\|invoke>`)

// dsmlParamRe matches each <|DSML|parameter name="..." string="true|false">VALUE</|DSML|parameter>.
var dsmlParamRe = regexp.MustCompile(`(?s)<\|DSML\|parameter name="([^"]+)" string="([^"]+)">(.*?)</\|DSML\|parameter>`)

// HasDSMLToolCalls reports whether the content contains DSML-formatted tool calls.
func HasDSMLToolCalls(content string) bool {
	return dsmlToolCallsRe.MatchString(content)
}

// ParseDSMLToolCalls extracts tool calls from DSML-formatted text content.
// It returns the parsed ToolCall slice and the remaining text content with DSML blocks removed.
// If the content contains no DSML markers, it returns an empty slice and the original content.
// Malformed DSML blocks are skipped with a best-effort approach rather than returning errors.
func ParseDSMLToolCalls(content string) ([]protocoltypes.ToolCall, string, error) {
	if !HasDSMLToolCalls(content) {
		return nil, content, nil
	}

	var toolCalls []protocoltypes.ToolCall
	var parseErrors []string

	// Remove DSML blocks from content to get remaining text
	remainingContent := dsmlToolCallsRe.ReplaceAllStringFunc(content, func(match string) string {
		// Extract inner content of the tool_calls block
		submatch := dsmlToolCallsRe.FindStringSubmatch(match)
		if len(submatch) < 2 {
			parseErrors = append(parseErrors, "failed to extract tool_calls block content")
			return ""
		}
		innerContent := submatch[1]

		// Parse each invoke block
		invokeMatches := dsmlInvokeRe.FindAllStringSubmatch(innerContent, -1)
		for _, invokeMatch := range invokeMatches {
			if len(invokeMatch) < 3 {
				parseErrors = append(parseErrors, "malformed invoke block")
				continue
			}

			toolName := invokeMatch[1]
			invokeBody := invokeMatch[2]

			args, err := parseDSMLParameters(invokeBody)
			if err != nil {
				parseErrors = append(parseErrors, fmt.Sprintf("invoke %q: %v", toolName, err))
				continue
			}

			argsJSON, err := json.Marshal(args)
			if err != nil {
				parseErrors = append(parseErrors, fmt.Sprintf("invoke %q: marshal args: %v", toolName, err))
				continue
			}

			tc := protocoltypes.ToolCall{
				ID:   fmt.Sprintf("dsml_%s", toolName),
				Type: "function",
				Function: &protocoltypes.FunctionCall{
					Name:      toolName,
					Arguments: string(argsJSON),
				},
				Name:      toolName,
				Arguments: args,
			}
			toolCalls = append(toolCalls, tc)
		}

		return "" // Remove DSML block from remaining content
	})

	// Clean up remaining content
	remainingContent = strings.TrimSpace(remainingContent)

	var err error
	if len(parseErrors) > 0 {
		err = fmt.Errorf("DSML parse warnings: %s", strings.Join(parseErrors, "; "))
	}

	return toolCalls, remainingContent, err
}

// parseDSMLParameters extracts parameters from the body of an <|DSML|invoke> block.
func parseDSMLParameters(invokeBody string) (map[string]any, error) {
	args := make(map[string]any)
	var errs []string

	paramMatches := dsmlParamRe.FindAllStringSubmatch(invokeBody, -1)
	for _, pm := range paramMatches {
		if len(pm) < 4 {
			continue
		}

		paramName := pm[1]
		isString := pm[2] == "true"
		rawValue := pm[3]

		if isString {
			// String parameter: use the raw value directly
			args[paramName] = rawValue
		} else {
			// Non-string parameter: parse as JSON
			parsed, err := parseJSONValue(rawValue)
			if err != nil {
				errs = append(errs, fmt.Sprintf("param %q: %v", paramName, err))
				// Fall back to raw string value
				args[paramName] = rawValue
				continue
			}
			args[paramName] = parsed
		}
	}

	var err error
	if len(errs) > 0 {
		err = fmt.Errorf("parameter parse errors: %s", strings.Join(errs, "; "))
	}

	return args, err
}

// parseJSONValue attempts to parse a value as JSON, falling back to
// intelligent type inference for common non-quoted values.
func parseJSONValue(raw string) (any, error) {
	raw = strings.TrimSpace(raw)

	// Try standard JSON parsing first
	var val any
	if err := json.Unmarshal([]byte(raw), &val); err == nil {
		return val, nil
	}

	// Fallback: try common unquoted types

	// Boolean
	if raw == "true" {
		return true, nil
	}
	if raw == "false" {
		return false, nil
	}

	// Null
	if raw == "null" {
		return nil, nil
	}

	// Number (integer or float)
	if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return v, nil
	}
	if v, err := strconv.ParseFloat(raw, 64); err == nil {
		return v, nil
	}

	return nil, fmt.Errorf("cannot parse %q as JSON", raw)
}
