package tracing

import (
	"github.com/sipeed/picoclaw/pkg/config"
)

// ConfigFromGateway converts a config.TracingConfig to a TraceConfig.
func ConfigFromGateway(cfg config.TracingConfig) TraceConfig {
	return TraceConfig{
		Enabled:               cfg.Enabled,
		DBPath:                cfg.DBPath,
		WALMode:               cfg.WALMode,
		MaxDBSizeMB:           cfg.MaxDBSizeMB,
		EventRetentionDays:    cfg.EventRetentionDays,
		LLMCallRetentionDays:  cfg.LLMCallRetentionDays,
		ContextRetentionDays:  cfg.ContextRetentionDays,
		SessionRetentionDays:  cfg.SessionRetentionDays,
		PruneIntervalMinutes:  cfg.PruneIntervalMinutes,
		SnippetMaxChars:       cfg.SnippetMaxChars,
		CaptureRequestMsgs:    cfg.CaptureRequestMsgs,
		CaptureResponseContent: cfg.CaptureResponseContent,
		SanitizeSecrets:       cfg.SanitizeSecrets,
	}
}
