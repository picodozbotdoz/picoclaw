package tracing

import (
	"regexp"
	"strings"
)

var secretPatterns = []*regexp.Regexp{
	// Bearer tokens
	regexp.MustCompile(`(?i)(Authorization:\s*Bearer\s+)\S+`),
	regexp.MustCompile(`(?i)("authorization"\s*:\s*"?Bearer\s+)\S+("?|\s)`),
	// OpenAI-style API keys
	regexp.MustCompile(`sk-[a-zA-Z0-9]{20,}`),
	// Anthropic-style API keys
	regexp.MustCompile(`sk-ant-[a-zA-Z0-9\-]{20,}`),
	// Generic API key patterns in headers or JSON
	regexp.MustCompile(`(?i)(api[_-]?key\s*[:=]\s*"?)[\w\-]{16,}`),
	regexp.MustCompile(`(?i)(api[_-]?secret\s*[:=]\s*"?)[\w\-]{16,}`),
	// Generic token patterns
	regexp.MustCompile(`(?i)(token\s*[:=]\s*"?)[\w\-]{16,}`),
	// Telegram bot tokens
	regexp.MustCompile(`\d{8,10}:[a-zA-Z0-9_-]{35}`),
}

const redacted = "[REDACTED]"

// SanitizeSecrets redacts sensitive values such as API keys, bearer tokens,
// and other credential patterns from the input string.
func SanitizeSecrets(data string) string {
	result := data

	// Bearer token patterns (preserve prefix, redact value)
	bearerPatterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)(Authorization:\s*Bearer\s+)\S+`),
		regexp.MustCompile(`(?i)("authorization"\s*:\s*"Bearer\s+)[^"]*("?)`),
	}
	for _, p := range bearerPatterns {
		result = p.ReplaceAllStringFunc(result, func(match string) string {
			parts := p.FindStringSubmatch(match)
			if len(parts) >= 2 {
				suffix := ""
				if len(parts) >= 3 && parts[2] != "" {
					suffix = parts[2]
				}
				return parts[1] + redacted + suffix
			}
			return redacted
		})
	}

	// Full key replacement patterns
	replacePatterns := []*regexp.Regexp{
		// OpenAI keys
		regexp.MustCompile(`sk-[a-zA-Z0-9]{20,}`),
		// Anthropic keys
		regexp.MustCompile(`sk-ant-[a-zA-Z0-9\-]{20,}`),
		// Telegram bot tokens
		regexp.MustCompile(`\d{8,10}:[a-zA-Z0-9_-]{35}`),
	}
	for _, p := range replacePatterns {
		result = p.ReplaceAllString(result, redacted)
	}

	// Key-value patterns (preserve key, redact value)
	kvPatterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)(api[_-]?key\s*[:=]\s*"?)\w[\w\-]{15,}`),
		regexp.MustCompile(`(?i)(api[_-]?secret\s*[:=]\s*"?)\w[\w\-]{15,}`),
		regexp.MustCompile(`(?i)(token\s*[:=]\s*"?)\w[\w\-]{15,}`),
		regexp.MustCompile(`(?i)(password\s*[:=]\s*"?)\S+`),
		regexp.MustCompile(`(?i)(secret\s*[:=]\s*"?)\S+`),
	}
	for _, p := range kvPatterns {
		result = p.ReplaceAllStringFunc(result, func(match string) string {
			parts := p.FindStringSubmatch(match)
			if len(parts) >= 2 {
				return parts[1] + redacted
			}
			return redacted
		})
	}

	// Sanitize Authorization header values in various formats
	result = regexp.MustCompile(`(?i)("authorization"\s*:\s*")[^"]*(")`).ReplaceAllString(result, "${1}"+redacted+"${2}")

	return strings.TrimSpace(result)
}
