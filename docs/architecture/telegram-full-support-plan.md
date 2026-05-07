# Picoclaw Telegram Full Support — Implementation Plan for Complete Telegram Bot API Integration

**Repository:** picodozbotdoz/picoclaw  
**Branch:** exp/telegram_full_support  
**Base Branch:** imp/0505  
**Date:** 2026-05-07

---

## 1. Executive Summary

The picoclaw project currently includes a Telegram channel implementation that supports basic chat functionality, including text messaging, media exchange (photos, voice, audio, documents), group chat interaction with mention detection, and forum topic isolation for session management. However, the implementation has significant gaps when compared to the full Telegram Bot API 9.6 feature set, and several critical features that users expect in production Telegram bots are either missing or incomplete.

This plan identifies 23 distinct feature gaps across 5 priority tiers, proposes a phased implementation roadmap spanning 8 phases, and provides detailed technical specifications for each feature. The most impactful issues are the missing `allowed_updates` configuration (which prevents callback queries and inline queries from being received), the absence of `ReactionCapable` implementation, incomplete inbound media type handling (stickers, video, location, etc. are silently dropped), and the lack of callback query and inline query handlers. Addressing these gaps will transform picoclaw from a basic Telegram chatbot into a fully-featured Telegram bot platform.

The implementation follows a risk-prioritized approach: critical fixes that require minimal code changes come first, followed by feature additions that build on existing infrastructure, and finally advanced capabilities that may require architectural changes. Each phase is designed to be independently deployable, with no phase depending on a later phase for correctness.

---

## 2. Current Implementation Analysis

### 2.1 Implemented Features

| Feature | Implementation Status | Key File |
|---------|----------------------|----------|
| Text messaging (inbound + outbound) | Complete | `telegram.go:715-918`, `185-318` |
| Media sending (photo, audio, video, document, voice) | Complete | `telegram.go:594-713` |
| Media receiving (photo, voice, audio, document) | Complete | `telegram.go:770-823` |
| Group chat mention detection | Complete | `telegram.go:1193-1227` |
| Bot mention stripping | Complete | `telegram.go:1270-1280` |
| Forum topic session isolation | Complete | `telegram.go:863-871`, `allocator.go:150` |
| Outbound topic-aware messaging | Complete | `telegram.go:1156-1176` |
| Streaming (SendMessageDraft API) | Complete | `telegram.go:1282-1387` |
| Message editing (for placeholders/tool feedback) | Complete | `telegram.go:412-466` |
| Message deletion | Complete | `telegram.go:469-482` |
| Typing indicator | Complete | `telegram.go:377-409` |
| Placeholder message (Thinking...) | Complete | `telegram.go:570-591` |
| Tool feedback animation | Complete | `telegram.go` + `tool_feedback_animator.go` |
| Bot command registration | Complete | `command_registration.go` |
| Markdown/HTML conversion | Complete | `parser_markdown_to_html.go`, `parse_markdown_to_md_v2.go` |
| Proxy support | Complete | `telegram.go` (proxy config) |
| Custom Bot API base URL | Complete | `telegram.go` (base_url config) |

### 2.2 Channel Interface Implementation

| Interface | Implemented | Method |
|-----------|-------------|--------|
| Channel | Yes | `Name`, `Start`, `Stop`, `Send`, `IsRunning`, `IsAllowed`, `IsAllowedSender`, `ReasoningChannelID` |
| TypingCapable | Yes | `StartTyping()` |
| MessageEditor | Yes | `EditMessage()` |
| MessageDeleter | Yes | `DeleteMessage()` |
| PlaceholderCapable | Yes | `SendPlaceholder()` |
| StreamingCapable | Yes | `BeginStream()` |
| MediaSender | Yes | `SendMedia()` |
| CommandRegistrarCapable | Yes | `RegisterCommands()` |
| VoiceCapabilityProvider | Yes | `VoiceCapabilities()` |
| ReactionCapable | **No** | `ReactToMessage()` — not implemented despite Telegram supporting `SetMessageReaction` |

---

## 3. Gap Analysis

### 3.1 Critical Gaps (Tier 1)

These gaps represent fundamental infrastructure issues that block multiple features from working. They require minimal code changes but have outsized impact on functionality.

#### GAP-1: Missing `allowed_updates` in GetUpdatesParams

The `Start()` method calls `bot.UpdatesViaLongPolling()` without setting `AllowedUpdates`. Telegram's default only delivers `message` updates, which means `callback_query`, `inline_query`, `message_reaction`, `edited_message`, and `channel_post` updates are never delivered to the bot. Even if handlers were added for these update types, they would never fire because the long polling connection does not request them. This single configuration omission blocks the entire interactive button, inline mode, reaction tracking, and edited message feature categories.

**File:** `pkg/channels/telegram/telegram.go:120-160`

**Impact:** Blocks callback queries, inline queries, reaction updates, edited messages, channel posts

**Effort:** 1-2 lines of configuration change

#### GAP-2: No callback query handler

There is zero implementation for callback queries. No `bh.HandleCallbackQuery()` registration exists, no `AnswerCallbackQuery` call is present, and no inline keyboard generation code exists anywhere in the Telegram channel. This means any feature that would add inline keyboard buttons (pagination, confirm/cancel dialogs, quick actions) would have no way to process user interactions. Callback queries are essential for interactive bot experiences and are used by virtually every production Telegram bot.

**File:** `pkg/channels/telegram/telegram.go` (missing handler)

**Impact:** Blocks all interactive button functionality

**Effort:** Medium — new handler + callback routing + bus integration

#### GAP-3: No inline query handler

There is zero implementation for inline queries. No `bh.HandleInlineQuery()` registration exists, and no `AnswerInlineQuery` call is present. This prevents the bot from being used in inline mode (typing `@botname query` in any chat), which is a key Telegram feature for bot discoverability and cross-chat interaction. Inline mode allows users to invoke the bot in any conversation without adding it to the chat.

**File:** `pkg/channels/telegram/telegram.go` (missing handler)

**Impact:** Blocks inline mode usage across all chats

**Effort:** Medium — new handler + result generation + bus integration

### 3.2 High Priority Gaps (Tier 2)

These gaps represent features that users commonly expect from a Telegram bot. Their absence degrades the user experience noticeably and causes confusion (e.g., sent stickers producing no response).

#### GAP-4: ReactionCapable not implemented

The `TelegramChannel` does not implement the `ReactionCapable` interface, despite Telegram supporting the `SetMessageReaction` API since Bot API 6.8. Other channels (Slack, OneBot, Feishu) all implement this interface. The `BaseChannel.HandleMessageWithContext()` auto-triggers a reaction (typically eyes emoji) on inbound messages when the channel implements `ReactionCapable`. The absence means Telegram users receive no visual acknowledgment that the bot has received their message, while users on other channels do get this feedback.

**Interface:** `channels.ReactionCapable` — method `ReactToMessage()`

**Impact:** No visual receipt acknowledgment; inconsistent with other channels

**Effort:** Low — implement `ReactToMessage()` using `bot.SetMessageReaction()`

#### GAP-5: Many inbound message types silently dropped

The `handleMessage()` method only processes text, photo, voice, audio, and document content. All other message types (sticker, video, video_note, animation, contact, location, venue, poll, dice, game) are silently dropped because they produce no content string and no media paths. The user receives no feedback that their message was ignored. This is particularly confusing for stickers (commonly used in Telegram conversations) and location/venue sharing (common in logistics and meeting coordination use cases).

| Message Type | Current Behavior | Proposed Behavior |
|-------------|-----------------|-------------------|
| Sticker | Silently dropped | Convert to text description `[sticker: emoji]` + download if animated |
| Video | Silently dropped | Download and process as media attachment |
| VideoNote | Silently dropped | Download and process as video media |
| Animation (GIF) | Silently dropped | Download and process as document/media |
| Contact | Silently dropped | Convert to text `[contact: Name, Phone]` |
| Location | Silently dropped | Convert to text `[location: lat, lng]` + Google Maps link |
| Venue | Silently dropped | Convert to text `[venue: Name, Address]` + location link |
| Poll | Silently dropped | Convert to text `[poll: Question]` with options list |
| Dice | Silently dropped | Convert to text `[dice: emoji value]` |

**Impact:** Users confused when common Telegram content types receive no response

**Effort:** Low-Medium — additive content extraction in `handleMessage()`

#### GAP-6: Incomplete quoted/reply media handling

When a user replies to a message, the `handleMessage()` method downloads quoted voice and audio attachments but ignores quoted photos, documents, videos, and stickers. This means if a user replies to a photo with text, the bot loses the photo context. The fix involves extending the `quotedTelegramMediaRefs` map to include all media types that the bot already knows how to download, and processing them the same way as direct media attachments.

**Impact:** Lost context in reply chains involving photos, documents, videos

**Effort:** Low — extend existing quoted media download logic

#### GAP-7: ReplyToSenderID not populated

The `InboundContext.ReplyToSenderID` field is never set by the Telegram channel, even though the sender information is available from `message.ReplyToMessage.From.ID`. This field is used by the agent system for conversation threading and context tracking. Without it, the agent cannot determine who the user is replying to, which affects multi-user group conversation coherence.

**Impact:** Agent cannot track reply threads in multi-user conversations

**Effort:** Trivial — single line addition in `handleMessage()`

### 3.3 Medium Priority Gaps (Tier 3)

#### GAP-8: No edited message handling

When users edit their messages in Telegram, the bot receives an `edited_message` update. However, this update type is not in `allowed_updates` (see GAP-1) and there is no handler for it. The bot should be able to process edited messages to update its understanding of user intent, especially in scenarios where users correct typos or refine their prompts. The implementation would involve adding `"edited_message"` to `AllowedUpdates` and routing edited messages through the same `handleMessage` pipeline with an `is_edit` flag in `InboundContext`.

#### GAP-9: No media group (album) support

Telegram allows sending 2-10 photos or videos as a grouped album via `sendMediaGroup`. The current `SendMedia()` implementation sends each media item as a separate message, which breaks the album experience. Users who send multiple photos at once receive them back as individual messages rather than a cohesive album. Implementing `sendMediaGroup` requires batching outbound media items and creating `InputMediaPhoto`/`InputMediaVideo` objects with shared reply parameters.

#### GAP-10: No message pinning support

Bots can pin important messages in chats where they have pin permissions. This is useful for pinning conversation summaries, instructions, or status updates. The current implementation has no `pinChatMessage` or `unpinChatMessage` capability. This should be implemented as a new channel interface (`PinnableCapable`) and exposed as a tool so the agent can pin important messages when needed.

#### GAP-11: No outbound sticker support

The bot cannot send stickers via `sendSticker`, even though this is a common way to add personality to bot interactions. Stickers are especially popular in Telegram culture and would enhance the bot's conversational presence. The implementation would add a `"sticker"` part type to `SendMedia()` and use `bot.SendSticker()` for delivery.

#### GAP-12: No location/venue outbound support

The bot cannot share locations or venues via `sendLocation`/`sendVenue`. This is relevant for bots that provide meeting coordination, delivery tracking, or point-of-interest recommendations. These would be new part types in `SendMedia()` that map to the corresponding Bot API methods.

#### GAP-13: No batch message deletion

The current `DeleteMessage()` only supports deleting a single message. Telegram's `deleteMessages` API supports deleting up to 100 messages in one call, which is important for cleanup operations (e.g., deleting streaming draft messages, cleaning up tool feedback messages). This should be added as a new `DeleteMessages()` method on a `BatchMessageDeleter` interface.

### 3.4 Lower Priority Gaps (Tier 4)

#### GAP-14: No channel post handling

Bots that are administrators in Telegram channels receive `channel_post` updates. The current implementation does not request this update type and has no handler for it. Channel posts could be processed similarly to group messages but with channel-specific semantics (no mention detection needed, different session isolation strategy).

#### GAP-15: No forum topic management API

Telegram provides a full set of forum topic management methods (`createForumTopic`, `editForumTopic`, `closeForumTopic`, `reopenForumTopic`, `deleteForumTopic`). The current implementation only reads the `message_thread_id` from inbound messages but cannot create, modify, or manage topics. This would be useful for organized knowledge bases, project management, or multi-topic discussion bots.

#### GAP-16: No forward/copy message support

The `forwardMessage`, `forwardMessages`, `copyMessage`, and `copyMessages` APIs allow bots to forward or copy messages between chats. This is useful for cross-chat information sharing, archiving, and message relay functionality.

#### GAP-17: No poll creation support

The `sendPoll` API allows bots to create polls and quizzes in groups. This could be used for decision-making tools, feedback collection, or interactive engagement features.

#### GAP-18: No invoice/payment support

Telegram's payment system (`sendInvoice`, `answerShippingQuery`, `answerPreCheckoutQuery`) enables bots to accept payments. This is relevant for commercial bot applications, premium feature gating, and digital goods sales. Implementation would require significant infrastructure (payment provider integration, order tracking) but the Bot API surface is well-defined.

### 3.5 Advanced / Future Gaps (Tier 5)

#### GAP-19: No Telegram Mini App (Web App) support

Telegram Mini Apps run in a WebView and provide rich interactive experiences. While the client-side JavaScript API is extensive, the server-side integration involves validating `WebAppInitData`, handling `web_app_data` messages, and optionally setting `MenuButtonWebApp`. This is a significant feature that would enable custom UI experiences within Telegram.

#### GAP-20: No webhook mode

The current implementation uses long-polling exclusively. For production deployments behind load balancers or in serverless environments, webhook mode (`setWebhook` + HTTP endpoint) is more appropriate. Webhook mode also enables better horizontal scaling and reduced latency compared to long-polling.

#### GAP-21: No private chat topic support (Bot API 9.3+)

Bot API 9.3 added topic support for private chats, controlled via @BotFather settings. The current forum topic handling only considers group `IsForum` flag. Private chat topics would require additional handling in the session key generation logic and inbound context population.

#### GAP-22: No chat member update tracking

The `chat_member` and `my_chat_member` update types allow bots to track when users join, leave, or are promoted/demoted in groups. This enables welcome messages, access control, and group management features. The current implementation does not request these update types.

#### GAP-23: No rate limiting on outbound sends

Telegram enforces rate limits of approximately 20 messages per minute per group and 30 messages per second globally. The current implementation has no rate limiting, which risks hitting Telegram's rate limits during burst operations (e.g., sending tool feedback updates, streaming, or responding to multiple users simultaneously). A rate limiter should be implemented to prevent 429 errors and potential temporary bans.

---

## 4. Phased Implementation Plan

The implementation is organized into 8 phases, ordered by risk-adjusted priority. Each phase is independently deployable and tested. No phase depends on a later phase for correctness, though later phases build on the infrastructure established by earlier ones.

### 4.1 Phase Overview

| Phase | Focus | Gaps Addressed | Estimated Effort | Risk Level |
|-------|-------|---------------|-----------------|------------|
| Phase 1 | Critical Infrastructure Fixes | GAP-1, GAP-7 | 0.5 day | Very Low |
| Phase 2 | Reaction Support | GAP-4 | 0.5 day | Low |
| Phase 3 | Inbound Media Completeness | GAP-5, GAP-6 | 2 days | Low |
| Phase 4 | Callback Query Framework | GAP-2 | 3 days | Medium |
| Phase 5 | Inline Query Support | GAP-3 | 2 days | Medium |
| Phase 6 | Edited Messages + Channel Posts | GAP-8, GAP-14, GAP-22 | 2 days | Low |
| Phase 7 | Enhanced Media Features | GAP-9, GAP-10, GAP-11, GAP-12, GAP-13 | 3 days | Low-Medium |
| Phase 8 | Advanced Capabilities | GAP-15, GAP-17, GAP-18, GAP-19, GAP-20, GAP-21, GAP-23 | 5+ days | Medium-High |

### 4.2 Phase 1: Critical Infrastructure Fixes

**Timeline:** 0.5 day  
**Risk:** Very Low — configuration changes and single-line additions  
**Dependencies:** None

This phase addresses the most impactful gaps with the least code change. Both fixes are essentially one-liners that unlock significant downstream functionality.

#### GAP-1: Add `allowed_updates` to GetUpdatesParams

Add the `AllowedUpdates` field to the `GetUpdatesParams` struct in `Start()`. This is the single most important configuration change because it unlocks all update types that the bot can receive. Without this, no amount of handler code will process callback queries, inline queries, reactions, or edited messages.

```go
// In Start() method, change:
updates, err := c.bot.UpdatesViaLongPolling(c.ctx, &telego.GetUpdatesParams{
    Timeout: 30,
})

// To:
updates, err := c.bot.UpdatesViaLongPolling(c.ctx, &telego.GetUpdatesParams{
    Timeout: 30,
    AllowedUpdates: []string{
        "message", "edited_message",
        "channel_post", "edited_channel_post",
        "inline_query", "chosen_inline_result",
        "callback_query",
        "message_reaction", "message_reaction_count",
        "my_chat_member", "chat_member",
    },
})
```

The `AllowedUpdates` list includes all update types that will be needed across all phases. Adding them upfront avoids the need to restart the bot when new handlers are added in later phases. Update types that have no handler will simply be ignored by the telego `BotHandler`, so there is no risk in requesting them early.

#### GAP-7: Populate ReplyToSenderID

Add a single line in `handleMessage()` to populate the `ReplyToSenderID` field when the inbound message is a reply. This information is critical for multi-user conversation coherence and enables the agent to understand who the user is responding to.

```go
// After setting ReplyToMessageID, add:
if message.ReplyToMessage != nil && message.ReplyToMessage.From != nil {
    inboundCtx.ReplyToSenderID = fmt.Sprintf("%d", message.ReplyToMessage.From.ID)
}
```

### 4.3 Phase 2: Reaction Support

**Timeline:** 0.5 day  
**Risk:** Low — implementing a well-defined interface  
**Dependencies:** Phase 1 (`allowed_updates` must include `message_reaction`)

#### GAP-4: Implement ReactionCapable

Implement the `ReactionCapable` interface on `TelegramChannel` to add visual receipt acknowledgment. When a user sends a message, the bot will automatically add a reaction (eyes emoji by default, configurable) to indicate the message has been received and is being processed. This is consistent with how Slack, OneBot, and Feishu channels already behave.

The implementation involves adding a `ReactToMessage(msg bus.OutboundMessage) error` method that calls `bot.SetMessageReaction()` with the appropriate chat ID, message ID, and reaction emoji. The method should handle the case where the bot lacks reaction permissions in the chat by gracefully degrading (logging a warning and returning nil).

```go
func (c *TelegramChannel) ReactToMessage(msg bus.OutboundMessage) error {
    chatID, msgID, err := parseTelegramChatIDAndMsgID(msg.ChatID, msg.Context.MessageID)
    if err != nil {
        return err
    }
    reaction := msg.Context.ReactionEmoji
    if reaction == "" {
        reaction = "\U0001F440" // eyes emoji default
    }
    return c.bot.SetMessageReaction(c.ctx, &telego.SetMessageReactionParams{
        ChatID:    telego.ChatID{ID: chatID},
        MessageID: msgID,
        Reaction: []telego.ReactionType{{
            Type:  "emoji",
            Emoji: reaction,
        }},
    })
}
```

### 4.4 Phase 3: Inbound Media Completeness

**Timeline:** 2 days  
**Risk:** Low — additive content extraction, no existing behavior changes  
**Dependencies:** None (independent of Phase 1-2 but recommended after)

#### GAP-5: Add content extraction for all message types

Extend `handleMessage()` to extract meaningful content from all Telegram message types. The approach is to convert non-text message types into descriptive text strings that the agent can understand, and where applicable, download and attach media files. This ensures the bot always responds to user messages rather than silently ignoring them.

The implementation follows a consistent pattern for each new type: check if the field is non-nil, download any associated file if needed, and construct a human-readable text description. For sticker messages, include the emoji associated with the sticker. For location messages, include a Google Maps link. For contact messages, include the name and phone number. For poll messages, include the question and options.

#### GAP-6: Extend quoted media handling

Extend the quoted message processing in `handleMessage()` to download photos, documents, and videos from the replied-to message, not just voice and audio. The existing `quotedTelegramMediaRefs` map and download logic should be extended with entries for photo, document, and video file IDs. This ensures the agent has full context when a user replies to a media message with text.

### 4.5 Phase 4: Callback Query Framework

**Timeline:** 3 days  
**Risk:** Medium — requires new bus event types and agent integration  
**Dependencies:** Phase 1 (`allowed_updates` must include `callback_query`)

#### GAP-2: Implement callback query handling

This is the most architecturally significant addition. Callback queries enable interactive inline keyboard buttons, which are used for pagination, confirm/cancel dialogs, option selection, and many other interactive patterns. The implementation requires several coordinated changes across the codebase.

First, a new bus event type (`CallbackQueryEvent`) must be defined in the bus package, containing the callback data, originating chat, message ID, and user information. Second, a new handler must be registered in `Start()` using `bh.HandleCallbackQuery()`. Third, the handler must call `bot.AnswerCallbackQuery()` within 30 seconds to acknowledge the callback. Fourth, the agent system must be extended to process callback events and generate appropriate responses.

**Key design decisions:**

- Callback data is limited to 64 bytes — use short identifiers that map to richer context stored server-side
- `AnswerCallbackQuery` must be called within 30 seconds regardless of processing time — acknowledge immediately, process asynchronously
- Inline keyboards should be generated by the agent via a new tool (e.g., `telegram_inline_keyboard`) rather than hard-coded
- Callback routing should map to the existing session key system so callbacks are processed in the correct conversation context

### 4.6 Phase 5: Inline Query Support

**Timeline:** 2 days  
**Risk:** Medium — requires new bus event types and agent integration  
**Dependencies:** Phase 1 (`allowed_updates` must include `inline_query`)

#### GAP-3: Implement inline query handling

Inline queries allow users to invoke the bot by typing `@botname` followed by a query in any chat. The bot responds with a list of results that the user can tap to send. This is a powerful feature for bot discoverability and cross-chat utility. The implementation requires a new handler for inline queries, a mechanism to generate inline query results, and integration with the agent system for dynamic result generation.

The key challenge is generating results quickly — `answerInlineQuery` should be called within 10 seconds. For an LLM-powered bot, this may require a fast-path response mechanism (e.g., cached responses, pre-computed suggestions) rather than waiting for a full agent response. A hybrid approach is recommended: return cached/quick results immediately, and optionally update with richer results if the user waits.

### 4.7 Phase 6: Edited Messages + Channel Posts + Chat Member Tracking

**Timeline:** 2 days  
**Risk:** Low — extending existing handler patterns  
**Dependencies:** Phase 1 (`allowed_updates` must include `edited_message`, `channel_post`, `chat_member`)

#### GAP-8: Handle edited messages

Register an `edited_message` handler that processes message edits through the same `handleMessage` pipeline with an `IsEdit` flag in `InboundContext`. The agent should be able to distinguish between new messages and edits to update its understanding without creating duplicate conversation entries. The `Raw` metadata map should include the `edit_date` timestamp.

#### GAP-14: Handle channel posts

Register a `channel_post` handler that processes posts from channels where the bot is an administrator. Channel posts should be routed with a `"channel"` chat type and may require different session isolation logic than group messages (e.g., one session per channel rather than per user).

#### GAP-22: Track chat member changes

Register handlers for `my_chat_member` and `chat_member` updates. These enable welcome messages for new members, access control based on group membership, and group management features. The `chat_member` updates require the bot to have appropriate administrator permissions in the group.

### 4.8 Phase 7: Enhanced Media Features

**Timeline:** 3 days  
**Risk:** Low-Medium — extending existing `SendMedia()` with new part types  
**Dependencies:** None

#### GAP-9: Media group (album) support

Implement `sendMediaGroup` for sending 2-10 photos or videos as a grouped album. The `SendMedia()` method should detect when multiple image/video parts are queued and batch them into a single `sendMediaGroup` call. This requires changes to how outbound messages are assembled — currently each media part is sent individually. A batching layer could queue media parts for a short window and then send them as a group.

#### GAP-10: Message pinning

Add a `PinnableCapable` interface with `PinMessage()` and `UnpinMessage()` methods. Expose these as tools so the agent can pin important messages (summaries, instructions, status updates). The implementation requires the bot to have pin permissions in the target chat, which should be checked gracefully.

#### GAP-11: Outbound sticker support

Add a `"sticker"` part type to `SendMedia()` that uses `bot.SendSticker()`. This could be used by the agent to add personality to conversations or to acknowledge messages with a visual response. The sticker file ID or URL must be provided by the agent, potentially via a sticker search tool.

#### GAP-12: Location/venue outbound support

Add `"location"` and `"venue"` part types to `SendMedia()` that use `bot.SendLocation()` and `bot.SendVenue()` respectively. These enable the bot to share meeting locations, delivery points, or points of interest. The part data should include latitude, longitude, and optionally name/address for venues.

#### GAP-13: Batch message deletion

Add a `BatchMessageDeleter` interface with `DeleteMessages(chatID string, messageIDs []string) error` method that calls `bot.DeleteMessages()`. This is useful for cleaning up multiple streaming draft messages or tool feedback messages in a single API call, reducing latency and API usage.

### 4.9 Phase 8: Advanced Capabilities

**Timeline:** 5+ days (ongoing)  
**Risk:** Medium-High — significant new features, some require external integrations  
**Dependencies:** Phases 1-4 (infrastructure) must be complete

Phase 8 encompasses features that are valuable but not essential for a functional Telegram bot. These should be prioritized based on user demand and deployment context. Each feature in this phase is largely independent and can be implemented separately.

| Feature | GAP | Key Considerations | External Dependencies |
|---------|-----|--------------------|-----------------------|
| Forum topic management | GAP-15 | Requires bot admin permissions; API surface well-defined | None |
| Poll creation | GAP-17 | Useful for group engagement; quiz mode adds gamification | None |
| Payment/invoice support | GAP-18 | Complex flow: invoice, shipping, pre-checkout, success | Payment provider (Stripe, etc.) |
| Mini App support | GAP-19 | Significant frontend development; WebAppInitData validation | Web hosting for Mini App |
| Webhook mode | GAP-20 | Alternative to long-polling; requires public HTTPS endpoint | TLS certificate, public endpoint |
| Private chat topics | GAP-21 | Bot API 9.3+; @BotFather configuration required | None |
| Outbound rate limiting | GAP-23 | Essential for production; prevents 429 errors and bans | None |

---

## 5. Technical Design Specifications

### 5.1 Bus Event Extensions

The bus package (`pkg/bus/`) needs new event types to support callback queries and inline queries. These events follow the existing publish/subscribe pattern used for inbound messages. The key design principle is that callback and inline events should integrate with the existing session system so they are processed in the correct conversation context.

```go
// New event types in pkg/bus/types.go:

type CallbackQueryEvent struct {
    Channel      string            `json:"channel"`
    ChatID       string            `json:"chat_id"`
    MessageID    string            `json:"message_id"`
    CallbackData string            `json:"callback_data"`
    QueryID      string            `json:"query_id"`
    SenderID     string            `json:"sender_id"`
    TopicID      string            `json:"topic_id,omitempty"`
    Raw          map[string]string `json:"raw,omitempty"`
}

type InlineQueryEvent struct {
    Channel   string            `json:"channel"`
    Query     string            `json:"query"`
    QueryID   string            `json:"query_id"`
    SenderID  string            `json:"sender_id"`
    ChatType  string            `json:"chat_type,omitempty"`
    Raw       map[string]string `json:"raw,omitempty"`
}
```

### 5.2 InboundContext Extensions

The `InboundContext` struct needs additional fields to support edited messages and callback queries. These fields should be added with `omitempty` tags to maintain backward compatibility with existing serialized contexts.

```go
// Additional fields in InboundContext:
IsEdit        bool   `json:"is_edit,omitempty"`
EditDate      int64  `json:"edit_date,omitempty"`
CallbackData  string `json:"callback_data,omitempty"`
```

### 5.3 Channel Interface Extensions

New channel interfaces should be defined in `pkg/channels/interfaces.go`. Each interface should follow the existing pattern of defining a single method that can be optionally implemented by channels that support the capability.

```go
// New interfaces:

type ReactionCapable interface {
    ReactToMessage(msg OutboundMessage) error
}

type PinnableCapable interface {
    PinMessage(chatID, messageID string) error
    UnpinMessage(chatID, messageID string) error
}

type BatchMessageDeleter interface {
    DeleteMessages(chatID string, messageIDs []string) error
}

type CallbackQueryHandler interface {
    HandleCallbackQuery(query CallbackQueryEvent) error
}

type InlineQueryHandler interface {
    HandleInlineQuery(query InlineQueryEvent) ([]InlineQueryResult, error)
}
```

### 5.4 Config Extensions

The `TelegramSettings` struct needs additional configuration options for new features. These should be added with `omitzero` or `omitempty` tags to maintain backward compatibility.

```go
// Additional fields in TelegramSettings:
ReactionEmoji    string `json:"reaction_emoji,omitempty"`  // default: eyes
EnableInline     bool   `json:"enable_inline,omitempty"`  // enable inline mode
WebhookURL       string `json:"webhook_url,omitempty"`    // for webhook mode
WebhookSecret    string `json:"webhook_secret,omitempty"` // HMAC verification
RateLimitRPS     int    `json:"rate_limit_rps,omitempty"` // outbound rate limit
```

### 5.5 Session Key Impact Analysis

Several new features require careful consideration of session key generation to ensure correct conversation isolation. The existing `shouldPreserveTelegramForumIsolation()` function in `allocator.go` provides the pattern for Telegram-specific session handling.

| Feature | Session Key Impact | Action Required |
|---------|-------------------|-----------------|
| Edited messages | None — same session as original message | No change needed |
| Callback queries | Must route to originating message's session | Extract chatID/threadID from callback message |
| Inline queries | No session — transient, results sent directly | New `InlineQueryEvent`, no session allocation |
| Channel posts | New session per channel (not per user) | Add `"channel"` chat type handling in allocator |
| Private chat topics | New session per topic (like forum topics) | Extend forum isolation logic for private chats |
| Chat member events | No session — administrative events | New `ChatMemberEvent`, no session allocation |

---

## 6. File Change Map

The following table maps each phase to the specific files that need to be modified or created. This serves as a checklist for implementation and code review.

| Phase | File | Action | Description |
|-------|------|--------|-------------|
| 1 | `pkg/channels/telegram/telegram.go` | Modify | Add `AllowedUpdates` to `GetUpdatesParams` |
| 1 | `pkg/channels/telegram/telegram.go` | Modify | Populate `ReplyToSenderID` in `handleMessage()` |
| 2 | `pkg/channels/telegram/telegram.go` | Modify | Implement `ReactToMessage()` method |
| 2 | `pkg/channels/telegram/telegram.go` | Modify | Add `ReactionCapable` interface assertion |
| 3 | `pkg/channels/telegram/telegram.go` | Modify | Add content extraction for sticker, video, video_note, animation, contact, location, venue, poll, dice |
| 3 | `pkg/channels/telegram/telegram.go` | Modify | Extend quoted media download for photo, document, video |
| 3 | `pkg/channels/telegram/telegram.go` | Modify | Add `downloadVideo()`, `downloadAnimation()` helpers |
| 4 | `pkg/bus/types.go` | Modify | Add `CallbackQueryEvent` type |
| 4 | `pkg/bus/bus.go` | Modify | Add `PublishCallbackQuery()` method |
| 4 | `pkg/channels/telegram/telegram.go` | Modify | Add callback query handler in `Start()` |
| 4 | `pkg/channels/telegram/telegram.go` | Modify | Implement `HandleCallbackQuery()` |
| 4 | `pkg/channels/interfaces.go` | Modify | Add `CallbackQueryHandler` interface |
| 4 | `pkg/agent/` | Modify | Agent callback processing integration |
| 5 | `pkg/bus/types.go` | Modify | Add `InlineQueryEvent` type |
| 5 | `pkg/bus/bus.go` | Modify | Add `PublishInlineQuery()` method |
| 5 | `pkg/channels/telegram/telegram.go` | Modify | Add inline query handler in `Start()` |
| 5 | `pkg/channels/telegram/telegram.go` | Modify | Implement `HandleInlineQuery()` |
| 5 | `pkg/channels/interfaces.go` | Modify | Add `InlineQueryHandler` interface |
| 6 | `pkg/channels/telegram/telegram.go` | Modify | Add `edited_message` handler |
| 6 | `pkg/channels/telegram/telegram.go` | Modify | Add `channel_post` handler |
| 6 | `pkg/channels/telegram/telegram.go` | Modify | Add `chat_member` handlers |
| 6 | `pkg/bus/types.go` | Modify | Add `IsEdit`, `EditDate` fields to `InboundContext` |
| 7 | `pkg/channels/telegram/telegram.go` | Modify | Add media group batching in `SendMedia()` |
| 7 | `pkg/channels/telegram/telegram.go` | Modify | Add sticker, location, venue part types to `SendMedia()` |
| 7 | `pkg/channels/telegram/telegram.go` | Modify | Implement `PinMessage()`, `UnpinMessage()` |
| 7 | `pkg/channels/telegram/telegram.go` | Modify | Implement `DeleteMessages()` (batch) |
| 7 | `pkg/channels/interfaces.go` | Modify | Add `PinnableCapable`, `BatchMessageDeleter` interfaces |
| 8 | `pkg/config/config_struct.go` | Modify | Add new `TelegramSettings` fields |
| 8 | `pkg/channels/telegram/telegram.go` | Modify | Add rate limiter for outbound sends |
| 8 | `pkg/channels/telegram/telegram.go` | Modify | Add forum topic management methods |
| 8 | `pkg/channels/telegram/telegram.go` | Modify | Add webhook mode support |

---

## 7. Testing Strategy

Each phase should include comprehensive tests that verify the new functionality without requiring a live Telegram bot token. The existing test suite in `telegram_test.go` (1102 lines) follows a pattern of mocking the telego bot and verifying method calls, which should be extended for all new features.

### 7.1 Test Categories

| Test Category | Scope | Approach |
|--------------|-------|----------|
| Unit tests | Individual methods | Mock telego bot, verify API calls with correct parameters |
| Integration tests | Handler pipeline | Simulate inbound updates, verify bus events published |
| Session isolation tests | Session key generation | Verify correct session keys for forums, private chats, channels |
| Error handling tests | Graceful degradation | Simulate API errors, verify fallback behavior |
| Rate limit tests | Outbound throttling | Verify rate limiter prevents bursts above threshold |

### 7.2 Critical Test Scenarios

- Forum topic: inbound message in topic 42 routes to compositeChatID `chatID/42` session
- Forum topic: outbound message to compositeChatID includes `MessageThreadID`
- Callback query: handler answers within 30 seconds, routes to correct session
- Inline query: handler returns results within 10 seconds
- Sticker message: produces `[sticker: emoji]` content, not silently dropped
- Edited message: sets `IsEdit=true`, same session as original
- Rate limiter: blocks outbound send when threshold exceeded
- Reaction: adds eyes emoji on inbound message, graceful on permission error

---

## 8. Risks and Mitigations

| Risk | Probability | Impact | Mitigation |
|------|-------------|--------|------------|
| telego library doesn't support new Bot API methods (9.3-9.6) | Medium | High | Verify telego version; contribute upstream if needed; use raw HTTP as fallback |
| Callback query 30-second timeout too short for LLM processing | High | Medium | Acknowledge immediately, process asynchronously, send follow-up message with result |
| Rate limiting causes message delivery failures | Medium | High | Implement outbound rate limiter with queue; test under load; add backoff on 429 |
| Breaking changes to existing session key format | Low | High | All new fields use `omitempty`; no changes to existing key generation logic |
| Test coverage insufficient for edge cases | Medium | Medium | Add property-based testing for session key generation; fuzz test message parsing |
| Webhook mode requires TLS and public endpoint | Low | Low | Keep long-polling as default; webhook as opt-in; document setup requirements |

---

## 9. Dependencies

### 9.1 Library Dependencies

| Library | Current Version | Required For | Action |
|---------|----------------|--------------|--------|
| telego | Check `go.mod` | All Telegram Bot API calls | Verify Bot API 9.6 support; upgrade if needed |
| telego handler (th) | Check `go.mod` | Update routing and handlers | Verify callback/inline handler support |
| pkg/bus | Internal | New event types | Add `CallbackQueryEvent`, `InlineQueryEvent` |
| pkg/channels | Internal | New interfaces | Add `ReactionCapable`, `PinnableCapable`, etc. |

### 9.2 telego Version Compatibility

Before starting implementation, verify that the telego library version used by picoclaw supports the Bot API methods needed for each phase. Key methods to verify include: `SetMessageReaction` (Phase 2), `HandleCallbackQuery`/`AnswerCallbackQuery` (Phase 4), `AnswerInlineQuery` (Phase 5), `SendMediaGroup` (Phase 7), and `PinChatMessage`/`UnpinChatMessage` (Phase 7). If telego does not support a required method, the implementation must either use raw HTTP calls to the Telegram Bot API or contribute the missing method upstream to telego.
