// PicoClaw - Ultra-lightweight personal AI agent
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package agent

import (
        "github.com/sipeed/picoclaw/pkg/config"
        "github.com/sipeed/picoclaw/pkg/logger"
)

// PartitionBudgets holds the calculated token budgets for each context partition.
// Computed from ContextPartitionConfig percentages and the total context window.
type PartitionBudgets struct {
        SystemPromptTokens    int
        WorkingMemoryTokens   int
        RetrievedContextTokens int
        HistoryTokens         int
        OutputTokens          int
}

// ComputePartitionBudgets calculates token budgets for each context partition
// based on the ContextPartitionConfig percentages and the total context window.
// Returns nil if partition config is not set (disabled).
func ComputePartitionBudgets(partition *config.ContextPartitionConfig, contextWindow int) *PartitionBudgets {
        if partition == nil {
                return nil
        }
        effective := partition.Effective()
        if effective == nil {
                return nil
        }
        return &PartitionBudgets{
                SystemPromptTokens:    int(float64(contextWindow) * effective.SystemPromptPct / 100),
                WorkingMemoryTokens:   int(float64(contextWindow) * effective.WorkingMemoryPct / 100),
                RetrievedContextTokens: int(float64(contextWindow) * effective.RetrievedContextPct / 100),
                HistoryTokens:         int(float64(contextWindow) * effective.HistoryPct / 100),
                OutputTokens:          int(float64(contextWindow) * effective.OutputPct / 100),
        }
}

// isOverPartitionBudget checks whether any context partition exceeds its budget.
// When partition budgets are configured, this replaces the flat isOverContextBudget
// check with per-partition enforcement. Returns true if any partition is over budget,
// along with the name of the overflowing partition.
//
// When model is non-empty, historyTokens and toolDefTokens should be computed
// using model-aware estimation (EstimateMessageTokensForModel) for better accuracy.
func isOverPartitionBudget(
        partition *config.ContextPartitionConfig,
        contextWindow int,
        historyTokens int,
        systemTokens int,
        injectedContextTokens int,
        toolDefTokens int,
        maxTokens int,
        model string,
) (bool, string) {
        budgets := ComputePartitionBudgets(partition, contextWindow)
        if budgets == nil {
                // No partition config — fall back to flat budget check
                return false, ""
        }

        // System prompt partition: log warning but allow overflow
        if systemTokens > budgets.SystemPromptTokens {
                logger.WarnCF("agent", "System prompt exceeds partition budget",
                        map[string]any{
                                "system_tokens":   systemTokens,
                                "budget_tokens":   budgets.SystemPromptTokens,
                                "overflow_tokens": systemTokens - budgets.SystemPromptTokens,
                        })
        }

        // History partition: trigger targeted compression if exceeded
        if historyTokens > budgets.HistoryTokens {
                return true, "history"
        }

        // Retrieved context partition: log warning (truncation handled at injection time)
        if injectedContextTokens > budgets.RetrievedContextTokens {
                logger.WarnCF("agent", "Injected context exceeds partition budget",
                        map[string]any{
                                "injected_tokens": injectedContextTokens,
                                "budget_tokens":   budgets.RetrievedContextTokens,
                        })
        }

        return false, ""
}
