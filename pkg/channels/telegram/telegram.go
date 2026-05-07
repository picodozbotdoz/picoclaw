package telegram

import (
        "context"
        "crypto/rand"
        "encoding/binary"
        "errors"
        "fmt"
        "io"
        "net/http"
        "net/url"
        "os"
        "regexp"
        "strconv"
        "strings"
        "sync"
        "time"

        "github.com/mymmrac/telego"
        th "github.com/mymmrac/telego/telegohandler"
        tu "github.com/mymmrac/telego/telegoutil"

        "github.com/sipeed/picoclaw/pkg/bus"
        "github.com/sipeed/picoclaw/pkg/channels"
        "github.com/sipeed/picoclaw/pkg/commands"
        "github.com/sipeed/picoclaw/pkg/config"
        "github.com/sipeed/picoclaw/pkg/identity"
        "github.com/sipeed/picoclaw/pkg/logger"
        "github.com/sipeed/picoclaw/pkg/media"
        "github.com/sipeed/picoclaw/pkg/utils"
)

var (
        reHeading    = regexp.MustCompile(`(?m)^#{1,6}\s+([^\n]+)`)
        reBlockquote = regexp.MustCompile(`^>\s*(.*)$`)
        reLink       = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
        reBoldStar   = regexp.MustCompile(`\*\*(.+?)\*\*`)
        reBoldUnder  = regexp.MustCompile(`__(.+?)__`)
        reItalic     = regexp.MustCompile(`_([^_]+)_`)
        reStrike     = regexp.MustCompile(`~~(.+?)~~`)
        reListItem   = regexp.MustCompile(`^[-*]\s+`)
        reCodeBlock  = regexp.MustCompile("```[\\w]*\\n?([\\s\\S]*?)```")
        reInlineCode = regexp.MustCompile("`([^`]+)`")
)

// Compile-time interface assertions.
var (
        _ channels.ReactionCapable      = (*TelegramChannel)(nil)
        _ channels.CallbackQueryCapable = (*TelegramChannel)(nil)
        _ channels.InlineQueryCapable   = (*TelegramChannel)(nil)
        _ channels.PinnableCapable      = (*TelegramChannel)(nil)
        _ channels.BatchMessageDeleter  = (*TelegramChannel)(nil)
)

type TelegramChannel struct {
        *channels.BaseChannel
        bot      *telego.Bot
        bh       *th.BotHandler
        bc       *config.Channel
        chatIDs  map[string]int64
        ctx      context.Context
        cancel   context.CancelFunc
        tgCfg    *config.TelegramSettings
        progress *channels.ToolFeedbackAnimator

        registerFunc      func(context.Context, []commands.Definition) error
        commandRegDelayFn func(int) time.Duration
        commandRegCancel  context.CancelFunc
}

func NewTelegramChannel(
        bc *config.Channel,
        telegramCfg *config.TelegramSettings,
        bus *bus.MessageBus,
) (*TelegramChannel, error) {
        channelName := bc.Name()
        var opts []telego.BotOption

        if telegramCfg.Proxy != "" {
                proxyURL, parseErr := url.Parse(telegramCfg.Proxy)
                if parseErr != nil {
                        return nil, fmt.Errorf("invalid proxy URL %q: %w", telegramCfg.Proxy, parseErr)
                }
                opts = append(opts, telego.WithHTTPClient(&http.Client{
                        Transport: &http.Transport{
                                Proxy: http.ProxyURL(proxyURL),
                        },
                }))
        } else if os.Getenv("HTTP_PROXY") != "" || os.Getenv("HTTPS_PROXY") != "" {
                // Use environment proxy if configured
                opts = append(opts, telego.WithHTTPClient(&http.Client{
                        Transport: &http.Transport{
                                Proxy: http.ProxyFromEnvironment,
                        },
                }))
        }

        if baseURL := strings.TrimRight(strings.TrimSpace(telegramCfg.BaseURL), "/"); baseURL != "" {
                opts = append(opts, telego.WithAPIServer(baseURL))
        }
        opts = append(opts, telego.WithLogger(logger.NewLogger("telego")))

        bot, err := telego.NewBot(telegramCfg.Token.String(), opts...)
        if err != nil {
                return nil, fmt.Errorf("failed to create telegram bot: %w", err)
        }

        base := channels.NewBaseChannel(
                channelName,
                telegramCfg,
                bus,
                bc.AllowFrom,
                channels.WithMaxMessageLength(4000),
                channels.WithGroupTrigger(bc.GroupTrigger),
                channels.WithReasoningChannelID(bc.ReasoningChannelID),
        )

        ch := &TelegramChannel{
                BaseChannel: base,
                bot:         bot,
                bc:          bc,
                chatIDs:     make(map[string]int64),
                tgCfg:       telegramCfg,
        }
        ch.progress = channels.NewToolFeedbackAnimator(ch.EditMessage)
        return ch, nil
}

func (c *TelegramChannel) Start(ctx context.Context) error {
        logger.InfoC("telegram", "Starting Telegram bot (polling mode)...")

        c.ctx, c.cancel = context.WithCancel(ctx)

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
        if err != nil {
                c.cancel()
                return fmt.Errorf("failed to start long polling: %w", err)
        }

        bh, err := th.NewBotHandler(c.bot, updates)
        if err != nil {
                c.cancel()
                return fmt.Errorf("failed to create bot handler: %w", err)
        }
        c.bh = bh

        bh.HandleMessage(func(ctx *th.Context, message telego.Message) error {
                return c.handleMessage(ctx, &message)
        }, th.AnyMessage())

        bh.HandleCallbackQuery(func(ctx *th.Context, query telego.CallbackQuery) error {
                return c.handleCallbackQuery(ctx, &query)
        })

        bh.HandleInlineQuery(func(ctx *th.Context, query telego.InlineQuery) error {
                return c.handleInlineQuery(ctx, &query)
        })

        // GAP-8: Handle edited messages through the same pipeline with IsEdit flag
        bh.HandleEditedMessage(func(ctx *th.Context, message telego.Message) error {
                return c.handleMessage(ctx, &message, true)
        })

        // GAP-14: Handle channel posts from channels where the bot is admin
        bh.HandleChannelPost(func(ctx *th.Context, message telego.Message) error {
                return c.handleChannelPost(ctx, &message)
        })

        // GAP-22: Track chat member changes for access control and welcome messages
        bh.HandleMyChatMemberUpdated(func(ctx *th.Context, chatMember telego.ChatMemberUpdated) error {
                return c.handleChatMemberUpdated(ctx, &chatMember, true)
        })
        bh.HandleChatMemberUpdated(func(ctx *th.Context, chatMember telego.ChatMemberUpdated) error {
                return c.handleChatMemberUpdated(ctx, &chatMember, false)
        })

        c.SetRunning(true)
        logger.InfoCF("telegram", "Telegram bot connected", map[string]any{
                "username": c.bot.Username(),
        })

        c.startCommandRegistration(c.ctx, commands.BuiltinDefinitions())

        go func() {
                if err = bh.Start(); err != nil {
                        logger.ErrorCF("telegram", "Bot handler failed", map[string]any{
                                "error": err.Error(),
                        })
                }
        }()

        return nil
}

func (c *TelegramChannel) Stop(ctx context.Context) error {
        logger.InfoC("telegram", "Stopping Telegram bot...")
        c.SetRunning(false)

        // Stop the bot handler
        if c.bh != nil {
                _ = c.bh.StopWithContext(ctx)
        }

        // Cancel our context (stops long polling)
        if c.cancel != nil {
                c.cancel()
        }
        if c.progress != nil {
                c.progress.StopAll()
        }
        if c.commandRegCancel != nil {
                c.commandRegCancel()
        }

        return nil
}

func (c *TelegramChannel) Send(ctx context.Context, msg bus.OutboundMessage) ([]string, error) {
        if !c.IsRunning() {
                return nil, channels.ErrNotRunning
        }

        useMarkdownV2 := c.tgCfg.UseMarkdownV2

        chatID, threadID, err := resolveTelegramOutboundTarget(msg.ChatID, &msg.Context)
        if err != nil {
                return nil, fmt.Errorf("invalid chat ID %s: %w", msg.ChatID, channels.ErrSendFailed)
        }

        logger.DebugCF("telegram", "Send resolved target", map[string]any{
                "msg_chat_id":   msg.ChatID,
                "ctx_chat_id":   msg.Context.ChatID,
                "ctx_topic_id":  msg.Context.TopicID,
                "ctx_raw":       msg.Context.Raw,
                "resolved_chat": chatID,
                "resolved_thr":  threadID,
        })

        if msg.Content == "" {
                return nil, nil
        }

        isToolFeedback := outboundMessageIsToolFeedback(msg)
        toolFeedbackContent := msg.Content
        if isToolFeedback {
                toolFeedbackContent = fitToolFeedbackForTelegram(msg.Content, useMarkdownV2, 4096)
        }
        trackedChatID := telegramToolFeedbackChatKey(msg.ChatID, &msg.Context)
        if isToolFeedback {
                if msgID, handled, err := c.progress.Update(ctx, trackedChatID, toolFeedbackContent); handled {
                        if err != nil {
                                return nil, err
                        }
                        return []string{msgID}, nil
                }
        }
        trackedMsgID, hasTrackedMsg := c.currentToolFeedbackMessage(trackedChatID)
        if !isToolFeedback {
                if msgIDs, handled := c.finalizeToolFeedbackMessageForChat(ctx, trackedChatID, msg); handled {
                        return msgIDs, nil
                }
        }

        // The Manager already splits messages to ≤4000 chars (WithMaxMessageLength),
        // so msg.Content is guaranteed to be within that limit. We still need to
        // check if HTML expansion pushes it beyond Telegram's 4096-char API limit.
        replyToID := msg.ReplyToMessageID
        var messageIDs []string
        queue := []string{msg.Content}
        if isToolFeedback {
                queue = []string{channels.InitialAnimatedToolFeedbackContent(toolFeedbackContent)}
        }
        for len(queue) > 0 {
                chunk := queue[0]
                queue = queue[1:]

                content := parseContent(chunk, useMarkdownV2)

                if len([]rune(content)) > 4096 {
                        if isToolFeedback {
                                fittedChunk := fitToolFeedbackForTelegram(chunk, useMarkdownV2, 4096)
                                if fittedChunk != "" && fittedChunk != chunk {
                                        queue = append([]string{fittedChunk}, queue...)
                                        continue
                                }
                        }
                        runeChunk := []rune(chunk)
                        ratio := float64(len(runeChunk)) / float64(len([]rune(content)))
                        smallerLen := int(float64(4096) * ratio * 0.95) // 5% safety margin

                        // Guarantee progress: if estimated length is >= chunk length, force it smaller
                        if smallerLen >= len(runeChunk) {
                                smallerLen = len(runeChunk) - 1
                        }

                        if smallerLen <= 0 {
                                msgID, err := c.sendChunk(ctx, sendChunkParams{
                                        chatID:        chatID,
                                        threadID:      threadID,
                                        content:       content,
                                        replyToID:     replyToID,
                                        mdFallback:    chunk,
                                        useMarkdownV2: useMarkdownV2,
                                })
                                if err != nil {
                                        return nil, err
                                }
                                messageIDs = append(messageIDs, msgID)
                                replyToID = ""
                                continue
                        }

                        // Use the estimated smaller length as a guide for SplitMessage.
                        // SplitMessage will find natural break points (newlines/spaces) and respect code blocks.
                        subChunks := channels.SplitMessage(chunk, smallerLen)

                        // Safety fallback: If SplitMessage failed to shorten the chunk, force a manual hard split.
                        if len(subChunks) == 1 && subChunks[0] == chunk {
                                part1 := string(runeChunk[:smallerLen])
                                part2 := string(runeChunk[smallerLen:])
                                subChunks = []string{part1, part2}
                        }

                        // Filter out empty chunks to avoid sending empty messages to Telegram.
                        nonEmpty := make([]string, 0, len(subChunks))
                        for _, s := range subChunks {
                                if s != "" {
                                        nonEmpty = append(nonEmpty, s)
                                }
                        }

                        // Push sub-chunks back to the front of the queue
                        queue = append(nonEmpty, queue...)
                        continue
                }

                msgID, err := c.sendChunk(ctx, sendChunkParams{
                        chatID:        chatID,
                        threadID:      threadID,
                        content:       content,
                        replyToID:     replyToID,
                        mdFallback:    chunk,
                        useMarkdownV2: useMarkdownV2,
                })
                if err != nil {
                        return nil, err
                }
                messageIDs = append(messageIDs, msgID)
                // Only the first chunk should be a reply; subsequent chunks are normal messages.
                replyToID = ""
        }

        if isToolFeedback && len(messageIDs) > 0 {
                c.RecordToolFeedbackMessage(trackedChatID, messageIDs[0], toolFeedbackContent)
        } else if !isToolFeedback && hasTrackedMsg {
                c.dismissTrackedToolFeedbackMessage(ctx, trackedChatID, trackedMsgID)
        }

        return messageIDs, nil
}

type sendChunkParams struct {
        chatID        int64
        threadID      int
        content       string
        replyToID     string
        mdFallback    string
        useMarkdownV2 bool
}

// sendChunk sends a single HTML/MarkdownV2 message, falling back to the original
// markdown as plain text on parse failure so users never see raw HTML/MarkdownV2 tags.
func (c *TelegramChannel) sendChunk(
        ctx context.Context,
        params sendChunkParams,
) (string, error) {
        logger.DebugCF("telegram", "sendChunk params", map[string]any{
                "chat_id":   params.chatID,
                "thread_id": params.threadID,
                "preview":   utils.Truncate(params.content, 40),
        })
        tgMsg := tu.Message(tu.ID(params.chatID), params.content)
        tgMsg.MessageThreadID = params.threadID
        if params.useMarkdownV2 {
                tgMsg.WithParseMode(telego.ModeMarkdownV2)
        } else {
                tgMsg.WithParseMode(telego.ModeHTML)
        }

        if params.replyToID != "" {
                if mid, parseErr := strconv.Atoi(params.replyToID); parseErr == nil {
                        tgMsg.ReplyParameters = &telego.ReplyParameters{
                                MessageID: mid,
                        }
                }
        }

        pMsg, err := c.bot.SendMessage(ctx, tgMsg)
        if err != nil {
                logParseFailed(err, params.useMarkdownV2)

                tgMsg.Text = params.mdFallback
                tgMsg.ParseMode = ""
                pMsg, err = c.bot.SendMessage(ctx, tgMsg)
                if err != nil {
                        return "", fmt.Errorf("telegram send: %w", channels.ErrTemporary)
                }
        }

        return strconv.Itoa(pMsg.MessageID), nil
}

// maxTypingDuration limits how long the typing indicator can run.
// Prevents endless typing when the LLM fails/hangs and preSend never invokes cancel.
// Matches channels.Manager's typingStopTTL (5 min) so behavior is consistent.
const maxTypingDuration = 5 * time.Minute

// StartTyping implements channels.TypingCapable.
// It sends ChatAction(typing) immediately and then repeats every 4 seconds
// (Telegram's typing indicator expires after ~5s) in a background goroutine.
// The returned stop function is idempotent and cancels the goroutine.
// The goroutine also exits automatically after maxTypingDuration if cancel is
// never called (e.g. when the LLM fails or times out without publishing).
func (c *TelegramChannel) StartTyping(ctx context.Context, chatID string) (func(), error) {
        cid, threadID, err := parseTelegramChatID(chatID)
        if err != nil {
                return func() {}, err
        }

        action := tu.ChatAction(tu.ID(cid), telego.ChatActionTyping)
        action.MessageThreadID = threadID

        // Send the first typing action immediately
        _ = c.bot.SendChatAction(ctx, action)

        typingCtx, cancel := context.WithCancel(ctx)
        // Cap lifetime so the goroutine cannot run indefinitely if cancel is never called
        maxCtx, maxCancel := context.WithTimeout(typingCtx, maxTypingDuration)
        go func() {
                defer maxCancel()
                ticker := time.NewTicker(4 * time.Second)
                defer ticker.Stop()
                for {
                        select {
                        case <-maxCtx.Done():
                                return
                        case <-ticker.C:
                                a := tu.ChatAction(tu.ID(cid), telego.ChatActionTyping)
                                a.MessageThreadID = threadID
                                _ = c.bot.SendChatAction(typingCtx, a)
                        }
                }
        }()

        return cancel, nil
}

// EditMessage implements channels.MessageEditor.
func (c *TelegramChannel) EditMessage(ctx context.Context, chatID string, messageID string, content string) error {
        useMarkdownV2 := c.tgCfg.UseMarkdownV2
        cid, _, err := parseTelegramChatID(chatID)
        if err != nil {
                return err
        }
        mid, err := strconv.Atoi(messageID)
        if err != nil {
                return err
        }
        parsedContent := parseContent(content, useMarkdownV2)
        editMsg := tu.EditMessageText(tu.ID(cid), mid, parsedContent)
        if useMarkdownV2 {
                editMsg.WithParseMode(telego.ModeMarkdownV2)
        } else {
                editMsg.WithParseMode(telego.ModeHTML)
        }
        _, err = c.bot.EditMessageText(ctx, editMsg)
        if err != nil {
                // If it failed because it was already modified (likely from a previous
                // attempt that timed out on our end but landed on Telegram), we treat
                // it as success to prevent the Manager from sending a duplicate message.
                if strings.Contains(err.Error(), "message is not modified") {
                        return nil
                }

                // Only fallback to plain text if the error looks like a parsing failure (Bad Request).
                // Network errors or timeouts should NOT trigger a retry with different content.
                if strings.Contains(err.Error(), "Bad Request") {
                        logParseFailed(err, useMarkdownV2)
                        _, err = c.bot.EditMessageText(ctx, tu.EditMessageText(tu.ID(cid), mid, content))
                }
        }

        if err != nil {
                if strings.Contains(err.Error(), "message is not modified") {
                        return nil
                }

                if isPostConnectError(err) {
                        logger.WarnCF(
                                "telegram",
                                "EditMessage likely landed but result is unknown; swallowing error to prevent duplicate",
                                map[string]any{
                                        "chat_id": chatID,
                                        "mid":     mid,
                                        "error":   err.Error(),
                                },
                        )
                        return nil // Swallow to prevent Manager fallback to a new SendMessage
                }
        }

        return err
}

// DeleteMessage implements channels.MessageDeleter.
func (c *TelegramChannel) DeleteMessage(ctx context.Context, chatID string, messageID string) error {
        cid, _, err := parseTelegramChatID(chatID)
        if err != nil {
                return err
        }
        mid, err := strconv.Atoi(messageID)
        if err != nil {
                return err
        }
        return c.bot.DeleteMessage(ctx, &telego.DeleteMessageParams{
                ChatID:    tu.ID(cid),
                MessageID: mid,
        })
}

// ReactToMessage implements channels.ReactionCapable.
// It adds an emoji reaction (default "👀") to the inbound message and returns
// an undo function that removes the reaction. If the bot lacks reaction
// permissions in the chat, the error is logged and a no-op undo is returned
// so that the calling pipeline is not disrupted.
func (c *TelegramChannel) ReactToMessage(ctx context.Context, chatID, messageID string) (func(), error) {
        cid, _, err := parseTelegramChatID(chatID)
        if err != nil {
                return func() {}, err
        }

        mid, err := strconv.Atoi(messageID)
        if err != nil {
                return func() {}, err
        }

        reaction := c.tgCfg.ReactionEmoji
        if reaction == "" {
                reaction = "\U0001F440" // 👀 eyes emoji default
        }

        err = c.bot.SetMessageReaction(ctx, &telego.SetMessageReactionParams{
                ChatID:    telego.ChatID{ID: cid},
                MessageID: mid,
                Reaction: []telego.ReactionType{
                        &telego.ReactionTypeEmoji{
                                Type:  "emoji",
                                Emoji: reaction,
                        },
                },
        })
        if err != nil {
                logger.WarnCF("telegram", "Failed to add reaction", map[string]any{
                        "chat_id":    chatID,
                        "message_id": messageID,
                        "emoji":      reaction,
                        "error":      err.Error(),
                })
                return func() {}, nil // graceful degradation
        }

        return func() {
                undoCtx := context.Background()
                undoErr := c.bot.SetMessageReaction(undoCtx, &telego.SetMessageReactionParams{
                        ChatID:    telego.ChatID{ID: cid},
                        MessageID: mid,
                        Reaction:  []telego.ReactionType{}, // empty = remove reaction
                })
                if undoErr != nil {
                        logger.DebugCF("telegram", "Failed to undo reaction", map[string]any{
                                "chat_id":    chatID,
                                "message_id": messageID,
                                "error":      undoErr.Error(),
                        })
                }
        }, nil
}

// AnswerCallbackQuery implements channels.CallbackQueryCapable.
// It acknowledges a callback query from an inline keyboard button tap.
// The Telegram API requires acknowledgment within 30 seconds; callers
// should invoke this immediately upon receiving a callback query.
func (c *TelegramChannel) AnswerCallbackQuery(ctx context.Context, queryID string, text string, showAlert bool) error {
        return c.bot.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
                CallbackQueryID: queryID,
                Text:            text,
                ShowAlert:       showAlert,
        })
}

// handleCallbackQuery processes callback queries from inline keyboard buttons.
// It immediately acknowledges the callback (Telegram requires this within 30s)
// and then publishes the callback data as an inbound message so the agent can
// process it in the normal conversation flow.
//
// The callback is converted into an InboundMessage with:
//   - Content: "[callback: <data>]" (the button's callback_data)
//   - Raw["message_kind"]: "callback_query"
//   - Raw["callback_query_id"]: the Telegram callback query ID
//   - Raw["callback_data"]: the button's callback data
//   - ChatID/MessageID: extracted from the originating message
//   - SenderID: the user who tapped the button
func (c *TelegramChannel) handleCallbackQuery(ctx context.Context, query *telego.CallbackQuery) error {
        if query == nil {
                return nil
        }

        // Immediately acknowledge the callback to satisfy Telegram's 30-second deadline.
        // We use a background context in case the original context is already expired,
        // since the acknowledgment must be sent regardless.
        ackErr := c.bot.AnswerCallbackQuery(context.Background(), &telego.AnswerCallbackQueryParams{
                CallbackQueryID: query.ID,
        })
        if ackErr != nil {
                logger.WarnCF("telegram", "Failed to answer callback query", map[string]any{
                        "query_id": query.ID,
                        "error":    ackErr.Error(),
                })
                // Non-fatal: continue processing even if acknowledgment fails.
        }

        // Extract the user who tapped the button.
        platformID := fmt.Sprintf("%d", query.From.ID)
        sender := bus.SenderInfo{
                Platform:    "telegram",
                PlatformID:  platformID,
                CanonicalID: identity.BuildCanonicalID("telegram", platformID),
                Username:    query.From.Username,
                DisplayName: query.From.FirstName,
        }

        // Allowlist check.
        if !c.IsAllowedSender(sender) {
                logger.DebugCF("telegram", "Callback query rejected by allowlist", map[string]any{
                        "user_id": platformID,
                })
                return nil
        }

        // Extract chat and message info from the callback.
        // Callback queries can originate from a regular message or an inline message.
        var chatID int64
        var chatType string
        var messageID string
        var threadID int

        if query.Message != nil {
                chatInfo := query.Message.GetChat()
                chatID = chatInfo.ID
                chatType = chatInfo.Type
                messageID = fmt.Sprintf("%d", query.Message.GetMessageID())

                // Try to extract thread/topic ID from accessible messages.
                if msg := query.Message.Message(); msg != nil && msg.MessageThreadID != 0 {
                        threadID = msg.MessageThreadID
                }
        } else if query.InlineMessageID != "" {
                // Inline messages don't carry chat/message IDs directly.
                // Use chat_instance as a best-effort identifier.
                chatID = 0
                chatType = "inline"
                messageID = query.InlineMessageID
        }

        // If we couldn't determine the chat, we can't route the callback.
        if chatID == 0 && query.InlineMessageID == "" {
                logger.WarnCF("telegram", "Callback query has no identifiable chat", map[string]any{
                        "query_id": query.ID,
                })
                return nil
        }

        // Build the content string from the callback data.
        callbackData := query.Data
        content := fmt.Sprintf("[callback: %s]", callbackData)

        // Determine the delivery chat ID (with forum topic isolation if applicable).
        chatIDStr := fmt.Sprintf("%d", chatID)
        var deliveryChatID string
        var topicID string

        isForum := false
        if query.Message != nil {
                chatInfo := query.Message.GetChat()
                isForum = chatInfo.IsForum
        }

        if isForum && threadID != 0 {
                deliveryChatID = fmt.Sprintf("%d/%d", chatID, threadID)
                topicID = fmt.Sprintf("%d", threadID)
        } else {
                deliveryChatID = chatIDStr
        }

        // Store the chatID for potential outbound use.
        c.chatIDs[platformID] = chatID

        // Map Telegram chat types to platform-agnostic types.
        normalizedChatType := "direct"
        switch chatType {
        case "group", "supergroup":
                normalizedChatType = "group"
        case "channel":
                normalizedChatType = "channel"
        case "inline":
                normalizedChatType = "inline"
        }

        // Determine if this is a group where the bot is mentioned.
        // Callback queries are always intentional (user explicitly tapped a button),
        // so we always process them regardless of group trigger settings.
        mentioned := true

        inboundCtx := bus.InboundContext{
                Channel:  c.Name(),
                ChatID:   deliveryChatID,
                ChatType: normalizedChatType,
                SenderID: sender.CanonicalID,
                MessageID: messageID,
                TopicID:  topicID,
                Mentioned: mentioned,
                Raw: map[string]string{
                        "message_kind":       "callback_query",
                        "callback_query_id":  query.ID,
                        "callback_data":      callbackData,
                        "chat_instance":      query.ChatInstance,
                },
        }

        // If the callback came from a game, note that.
        if query.GameShortName != "" {
                inboundCtx.Raw["game_short_name"] = query.GameShortName
        }

        // If the callback originated from an inline message, record that.
        if query.InlineMessageID != "" {
                inboundCtx.Raw["inline_message_id"] = query.InlineMessageID
        }

        c.HandleMessageWithContext(ctx, deliveryChatID, content, nil, inboundCtx, sender)
        return nil
}

// AnswerInlineQuery implements channels.InlineQueryCapable.
// It responds to an inline query with a list of results. The Telegram API
// requires a response within 10 seconds; callers should invoke this as
// soon as results are available.
func (c *TelegramChannel) AnswerInlineQuery(ctx context.Context, queryID string, results []channels.InlineQueryResult) error {
        var tgResults []telego.InlineQueryResult
        for _, r := range results {
                article := &telego.InlineQueryResultArticle{
                        Type:  telego.ResultTypeArticle,
                        ID:    r.ID,
                        Title: r.Title,
                        InputMessageContent: &telego.InputTextMessageContent{
                                MessageText: r.Content,
                        },
                        Description:  r.Description,
                        ThumbnailURL: r.ThumbURL,
                        URL:          r.URL,
                }
                tgResults = append(tgResults, article)
        }

        params := tu.InlineQuery(queryID, tgResults...)
        params.CacheTime = 30    // Cache results for 30 seconds
        params.IsPersonal = true // Results are personal (tailored to the user)

        return c.bot.AnswerInlineQuery(ctx, params)
}

// handleInlineQuery processes inline queries from users typing @botname <query>
// in any chat. Inline queries are transient — they do not belong to a session
// and the bot must respond with results directly via AnswerInlineQuery within
// 10 seconds.
//
// The implementation follows a "quick response" strategy:
//   - If inline mode is disabled (EnableInline is false), return empty results
//   - If the query is empty, return a help/usage article
//   - Otherwise, publish the query as an InboundMessage so the agent can
//     process it, and return a "thinking" placeholder article while the
//     agent generates the actual response
func (c *TelegramChannel) handleInlineQuery(ctx context.Context, query *telego.InlineQuery) error {
        if query == nil {
                return nil
        }

        // If inline mode is not enabled, return empty results to acknowledge
        // the query without exposing bot functionality.
        if !c.tgCfg.EnableInline {
                _ = c.bot.AnswerInlineQuery(context.Background(), &telego.AnswerInlineQueryParams{
                        InlineQueryID: query.ID,
                        Results:       []telego.InlineQueryResult{},
                        CacheTime:     0,
                })
                return nil
        }

        // Extract the user who sent the inline query.
        platformID := fmt.Sprintf("%d", query.From.ID)
        sender := bus.SenderInfo{
                Platform:    "telegram",
                PlatformID:  platformID,
                CanonicalID: identity.BuildCanonicalID("telegram", platformID),
                Username:    query.From.Username,
                DisplayName: query.From.FirstName,
        }

        // Allowlist check.
        if !c.IsAllowedSender(sender) {
                logger.DebugCF("telegram", "Inline query rejected by allowlist", map[string]any{
                        "user_id": platformID,
                })
                _ = c.bot.AnswerInlineQuery(context.Background(), &telego.AnswerInlineQueryParams{
                        InlineQueryID: query.ID,
                        Results:       []telego.InlineQueryResult{},
                        CacheTime:     0,
                })
                return nil
        }

        queryText := query.Query
        chatType := query.ChatType

        // Build the inline query event for logging/processing.
        inlineEvent := bus.InlineQueryEvent{
                Channel:  c.Name(),
                Query:    queryText,
                QueryID:  query.ID,
                SenderID: sender.CanonicalID,
                ChatType: chatType,
                Offset:   query.Offset,
                Raw: map[string]string{
                        "message_kind": "inline_query",
                },
        }

        // If the query includes a location, record it.
        if query.Location != nil {
                inlineEvent.Raw["location_lat"] = fmt.Sprintf("%f", query.Location.Latitude)
                inlineEvent.Raw["location_lng"] = fmt.Sprintf("%f", query.Location.Longitude)
        }

        logger.InfoCF("telegram", "Received inline query", map[string]any{
                "query_id":   query.ID,
                "query":      queryText,
                "user_id":    platformID,
                "chat_type":  chatType,
                "offset":     query.Offset,
        })

        // For an LLM-powered bot, we can't wait for a full agent response
        // within the 10-second Telegram deadline. Instead, we publish the
        // query as an inbound message for the agent to process, and return
        // a "thinking" placeholder article that the user can tap to send
        // the query text to the bot as a regular message.
        //
        // If the query is empty, provide a help article instead.
        var results []telego.InlineQueryResult

        if strings.TrimSpace(queryText) == "" {
                // Empty query: return a help article suggesting the user type something.
                results = []telego.InlineQueryResult{
                        &telego.InlineQueryResultArticle{
                                Type:  telego.ResultTypeArticle,
                                ID:    "help",
                                Title: "Ask me anything",
                                InputMessageContent: &telego.InputTextMessageContent{
                                        MessageText: fmt.Sprintf("Hello! I'm %s. Type your question after my name to get started.", c.bot.Username()),
                                },
                                Description: fmt.Sprintf("Type @%s <your question> to ask me anything.", c.bot.Username()),
                        },
                }
        } else {
                // Non-empty query: return a single article that sends the query
                // text to the current chat. The user's query is also published
                // as an inbound message so the agent can process it and respond
                // in the user's private chat with the bot.
                results = []telego.InlineQueryResult{
                        &telego.InlineQueryResultArticle{
                                Type:  telego.ResultTypeArticle,
                                ID:    "query",
                                Title: queryText,
                                InputMessageContent: &telego.InputTextMessageContent{
                                        MessageText: queryText,
                                },
                                Description: "Tap to send this query to the chat",
                        },
                }

                // Also publish as an inbound message so the agent can process
                // it and potentially respond in the user's private chat with
                // the bot. Use the sender's canonical ID as the chat ID so
                // the agent routes the response to the private conversation.
                content := fmt.Sprintf("[inline_query: %s]", queryText)
                deliveryChatID := fmt.Sprintf("inline:%s", platformID)

                inboundCtx := bus.InboundContext{
                        Channel:   c.Name(),
                        ChatID:    deliveryChatID,
                        ChatType:  "inline",
                        SenderID:  sender.CanonicalID,
                        MessageID: query.ID,
                        Mentioned: true,
                        Raw: map[string]string{
                                "message_kind":   "inline_query",
                                "inline_query_id": query.ID,
                                "query_text":     queryText,
                                "chat_type":       chatType,
                                "offset":          query.Offset,
                        },
                }

                // Include location data if present.
                if query.Location != nil {
                        inboundCtx.Raw["location_lat"] = fmt.Sprintf("%f", query.Location.Latitude)
                        inboundCtx.Raw["location_lng"] = fmt.Sprintf("%f", query.Location.Longitude)
                }

                // Publish asynchronously to avoid blocking the inline query
                // response deadline. The agent will process the message and
                // send a response to the user's private chat.
                go c.HandleMessageWithContext(context.Background(), deliveryChatID, content, nil, inboundCtx, sender)
        }

        // Answer the inline query with our results.
        // Use a background context in case the original context has expired.
        err := c.bot.AnswerInlineQuery(context.Background(), &telego.AnswerInlineQueryParams{
                InlineQueryID: query.ID,
                Results:       results,
                CacheTime:     30,
                IsPersonal:    true,
        })
        if err != nil {
                logger.WarnCF("telegram", "Failed to answer inline query", map[string]any{
                        "query_id": query.ID,
                        "error":    err.Error(),
                })
        }

        return nil
}

func outboundMessageIsToolFeedback(msg bus.OutboundMessage) bool {
        if len(msg.Context.Raw) == 0 {
                return false
        }
        return strings.EqualFold(strings.TrimSpace(msg.Context.Raw["message_kind"]), "tool_feedback")
}

func (c *TelegramChannel) currentToolFeedbackMessage(chatID string) (string, bool) {
        if c.progress == nil {
                return "", false
        }
        return c.progress.Current(chatID)
}

func (c *TelegramChannel) takeToolFeedbackMessage(chatID string) (string, string, bool) {
        if c.progress == nil {
                return "", "", false
        }
        return c.progress.Take(chatID)
}

func (c *TelegramChannel) RecordToolFeedbackMessage(chatID, messageID, content string) {
        if c.progress == nil {
                return
        }
        c.progress.Record(chatID, messageID, content)
}

func (c *TelegramChannel) ClearToolFeedbackMessage(chatID string) {
        if c.progress == nil {
                return
        }
        c.progress.Clear(chatID)
}

func (c *TelegramChannel) DismissToolFeedbackMessage(ctx context.Context, chatID string) {
        msgID, ok := c.currentToolFeedbackMessage(chatID)
        if !ok {
                return
        }
        c.dismissTrackedToolFeedbackMessage(ctx, chatID, msgID)
}

func (c *TelegramChannel) dismissTrackedToolFeedbackMessage(ctx context.Context, chatID, messageID string) {
        if strings.TrimSpace(chatID) == "" || strings.TrimSpace(messageID) == "" {
                return
        }
        c.ClearToolFeedbackMessage(chatID)
        _ = c.DeleteMessage(ctx, chatID, messageID)
}

func (c *TelegramChannel) finalizeTrackedToolFeedbackMessage(
        ctx context.Context,
        chatID string,
        content string,
        editFn func(context.Context, string, string, string) error,
) ([]string, bool) {
        msgID, baseContent, ok := c.takeToolFeedbackMessage(chatID)
        if !ok || editFn == nil {
                return nil, false
        }
        if err := editFn(ctx, chatID, msgID, content); err != nil {
                c.RecordToolFeedbackMessage(chatID, msgID, baseContent)
                return nil, false
        }
        return []string{msgID}, true
}

func (c *TelegramChannel) FinalizeToolFeedbackMessage(ctx context.Context, msg bus.OutboundMessage) ([]string, bool) {
        if outboundMessageIsToolFeedback(msg) {
                return nil, false
        }
        return c.finalizeToolFeedbackMessageForChat(ctx, telegramToolFeedbackChatKey(msg.ChatID, &msg.Context), msg)
}

func (c *TelegramChannel) finalizeToolFeedbackMessageForChat(
        ctx context.Context,
        chatID string,
        msg bus.OutboundMessage,
) ([]string, bool) {
        return c.finalizeTrackedToolFeedbackMessage(ctx, chatID, msg.Content, c.EditMessage)
}

// SendPlaceholder implements channels.PlaceholderCapable.
// It sends a placeholder message (e.g. "Thinking... 💭") that will later be
// edited to the actual response via EditMessage (channels.MessageEditor).
func (c *TelegramChannel) SendPlaceholder(ctx context.Context, chatID string) (string, error) {
        phCfg := c.bc.Placeholder
        if !phCfg.Enabled {
                return "", nil
        }

        text := phCfg.GetRandomText()

        cid, threadID, err := parseTelegramChatID(chatID)
        if err != nil {
                return "", err
        }

        phMsg := tu.Message(tu.ID(cid), text)
        phMsg.MessageThreadID = threadID
        pMsg, err := c.bot.SendMessage(ctx, phMsg)
        if err != nil {
                return "", err
        }

        return fmt.Sprintf("%d", pMsg.MessageID), nil
}

// SendMedia implements the channels.MediaSender interface.
// GAP-9: Consecutive image/video parts are batched into a media group (album)
// via sendMediaGroup when there are 2+ items. Single items fall through to
// individual send calls. Non-file types (sticker, location, venue) are
// dispatched to dedicated send methods.
func (c *TelegramChannel) SendMedia(ctx context.Context, msg bus.OutboundMediaMessage) ([]string, error) {
        if !c.IsRunning() {
                return nil, channels.ErrNotRunning
        }
        trackedChatID := telegramToolFeedbackChatKey(msg.ChatID, &msg.Context)
        trackedMsgID, hasTrackedMsg := c.currentToolFeedbackMessage(trackedChatID)

        chatID, threadID, err := resolveTelegramOutboundTarget(msg.ChatID, &msg.Context)
        if err != nil {
                return nil, fmt.Errorf("invalid chat ID %s: %w", msg.ChatID, channels.ErrSendFailed)
        }

        var messageIDs []string

        // Scan parts list; batch consecutive image/video parts into albums.
        i := 0
        for i < len(msg.Parts) {
                part := msg.Parts[i]

                // Handle non-file-based types that don't need the media store.
                switch part.Type {
                case "location":
                        mid, sendErr := c.sendLocationPart(ctx, chatID, threadID, part)
                        if sendErr != nil {
                                return nil, fmt.Errorf("telegram send location: %w", channels.ErrTemporary)
                        }
                        if mid != "" {
                                messageIDs = append(messageIDs, mid)
                        }
                        i++
                        continue
                case "venue":
                        mid, sendErr := c.sendVenuePart(ctx, chatID, threadID, part)
                        if sendErr != nil {
                                return nil, fmt.Errorf("telegram send venue: %w", channels.ErrTemporary)
                        }
                        if mid != "" {
                                messageIDs = append(messageIDs, mid)
                        }
                        i++
                        continue
                case "sticker":
                        mid, sendErr := c.sendStickerPart(ctx, chatID, threadID, part)
                        if sendErr != nil {
                                return nil, fmt.Errorf("telegram send sticker: %w", channels.ErrTemporary)
                        }
                        if mid != "" {
                                messageIDs = append(messageIDs, mid)
                        }
                        i++
                        continue
                }

                // Try to batch consecutive image/video parts into a media group (GAP-9).
                if part.Type == "image" || part.Type == "video" {
                        batchStart := i
                        for i < len(msg.Parts) && (msg.Parts[i].Type == "image" || msg.Parts[i].Type == "video") {
                                i++
                        }
                        batch := msg.Parts[batchStart:i]
                        if len(batch) >= 2 {
                                // Send as a media group (album).
                                ids, batchErr := c.sendMediaGroup(ctx, chatID, threadID, batch)
                                if batchErr != nil {
                                        // Fallback: send individually on album failure.
                                        logger.WarnCF("telegram", "Media group failed, sending individually", map[string]any{
                                                "error": batchErr.Error(),
                                        })
                                        for _, p := range batch {
                                                mid, singleErr := c.sendSingleMediaPart(ctx, chatID, threadID, p)
                                                if singleErr != nil {
                                                        return nil, fmt.Errorf("telegram send media: %w", channels.ErrTemporary)
                                                }
                                                if mid != "" {
                                                        messageIDs = append(messageIDs, mid)
                                                }
                                        }
                                } else {
                                        messageIDs = append(messageIDs, ids...)
                                }
                        } else {
                                // Single image/video — send individually.
                                mid, singleErr := c.sendSingleMediaPart(ctx, chatID, threadID, batch[0])
                                if singleErr != nil {
                                        return nil, fmt.Errorf("telegram send media: %w", channels.ErrTemporary)
                                }
                                if mid != "" {
                                        messageIDs = append(messageIDs, mid)
                                }
                        }
                        continue
                }

                // Default: audio, file, etc.
                mid, singleErr := c.sendSingleMediaPart(ctx, chatID, threadID, part)
                if singleErr != nil {
                        return nil, fmt.Errorf("telegram send media: %w", channels.ErrTemporary)
                }
                if mid != "" {
                        messageIDs = append(messageIDs, mid)
                }
                i++
        }

        if hasTrackedMsg {
                c.dismissTrackedToolFeedbackMessage(ctx, trackedChatID, trackedMsgID)
        }

        return messageIDs, nil
}

// sendSingleMediaPart sends a single file-based media part (image, audio,
// video, file). It resolves the media store ref, opens the file, and
// dispatches to the appropriate Telegram send method.
func (c *TelegramChannel) sendSingleMediaPart(ctx context.Context, chatID int64, threadID int, part bus.MediaPart) (string, error) {
        store := c.GetMediaStore()
        if store == nil {
                return "", fmt.Errorf("no media store available")
        }

        localPath, err := store.Resolve(part.Ref)
        if err != nil {
                logger.ErrorCF("telegram", "Failed to resolve media ref", map[string]any{
                        "ref":   part.Ref,
                        "error": err.Error(),
                })
                return "", err
        }

        file, err := os.Open(localPath)
        if err != nil {
                logger.ErrorCF("telegram", "Failed to open media file", map[string]any{
                        "path":  localPath,
                        "error": err.Error(),
                })
                return "", err
        }
        defer file.Close()

        var tgResult *telego.Message
        switch part.Type {
        case "image":
                params := &telego.SendPhotoParams{
                        ChatID:          tu.ID(chatID),
                        MessageThreadID: threadID,
                        Photo:           telego.InputFile{File: file},
                        Caption:         part.Caption,
                }
                tgResult, err = c.bot.SendPhoto(ctx, params)
                if err != nil && strings.Contains(err.Error(), "PHOTO_INVALID_DIMENSIONS") {
                        if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
                                return "", fmt.Errorf("telegram rewind media after photo failure: %w", channels.ErrTemporary)
                        }

                        docParams := &telego.SendDocumentParams{
                                ChatID:          tu.ID(chatID),
                                MessageThreadID: threadID,
                                Document:        telego.InputFile{File: file},
                                Caption:         part.Caption,
                        }
                        tgResult, err = c.bot.SendDocument(ctx, docParams)
                }
        case "audio":
                // Send OGG files with "voice" in the filename as Telegram voice
                // bubbles (SendVoice) instead of audio attachments (SendAudio).
                fn := strings.ToLower(part.Filename)
                if strings.Contains(fn, "voice") && (strings.HasSuffix(fn, ".ogg") || strings.HasSuffix(fn, ".oga")) {
                        vparams := &telego.SendVoiceParams{
                                ChatID:          tu.ID(chatID),
                                MessageThreadID: threadID,
                                Voice:           telego.InputFile{File: file},
                                Caption:         part.Caption,
                        }
                        tgResult, err = c.bot.SendVoice(ctx, vparams)
                } else {
                        params := &telego.SendAudioParams{
                                ChatID:          tu.ID(chatID),
                                MessageThreadID: threadID,
                                Audio:           telego.InputFile{File: file},
                                Caption:         part.Caption,
                        }
                        tgResult, err = c.bot.SendAudio(ctx, params)
                }
        case "video":
                params := &telego.SendVideoParams{
                        ChatID:          tu.ID(chatID),
                        MessageThreadID: threadID,
                        Video:           telego.InputFile{File: file},
                        Caption:         part.Caption,
                }
                tgResult, err = c.bot.SendVideo(ctx, params)
        default: // "file" or unknown types
                params := &telego.SendDocumentParams{
                        ChatID:          tu.ID(chatID),
                        MessageThreadID: threadID,
                        Document:        telego.InputFile{File: file},
                        Caption:         part.Caption,
                }
                tgResult, err = c.bot.SendDocument(ctx, params)
        }

        if err != nil {
                logger.ErrorCF("telegram", "Failed to send media", map[string]any{
                        "type":  part.Type,
                        "error": err.Error(),
                })
                return "", err
        }

        if tgResult != nil {
                return strconv.Itoa(tgResult.MessageID), nil
        }
        return "", nil
}

// sendMediaGroup sends 2-10 image/video parts as a Telegram media group
// (album) using the sendMediaGroup API. The first item gets the caption;
// remaining items are sent without captions (Telegram only supports one
// caption per album).
func (c *TelegramChannel) sendMediaGroup(ctx context.Context, chatID int64, threadID int, parts []bus.MediaPart) ([]string, error) {
        store := c.GetMediaStore()
        if store == nil {
                return nil, fmt.Errorf("no media store available")
        }

        var inputMedia []telego.InputMedia
        // Keep track of open files so we can close them after sending.
        var files []*os.File
        defer func() {
                for _, f := range files {
                        _ = f.Close()
                }
        }()

        for idx, part := range parts {
                localPath, err := store.Resolve(part.Ref)
                if err != nil {
                        return nil, fmt.Errorf("resolve media ref %s: %w", part.Ref, err)
                }

                file, err := os.Open(localPath)
                if err != nil {
                        return nil, fmt.Errorf("open media file %s: %w", localPath, err)
                }
                files = append(files, file)

                switch part.Type {
                case "image":
                        img := &telego.InputMediaPhoto{
                                Type:  telego.MediaTypePhoto,
                                Media: telego.InputFile{File: file},
                        }
                        // Only the first item gets the caption.
                        if idx == 0 && part.Caption != "" {
                                img.Caption = part.Caption
                        }
                        inputMedia = append(inputMedia, img)
                case "video":
                        vid := &telego.InputMediaVideo{
                                Type:  telego.MediaTypeVideo,
                                Media: telego.InputFile{File: file},
                        }
                        if idx == 0 && part.Caption != "" {
                                vid.Caption = part.Caption
                        }
                        inputMedia = append(inputMedia, vid)
                default:
                        return nil, fmt.Errorf("unsupported media group part type: %s", part.Type)
                }
        }

        params := &telego.SendMediaGroupParams{
                ChatID:          tu.ID(chatID),
                MessageThreadID: threadID,
                Media:           inputMedia,
        }

        messages, err := c.bot.SendMediaGroup(ctx, params)
        if err != nil {
                return nil, err
        }

        var ids []string
        for _, m := range messages {
                ids = append(ids, strconv.Itoa(m.MessageID))
        }
        return ids, nil
}

// sendStickerPart sends a sticker using the Telegram sendSticker API.
// The Ref field may contain a file_id, a URL, or a media store reference.
// Stickers are sent as-is without captions (Telegram does not support
// sticker captions).
func (c *TelegramChannel) sendStickerPart(ctx context.Context, chatID int64, threadID int, part bus.MediaPart) (string, error) {
        params := &telego.SendStickerParams{
                ChatID:          tu.ID(chatID),
                MessageThreadID: threadID,
        }

        // Determine if Ref is a URL, a media store reference, or a file_id.
        ref := part.Ref
        if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
                params.Sticker = telego.InputFile{URL: ref}
        } else {
                // Try as media store reference — resolve to local file.
                store := c.GetMediaStore()
                if store != nil {
                        if localPath, resolveErr := store.Resolve(ref); resolveErr == nil {
                                file, openErr := os.Open(localPath)
                                if openErr == nil {
                                        defer file.Close()
                                        params.Sticker = telego.InputFile{File: file}
                                } else {
                                        params.Sticker = telego.InputFile{FileID: ref}
                                }
                        } else {
                                // Not a media store ref — use as file_id directly.
                                params.Sticker = telego.InputFile{FileID: ref}
                        }
                } else {
                        params.Sticker = telego.InputFile{FileID: ref}
                }
        }

        tgResult, err := c.bot.SendSticker(ctx, params)
        if err != nil {
                logger.ErrorCF("telegram", "Failed to send sticker", map[string]any{
                        "ref":   ref,
                        "error": err.Error(),
                })
                return "", err
        }
        if tgResult != nil {
                return strconv.Itoa(tgResult.MessageID), nil
        }
        return "", nil
}

// sendLocationPart sends a location pin using the Telegram sendLocation API.
func (c *TelegramChannel) sendLocationPart(ctx context.Context, chatID int64, threadID int, part bus.MediaPart) (string, error) {
        params := &telego.SendLocationParams{
                ChatID:          tu.ID(chatID),
                MessageThreadID: threadID,
                Latitude:        part.Latitude,
                Longitude:       part.Longitude,
        }

        tgResult, err := c.bot.SendLocation(ctx, params)
        if err != nil {
                logger.ErrorCF("telegram", "Failed to send location", map[string]any{
                        "lat":   part.Latitude,
                        "lng":   part.Longitude,
                        "error": err.Error(),
                })
                return "", err
        }
        if tgResult != nil {
                return strconv.Itoa(tgResult.MessageID), nil
        }
        return "", nil
}

// sendVenuePart sends a venue (named location) using the Telegram sendVenue API.
func (c *TelegramChannel) sendVenuePart(ctx context.Context, chatID int64, threadID int, part bus.MediaPart) (string, error) {
        params := &telego.SendVenueParams{
                ChatID:          tu.ID(chatID),
                MessageThreadID: threadID,
                Latitude:        part.Latitude,
                Longitude:       part.Longitude,
                Title:           part.Title,
                Address:         part.Address,
        }

        tgResult, err := c.bot.SendVenue(ctx, params)
        if err != nil {
                logger.ErrorCF("telegram", "Failed to send venue", map[string]any{
                        "title":   part.Title,
                        "address": part.Address,
                        "error":   err.Error(),
                })
                return "", err
        }
        if tgResult != nil {
                return strconv.Itoa(tgResult.MessageID), nil
        }
        return "", nil
}

// PinMessage implements channels.PinnableCapable.
// It pins a message in the specified chat. The bot must have pin permissions
// in the target chat. If pinning fails due to insufficient permissions, the
// error is returned so the caller can decide how to handle it.
func (c *TelegramChannel) PinMessage(ctx context.Context, chatID string, messageID string) error {
        cid, _, err := parseTelegramChatID(chatID)
        if err != nil {
                return err
        }
        mid, err := strconv.Atoi(messageID)
        if err != nil {
                return err
        }
        return c.bot.PinChatMessage(ctx, &telego.PinChatMessageParams{
                ChatID:    tu.ID(cid),
                MessageID: mid,
        })
}

// UnpinMessage implements channels.PinnableCapable.
// It unpins a specific message in the specified chat. The bot must have pin
// permissions in the target chat.
func (c *TelegramChannel) UnpinMessage(ctx context.Context, chatID string, messageID string) error {
        cid, _, err := parseTelegramChatID(chatID)
        if err != nil {
                return err
        }
        mid, err := strconv.Atoi(messageID)
        if err != nil {
                return err
        }
        return c.bot.UnpinChatMessage(ctx, &telego.UnpinChatMessageParams{
                ChatID:    tu.ID(cid),
                MessageID: mid,
        })
}

// DeleteMessages implements channels.BatchMessageDeleter.
// It deletes multiple messages in a single API call. This is significantly
// more efficient than calling DeleteMessage individually for each message.
// Telegram supports deleting up to 100 messages per call; if more than 100
// IDs are provided, they are batched automatically.
func (c *TelegramChannel) DeleteMessages(ctx context.Context, chatID string, messageIDs []string) error {
        if len(messageIDs) == 0 {
                return nil
        }

        cid, _, err := parseTelegramChatID(chatID)
        if err != nil {
                return err
        }

        // Convert string IDs to integers.
        mids := make([]int, 0, len(messageIDs))
        for _, id := range messageIDs {
                mid, convErr := strconv.Atoi(id)
                if convErr != nil {
                        return fmt.Errorf("invalid message ID %q: %w", id, convErr)
                }
                mids = append(mids, mid)
        }

        // Batch in groups of 100 (Telegram's maximum per call).
        for batchStart := 0; batchStart < len(mids); batchStart += 100 {
                batchEnd := batchStart + 100
                if batchEnd > len(mids) {
                        batchEnd = len(mids)
                }
                batch := mids[batchStart:batchEnd]

                if err = c.bot.DeleteMessages(ctx, &telego.DeleteMessagesParams{
                        ChatID:     tu.ID(cid),
                        MessageIDs: batch,
                }); err != nil {
                        return err
                }
        }

        return nil
}

func (c *TelegramChannel) handleMessage(ctx context.Context, message *telego.Message, isEdit ...bool) error {
        if message == nil {
                return fmt.Errorf("message is nil")
        }

        edited := len(isEdit) > 0 && isEdit[0]

        // For channel posts, the sender may be nil (SenderChat used instead).
        // We require at least one of From or SenderChat.
        user := message.From
        if user == nil && message.SenderChat == nil {
                return fmt.Errorf("message sender (user) is nil")
        }

        // Build sender info — prefer From (user), fall back to SenderChat (channel).
        var platformID string
        var sender bus.SenderInfo
        if user != nil {
                platformID = fmt.Sprintf("%d", user.ID)
                sender = bus.SenderInfo{
                        Platform:    "telegram",
                        PlatformID:  platformID,
                        CanonicalID: identity.BuildCanonicalID("telegram", platformID),
                        Username:    user.Username,
                        DisplayName: user.FirstName,
                }
        } else {
                platformID = fmt.Sprintf("chat_%d", message.SenderChat.ID)
                sender = bus.SenderInfo{
                        Platform:    "telegram",
                        PlatformID:  platformID,
                        CanonicalID: identity.BuildCanonicalID("telegram", platformID),
                        Username:    message.SenderChat.Username,
                        DisplayName: message.SenderChat.Title,
                }
        }

        // check allowlist to avoid downloading attachments for rejected users
        if !c.IsAllowedSender(sender) {
                logger.DebugCF("telegram", "Message rejected by allowlist", map[string]any{
                        "user_id": platformID,
                })
                return nil
        }

        chatID := message.Chat.ID
        c.chatIDs[platformID] = chatID

        content := ""
        mediaPaths := []string{}

        chatIDStr := fmt.Sprintf("%d", chatID)
        messageIDStr := fmt.Sprintf("%d", message.MessageID)
        scope := channels.BuildMediaScope("telegram", chatIDStr, messageIDStr)

        // Helper to register a local file with the media store
        storeMedia := func(localPath, filename string) string {
                if store := c.GetMediaStore(); store != nil {
                        ref, err := store.Store(localPath, media.MediaMeta{
                                Filename:      filename,
                                Source:        "telegram",
                                CleanupPolicy: media.CleanupPolicyDeleteOnCleanup,
                        }, scope)
                        if err == nil {
                                return ref
                        }
                }
                return localPath // fallback: use raw path
        }

        if message.Text != "" {
                content += message.Text
        }

        if message.Caption != "" {
                if content != "" {
                        content += "\n"
                }
                content += message.Caption
        }

        if len(message.Photo) > 0 {
                photo := message.Photo[len(message.Photo)-1]
                photoPath := c.downloadPhoto(ctx, photo.FileID)
                if photoPath != "" {
                        mediaPaths = append(mediaPaths, storeMedia(photoPath, "photo.jpg"))
                        if content != "" {
                                content += "\n"
                        }
                        content += "[image: photo]"
                }
        }

        if message.Voice != nil {
                voicePath := c.downloadFile(ctx, message.Voice.FileID, ".ogg")
                if voicePath != "" {
                        mediaPaths = append(mediaPaths, storeMedia(voicePath, "voice.ogg"))

                        if content != "" {
                                content += "\n"
                        }
                        content += "[voice]"
                }
        }

        if message.Audio != nil {
                audioPath := c.downloadFile(ctx, message.Audio.FileID, ".mp3")
                if audioPath != "" {
                        mediaPaths = append(mediaPaths, storeMedia(audioPath, "audio.mp3"))
                        if content != "" {
                                content += "\n"
                        }
                        content += "[audio]"
                }
        }

        if message.Document != nil {
                docPath := c.downloadFile(ctx, message.Document.FileID, "")
                if docPath != "" {
                        mediaPaths = append(mediaPaths, storeMedia(docPath, "document"))
                        if content != "" {
                                content += "\n"
                        }
                        content += "[file]"
                }
        }

        // --- Phase 3: Previously dropped message types ---

        if message.Sticker != nil {
                emoji := message.Sticker.Emoji
                if emoji == "" {
                        emoji = "?"
                }
                if content != "" {
                        content += "\n"
                }
                content += fmt.Sprintf("[sticker: %s]", emoji)
                // Download animated/video stickers as media; static stickers are
                // small enough that the emoji description is sufficient context.
                if message.Sticker.IsAnimated || message.Sticker.IsVideo {
                        stickerPath := c.downloadFile(ctx, message.Sticker.FileID, ".webm")
                        if stickerPath != "" {
                                mediaPaths = append(mediaPaths, storeMedia(stickerPath, "sticker.webm"))
                        }
                }
        }

        if message.Video != nil {
                videoPath := c.downloadFile(ctx, message.Video.FileID, ".mp4")
                if videoPath != "" {
                        mediaPaths = append(mediaPaths, storeMedia(videoPath, "video.mp4"))
                        if content != "" {
                                content += "\n"
                        }
                        content += "[video]"
                }
        }

        if message.VideoNote != nil {
                vnPath := c.downloadFile(ctx, message.VideoNote.FileID, ".mp4")
                if vnPath != "" {
                        mediaPaths = append(mediaPaths, storeMedia(vnPath, "video_note.mp4"))
                        if content != "" {
                                content += "\n"
                        }
                        content += "[video note]"
                }
        }

        if message.Animation != nil {
                animPath := c.downloadFile(ctx, message.Animation.FileID, ".mp4")
                if animPath != "" {
                        mediaPaths = append(mediaPaths, storeMedia(animPath, "animation.mp4"))
                        if content != "" {
                                content += "\n"
                        }
                        content += "[animation]"
                }
        }

        if message.Contact != nil {
                if content != "" {
                        content += "\n"
                }
                contactParts := []string{message.Contact.FirstName}
                if message.Contact.LastName != "" {
                        contactParts = append(contactParts, message.Contact.LastName)
                }
                name := strings.Join(contactParts, " ")
                content += fmt.Sprintf("[contact: %s, %s]", name, message.Contact.PhoneNumber)
        }

        if message.Location != nil {
                if content != "" {
                        content += "\n"
                }
                content += fmt.Sprintf("[location: %.6f, %.6f]", message.Location.Latitude, message.Location.Longitude)
        }

        if message.Venue != nil {
                if content != "" {
                        content += "\n"
                }
                content += fmt.Sprintf("[venue: %s, %s]", message.Venue.Title, message.Venue.Address)
        }

        if message.Poll != nil {
                if content != "" {
                        content += "\n"
                }
                optionTexts := make([]string, len(message.Poll.Options))
                for i, opt := range message.Poll.Options {
                        optionTexts[i] = opt.Text
                }
                pollType := "poll"
                if message.Poll.Type == "quiz" {
                        pollType = "quiz"
                }
                content += fmt.Sprintf("[%s: %s (%s)]", pollType, message.Poll.Question, strings.Join(optionTexts, " / "))
        }

        if message.Dice != nil {
                if content != "" {
                        content += "\n"
                }
                content += fmt.Sprintf("[dice: %s %d]", message.Dice.Emoji, message.Dice.Value)
        }

        if content == "" && len(mediaPaths) == 0 {
                return nil
        }

        if content == "" {
                content = "[media only]"
        }

        // In group chats, apply unified group trigger filtering
        isMentioned := false
        if message.Chat.Type != "private" {
                isMentioned = c.isBotMentioned(message)
                if isMentioned {
                        content = c.stripBotMention(content)
                }
                respond, cleaned := c.ShouldRespondInGroup(isMentioned, content)
                if !respond {
                        return nil
                }
                content = cleaned
        }

        if message.ReplyToMessage != nil {
                quotedMedia := quotedTelegramMediaRefs(
                        message.ReplyToMessage,
                        func(fileID, ext, filename string) string {
                                localPath := c.downloadFile(ctx, fileID, ext)
                                if localPath == "" {
                                        return ""
                                }
                                return storeMedia(localPath, filename)
                        },
                )
                if len(quotedMedia) > 0 {
                        mediaPaths = append(quotedMedia, mediaPaths...)
                }
                content = c.prependTelegramQuotedReply(content, message.ReplyToMessage)
        }

        // For forum topics, embed the thread ID as "chatID/threadID" so replies
        // route to the correct topic and each topic gets its own session.
        // Only forum groups (IsForum) are handled; regular group reply threads
        // must share one session per group.
        compositeChatID := fmt.Sprintf("%d", chatID)
        threadID := message.MessageThreadID
        if threadID != 0 {
                compositeChatID = fmt.Sprintf("%d/%d", chatID, threadID)
        }

        logger.DebugCF("telegram", "Received message", map[string]any{
                "sender_id": sender.CanonicalID,
                "chat_id":   compositeChatID,
                "thread_id": threadID,
                "preview":   utils.Truncate(content, 50),
        })

        peerKind := "direct"
        if message.Chat.Type != "private" {
                peerKind = "group"
        }
        messageID := fmt.Sprintf("%d", message.MessageID)

        metadata := map[string]string{
                "is_group": fmt.Sprintf("%t", message.Chat.Type != "private"),
        }
        if threadID != 0 {
                metadata["thread_id"] = fmt.Sprintf("%d", threadID)
        }
        if user != nil {
                metadata["user_id"] = fmt.Sprintf("%d", user.ID)
                metadata["username"] = user.Username
                metadata["first_name"] = user.FirstName
        } else if message.SenderChat != nil {
                metadata["sender_chat_id"] = fmt.Sprintf("%d", message.SenderChat.ID)
                metadata["sender_chat_title"] = message.SenderChat.Title
        }

        inboundCtx := bus.InboundContext{
                Channel:   c.Name(),
                ChatID:    compositeChatID,
                ChatType:  peerKind,
                SenderID:  platformID,
                MessageID: messageID,
                Mentioned: isMentioned,
                IsEdit:    edited,
                Raw:       metadata,
        }
        if edited && message.EditDate != 0 {
                inboundCtx.EditDate = int64(message.EditDate)
        }
        if threadID != 0 {
                inboundCtx.TopicID = fmt.Sprintf("%d", threadID)
        }
        if message.ReplyToMessage != nil {
                inboundCtx.ReplyToMessageID = fmt.Sprintf("%d", message.ReplyToMessage.MessageID)
                if message.ReplyToMessage.From != nil {
                        inboundCtx.ReplyToSenderID = fmt.Sprintf("%d", message.ReplyToMessage.From.ID)
                }
        }

        c.HandleMessageWithContext(
                c.ctx,
                compositeChatID,
                content,
                mediaPaths,
                inboundCtx,
                sender,
        )
        return nil
}

// handleChannelPost processes posts from Telegram channels where the bot is
// an administrator. Channel posts are routed with a "channel" chat type and
// use one session per channel (not per user). The SenderChat is used as the
// sender identity since channel posts may not have a From user.
func (c *TelegramChannel) handleChannelPost(ctx context.Context, message *telego.Message) error {
        if message == nil {
                return nil
        }

        chatID := message.Chat.ID

        logger.InfoCF("telegram", "Received channel post", map[string]any{
                "chat_id":    chatID,
                "message_id": message.MessageID,
                "preview":    utils.Truncate(message.Text, 50),
        })

        // Channel posts use the channel chat ID as the session key.
        // This means one session per channel rather than per user.
        content := message.Text
        if content == "" && message.Caption != "" {
                content = message.Caption
        }
        if content == "" {
                return nil
        }

        // Build sender from SenderChat (the channel itself).
        chatIDStr := fmt.Sprintf("%d", chatID)
        var platformID string
        var sender bus.SenderInfo
        if message.SenderChat != nil {
                platformID = fmt.Sprintf("chat_%d", message.SenderChat.ID)
                sender = bus.SenderInfo{
                        Platform:    "telegram",
                        PlatformID:  platformID,
                        CanonicalID: identity.BuildCanonicalID("telegram", platformID),
                        Username:    message.SenderChat.Username,
                        DisplayName: message.SenderChat.Title,
                }
        } else {
                platformID = chatIDStr
                sender = bus.SenderInfo{
                        Platform:    "telegram",
                        PlatformID:  platformID,
                        CanonicalID: identity.BuildCanonicalID("telegram", platformID),
                }
        }

        metadata := map[string]string{
                "chat_type": "channel",
        }
        if message.SenderChat != nil {
                metadata["sender_chat_id"] = fmt.Sprintf("%d", message.SenderChat.ID)
                metadata["sender_chat_title"] = message.SenderChat.Title
        }

        inboundCtx := bus.InboundContext{
                Channel:   c.Name(),
                ChatID:    chatIDStr,
                ChatType:  "channel",
                SenderID:  platformID,
                MessageID: fmt.Sprintf("%d", message.MessageID),
                Raw:       metadata,
        }

        c.HandleMessageWithContext(
                c.ctx,
                chatIDStr,
                content,
                nil,
                inboundCtx,
                sender,
        )
        return nil
}

// handleChatMemberUpdated processes chat member status changes (joins, leaves,
// promotions, etc.). These are administrative events that do not belong to a
// conversation session. They are logged and published to the bus for consumers
// that need access control, welcome messages, or group management features.
func (c *TelegramChannel) handleChatMemberUpdated(ctx context.Context, update *telego.ChatMemberUpdated, isMyChatMember bool) error {
        if update == nil {
                return nil
        }

        chatID := fmt.Sprintf("%d", update.Chat.ID)
        actorID := fmt.Sprintf("%d", update.From.ID)
        userID := fmt.Sprintf("%d", update.NewChatMember.MemberUser().ID)

        // Extract the old and new status strings from ChatMember interface.
        oldStatus := update.OldChatMember.MemberStatus()
        newStatus := update.NewChatMember.MemberStatus()

        logger.InfoCF("telegram", "Chat member updated", map[string]any{
                "chat_id":          chatID,
                "chat_type":        update.Chat.Type,
                "user_id":          userID,
                "old_status":        oldStatus,
                "new_status":        newStatus,
                "is_my_chat_member": isMyChatMember,
        })

        event := bus.ChatMemberEvent{
                Channel:        c.Name(),
                ChatID:         chatID,
                ChatType:       update.Chat.Type,
                ActorID:        actorID,
                UserID:         userID,
                OldStatus:      oldStatus,
                NewStatus:      newStatus,
                IsMyChatMember: isMyChatMember,
                Date:           update.Date,
                Raw: map[string]string{
                        "actor_id":           actorID,
                        "user_id":            userID,
                        "old_status":         oldStatus,
                        "new_status":         newStatus,
                        "is_my_chat_member":  fmt.Sprintf("%t", isMyChatMember),
                },
        }

        // If the bot itself was added to a group, store the chat ID mapping
        // so it can respond in that chat later.
        if isMyChatMember && newStatus == "member" {
                c.chatIDs["bot"] = update.Chat.ID
        }

        // Publish the event to the bus for downstream consumers.
        if bus := c.GetBus(); bus != nil {
                bus.PublishChatMemberEvent(event)
        }

        return nil
}

// telegramChatMemberStatus extracts a human-readable status string from a
// ChatMember interface. The interface provides MemberStatus() which returns
// the status string ("creator", "administrator", "member", "restricted",
// "left", "kicked"). This function is kept for backward compatibility and
// as a convenience wrapper.
func telegramChatMemberStatus(member telego.ChatMember) string {
        if member == nil {
                return "unknown"
        }
        return member.MemberStatus()
}

func (c *TelegramChannel) prependTelegramQuotedReply(content string, reply *telego.Message) string {
        quoted := strings.TrimSpace(telegramQuotedContent(reply))
        if quoted == "" {
                return content
        }

        author := telegramQuotedAuthor(reply)
        role := c.telegramQuotedRole(reply)
        if strings.TrimSpace(content) == "" {
                return fmt.Sprintf("[quoted %s message from %s]: %s", role, author, quoted)
        }
        return fmt.Sprintf("[quoted %s message from %s]: %s\n\n%s", role, author, quoted, content)
}

func (c *TelegramChannel) telegramQuotedRole(message *telego.Message) string {
        if message == nil {
                return "unknown"
        }

        if message.From != nil {
                if !message.From.IsBot {
                        return "user"
                }
                if c.isOwnBotUser(message.From) {
                        return "assistant"
                }
                return "bot"
        }

        if message.SenderChat != nil {
                return "chat"
        }

        return "unknown"
}

func (c *TelegramChannel) isOwnBotUser(user *telego.User) bool {
        if c == nil || c.bot == nil || user == nil || !user.IsBot {
                return false
        }

        if botID := c.bot.ID(); botID != 0 && user.ID == botID {
                return true
        }

        botUsername := strings.TrimPrefix(strings.TrimSpace(c.bot.Username()), "@")
        if botUsername == "" {
                return false
        }
        return strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(user.Username), "@"), botUsername)
}

func telegramQuotedAuthor(message *telego.Message) string {
        if message == nil || message.From == nil {
                return "unknown"
        }
        if username := strings.TrimSpace(message.From.Username); username != "" {
                return username
        }
        if firstName := strings.TrimSpace(message.From.FirstName); firstName != "" {
                return firstName
        }
        return "unknown"
}

func telegramQuotedContent(message *telego.Message) string {
        if message == nil {
                return ""
        }

        var parts []string
        if text := strings.TrimSpace(message.Text); text != "" {
                parts = append(parts, text)
        }
        if caption := strings.TrimSpace(message.Caption); caption != "" {
                parts = append(parts, caption)
        }
        switch {
        case len(message.Photo) > 0:
                parts = append(parts, "[image: photo]")
        }
        switch {
        case message.Voice != nil:
                parts = append(parts, "[voice]")
        case message.Audio != nil:
                parts = append(parts, "[audio]")
        case message.Video != nil:
                parts = append(parts, "[video]")
        case message.VideoNote != nil:
                parts = append(parts, "[video note]")
        case message.Animation != nil:
                parts = append(parts, "[animation]")
        }
        if message.Document != nil {
                parts = append(parts, "[file]")
        }
        if message.Sticker != nil {
                emoji := message.Sticker.Emoji
                if emoji == "" {
                        emoji = "?"
                }
                parts = append(parts, fmt.Sprintf("[sticker: %s]", emoji))
        }
        if message.Contact != nil {
                contactParts := []string{message.Contact.FirstName}
                if message.Contact.LastName != "" {
                        contactParts = append(contactParts, message.Contact.LastName)
                }
                name := strings.Join(contactParts, " ")
                parts = append(parts, fmt.Sprintf("[contact: %s, %s]", name, message.Contact.PhoneNumber))
        }
        if message.Location != nil {
                parts = append(parts, fmt.Sprintf("[location: %.6f, %.6f]", message.Location.Latitude, message.Location.Longitude))
        }
        if message.Venue != nil {
                parts = append(parts, fmt.Sprintf("[venue: %s, %s]", message.Venue.Title, message.Venue.Address))
        }
        if message.Poll != nil {
                optionTexts := make([]string, len(message.Poll.Options))
                for i, opt := range message.Poll.Options {
                        optionTexts[i] = opt.Text
                }
                pollType := "poll"
                if message.Poll.Type == "quiz" {
                        pollType = "quiz"
                }
                parts = append(parts, fmt.Sprintf("[%s: %s (%s)]", pollType, message.Poll.Question, strings.Join(optionTexts, " / ")))
        }
        if message.Dice != nil {
                parts = append(parts, fmt.Sprintf("[dice: %s %d]", message.Dice.Emoji, message.Dice.Value))
        }

        return strings.Join(parts, "\n")
}

func quotedTelegramMediaRefs(
        message *telego.Message,
        resolve func(fileID, ext, filename string) string,
) []string {
        if message == nil || resolve == nil {
                return nil
        }

        var refs []string

        // Photo: download the largest size
        if len(message.Photo) > 0 {
                photo := message.Photo[len(message.Photo)-1]
                if ref := resolve(photo.FileID, ".jpg", "photo.jpg"); ref != "" {
                        refs = append(refs, ref)
                }
        }

        if message.Voice != nil {
                if ref := resolve(message.Voice.FileID, ".ogg", "voice.ogg"); ref != "" {
                        refs = append(refs, ref)
                }
        }
        if message.Audio != nil {
                if ref := resolve(message.Audio.FileID, ".mp3", "audio.mp3"); ref != "" {
                        refs = append(refs, ref)
                }
        }
        if message.Document != nil {
                filename := "document"
                if message.Document.FileName != "" {
                        filename = message.Document.FileName
                }
                if ref := resolve(message.Document.FileID, "", filename); ref != "" {
                        refs = append(refs, ref)
                }
        }
        if message.Video != nil {
                filename := "video.mp4"
                if message.Video.FileName != "" {
                        filename = message.Video.FileName
                }
                if ref := resolve(message.Video.FileID, ".mp4", filename); ref != "" {
                        refs = append(refs, ref)
                }
        }

        return refs
}

func (c *TelegramChannel) downloadPhoto(ctx context.Context, fileID string) string {
        file, err := c.bot.GetFile(ctx, &telego.GetFileParams{FileID: fileID})
        if err != nil {
                logger.ErrorCF("telegram", "Failed to get photo file", map[string]any{
                        "error": err.Error(),
                })
                return ""
        }

        return c.downloadFileWithInfo(file, ".jpg")
}

func (c *TelegramChannel) downloadFileWithInfo(file *telego.File, ext string) string {
        if file.FilePath == "" {
                return ""
        }

        url := c.bot.FileDownloadURL(file.FilePath)
        logger.DebugCF("telegram", "File URL", map[string]any{"url": url})

        // Use FilePath as filename for better identification
        filename := file.FilePath + ext
        return utils.DownloadFile(url, filename, utils.DownloadOptions{
                LoggerPrefix: "telegram",
        })
}

func (c *TelegramChannel) downloadFile(ctx context.Context, fileID, ext string) string {
        file, err := c.bot.GetFile(ctx, &telego.GetFileParams{FileID: fileID})
        if err != nil {
                logger.ErrorCF("telegram", "Failed to get file", map[string]any{
                        "error": err.Error(),
                })
                return ""
        }

        return c.downloadFileWithInfo(file, ext)
}

func parseContent(text string, useMarkdownV2 bool) string {
        if useMarkdownV2 {
                return markdownToTelegramMarkdownV2(text)
        }

        return markdownToTelegramHTML(text)
}

func fitToolFeedbackForTelegram(content string, useMarkdownV2 bool, maxParsedLen int) string {
        content = strings.TrimSpace(content)
        if content == "" || maxParsedLen <= 0 {
                return ""
        }
        animationSafeLen := maxParsedLen - channels.MaxToolFeedbackAnimationFrameLength()
        if animationSafeLen <= 0 {
                animationSafeLen = maxParsedLen
        }
        if len([]rune(parseContent(content, useMarkdownV2))) <= animationSafeLen {
                return content
        }

        low := 1
        high := len([]rune(content))
        best := utils.Truncate(content, 1)

        for low <= high {
                mid := (low + high) / 2
                candidate := utils.FitToolFeedbackMessage(content, mid)
                if candidate == "" {
                        high = mid - 1
                        continue
                }
                if len([]rune(parseContent(candidate, useMarkdownV2))) <= animationSafeLen {
                        best = candidate
                        low = mid + 1
                        continue
                }
                high = mid - 1
        }

        return best
}

func (c *TelegramChannel) PrepareToolFeedbackMessageContent(content string) string {
        if c == nil || c.tgCfg == nil {
                return strings.TrimSpace(content)
        }
        return fitToolFeedbackForTelegram(content, c.tgCfg.UseMarkdownV2, 4096)
}

func telegramToolFeedbackChatKey(chatID string, outboundCtx *bus.InboundContext) string {
        resolvedChatID, threadID, err := resolveTelegramOutboundTarget(chatID, outboundCtx)
        if err != nil || threadID == 0 {
                return strings.TrimSpace(chatID)
        }
        return fmt.Sprintf("%d/%d", resolvedChatID, threadID)
}

func (c *TelegramChannel) ToolFeedbackMessageChatID(chatID string, outboundCtx *bus.InboundContext) string {
        return telegramToolFeedbackChatKey(chatID, outboundCtx)
}

// parseTelegramChatID splits "chatID/threadID" into its components.
// Returns threadID=0 when no "/" is present (non-forum messages).
func parseTelegramChatID(chatID string) (int64, int, error) {
        idx := strings.Index(chatID, "/")
        if idx == -1 {
                cid, err := strconv.ParseInt(chatID, 10, 64)
                return cid, 0, err
        }
        cid, err := strconv.ParseInt(chatID[:idx], 10, 64)
        if err != nil {
                return 0, 0, err
        }
        tid, err := strconv.Atoi(chatID[idx+1:])
        if err != nil {
                return 0, 0, fmt.Errorf("invalid thread ID in chat ID %q: %w", chatID, err)
        }
        return cid, tid, nil
}

func resolveTelegramOutboundTarget(chatID string, outboundCtx *bus.InboundContext) (int64, int, error) {
        targetChatID := strings.TrimSpace(chatID)
        if targetChatID == "" && outboundCtx != nil {
                targetChatID = strings.TrimSpace(outboundCtx.ChatID)
        }
        resolvedChatID, resolvedThreadID, err := parseTelegramChatID(targetChatID)
        if err != nil {
                return 0, 0, err
        }
        if resolvedThreadID != 0 || outboundCtx == nil {
                return resolvedChatID, resolvedThreadID, nil
        }
        // Check TopicID first (forum topics)
        topicID := strings.TrimSpace(outboundCtx.TopicID)
        if topicID != "" {
                if tid, convErr := strconv.Atoi(topicID); convErr == nil {
                        return resolvedChatID, tid, nil
                }
        }
        // Fallback: check raw metadata for thread_id (non-forum reply threads)
        if outboundCtx.Raw != nil {
                if rawTID, ok := outboundCtx.Raw["thread_id"]; ok && rawTID != "" {
                        if tid, convErr := strconv.Atoi(rawTID); convErr == nil {
                                return resolvedChatID, tid, nil
                        }
                }
        }
        return resolvedChatID, resolvedThreadID, nil
}

func logParseFailed(err error, useMarkdownV2 bool) {
        parsingName := "HTML"
        if useMarkdownV2 {
                parsingName = "MarkdownV2"
        }

        logger.ErrorCF("telegram",
                fmt.Sprintf("%s parse failed, falling back to plain text", parsingName),
                map[string]any{
                        "error": err.Error(),
                },
        )
}

// isBotMentioned checks if the bot is mentioned in the message via entities.
func (c *TelegramChannel) isBotMentioned(message *telego.Message) bool {
        text, entities := telegramEntityTextAndList(message)
        if text == "" || len(entities) == 0 {
                return false
        }

        botUsername := ""
        if c.bot != nil {
                botUsername = c.bot.Username()
        }
        runes := []rune(text)

        for _, entity := range entities {
                entityText, ok := telegramEntityText(runes, entity)
                if !ok {
                        continue
                }

                switch entity.Type {
                case telego.EntityTypeMention:
                        if botUsername != "" && strings.EqualFold(entityText, "@"+botUsername) {
                                return true
                        }
                case telego.EntityTypeTextMention:
                        if botUsername != "" && entity.User != nil && strings.EqualFold(entity.User.Username, botUsername) {
                                return true
                        }
                case telego.EntityTypeBotCommand:
                        if isBotCommandEntityForThisBot(entityText, botUsername) {
                                return true
                        }
                }
        }
        return false
}

func telegramEntityTextAndList(message *telego.Message) (string, []telego.MessageEntity) {
        if message.Text != "" {
                return message.Text, message.Entities
        }
        return message.Caption, message.CaptionEntities
}

func telegramEntityText(runes []rune, entity telego.MessageEntity) (string, bool) {
        if entity.Offset < 0 || entity.Length <= 0 {
                return "", false
        }
        end := entity.Offset + entity.Length
        if entity.Offset >= len(runes) || end > len(runes) {
                return "", false
        }
        return string(runes[entity.Offset:end]), true
}

func isBotCommandEntityForThisBot(entityText, botUsername string) bool {
        if !strings.HasPrefix(entityText, "/") {
                return false
        }
        command := strings.TrimPrefix(entityText, "/")
        if command == "" {
                return false
        }

        at := strings.IndexRune(command, '@')
        if at == -1 {
                // A bare /command delivered to this bot is intended for this bot.
                return true
        }

        mentionUsername := command[at+1:]
        if mentionUsername == "" || botUsername == "" {
                return false
        }
        return strings.EqualFold(mentionUsername, botUsername)
}

// stripBotMention removes the @bot mention from the content.
func (c *TelegramChannel) stripBotMention(content string) string {
        botUsername := c.bot.Username()
        if botUsername == "" {
                return content
        }
        // Case-insensitive replacement
        re := regexp.MustCompile(`(?i)@` + regexp.QuoteMeta(botUsername))
        content = re.ReplaceAllString(content, "")
        return strings.TrimSpace(content)
}

// BeginStream implements channels.StreamingCapable.
func (c *TelegramChannel) BeginStream(ctx context.Context, chatID string) (channels.Streamer, error) {
        if !c.tgCfg.Streaming.Enabled {
                return nil, fmt.Errorf("streaming disabled in config")
        }

        cid, threadID, err := parseTelegramChatID(chatID)
        if err != nil {
                return nil, err
        }

        streamCfg := c.tgCfg.Streaming
        return &telegramStreamer{
                bot:              c.bot,
                chatID:           cid,
                threadID:         threadID,
                draftID:          cryptoRandInt(),
                throttleInterval: time.Duration(streamCfg.ThrottleSeconds) * time.Second,
                minGrowth:        streamCfg.MinGrowthChars,
        }, nil
}

// telegramStreamer streams partial LLM output via Telegram's sendMessageDraft API.
// On first API error (e.g. bot lacks forum mode), it silently degrades: Update
// becomes a no-op, while Finalize still delivers the final message.
type telegramStreamer struct {
        bot              *telego.Bot
        chatID           int64
        threadID         int
        draftID          int
        throttleInterval time.Duration
        minGrowth        int
        lastLen          int
        lastAt           time.Time
        failed           bool
        mu               sync.Mutex
}

func (s *telegramStreamer) Update(ctx context.Context, content string) error {
        s.mu.Lock()
        defer s.mu.Unlock()

        if s.failed {
                return nil
        }

        // Throttle: skip if not enough time or content has passed
        now := time.Now()
        growth := len(content) - s.lastLen
        if s.lastLen > 0 && now.Sub(s.lastAt) < s.throttleInterval && growth < s.minGrowth {
                return nil
        }

        htmlContent := markdownToTelegramHTML(content)

        err := s.bot.SendMessageDraft(ctx, &telego.SendMessageDraftParams{
                ChatID:          s.chatID,
                MessageThreadID: s.threadID,
                DraftID:         s.draftID,
                Text:            htmlContent,
                ParseMode:       telego.ModeHTML,
        })
        if err != nil {
                // First error → degrade silently (e.g. no forum mode)
                logger.WarnCF("telegram", "sendMessageDraft failed, disabling streaming", map[string]any{
                        "error": err.Error(),
                })
                s.failed = true
                return nil // don't propagate — Finalize will still deliver
        }

        s.lastLen = len(content)
        s.lastAt = now
        return nil
}

func (s *telegramStreamer) Finalize(ctx context.Context, content string) error {
        htmlContent := markdownToTelegramHTML(content)
        tgMsg := tu.Message(tu.ID(s.chatID), htmlContent)
        tgMsg.MessageThreadID = s.threadID
        tgMsg.ParseMode = telego.ModeHTML

        if _, err := s.bot.SendMessage(ctx, tgMsg); err != nil {
                // Fallback to plain text
                tgMsg.ParseMode = ""
                if _, err = s.bot.SendMessage(ctx, tgMsg); err != nil {
                        logger.ErrorCF("telegram", "Finalize failed after HTML and plain-text attempts", map[string]any{
                                "chat_id": s.chatID,
                                "error":   err.Error(),
                                "len":     len(content),
                        })
                        return fmt.Errorf("telegram finalize: %w", err)
                }
        }
        return nil
}

func (s *telegramStreamer) Cancel(ctx context.Context) {
        // Draft auto-expires on Telegram's side; nothing to clean up.
}

// cryptoRandInt returns a non-zero random int using crypto/rand.
func cryptoRandInt() int {
        var b [4]byte
        _, _ = rand.Read(b[:])
        return int(binary.BigEndian.Uint32(b[:])) | 1 // ensure non-zero
}

// isPostConnectError identifies network errors that likely occurred after
// the request was transmitted to Telegram (e.g. dropped connection while
// waiting for response). Swallowing these for edits prevents duplicate
// fallbacks, at the small risk of leaving a stale placeholder if the
// edit never actually reached the server.
func isPostConnectError(err error) bool {
        if err == nil {
                return false
        }

        // Context errors (timeout/canceled) are too broad; they can be triggered
        // locally before any data is sent. Never swallow them.
        if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
                return false
        }

        msg := strings.ToLower(err.Error())
        // Narrowly target connection dropouts where the request likely landed.
        return strings.Contains(msg, "connection reset by peer") ||
                strings.Contains(msg, "unexpected eof") ||
                strings.Contains(msg, "connection closed by foreign host") ||
                strings.Contains(msg, "broken pipe")
}

// VoiceCapabilities returns the voice capabilities of the channel.
func (c *TelegramChannel) VoiceCapabilities() channels.VoiceCapabilities {
        return channels.VoiceCapabilities{ASR: true, TTS: true}
}
