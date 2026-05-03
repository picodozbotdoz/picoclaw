//go:build integration || compliance

package v4_integration

import (
	"os"
	"testing"
)

// getDeepSeekAPIKey returns the DeepSeek API key from the environment.
// Shared by both integration and compliance test suites.
func getDeepSeekAPIKey(t *testing.T) string {
	t.Helper()
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		t.Skip("DEEPSEEK_API_KEY not set, skipping DeepSeek V4 test")
	}
	return key
}
