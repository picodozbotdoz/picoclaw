package commands

import (
        "context"

        "github.com/sipeed/picoclaw/pkg/config"
)

type MCPServerInfo struct {
        Name      string
        Enabled   bool
        Deferred  bool
        Connected bool
        ToolCount int
}

type MCPToolParameterInfo struct {
        Name        string
        Type        string
        Description string
        Required    bool
}

type MCPToolInfo struct {
        Name        string
        Description string
        Parameters  []MCPToolParameterInfo
}

// ContextStats describes current session context window usage.
type ContextStats struct {
        UsedTokens       int
        TotalTokens      int // model context window
        CompressAtTokens int // compression threshold
        UsedPercent      int // 0-100
        MessageCount     int
        // DeepSeek V4 enhanced tracking (WS 4.3)
        CacheHitTokens   int // tokens served from prefix cache
        CacheMissTokens  int // tokens computed fresh (cache miss)
        OutputTokens     int // tokens in the final response (completion)
        ReasoningTokens  int // tokens used for reasoning/thinking
        // Partition breakdown
        InjectedContextTokens int // retrieved/injected context partition usage
        SystemPromptTokens   int // system prompt partition usage
        HistoryTokens        int // conversation history partition usage
        ToolDefTokens        int // tool definition tokens
}

// Runtime provides runtime dependencies to command handlers. It is constructed
// per-request by the agent loop so that per-request state (like session scope)
// can coexist with long-lived callbacks (like GetModelInfo).
type Runtime struct {
        Config             *config.Config
        GetModelInfo       func() (name, provider string)
        AskSideQuestion    func(ctx context.Context, question string) (string, error)
        ListAgentIDs       func() []string
        ListDefinitions    func() []Definition
        ListSkillNames     func() []string
        ListMCPServers     func(ctx context.Context) []MCPServerInfo
        ListMCPTools       func(ctx context.Context, serverName string) ([]MCPToolInfo, error)
        GetEnabledChannels func() []string
        GetActiveTurn      func() any // Returning any to avoid circular dependency with agent package
        GetContextStats    func() *ContextStats
        GetCostBreakdown   func() string // WS 4.3: returns formatted session cost breakdown
        SwitchModel        func(value string) (oldModel string, err error)
        SwitchChannel      func(value string) error
        ClearHistory       func() error
        ReloadConfig       func() error
}
