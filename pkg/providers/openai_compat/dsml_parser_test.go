package openai_compat

import (
        "encoding/json"
        "testing"
)

func TestHasDSMLToolCalls(t *testing.T) {
        tests := []struct {
                name    string
                content string
                want    bool
        }{
                {"no dsml", "Hello world", false},
                {"with dsml", `<|DSML|tool_calls><|DSML|invoke name="fn"></|DSML|invoke></|DSML|tool_calls>`, true},
                {"dsml in thinking", `<think>reasoning</think><|DSML|tool_calls><|DSML|invoke name="fn"></|DSML|invoke></|DSML|tool_calls>`, true},
                {"partial marker", `<|DSML|tool`, false},
        }

        for _, tt := range tests {
                t.Run(tt.name, func(t *testing.T) {
                        if got := HasDSMLToolCalls(tt.content); got != tt.want {
                                t.Errorf("HasDSMLToolCalls() = %v, want %v", got, tt.want)
                        }
                })
        }
}

func TestParseDSMLToolCalls_SingleToolCall(t *testing.T) {
        content := `<|DSML|tool_calls>
<|DSML|invoke name="get_weather">
<|DSML|parameter name="location" string="true">San Francisco, CA</|DSML|parameter>
<|DSML|parameter name="unit" string="true">celsius</|DSML|parameter>
</|DSML|invoke>
</|DSML|tool_calls>`

        toolCalls, remaining, err := ParseDSMLToolCalls(content)
        if err != nil {
                t.Fatalf("ParseDSMLToolCalls() error = %v", err)
        }

        if len(toolCalls) != 1 {
                t.Fatalf("len(toolCalls) = %d, want 1", len(toolCalls))
        }

        tc := toolCalls[0]
        if tc.Name != "get_weather" {
                t.Errorf("Name = %q, want %q", tc.Name, "get_weather")
        }
        if tc.Function == nil {
                t.Fatal("Function is nil")
        }
        if tc.Function.Name != "get_weather" {
                t.Errorf("Function.Name = %q, want %q", tc.Function.Name, "get_weather")
        }

        // Check arguments
        loc, ok := tc.Arguments["location"].(string)
        if !ok || loc != "San Francisco, CA" {
                t.Errorf("Arguments[location] = %v, want %q", tc.Arguments["location"], "San Francisco, CA")
        }
        unit, ok := tc.Arguments["unit"].(string)
        if !ok || unit != "celsius" {
                t.Errorf("Arguments[unit] = %v, want %q (string param)", tc.Arguments["unit"], "celsius")
        }

        if remaining != "" {
                t.Errorf("remaining = %q, want empty", remaining)
        }
}

func TestParseDSMLToolCalls_MultipleToolCalls(t *testing.T) {
        content := `<|DSML|tool_calls>
<|DSML|invoke name="read_file">
<|DSML|parameter name="path" string="true">/tmp/test.go</|DSML|parameter>
</|DSML|invoke>
<|DSML|invoke name="exec">
<|DSML|parameter name="command" string="true">go test</|DSML|parameter>
<|DSML|parameter name="timeout" string="false">30</|DSML|parameter>
</|DSML|invoke>
</|DSML|tool_calls>`

        toolCalls, _, err := ParseDSMLToolCalls(content)
        if err != nil {
                t.Fatalf("ParseDSMLToolCalls() error = %v", err)
        }

        if len(toolCalls) != 2 {
                t.Fatalf("len(toolCalls) = %d, want 2", len(toolCalls))
        }

        if toolCalls[0].Name != "read_file" {
                t.Errorf("toolCalls[0].Name = %q, want %q", toolCalls[0].Name, "read_file")
        }
        if toolCalls[1].Name != "exec" {
                t.Errorf("toolCalls[1].Name = %q, want %q", toolCalls[1].Name, "exec")
        }

        // Check integer argument (JSON numbers parse as float64 by default)
        timeout, ok := toolCalls[1].Arguments["timeout"].(float64)
        if !ok || timeout != 30 {
                t.Errorf("Arguments[timeout] = %v, want 30", toolCalls[1].Arguments["timeout"])
        }
}

func TestParseDSMLToolCalls_NestedJSONParameters(t *testing.T) {
        content := `<|DSML|tool_calls>
<|DSML|invoke name="update_config">
<|DSML|parameter name="name" string="true">my_config</|DSML|parameter>
<|DSML|parameter name="settings" string="false">{"debug": true, "level": 5}</|DSML|parameter>
<|DSML|parameter name="tags" string="false">["prod", "us-west"]</|DSML|parameter>
</|DSML|invoke>
</|DSML|tool_calls>`

        toolCalls, _, err := ParseDSMLToolCalls(content)
        if err != nil {
                t.Fatalf("ParseDSMLToolCalls() error = %v", err)
        }

        if len(toolCalls) != 1 {
                t.Fatalf("len(toolCalls) = %d, want 1", len(toolCalls))
        }

        // Check object argument
        settings, ok := toolCalls[0].Arguments["settings"].(map[string]any)
        if !ok {
                t.Fatalf("Arguments[settings] type = %T, want map[string]any", toolCalls[0].Arguments["settings"])
        }
        if settings["debug"] != true {
                t.Errorf("settings[debug] = %v, want true", settings["debug"])
        }

        // Check array argument
        tags, ok := toolCalls[0].Arguments["tags"].([]any)
        if !ok {
                t.Fatalf("Arguments[tags] type = %T, want []any", toolCalls[0].Arguments["tags"])
        }
        if len(tags) != 2 {
                t.Errorf("len(tags) = %d, want 2", len(tags))
        }
}

func TestParseDSMLToolCalls_MixedContent(t *testing.T) {
        content := `I will help you with that.
<|DSML|tool_calls>
<|DSML|invoke name="search">
<|DSML|parameter name="query" string="true">golang testing</|DSML|parameter>
</|DSML|invoke>
</|DSML|tool_calls>
Here are the results.`

        toolCalls, remaining, err := ParseDSMLToolCalls(content)
        if err != nil {
                t.Fatalf("ParseDSMLToolCalls() error = %v", err)
        }

        if len(toolCalls) != 1 {
                t.Fatalf("len(toolCalls) = %d, want 1", len(toolCalls))
        }

        if toolCalls[0].Name != "search" {
                t.Errorf("Name = %q, want %q", toolCalls[0].Name, "search")
        }

        // Remaining content should have DSML blocks removed
        if remaining == "" {
                t.Error("remaining is empty, expected non-DSML text to be preserved")
        }
        if !containsStr(remaining, "I will help you with that") {
                t.Errorf("remaining = %q, want to contain intro text", remaining)
        }
        if !containsStr(remaining, "Here are the results") {
                t.Errorf("remaining = %q, want to contain trailing text", remaining)
        }
}

func TestParseDSMLToolCalls_NoDSML(t *testing.T) {
        content := "Just a regular response with no tool calls."

        toolCalls, remaining, err := ParseDSMLToolCalls(content)
        if err != nil {
                t.Fatalf("ParseDSMLToolCalls() error = %v", err)
        }

        if len(toolCalls) != 0 {
                t.Fatalf("len(toolCalls) = %d, want 0", len(toolCalls))
        }
        if remaining != content {
                t.Errorf("remaining = %q, want %q", remaining, content)
        }
}

func TestParseDSMLToolCalls_EmptyDSMLBlock(t *testing.T) {
        content := `<|DSML|tool_calls>
</|DSML|tool_calls>`

        toolCalls, remaining, err := ParseDSMLToolCalls(content)
        if err != nil {
                t.Fatalf("ParseDSMLToolCalls() error = %v", err)
        }

        if len(toolCalls) != 0 {
                t.Fatalf("len(toolCalls) = %d, want 0 for empty DSML block", len(toolCalls))
        }
        if remaining != "" {
                t.Errorf("remaining = %q, want empty", remaining)
        }
}

func TestParseDSMLToolCalls_BooleanAndNullParameters(t *testing.T) {
        content := `<|DSML|tool_calls>
<|DSML|invoke name="set_flag">
<|DSML|parameter name="enabled" string="false">true</|DSML|parameter>
<|DSML|parameter name="disabled" string="false">false</|DSML|parameter>
<|DSML|parameter name="optional" string="false">null</|DSML|parameter>
</|DSML|invoke>
</|DSML|tool_calls>`

        toolCalls, _, err := ParseDSMLToolCalls(content)
        if err != nil {
                t.Fatalf("ParseDSMLToolCalls() error = %v", err)
        }

        if len(toolCalls) != 1 {
                t.Fatalf("len(toolCalls) = %d, want 1", len(toolCalls))
        }

        if toolCalls[0].Arguments["enabled"] != true {
                t.Errorf("enabled = %v, want true", toolCalls[0].Arguments["enabled"])
        }
        if toolCalls[0].Arguments["disabled"] != false {
                t.Errorf("disabled = %v, want false", toolCalls[0].Arguments["disabled"])
        }
        if toolCalls[0].Arguments["optional"] != nil {
                t.Errorf("optional = %v, want nil", toolCalls[0].Arguments["optional"])
        }
}

func TestParseDSMLToolCalls_SpecialCharactersInStringParam(t *testing.T) {
        content := `<|DSML|tool_calls>
<|DSML|invoke name="write_file">
<|DSML|parameter name="content" string="true">package main

import "fmt"

func main() {
        fmt.Println("hello")
}</|DSML|parameter>
<|DSML|parameter name="path" string="true">/tmp/test.go</|DSML|parameter>
</|DSML|invoke>
</|DSML|tool_calls>`

        toolCalls, _, err := ParseDSMLToolCalls(content)
        if err != nil {
                t.Fatalf("ParseDSMLToolCalls() error = %v", err)
        }

        if len(toolCalls) != 1 {
                t.Fatalf("len(toolCalls) = %d, want 1", len(toolCalls))
        }

        contentParam, ok := toolCalls[0].Arguments["content"].(string)
        if !ok {
                t.Fatalf("Arguments[content] type = %T, want string", toolCalls[0].Arguments["content"])
        }
        if !containsStr(contentParam, `fmt.Println("hello")`) {
                t.Errorf("content param missing expected code, got: %q", contentParam)
        }
}

func TestParseDSMLToolCalls_FloatParameter(t *testing.T) {
        content := `<|DSML|tool_calls>
<|DSML|invoke name="set_threshold">
<|DSML|parameter name="value" string="false">0.95</|DSML|parameter>
</|DSML|invoke>
</|DSML|tool_calls>`

        toolCalls, _, err := ParseDSMLToolCalls(content)
        if err != nil {
                t.Fatalf("ParseDSMLToolCalls() error = %v", err)
        }

        val, ok := toolCalls[0].Arguments["value"].(float64)
        if !ok {
                t.Fatalf("Arguments[value] type = %T, want float64", toolCalls[0].Arguments["value"])
        }
        if val != 0.95 {
                t.Errorf("value = %v, want 0.95", val)
        }
}

func TestParseDSMLToolCalls_FunctionArgumentsJSON(t *testing.T) {
        content := `<|DSML|tool_calls>
<|DSML|invoke name="get_weather">
<|DSML|parameter name="city" string="true">Shanghai</|DSML|parameter>
<|DSML|parameter name="days" string="false">7</|DSML|parameter>
</|DSML|invoke>
</|DSML|tool_calls>`

        toolCalls, _, err := ParseDSMLToolCalls(content)
        if err != nil {
                t.Fatalf("ParseDSMLToolCalls() error = %v", err)
        }

        if toolCalls[0].Function == nil {
                t.Fatal("Function is nil")
        }

        // Function.Arguments should be a valid JSON string
        argsJSON := toolCalls[0].Function.Arguments
        if argsJSON == "" {
                t.Fatal("Function.Arguments is empty")
        }

        // Verify it's valid JSON by parsing it
        var parsed map[string]any
        if err := json.Unmarshal([]byte(argsJSON), &parsed); err != nil {
                t.Fatalf("Function.Arguments is not valid JSON: %q, error: %v", argsJSON, err)
        }

        if parsed["city"] != "Shanghai" {
                t.Errorf("parsed[city] = %v, want Shanghai", parsed["city"])
        }
        if parsed["days"] != float64(7) { // JSON numbers are float64 by default
                t.Errorf("parsed[days] = %v, want 7", parsed["days"])
        }
}

func TestParseDSMLToolCalls_MalformedDSML(t *testing.T) {
        // Test with unclosed invoke tag - the regex won't match, so no tool calls
        content := `<|DSML|tool_calls>
<|DSML|invoke name="broken">
<|DSML|parameter name="x" string="true">value</|DSML|parameter>
`

        toolCalls, _, err := ParseDSMLToolCalls(content)
        // Should not panic; may return 0 tool calls due to unmatched regex
        if err != nil && len(toolCalls) > 0 {
                // Having an error is fine as long as we didn't panic
                t.Logf("ParseDSMLToolCalls returned err=%v with %d tool calls (acceptable)", err, len(toolCalls))
        }
}

// Helper function since we can't use strings.Contains directly in test comparisons easily
func containsStr(s, substr string) bool {
        return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
        for i := 0; i <= len(s)-len(substr); i++ {
                if s[i:i+len(substr)] == substr {
                        return true
                }
        }
        return false
}
