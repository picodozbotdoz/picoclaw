package tracing

import (
        "encoding/json"
        "time"

        "github.com/sipeed/picoclaw/pkg/agent"
        "github.com/sipeed/picoclaw/pkg/logger"
)

// EventSubscriber subscribes to the agent EventBus and persists events to TraceStore.
type EventSubscriber struct {
        store *TraceStore
        sub   *agent.EventSubscription
        al    *agent.AgentLoop
}

// NewEventSubscriber creates a new EventSubscriber. Returns nil if store is nil.
func NewEventSubscriber(store *TraceStore, al *agent.AgentLoop) *EventSubscriber {
        if store == nil {
                return nil
        }
        return &EventSubscriber{store: store, al: al}
}

// Start subscribes to the agent EventBus with the specified buffer size.
func (es *EventSubscriber) Start(buffer int) {
        if es == nil || es.al == nil {
                return
        }
        if buffer <= 0 {
                buffer = 256
        }
        sub := es.al.SubscribeEvents(buffer)
        es.sub = &sub

        go es.consumeLoop()
        logger.InfoCF("tracing", "EventSubscriber started", map[string]any{"buffer": buffer})
}

// Stop unsubscribes from the agent EventBus.
func (es *EventSubscriber) Stop() {
        if es == nil || es.sub == nil {
                return
        }
        es.al.UnsubscribeEvents(es.sub.ID)
        es.sub = nil
        logger.InfoCF("tracing", "EventSubscriber stopped", nil)
}

// consumeLoop reads events from the subscription channel and persists them.
func (es *EventSubscriber) consumeLoop() {
        if es.sub == nil {
                return
        }
        ch := es.sub.C
        for evt := range ch {
                es.handleEvent(evt)
        }
}

// handleEvent converts an agent.Event to an eventWrite and enqueues it.
func (es *EventSubscriber) handleEvent(evt agent.Event) {
        if es.store == nil {
                return
        }

        payloadJSON, err := json.Marshal(evt.Payload)
        if err != nil {
                logger.WarnCF("tracing", "Failed to marshal event payload", map[string]any{
                        "event_kind": evt.Kind.String(),
                        "error":      err.Error(),
                })
                payloadJSON = []byte("{}")
        }

        ts := evt.Time
        if ts.IsZero() {
                ts = time.Now()
        }

        ew := eventWrite{
                timestamp:  ts.UTC().Format(time.RFC3339Nano),
                eventKind:  evt.Kind.String(),
                agentID:    evt.Meta.AgentID,
                sessionKey: evt.Meta.SessionKey,
                turnID:     evt.Meta.TurnID,
                iteration:  evt.Meta.Iteration,
                tracePath:  evt.Meta.TracePath,
                payload:    string(payloadJSON),
        }

        es.store.enqueueEvent(ew)
        es.updateSession(evt)
}

// updateSession upserts session records based on event kind.
func (es *EventSubscriber) updateSession(evt agent.Event) {
        if es.store == nil || evt.Meta.SessionKey == "" {
                return
        }

        now := time.Now().UTC().Format(time.RFC3339Nano)

        // Base upsert with zero increments
        args := []any{
                evt.Meta.SessionKey, // session_key
                evt.Meta.AgentID,    // agent_id
                "",                  // channel (not available from event)
                "",                  // model
                0,                   // context_window
                0, 0, 0, 0,         // total_input/output/cache/cost (incremented via COALESCE+excluded)
                now,                 // first_event_at
                now,                 // last_event_at
                0, 0, 0, 0,         // llm_call/tool/turn/compress counts
                0, 0,                // budget_usd, budget_exceeded
        }

        // Adjust counters based on event kind
        switch evt.Kind {
        case agent.EventKindTurnStart:
                args[11] = 0  // llm_call_count
                args[12] = 0  // tool_call_count
                args[13] = 1  // turn_count
                args[14] = 0  // compress_count
                es.store.metrics.ActiveSessions.Add(1)
        case agent.EventKindTurnEnd:
                es.store.metrics.ActiveSessions.Add(-1)
        case agent.EventKindLLMResponse:
                args[11] = 1 // llm_call_count
                if _, ok := evt.Payload.(agent.LLMResponsePayload); ok {
                        args[5] = 0 // total_input_tokens (not available here)
                        args[6] = 0 // total_output_tokens
                }
                es.store.metrics.TotalLLMCalls.Add(1)
        case agent.EventKindToolExecEnd:
                args[12] = 1 // tool_call_count
                es.store.metrics.TotalToolCalls.Add(1)
        case agent.EventKindContextCompress:
                args[14] = 1 // compress_count
        default:
                // No counter updates for other events
        }

        es.store.enqueueBatch(batchItem{
                query: sqlUpsertSession,
                args:  args,
        })
}
