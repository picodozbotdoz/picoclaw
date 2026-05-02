package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// --- isBuildSuccess tests ---

func TestIsBuildSuccess_EmptyOutput(t *testing.T) {
	if !isBuildSuccess("") {
		t.Error("empty output should indicate build success")
	}
}

func TestIsBuildSuccess_CleanBuild(t *testing.T) {
	output := "Building project...\nDone."
	if !isBuildSuccess(output) {
		t.Errorf("clean build output should indicate success: %q", output)
	}
}

func TestIsBuildSuccess_ErrorIndicator(t *testing.T) {
	testCases := []string{
		"main.go:10: error: undefined variable",
		"fatal error: compilation failed",
		"build failed: exit code 1",
		"compilation failed",
		"undefined: someFunction",
		"cannot find package: missing",
		"no such file or directory: config.yaml",
		"syntax error: unexpected token",
		"link error: undefined reference",
		"ld returned 1 exit status",
	}
	for _, tc := range testCases {
		if isBuildSuccess(tc) {
			t.Errorf("expected build failure for: %q", tc)
		}
	}
}

func TestIsBuildSuccess_CaseInsensitive(t *testing.T) {
	if isBuildSuccess("ERROR: something went wrong") {
		t.Error("expected 'ERROR' (uppercase) to indicate failure")
	}
	if isBuildSuccess("Build Failed") {
		t.Error("expected 'Build Failed' (mixed case) to indicate failure")
	}
}

func TestIsBuildSuccess_SuccessWithWarnings(t *testing.T) {
	output := "Building...\nWarning: unused variable\nDone. 2 warnings."
	if !isBuildSuccess(output) {
		t.Error("build with warnings (no error indicators) should be considered success")
	}
}

// --- isTestSuccess tests ---

func TestIsTestSuccess_EmptyOutput(t *testing.T) {
	if !isTestSuccess("") {
		t.Error("empty output should indicate test success (no failures found)")
	}
}

func TestIsTestSuccess_ExplicitPass(t *testing.T) {
	testCases := []string{
		"ok  github.com/example/pkg  0.123s",
		"All tests passed",
		"PASS",
		"no tests to run",
	}
	for _, tc := range testCases {
		if !isTestSuccess(tc) {
			t.Errorf("expected test success for: %q", tc)
		}
	}
}

func TestIsTestSuccess_FailureIndicators(t *testing.T) {
	testCases := []string{
		"FAIL: test_something",
		"tests failed: 2",
		"panic: runtime error",
		"assertion failed: expected 5, got 3",
		"Expected true, received false",
	}
	for _, tc := range testCases {
		if isTestSuccess(tc) {
			t.Errorf("expected test failure for: %q", tc)
		}
	}
}

func TestIsTestSuccess_CaseInsensitive(t *testing.T) {
	if isTestSuccess("FAILED: test case 1") {
		t.Error("expected 'FAILED' (uppercase) to indicate failure")
	}
}

// --- VerificationResult JSON round-trip ---

func TestVerificationResult_JSONRoundTrip(t *testing.T) {
	v := VerificationResult{
		BuildPassed: true,
		BuildOutput: "ok",
		TestPassed:  true,
		TestOutput:  "all passed",
		RetriesUsed: 1,
		ProjectType: "go",
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Failed to marshal VerificationResult: %v", err)
	}
	var v2 VerificationResult
	if err := json.Unmarshal(data, &v2); err != nil {
		t.Fatalf("Failed to unmarshal VerificationResult: %v", err)
	}
	if v2.BuildPassed != v.BuildPassed {
		t.Errorf("BuildPassed = %v, want %v", v2.BuildPassed, v.BuildPassed)
	}
	if v2.TestPassed != v.TestPassed {
		t.Errorf("TestPassed = %v, want %v", v2.TestPassed, v.TestPassed)
	}
	if v2.ProjectType != v.ProjectType {
		t.Errorf("ProjectType = %q, want %q", v2.ProjectType, v.ProjectType)
	}
	if v2.RetriesUsed != v.RetriesUsed {
		t.Errorf("RetriesUsed = %d, want %d", v2.RetriesUsed, v.RetriesUsed)
	}
}

func TestVerificationResult_FailedBuild_OmitEmptyTestOutput(t *testing.T) {
	v := VerificationResult{
		BuildPassed: false,
		BuildOutput: "main.go:10: error: undefined",
		TestPassed:  false,
		RetriesUsed: 2,
		ProjectType: "go",
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Failed to marshal failed VerificationResult: %v", err)
	}
	if strings.Contains(string(data), "test_output") {
		t.Error("expected test_output to be omitted when empty")
	}
}

// --- constant checks ---

func TestMaxVerificationOutput(t *testing.T) {
	if maxVerificationOutput != 5000 {
		t.Errorf("maxVerificationOutput = %d, want 5000", maxVerificationOutput)
	}
}

func TestMaxExplorationFileContent(t *testing.T) {
	if maxExplorationFileContent != 5000 {
		t.Errorf("maxExplorationFileContent = %d, want 5000", maxExplorationFileContent)
	}
}
