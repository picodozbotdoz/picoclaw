// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
        "context"

        "github.com/sipeed/picoclaw/pkg/bus"
        "github.com/sipeed/picoclaw/pkg/logger"
)

const (
        // maxConsecutiveSteeringFailures is the maximum number of consecutive
        // failures allowed in the steering drain loop before breaking. This
        // prevents infinite retry loops when a session is stuck (e.g., compressed
        // context contains image_url content that a non-vision model rejects).
        maxConsecutiveSteeringFailures = 3
)

func (al *AgentLoop) processMessageSync(ctx context.Context, msg bus.InboundMessage) {
        if al.channelManager != nil {
                defer al.channelManager.InvokeTypingStop(msg.Channel, msg.ChatID)
        }

        response, err := al.processMessage(ctx, msg)
        al.publishResponseOrError(ctx, msg.Channel, msg.ChatID, msg.SessionKey, response, err)
}

func (al *AgentLoop) runTurnWithSteering(ctx context.Context, initialMsg bus.InboundMessage) {
        // Process the initial message
        response, err := al.processMessage(ctx, initialMsg)
        if err != nil {
                if !al.maybePublishError(ctx, initialMsg.Channel, initialMsg.ChatID, initialMsg.SessionKey, err) {
                        return // context canceled
                }
                response = ""
        }
        finalResponse := response

        // Build continuation target
        target, targetErr := al.buildContinuationTarget(initialMsg)
        if targetErr != nil {
                logger.WarnCF("agent", "Failed to build steering continuation target",
                        map[string]any{
                                "channel": initialMsg.Channel,
                                "error":   targetErr.Error(),
                        })
                return
        }
        if target == nil {
                // System message or non-routable, response already published
                return
        }

        // Drain steering queue using existing Continue mechanism.
        // A circuit breaker stops the loop after maxConsecutiveSteeringFailures
        // consecutive errors, preventing infinite retry loops on stuck sessions.
        consecutiveFailures := 0
        for al.pendingSteeringCountForScope(target.SessionKey) > 0 {
                // Check for context cancellation between iterations
                if ctx.Err() != nil {
                        return
                }

                // Circuit breaker: too many consecutive failures on this session
                if consecutiveFailures >= maxConsecutiveSteeringFailures {
                        logger.WarnCF("agent", "Breaking steering loop: too many consecutive failures",
                                map[string]any{
                                        "channel":     target.Channel,
                                        "chat_id":     target.ChatID,
                                        "session_key": target.SessionKey,
                                        "failures":    consecutiveFailures,
                                        "queue_depth": al.pendingSteeringCountForScope(target.SessionKey),
                                })
                        break
                }

                logger.InfoCF("agent", "Continuing queued steering after turn end",
                        map[string]any{
                                "channel":     target.Channel,
                                "chat_id":     target.ChatID,
                                "session_key": target.SessionKey,
                                "queue_depth": al.pendingSteeringCountForScope(target.SessionKey),
                        })

                continued, continueErr := al.Continue(ctx, target.SessionKey, target.Channel, target.ChatID)
                if continueErr != nil {
                        consecutiveFailures++
                        logger.WarnCF("agent", "Failed to continue queued steering",
                                map[string]any{
                                        "channel":     target.Channel,
                                        "chat_id":     target.ChatID,
                                        "error":       continueErr.Error(),
                                        "consecutive": consecutiveFailures,
                                })
                        continue
                }
                if continued == "" {
                        consecutiveFailures++
                        break
                }
                consecutiveFailures = 0 // Reset on success
                finalResponse = continued
        }

        // Publish final response
        if finalResponse != "" {
                al.PublishResponseIfNeeded(ctx, target.Channel, target.ChatID, target.SessionKey, finalResponse)
        }
}

func (al *AgentLoop) resolveSteeringTarget(msg bus.InboundMessage) (string, string, bool) {
        if msg.Channel == "system" {
                return "", "", false
        }

        route, agent, err := al.resolveMessageRoute(msg)
        if err != nil || agent == nil {
                return "", "", false
        }
        allocation := al.allocateRouteSession(route, msg)

        return resolveScopeKey(allocation.SessionKey, msg.SessionKey), agent.ID, true
}
