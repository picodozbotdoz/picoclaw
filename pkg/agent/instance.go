package agent

import (
        "context"
        "fmt"
        "os"
        "path/filepath"
        "regexp"
        "strings"

        "github.com/sipeed/picoclaw/pkg/config"
        "github.com/sipeed/picoclaw/pkg/isolation"
        "github.com/sipeed/picoclaw/pkg/logger"
        "github.com/sipeed/picoclaw/pkg/media"
        "github.com/sipeed/picoclaw/pkg/memory"
        "github.com/sipeed/picoclaw/pkg/providers"
        "github.com/sipeed/picoclaw/pkg/routing"
        "github.com/sipeed/picoclaw/pkg/session"
        "github.com/sipeed/picoclaw/pkg/tools"
)

// AgentInstance represents a fully configured agent with its own workspace,
// session manager, context builder, and tool registry.
type AgentInstance struct {
        ID                        string
        Name                      string
        Model                     string
        Fallbacks                 []string
        Workspace                 string
        MaxIterations             int
        MaxTokens                 int
        MaxOutputTokens           int
        Temperature               float64
        ThinkingLevel             ThinkingLevel
        ContextWindow             int
        SummarizeMessageThreshold int
        SummarizeTokenPercent     int
        StrictToolCalls           bool
        ResponseFormat            string
        PrefixCompletion          string // DeepSeek V4 Chat Prefix Completion (Beta)
        ReasoningPrefix           string // DeepSeek V4 reasoning_content prefix for prefix completion
        CompressionStrategy       string // "eager", "adaptive", "conservative"
        FullContextMode           bool   // disables summarization, only emergency compression
        StreamingMode             string // "auto" (default), "always", "never"
        DynamicThinkingMode       string // "auto" (dynamic switching), "fixed" (always use configured level)
        ContextPartition          *config.ContextPartitionConfig // partition-based context budget allocation
        Provider                  providers.LLMProvider
        Sessions                  session.SessionStore
        ContextBuilder            *ContextBuilder
        Tools                     *tools.ToolRegistry
        Subagents                 *config.SubagentsConfig
        SkillsFilter              []string
        Candidates                []providers.FallbackCandidate

        // InjectedContext holds the session's injected/retrieved context store.
        // Populated during agent initialization and wired into the ContextBuilder
        // and context_inject/context_list/context_clear tools.
        InjectedContext *InjectedContextStore

        // Router is non-nil when model routing is configured and the light model
        // was successfully resolved. It scores each incoming message and decides
        // whether to route to LightCandidates or stay with Candidates.
        Router *routing.Router
        // LightCandidates holds the resolved provider candidates for the light model.
        // Pre-computed at agent creation to avoid repeated model_list lookups at runtime.
        LightCandidates []providers.FallbackCandidate
        // LightProvider is the concrete provider instance for the configured light model.
        // It is only used when routing selects the light tier for a turn.
        LightProvider providers.LLMProvider
        // CandidateProviders maps "provider/model" keys to per-candidate LLMProvider
        // instances. This allows each fallback model to use its own api_base and api_key
        // from model_list, instead of inheriting the primary model's provider config.
        CandidateProviders map[string]providers.LLMProvider

        // CostTracker tracks per-session cost for /cost reporting. Initialized in
        // NewAgentInstance and updated after each LLM response (WS 4.3).
        CostTracker *SessionCostTracker
}

// NewAgentInstance creates an agent instance from config.
func NewAgentInstance(
        agentCfg *config.AgentConfig,
        defaults *config.AgentDefaults,
        cfg *config.Config,
        provider providers.LLMProvider,
) *AgentInstance {
        if cfg != nil {
                // Keep the subprocess isolation runtime aligned with the latest loaded config
                // before any tools or providers start spawning child processes.
                isolation.Configure(cfg)
        }

        workspace := resolveAgentWorkspace(agentCfg, defaults)
        os.MkdirAll(workspace, 0o755)

        model := resolveAgentModel(agentCfg, defaults)
        fallbacks := resolveAgentFallbacks(agentCfg, defaults)

        restrict := defaults.RestrictToWorkspace
        readRestrict := restrict && !defaults.AllowReadOutsideWorkspace

        // Compile path whitelist patterns from config.
        allowReadPaths := buildAllowReadPatterns(cfg)
        allowWritePaths := compilePatterns(cfg.Tools.AllowWritePaths)

        toolsRegistry := tools.NewToolRegistry()

        if cfg.Tools.IsToolEnabled("read_file") {
                maxReadFileSize := cfg.Tools.ReadFile.MaxReadFileSize
                switch cfg.Tools.ReadFile.EffectiveMode() {
                case config.ReadFileModeLines:
                        toolsRegistry.Register(tools.NewReadFileLinesTool(workspace, readRestrict, maxReadFileSize, allowReadPaths))
                default:
                        toolsRegistry.Register(tools.NewReadFileBytesTool(workspace, readRestrict, maxReadFileSize, allowReadPaths))
                }
        }
        if cfg.Tools.IsToolEnabled("write_file") {
                toolsRegistry.Register(tools.NewWriteFileTool(workspace, restrict, allowWritePaths))
        }
        if cfg.Tools.IsToolEnabled("list_dir") {
                toolsRegistry.Register(tools.NewListDirTool(workspace, readRestrict, allowReadPaths))
        }
        if cfg.Tools.IsToolEnabled("exec") {
                execTool, err := tools.NewExecToolWithConfig(workspace, restrict, cfg, allowReadPaths)
                if err != nil {
                        logger.ErrorCF("agent", "Failed to initialize exec tool; continuing without exec",
                                map[string]any{"error": err.Error()})
                } else {
                        toolsRegistry.Register(execTool)
                }
        }

        if cfg.Tools.IsToolEnabled("edit_file") {
                toolsRegistry.Register(tools.NewEditFileTool(workspace, restrict, allowWritePaths))
        }
        if cfg.Tools.IsToolEnabled("append_file") {
                toolsRegistry.Register(tools.NewAppendFileTool(workspace, restrict, allowWritePaths))
        }

        // Register context injection tools (context_inject, context_list, context_clear)
        toolsRegistry.Register(tools.NewContextInjectTool())
        toolsRegistry.Register(tools.NewContextListTool())
        toolsRegistry.Register(tools.NewContextClearTool())

        sessionsDir := filepath.Join(workspace, "sessions")
        sessions := initSessionStore(sessionsDir)

        mcpDiscoveryActive := cfg.Tools.MCP.Enabled && cfg.Tools.MCP.Discovery.Enabled
        injectedContextStore := NewInjectedContextStore()
        contextBuilder := NewContextBuilder(workspace).
                WithToolDiscovery(
                        mcpDiscoveryActive && cfg.Tools.MCP.Discovery.UseBM25,
                        mcpDiscoveryActive && cfg.Tools.MCP.Discovery.UseRegex,
                ).
                WithSplitOnMarker(cfg.Agents.Defaults.SplitOnMarker)
        contextBuilder.InjectedContext = injectedContextStore

        agentID := routing.DefaultAgentID
        agentName := ""
        var subagents *config.SubagentsConfig
        var skillsFilter []string

        if agentCfg != nil {
                agentID = routing.NormalizeAgentID(agentCfg.ID)
                agentName = agentCfg.Name
                subagents = agentCfg.Subagents
                skillsFilter = agentCfg.Skills
        }

        maxIter := defaults.MaxToolIterations
        if maxIter == 0 {
                maxIter = 20
        }

        maxTokens := defaults.MaxTokens
        if maxTokens == 0 {
                maxTokens = 8192
        }

        // MaxOutputTokens caps max_tokens to prevent accidental expensive requests.
        // For example, DeepSeek V4 supports up to 384K output tokens at $0.28/M,
        // but the default should be conservative (16K).
        var maxOutputTokens int
        if mc, err := cfg.GetModelConfig(model); err == nil && mc.MaxOutputTokens > 0 {
                maxOutputTokens = mc.MaxOutputTokens
        }
        // Fallback to runtime model defaults if ModelConfig didn't specify MaxOutputTokens.
        if maxOutputTokens == 0 {
                if md := config.LookupModelDefaults(model); md != nil && md.MaxOutputTokens > 0 {
                        maxOutputTokens = md.MaxOutputTokens
                }
        }
        // Enforce the cap: if MaxTokens exceeds MaxOutputTokens, reduce it.
        if maxOutputTokens > 0 && maxTokens > maxOutputTokens {
                logger.InfoCF("agent", "MaxTokens exceeds MaxOutputTokens, capping to model limit",
                        map[string]any{"model": model, "max_tokens": maxTokens, "max_output_tokens": maxOutputTokens, "agent_id": agentID})
                maxTokens = maxOutputTokens
        }

        // DeepSeek V4 strict mode for tool calls and response format.
        var strictToolCalls bool
        var responseFormat string
        var prefixCompletion string
        var reasoningPrefix string
        if mc, err := cfg.GetModelConfig(model); err == nil {
                strictToolCalls = mc.StrictToolCalls
                responseFormat = mc.ResponseFormat
                prefixCompletion = mc.PrefixCompletion
                reasoningPrefix = mc.ReasoningPrefix
        }

        contextWindow := defaults.ContextWindow
        if contextWindow == 0 {
                // Check if the resolved model config specifies a model-level context window.
                // This takes precedence over the heuristic for models with known context
                // windows (e.g., DeepSeek V4 = 1M tokens).
                if mc, err := cfg.GetModelConfig(model); err == nil && mc.ContextWindow > 0 {
                        contextWindow = mc.ContextWindow
                }
        }
        if contextWindow == 0 {
                // Fallback to runtime model defaults (e.g., 1M for DeepSeek V4).
                if md := config.LookupModelDefaults(model); md != nil && md.ContextWindow > 0 {
                        contextWindow = md.ContextWindow
                }
        }
        if contextWindow == 0 {
                // Default heuristic: 4x the output token limit.
                // Most models have context windows well above their output limits
                // (e.g., GPT-4o 128k ctx / 16k out, Claude 200k ctx / 8k out).
                // 4x is a conservative lower bound that avoids premature
                // summarization while remaining safe — the reactive
                // forceCompression handles any overshoot.
                contextWindow = maxTokens * 4
        }

        // Warn if the context window is suspiciously small for a model that
        // likely supports a much larger window (potential misconfiguration).
        if contextWindow < 100000 {
                if mc, err := cfg.GetModelConfig(model); err == nil {
                        modelStr := strings.ToLower(mc.Model)
                        if strings.Contains(modelStr, "deepseek-v4") ||
                                strings.Contains(modelStr, "gpt-5") ||
                                strings.Contains(modelStr, "claude") ||
                                strings.Contains(modelStr, "gemini") {
                                logger.WarnCF("agent", "context_window may be too small for this model; consider setting it explicitly",
                                        map[string]any{"model": model, "context_window": contextWindow, "agent_id": agentID})
                        }
                }
        }

        temperature := 0.7
        if defaults.Temperature != nil {
                temperature = *defaults.Temperature
        }

        var thinkingLevelStr string
        if mc, err := cfg.GetModelConfig(model); err == nil {
                thinkingLevelStr = mc.ThinkingLevel
        }
        thinkingLevel := parseThinkingLevel(thinkingLevelStr)

        summarizeMessageThreshold := defaults.SummarizeMessageThreshold
        if summarizeMessageThreshold == 0 {
                summarizeMessageThreshold = 20
        }

        summarizeTokenPercent := defaults.SummarizeTokenPercent
        if summarizeTokenPercent == 0 {
                summarizeTokenPercent = 75
        }

        compressionStrategy := defaults.CompressionStrategy
        if compressionStrategy == "" {
                compressionStrategy = "eager"
        }
        fullContextMode := defaults.FullContextMode
        streamingMode := defaults.StreamingMode
        if streamingMode == "" {
                streamingMode = "auto"
        }
        dynamicThinkingMode := defaults.DynamicThinkingMode
        if dynamicThinkingMode == "" {
                dynamicThinkingMode = "auto"
        }

        // ContextPartition from ModelConfig (optional, enables partition-based budget enforcement)
        var contextPartition *config.ContextPartitionConfig
        if mc, err := cfg.GetModelConfig(model); err == nil && mc.ContextPartition != nil {
                if effective := mc.ContextPartition.Effective(); effective != nil {
                        contextPartition = effective
                }
        }

        // Resolve fallback candidates
        candidates := resolveModelCandidates(cfg, defaults.Provider, model, fallbacks)

        candidateProviders := make(map[string]providers.LLMProvider)
        populateCandidateProvidersFromNames(cfg, workspace, fallbacks, candidateProviders)

        // Model routing setup: pre-resolve light model candidates at creation time
        // to avoid repeated model_list lookups on every incoming message.
        var router *routing.Router
        var lightCandidates []providers.FallbackCandidate
        var lightProvider providers.LLMProvider
        if rc := defaults.Routing; rc != nil && rc.Enabled && rc.LightModel != "" {
                resolved := resolveModelCandidates(cfg, defaults.Provider, rc.LightModel, nil)
                if len(resolved) > 0 {
                        lightModelCfg, err := resolvedModelConfig(cfg, rc.LightModel, workspace)
                        if err != nil {
                                logger.WarnCF("agent", "Routing light model config invalid; routing disabled",
                                        map[string]any{"light_model": rc.LightModel, "agent_id": agentID, "error": err.Error()})
                        } else {
                                lp, _, err := providers.CreateProviderFromConfig(lightModelCfg)
                                if err != nil {
                                        logger.WarnCF("agent", "Routing light model provider init failed; routing disabled",
                                                map[string]any{"light_model": rc.LightModel, "agent_id": agentID, "error": err.Error()})
                                } else {
                                        router = routing.New(routing.RouterConfig{
                                                LightModel: rc.LightModel,
                                                Threshold:  rc.Threshold,
                                        })
                                        lightCandidates = resolved
                                        lightProvider = lp
                                        populateCandidateProvidersFromNames(cfg, workspace, []string{rc.LightModel}, candidateProviders)
                                }
                        }
                } else {
                        logger.WarnCF("agent", "Routing light model not found; routing disabled",
                                map[string]any{"light_model": rc.LightModel, "agent_id": agentID})
                }
        }

        return &AgentInstance{
                ID:                        agentID,
                Name:                      agentName,
                Model:                     model,
                Fallbacks:                 fallbacks,
                Workspace:                 workspace,
                MaxIterations:             maxIter,
                MaxTokens:                 maxTokens,
                MaxOutputTokens:           maxOutputTokens,
                Temperature:               temperature,
                ThinkingLevel:             thinkingLevel,
                ContextWindow:             contextWindow,
                SummarizeMessageThreshold: summarizeMessageThreshold,
                SummarizeTokenPercent:     summarizeTokenPercent,
                StrictToolCalls:           strictToolCalls,
                ResponseFormat:            responseFormat,
                PrefixCompletion:          prefixCompletion,
                ReasoningPrefix:           reasoningPrefix,
                CompressionStrategy:       compressionStrategy,
                FullContextMode:           fullContextMode,
                StreamingMode:             streamingMode,
                DynamicThinkingMode:       dynamicThinkingMode,
                ContextPartition:          contextPartition,
                Provider:                  provider,
                Sessions:                  sessions,
                ContextBuilder:            contextBuilder,
                Tools:                     toolsRegistry,
                Subagents:                 subagents,
                SkillsFilter:              skillsFilter,
                Candidates:                candidates,
                InjectedContext:           injectedContextStore,
                Router:                    router,
                LightCandidates:           lightCandidates,
                LightProvider:             lightProvider,
                CandidateProviders:        candidateProviders,
                CostTracker:               NewSessionCostTracker(),
        }
}

// populateCandidateProvidersFromNames resolves each model name (alias or
// "provider/model") via resolvedModelConfig and creates a dedicated LLMProvider
// for it. This reuses the canonical config resolution path (GetModelConfig) so
// alias handling and load-balancing stay consistent with the rest of the codebase.
func populateCandidateProvidersFromNames(
        cfg *config.Config,
        workspace string,
        names []string,
        out map[string]providers.LLMProvider,
) {
        if cfg == nil || len(names) == 0 {
                return
        }
        for _, name := range names {
                mc, err := resolvedModelConfig(cfg, strings.TrimSpace(name), workspace)
                if err != nil {
                        logger.WarnCF("agent",
                                "fallback provider: no model_list entry found; will inherit primary provider credentials",
                                map[string]any{"name": name, "error": err.Error()})
                        continue
                }
                protocol, modelID := providers.ExtractProtocol(mc)
                key := providers.ModelKey(protocol, modelID)
                if _, exists := out[key]; exists {
                        continue
                }
                p, _, err := providers.CreateProviderFromConfig(mc)
                if err != nil {
                        logger.WarnCF("agent", "fallback provider: failed to create provider",
                                map[string]any{"model": mc.Model, "error": err.Error()})
                        continue
                }
                out[key] = p
        }
}

// resolveAgentWorkspace determines the workspace directory for an agent.
func resolveAgentWorkspace(agentCfg *config.AgentConfig, defaults *config.AgentDefaults) string {
        if agentCfg != nil && strings.TrimSpace(agentCfg.Workspace) != "" {
                return expandHome(strings.TrimSpace(agentCfg.Workspace))
        }
        // Use the configured default workspace (respects PICOCLAW_HOME)
        if agentCfg == nil || agentCfg.Default || agentCfg.ID == "" || routing.NormalizeAgentID(agentCfg.ID) == "main" {
                return expandHome(defaults.Workspace)
        }
        // For named agents without explicit workspace, use default workspace with agent ID suffix
        id := routing.NormalizeAgentID(agentCfg.ID)
        return filepath.Join(expandHome(defaults.Workspace), "..", "workspace-"+id)
}

// resolveAgentModel resolves the primary model for an agent.
func resolveAgentModel(agentCfg *config.AgentConfig, defaults *config.AgentDefaults) string {
        if agentCfg != nil && agentCfg.Model != nil && strings.TrimSpace(agentCfg.Model.Primary) != "" {
                return strings.TrimSpace(agentCfg.Model.Primary)
        }
        return defaults.GetModelName()
}

// resolveAgentFallbacks resolves the fallback models for an agent.
func resolveAgentFallbacks(agentCfg *config.AgentConfig, defaults *config.AgentDefaults) []string {
        if agentCfg != nil && agentCfg.Model != nil && agentCfg.Model.Fallbacks != nil {
                return agentCfg.Model.Fallbacks
        }
        return defaults.ModelFallbacks
}

func compilePatterns(patterns []string) []*regexp.Regexp {
        compiled := make([]*regexp.Regexp, 0, len(patterns))
        for _, p := range patterns {
                re, err := regexp.Compile(p)
                if err != nil {
                        fmt.Printf("Warning: invalid path pattern %q: %v\n", p, err)
                        continue
                }
                compiled = append(compiled, re)
        }
        return compiled
}

func buildAllowReadPatterns(cfg *config.Config) []*regexp.Regexp {
        var configured []string
        if cfg != nil {
                configured = cfg.Tools.AllowReadPaths
        }

        compiled := compilePatterns(configured)
        mediaDirPattern := regexp.MustCompile(mediaTempDirPattern())
        for _, pattern := range compiled {
                if pattern.String() == mediaDirPattern.String() {
                        return compiled
                }
        }

        return append(compiled, mediaDirPattern)
}

func mediaTempDirPattern() string {
        sep := regexp.QuoteMeta(string(os.PathSeparator))
        return "^" + regexp.QuoteMeta(filepath.Clean(media.TempDir())) + "(?:" + sep + "|$)"
}

// Close releases resources held by the agent's session store.
func (a *AgentInstance) Close() error {
        if a.Sessions != nil {
                return a.Sessions.Close()
        }
        return nil
}

// initSessionStore creates the session persistence backend.
// It uses the JSONL store by default and auto-migrates legacy JSON sessions.
// Falls back to SessionManager if the JSONL store cannot be initialized or
// if migration fails (which indicates the store cannot write reliably).
func initSessionStore(dir string) session.SessionStore {
        store, err := memory.NewJSONLStore(dir)
        if err != nil {
                logger.WarnCF("agent", "Memory JSONL store init failed; falling back to json sessions",
                        map[string]any{"error": err.Error()})
                return session.NewSessionManager(dir)
        }

        if n, merr := memory.MigrateFromJSON(context.Background(), dir, store); merr != nil {
                // Migration failure means the store could not write data.
                // Fall back to SessionManager to avoid a split state where
                // some sessions are in JSONL and others remain in JSON.
                logger.WarnCF("agent", "Memory migration failed; falling back to json sessions",
                        map[string]any{"error": merr.Error()})
                store.Close()
                return session.NewSessionManager(dir)
        } else if n > 0 {
                logger.InfoCF("agent", "Memory migrated to JSONL", map[string]any{"sessions_migrated": n})
        }

        return session.NewJSONLBackend(store)
}

func expandHome(path string) string {
        if path == "" {
                return path
        }
        if path[0] == '~' {
                home, _ := os.UserHomeDir()
                if len(path) > 1 && path[1] == '/' {
                        return home + path[1:]
                }
                return home
        }
        return path
}
