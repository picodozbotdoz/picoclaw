package agent

import (
        "strings"

        "github.com/sipeed/picoclaw/pkg/logger"
        "github.com/sipeed/picoclaw/pkg/providers"
)

// isVisionModel reports whether the model name indicates a vision-capable model.
// Vision models can process image_url content; non-vision models reject it
// with API errors (e.g., ZhipuAI error code 1210).
func isVisionModel(model string) bool {
        m := strings.ToLower(model)
        // OpenAI vision models
        if strings.Contains(m, "gpt-4o") || strings.Contains(m, "gpt-4-turbo") ||
                strings.Contains(m, "gpt-4-vision") {
                return true
        }
        // Claude vision models (Claude 3+ all support vision)
        if strings.Contains(m, "claude-3") || strings.Contains(m, "claude-4") ||
                strings.Contains(m, "claude-opus") || strings.Contains(m, "claude-sonnet") ||
                strings.Contains(m, "claude-haiku") {
                return true
        }
        // ZhipuAI/GLM vision models
        if strings.Contains(m, "glm-4v") || strings.Contains(m, "glm-4.5v") ||
                strings.Contains(m, "glm-4.6v") {
                return true
        }
        // Gemini models (all support vision)
        if strings.Contains(m, "gemini") {
                return true
        }
        // DeepSeek V4 vision
        if strings.Contains(m, "deepseek-v4") {
                return true
        }
        return false
}

// stripMediaForNonVisionModel strips media references from historical messages
// when the active model does not support vision/image input. It preserves media
// on the last user message (current turn) so that the vision-unsupported retry
// path can handle it with proper error reporting and fallback.
//
// This prevents API errors when assembled context (from compression) contains
// image_url content from prior turns that would be rejected by non-vision models.
func stripMediaForNonVisionModel(messages []providers.Message, model string) []providers.Message {
        if isVisionModel(model) || !messagesContainMedia(messages) {
                return messages
        }

        // Find the index of the last user message to preserve its media
        // (current turn — the vision-unsupported retry path handles this case)
        lastUserIdx := -1
        for i := len(messages) - 1; i >= 0; i-- {
                if messages[i].Role == "user" {
                        lastUserIdx = i
                        break
                }
        }

        stripped := make([]providers.Message, len(messages))
        strippedCount := 0
        for i, msg := range messages {
                stripped[i] = msg
                // Preserve media on the last user message (current turn input)
                if i == lastUserIdx {
                        continue
                }
                if len(msg.Media) > 0 {
                        stripped[i].Media = nil
                        strippedCount++
                }
        }

        if strippedCount > 0 {
                logger.WarnCF("agent", "Stripping historical media from context: model does not support vision",
                        map[string]any{"model": model, "stripped_messages": strippedCount})
        }

        return stripped
}

func messagesContainMedia(messages []providers.Message) bool {
        for _, msg := range messages {
                for _, ref := range msg.Media {
                        if strings.TrimSpace(ref) != "" {
                                return true
                        }
                }
        }
        return false
}

func stripMessageMedia(messages []providers.Message) []providers.Message {
        if !messagesContainMedia(messages) {
                return messages
        }
        stripped := make([]providers.Message, len(messages))
        for i, msg := range messages {
                stripped[i] = msg
                stripped[i].Media = nil
        }
        return stripped
}

func isVisionUnsupportedError(err error) bool {
        if err == nil {
                return false
        }
        msg := strings.ToLower(err.Error())

        // OpenRouter (and OpenAI-compatible) style.
        if strings.Contains(msg, "no endpoints found that support image input") {
                return true
        }

        // Common provider variants.
        if strings.Contains(msg, "does not support image input") ||
                strings.Contains(msg, "does not support image inputs") ||
                strings.Contains(msg, "does not support images") ||
                strings.Contains(msg, "image input is not supported") ||
                strings.Contains(msg, "images are not supported") ||
                strings.Contains(msg, "does not support vision") ||
                strings.Contains(msg, "unsupported content type: image_url") {
                return true
        }

        // Some providers return a generic "invalid" message that still mentions image_url.
        if strings.Contains(msg, "image_url") && strings.Contains(msg, "invalid") {
                return true
        }

        // ZhipuAI/GLM returns error code 1210 ("API 调用参数有误") when image_url
        // content is sent to non-vision models. The error body contains neither
        // "image" nor "vision" keywords, so it must be matched by error code.
        // Format: {"error":{"code":"1210","message":"API 调用参数有误..."}}
        // HandleErrorResponse wraps it as: "API request failed: Status: 400 Body: ..."
        if strings.Contains(msg, `"code":"1210"`) || strings.Contains(msg, `"code": "1210"`) {
                return true
        }

        return false
}
