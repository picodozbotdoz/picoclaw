package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestIsVisionModel(t *testing.T) {
	tests := []struct {
		model    string
		expected bool
	}{
		// OpenAI vision models
		{"gpt-4o", true},
		{"gpt-4-turbo", true},
		{"gpt-4-vision", true},
		// Claude vision models
		{"claude-3-opus", true},
		{"claude-4-sonnet", true},
		{"claude-sonnet-4.6", true},
		{"claude-haiku", true},
		// ZhipuAI/GLM vision models
		{"glm-4v-flash", true},
		{"glm-4v", true},
		{"glm-4.5v", true},
		{"glm-4.6v", true},
		// Gemini
		{"gemini-2.0-flash", true},
		{"gemini-pro-vision", true},
		// DeepSeek V4 only (not V3/chat)
		{"deepseek-v4-pro", true},
		{"deepseek-v4-flash", true},
		// Non-vision models — these should NOT match
		{"deepseek-chat", false},
		{"deepseek-v3", false},
		{"deepseek-reasoner", false},
		{"gpt-3.5-turbo", false},
		{"gpt-4", false},
		{"llama-3-70b", false},
		{"qwen-2.5", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := isVisionModel(tt.model); got != tt.expected {
				t.Errorf("isVisionModel(%q) = %v, want %v", tt.model, got, tt.expected)
			}
		})
	}
}

func TestMessagesContainMedia(t *testing.T) {
	tests := []struct {
		name     string
		messages []providers.Message
		expected bool
	}{
		{
			name:     "empty",
			messages: nil,
			expected: false,
		},
		{
			name: "no media",
			messages: []providers.Message{
				{Role: "user", Content: "hello"},
				{Role: "assistant", Content: "hi"},
			},
			expected: false,
		},
		{
			name: "has media ref",
			messages: []providers.Message{
				{Role: "user", Content: "check this", Media: []string{"data:image/png;base64,abc"}},
			},
			expected: true,
		},
		{
			name: "empty media string",
			messages: []providers.Message{
				{Role: "user", Content: "hello", Media: []string{""}},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := messagesContainMedia(tt.messages); got != tt.expected {
				t.Errorf("messagesContainMedia() = %v, want %v", got, tt.expected)
			}
		})
	}
}
