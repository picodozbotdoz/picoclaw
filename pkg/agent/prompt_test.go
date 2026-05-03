package agent

import (
        "context"
        "encoding/json"
        "strings"
        "testing"

        "github.com/sipeed/picoclaw/pkg/providers"
)

func TestPromptRegistry_RejectsRegisteredSourceWrongPlacement(t *testing.T) {
        registry := NewPromptRegistry()
        if err := registry.RegisterSource(PromptSourceDescriptor{
                ID:      "test:source",
                Owner:   "test",
                Allowed: []PromptPlacement{{Layer: PromptLayerCapability, Slot: PromptSlotTooling}},
        }); err != nil {
                t.Fatalf("RegisterSource() error = %v", err)
        }

        err := registry.ValidatePart(PromptPart{
                ID:      "wrong.placement",
                Layer:   PromptLayerContext,
                Slot:    PromptSlotRuntime,
                Source:  PromptSource{ID: "test:source"},
                Content: "runtime text",
        })
        if err == nil {
                t.Fatal("ValidatePart() error = nil, want placement error")
        }
}

func TestPromptRegistry_AllowsUnregisteredSourceInCompatibilityMode(t *testing.T) {
        registry := NewPromptRegistry()

        err := registry.ValidatePart(PromptPart{
                ID:      "unregistered.part",
                Layer:   PromptLayerCapability,
                Slot:    PromptSlotMCP,
                Source:  PromptSource{ID: "mcp:dynamic-server"},
                Content: "dynamic MCP prompt",
        })
        if err != nil {
                t.Fatalf("ValidatePart() error = %v, want nil for unregistered source", err)
        }
}

func TestRenderPromptPartsLegacy_UsesLayerAndSlotOrder(t *testing.T) {
        parts := []PromptPart{
                {
                        ID:      "context.runtime",
                        Layer:   PromptLayerContext,
                        Slot:    PromptSlotRuntime,
                        Source:  PromptSource{ID: PromptSourceRuntime},
                        Content: "runtime",
                },
                {
                        ID:      "kernel.identity",
                        Layer:   PromptLayerKernel,
                        Slot:    PromptSlotIdentity,
                        Source:  PromptSource{ID: PromptSourceKernel},
                        Content: "kernel",
                },
                {
                        ID:      "capability.skill",
                        Layer:   PromptLayerCapability,
                        Slot:    PromptSlotActiveSkill,
                        Source:  PromptSource{ID: "skill:test"},
                        Content: "skill",
                },
                {
                        ID:      "instruction.workspace",
                        Layer:   PromptLayerInstruction,
                        Slot:    PromptSlotWorkspace,
                        Source:  PromptSource{ID: PromptSourceWorkspace},
                        Content: "workspace",
                },
        }

        got := renderPromptPartsLegacy(parts)
        want := strings.Join([]string{"kernel", "workspace", "skill", "runtime"}, "\n\n---\n\n")
        if got != want {
                t.Fatalf("renderPromptPartsLegacy() = %q, want %q", got, want)
        }
}

func TestBuildMessagesFromPrompt_IncludesSystemPromptOverlay(t *testing.T) {
        t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
        cb := NewContextBuilder(t.TempDir())

        messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{
                CurrentMessage: "do child task",
                Overlays: promptOverlaysForOptions(processOptions{
                        SystemPromptOverride: "Use child-only system instructions.",
                }),
        })

        if len(messages) < 2 {
                t.Fatalf("messages len = %d, want at least 2", len(messages))
        }
        if messages[0].Role != "system" {
                t.Fatalf("messages[0].Role = %q, want system", messages[0].Role)
        }
        // Overlay should appear in one of the system messages
        var foundOverlay bool
        for _, msg := range messages {
                if msg.Role == "system" && strings.Contains(msg.Content, "Use child-only system instructions.") {
                        foundOverlay = true
                }
        }
        if !foundOverlay {
                t.Fatalf("no system message contains overlay prompt: %q", messages[0].Content)
        }
        // Last message should be the user task
        lastMsg := messages[len(messages)-1]
        if lastMsg.Role != "user" || lastMsg.Content != "do child task" {
                t.Fatalf("last message = %#v, want user task", lastMsg)
        }
}

func TestBuildMessagesFromPrompt_AttachesInternalPromptMetadata(t *testing.T) {
        t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
        cb := NewContextBuilder(t.TempDir())

        messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{
                CurrentMessage: "hello",
                Summary:        "prior context",
        })
        if len(messages) < 3 {
                t.Fatalf("messages len = %d, want at least 3 (stable system + volatile system + user)", len(messages))
        }

        // First message must be the stable system message
        system := messages[0]
        if system.Role != "system" {
                t.Fatalf("messages[0].Role = %q, want system", system.Role)
        }
        if len(system.SystemParts) < 1 {
                t.Fatalf("stable system parts len = %d, want at least 1", len(system.SystemParts))
        }
        if system.SystemParts[0].PromptLayer != string(PromptLayerKernel) ||
                system.SystemParts[0].PromptSlot != string(PromptSlotIdentity) ||
                system.SystemParts[0].PromptSource != string(PromptSourceKernel) {
                t.Fatalf("static system metadata = %#v, want kernel identity", system.SystemParts[0])
        }

        // Second message should be the volatile system message with runtime
        var foundRuntime bool
        for _, msg := range messages {
                if msg.Role == "system" {
                        for _, part := range msg.SystemParts {
                                if part.PromptSource == string(PromptSourceRuntime) {
                                        foundRuntime = true
                                        if part.CacheControl != nil {
                                                t.Fatalf("runtime cache control = %#v, want nil", part.CacheControl)
                                        }
                                }
                        }
                }
        }
        if !foundRuntime {
                t.Fatal("system parts missing runtime prompt metadata")
        }

        // Summary is now a user/assistant message pair
        var foundSummaryUser bool
        for _, msg := range messages {
                if msg.Role == "user" && strings.Contains(msg.Content, "CONTEXT_SUMMARY:") {
                        foundSummaryUser = true
                }
        }
        if !foundSummaryUser {
                t.Fatal("summary user message missing")
        }

        // Last message should be the user message with metadata
        lastMsg := messages[len(messages)-1]
        if lastMsg.PromptLayer != string(PromptLayerTurn) ||
                lastMsg.PromptSlot != string(PromptSlotMessage) ||
                lastMsg.PromptSource != string(PromptSourceUserMessage) {
                t.Fatalf("user message metadata = %#v, want turn message", lastMsg)
        }

        data, err := json.Marshal(messages)
        if err != nil {
                t.Fatalf("json.Marshal() error = %v", err)
        }
        if strings.Contains(string(data), "PromptSource") ||
                strings.Contains(string(data), "PromptLayer") ||
                strings.Contains(string(data), "PromptSlot") {
                t.Fatalf("internal prompt metadata leaked into JSON: %s", data)
        }
}

func TestContextBuilder_CollectsToolDiscoveryContributor(t *testing.T) {
        t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
        cb := NewContextBuilder(t.TempDir()).WithToolDiscovery(true, false)

        messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{CurrentMessage: "hello"})

        // Tool discovery content should be in the stable system message (ephemeral cache)
        var allSystemContent string
        var allSystemParts []providers.ContentBlock
        for _, msg := range messages {
                if msg.Role == "system" {
                        allSystemContent += msg.Content
                        allSystemParts = append(allSystemParts, msg.SystemParts...)
                }
        }
        if !strings.Contains(allSystemContent, "tool_search_tool_bm25") {
                t.Fatalf("system prompt missing tool discovery rule: %q", allSystemContent)
        }

        var found bool
        for _, part := range allSystemParts {
                if part.PromptSource == string(PromptSourceToolDiscovery) {
                        found = true
                        if part.PromptLayer != string(PromptLayerCapability) || part.PromptSlot != string(PromptSlotTooling) {
                                t.Fatalf("tool discovery metadata = %#v, want capability/tooling", part)
                        }
                        if part.CacheControl == nil || part.CacheControl.Type != "ephemeral" {
                                t.Fatalf("tool discovery cache control = %#v, want ephemeral", part.CacheControl)
                        }
                }
        }
        if !found {
                t.Fatal("system parts missing tool discovery prompt metadata")
        }
}

func TestContextBuilder_CollectsMCPServerContributor(t *testing.T) {
        t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
        cb := NewContextBuilder(t.TempDir())
        err := cb.RegisterPromptContributor(mcpServerPromptContributor{
                serverName: "GitHub Server",
                toolCount:  3,
                deferred:   true,
        })
        if err != nil {
                t.Fatalf("RegisterPromptContributor() error = %v", err)
        }

        messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{CurrentMessage: "hello"})

        // MCP content should be in a system message (stable since it's ephemeral)
        var allSystemContent string
        var allSystemParts []providers.ContentBlock
        for _, msg := range messages {
                if msg.Role == "system" {
                        allSystemContent += msg.Content
                        allSystemParts = append(allSystemParts, msg.SystemParts...)
                }
        }
        if !strings.Contains(allSystemContent, "MCP server `GitHub Server` is connected") {
                t.Fatalf("system prompt missing MCP contributor content: %q", allSystemContent)
        }

        var found bool
        for _, part := range allSystemParts {
                if part.PromptSource == "mcp:github_server" {
                        found = true
                        if part.PromptLayer != string(PromptLayerCapability) || part.PromptSlot != string(PromptSlotMCP) {
                                t.Fatalf("mcp metadata = %#v, want capability/mcp", part)
                        }
                        if part.CacheControl == nil || part.CacheControl.Type != "ephemeral" {
                                t.Fatalf("mcp cache control = %#v, want ephemeral", part.CacheControl)
                        }
                }
        }
        if !found {
                t.Fatal("system parts missing MCP prompt metadata")
        }
}

type testPromptContributor struct {
        desc PromptSourceDescriptor
        part PromptPart
}

func (c testPromptContributor) PromptSource() PromptSourceDescriptor {
        return c.desc
}

func (c testPromptContributor) ContributePrompt(_ context.Context, _ PromptBuildRequest) ([]PromptPart, error) {
        return []PromptPart{c.part}, nil
}

func TestContextBuilder_CollectsRegisteredPromptContributors(t *testing.T) {
        t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
        cb := NewContextBuilder(t.TempDir())

        sourceID := PromptSourceID("test:contributor")
        err := cb.RegisterPromptContributor(testPromptContributor{
                desc: PromptSourceDescriptor{
                        ID:      sourceID,
                        Owner:   "test",
                        Allowed: []PromptPlacement{{Layer: PromptLayerCapability, Slot: PromptSlotMCP}},
                },
                part: PromptPart{
                        ID:      "capability.mcp.test",
                        Layer:   PromptLayerCapability,
                        Slot:    PromptSlotMCP,
                        Source:  PromptSource{ID: sourceID, Name: "test"},
                        Content: "registered contributor prompt",
                        Cache:   PromptCacheEphemeral, // stable by default for capability parts
                },
        })
        if err != nil {
                t.Fatalf("RegisterPromptContributor() error = %v", err)
        }

        messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{CurrentMessage: "hello"})
        var allSystemContent string
        for _, msg := range messages {
                if msg.Role == "system" {
                        allSystemContent += msg.Content
                }
        }
        if !strings.Contains(allSystemContent, "registered contributor prompt") {
                t.Fatalf("system prompt missing contributor content: %q", allSystemContent)
        }
}
