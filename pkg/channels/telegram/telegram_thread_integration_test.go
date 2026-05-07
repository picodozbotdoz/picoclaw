//go:build integration

package telegram

import (
        "context"
        "fmt"
        "os"
        "strconv"
        "testing"
        "time"

        "github.com/mymmrac/telego"
        th "github.com/mymmrac/telego/telegohandler"
        tu "github.com/mymmrac/telego/telegoutil"
        "github.com/stretchr/testify/assert"
        "github.com/stretchr/testify/require"

        "github.com/sipeed/picoclaw/pkg/bus"
        "github.com/sipeed/picoclaw/pkg/channels"
        "github.com/sipeed/picoclaw/pkg/config"
        "github.com/sipeed/picoclaw/pkg/media"
)

// ---------------------------------------------------------------------------
//  Threaded Conversation Integration Tests
// ---------------------------------------------------------------------------
//
// These tests verify that picoclaw's Telegram channel correctly handles
// threaded conversations — both forum topics and private-chat reply threads.
//
// Environment variables:
//
//   TELEGRAM_BOT_TOKEN       — (required) bot token
//   TELEGRAM_CHAT_ID         — (optional) any chat ID; if empty, auto-discovered
//   TELEGRAM_FORUM_CHAT_ID   — (optional) forum supergroup chat ID
//   TELEGRAM_THREAD_ID       — (optional) an existing thread/topic ID in the chat;
//                               if empty, auto-discovered from recent messages
//
// Run with:
//
//   TELEGRAM_BOT_TOKEN=xxx TELEGRAM_CHAT_ID=123 TELEGRAM_THREAD_ID=3004 \
//     go test -tags=integration -run TestThread -v -timeout 600s \
//     ./pkg/channels/telegram/...
// ---------------------------------------------------------------------------

// threadBot creates a bot from env var.
func threadBot(t *testing.T) *telego.Bot {
        t.Helper()
        token := os.Getenv("TELEGRAM_BOT_TOKEN")
        if token == "" {
                t.Skip("TELEGRAM_BOT_TOKEN not set")
        }
        bot, err := telego.NewBot(token, telego.WithDiscardLogger())
        require.NoError(t, err, "failed to create bot")
        return bot
}

// threadChatID returns TELEGRAM_CHAT_ID or discovers it via polling.
func threadChatID(t *testing.T, bot *telego.Bot) int64 {
        t.Helper()
        if raw := os.Getenv("TELEGRAM_CHAT_ID"); raw != "" {
                cid, err := strconv.ParseInt(raw, 10, 64)
                require.NoError(t, err)
                return cid
        }
        t.Log("TELEGRAM_CHAT_ID not set; waiting up to 60s for you to send any message to the bot...")
        ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
        defer cancel()
        updates, err := bot.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{Timeout: 10})
        require.NoError(t, err)
        for update := range updates {
                if update.Message != nil {
                        cid := update.Message.Chat.ID
                        t.Logf("Discovered chat ID: %d", cid)
                        return cid
                }
        }
        t.Fatal("timed out waiting for a message")
        return 0
}

// threadChannel creates a TelegramChannel wired to a real bot for testing.
func threadChannel(t *testing.T, bot *telego.Bot) *TelegramChannel {
        t.Helper()
        token := os.Getenv("TELEGRAM_BOT_TOKEN")
        secureToken := config.NewSecureString(token)
        tgCfg := &config.TelegramSettings{Token: *secureToken}
        base := channels.NewBaseChannel("telegram", tgCfg, nil, nil,
                channels.WithMaxMessageLength(4000),
        )
        base.SetRunning(true)
        store := media.NewFileMediaStore()
        ch := &TelegramChannel{
                BaseChannel: base,
                bot:         bot,
                chatIDs:     make(map[string]int64),
                bc:          &config.Channel{Type: config.ChannelTelegram, Enabled: true},
                tgCfg:       tgCfg,
                progress:    channels.NewToolFeedbackAnimator(nil),
        }
        ch.SetMediaStore(store)
        return ch
}

// discoverThreadID polls for an existing message with a thread ID and returns it.
// Falls back to TELEGRAM_THREAD_ID env var.
func discoverThreadID(t *testing.T, bot *telego.Bot, chatID int64) int {
        t.Helper()
        if raw := os.Getenv("TELEGRAM_THREAD_ID"); raw != "" {
                tid, err := strconv.Atoi(raw)
                require.NoError(t, err, "TELEGRAM_THREAD_ID must be a valid int")
                return tid
        }

        t.Log("TELEGRAM_THREAD_ID not set; waiting up to 60s for you to send a message in a thread...")
        ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
        defer cancel()

        updates, err := bot.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{
                Timeout:        10,
                AllowedUpdates: []string{"message"},
        })
        require.NoError(t, err)

        for update := range updates {
                if update.Message != nil && update.Message.MessageThreadID != 0 {
                        tid := update.Message.MessageThreadID
                        t.Logf("Discovered thread ID: %d", tid)
                        return tid
                }
        }
        t.Fatal("timed out waiting for a threaded message — set TELEGRAM_THREAD_ID")
        return 0
}

// ---------------------------------------------------------------------------
//  Phase 1: Bot connectivity
// ---------------------------------------------------------------------------

func TestThread_BotConnectivity(t *testing.T) {
        bot := threadBot(t)
        user, err := bot.GetMe(context.Background())
        require.NoError(t, err, "getMe failed — is the token correct?")
        assert.True(t, user.IsBot)
        assert.NotEmpty(t, user.Username)
        t.Logf("Bot connected: @%s (id=%d)", user.Username, user.ID)
}

// ---------------------------------------------------------------------------
//  Phase 2: Parse & resolve helpers (pure logic, no API calls)
// ---------------------------------------------------------------------------

func TestThread_ParseChatID_WithThread(t *testing.T) {
        cid, tid, err := parseTelegramChatID("-1001234567890/42")
        require.NoError(t, err)
        assert.Equal(t, int64(-1001234567890), cid)
        assert.Equal(t, 42, tid)
}

func TestThread_ParseChatID_WithoutThread(t *testing.T) {
        cid, tid, err := parseTelegramChatID("-1001234567890")
        require.NoError(t, err)
        assert.Equal(t, int64(-1001234567890), cid)
        assert.Equal(t, 0, tid)
}

func TestThread_ParseChatID_PositivePrivateChat(t *testing.T) {
        cid, tid, err := parseTelegramChatID("7479477860")
        require.NoError(t, err)
        assert.Equal(t, int64(7479477860), cid)
        assert.Equal(t, 0, tid)
}

func TestThread_ParseChatID_PrivateChatWithThread(t *testing.T) {
        cid, tid, err := parseTelegramChatID("7479477860/3004")
        require.NoError(t, err)
        assert.Equal(t, int64(7479477860), cid)
        assert.Equal(t, 3004, tid)
        t.Logf("Parsed 7479477860/3004 → cid=%d, tid=%d", cid, tid)
}

func TestThread_ParseChatID_InvalidThreadID(t *testing.T) {
        _, _, err := parseTelegramChatID("7479477860/not-a-thread")
        assert.Error(t, err)
        assert.Contains(t, err.Error(), "invalid thread ID")
}

func TestThread_ResolveTarget_CompositeChatID(t *testing.T) {
        cid, tid, err := resolveTelegramOutboundTarget("7479477860/3004", nil)
        require.NoError(t, err)
        assert.Equal(t, int64(7479477860), cid)
        assert.Equal(t, 3004, tid)
        t.Log("Composite chatID resolved correctly for private chat thread")
}

func TestThread_ResolveTarget_TopicIDFromContext(t *testing.T) {
        outboundCtx := &bus.InboundContext{TopicID: "3004"}
        cid, tid, err := resolveTelegramOutboundTarget("7479477860", outboundCtx)
        require.NoError(t, err)
        assert.Equal(t, int64(7479477860), cid)
        assert.Equal(t, 3004, tid)
        t.Log("TopicID from InboundContext resolved correctly")
}

func TestThread_ResolveTarget_CompositeOverridesContext(t *testing.T) {
        outboundCtx := &bus.InboundContext{TopicID: "9999"}
        cid, tid, err := resolveTelegramOutboundTarget("7479477860/3004", outboundCtx)
        require.NoError(t, err)
        assert.Equal(t, int64(7479477860), cid)
        assert.Equal(t, 3004, tid, "composite chatID thread should override context TopicID")
}

func TestThread_ResolveTarget_EmptyTopicIDIgnored(t *testing.T) {
        outboundCtx := &bus.InboundContext{TopicID: ""}
        _, tid, err := resolveTelegramOutboundTarget("7479477860", outboundCtx)
        require.NoError(t, err)
        assert.Equal(t, 0, tid)
}

func TestThread_ResolveTarget_FallbackToContextChatID(t *testing.T) {
        outboundCtx := &bus.InboundContext{ChatID: "7479477860", TopicID: "3004"}
        cid, tid, err := resolveTelegramOutboundTarget("", outboundCtx)
        require.NoError(t, err)
        assert.Equal(t, int64(7479477860), cid)
        assert.Equal(t, 3004, tid)
}

func TestThread_ResolveTarget_RawThreadIDFallback(t *testing.T) {
        // When TopicID is empty but raw metadata has thread_id,
        // resolveTelegramOutboundTarget should use it.
        outboundCtx := &bus.InboundContext{
                ChatID: "7479477860",
                Raw:    map[string]string{"thread_id": "3004"},
        }
        cid, tid, err := resolveTelegramOutboundTarget("7479477860", outboundCtx)
        require.NoError(t, err)
        assert.Equal(t, int64(7479477860), cid)
        assert.Equal(t, 3004, tid)
        t.Log("Raw thread_id fallback works for non-forum reply threads")
}

func TestThread_ResolveTarget_TopicIDOverridesRawThreadID(t *testing.T) {
        // TopicID should take precedence over raw thread_id
        outboundCtx := &bus.InboundContext{
                ChatID: "7479477860",
                TopicID: "42",
                Raw:    map[string]string{"thread_id": "3004"},
        }
        cid, tid, err := resolveTelegramOutboundTarget("7479477860", outboundCtx)
        require.NoError(t, err)
        assert.Equal(t, int64(7479477860), cid)
        assert.Equal(t, 42, tid, "TopicID should override raw thread_id")
        t.Log("TopicID correctly takes precedence over raw thread_id")
}

// ---------------------------------------------------------------------------
//  Phase 3: Direct API — verify message_thread_id works in private chat threads
// ---------------------------------------------------------------------------

func TestThread_DirectAPI_SendMessageToThread(t *testing.T) {
        bot := threadBot(t)
        chatID := threadChatID(t, bot)
        threadID := discoverThreadID(t, bot, chatID)

        msg, err := bot.SendMessage(context.Background(), &telego.SendMessageParams{
                ChatID:          tu.ID(chatID),
                MessageThreadID: threadID,
                Text:            fmt.Sprintf("🧵 Direct API: message sent to thread %d", threadID),
        })
        require.NoError(t, err, "SendMessage with message_thread_id should work in existing thread")
        t.Logf("Sent message to thread %d, msg ID: %d", threadID, msg.MessageID)
}

func TestThread_DirectAPI_SendLocationToThread(t *testing.T) {
        bot := threadBot(t)
        chatID := threadChatID(t, bot)
        threadID := discoverThreadID(t, bot, chatID)

        msg, err := bot.SendLocation(context.Background(), &telego.SendLocationParams{
                ChatID:          tu.ID(chatID),
                MessageThreadID: threadID,
                Latitude:        13.7563,
                Longitude:       100.5018,
        })
        require.NoError(t, err)
        t.Logf("Sent location to thread %d, msg ID: %d", threadID, msg.MessageID)
}

func TestThread_DirectAPI_TypingInThread(t *testing.T) {
        bot := threadBot(t)
        chatID := threadChatID(t, bot)
        threadID := discoverThreadID(t, bot, chatID)

        err := bot.SendChatAction(context.Background(), &telego.SendChatActionParams{
                ChatID:          tu.ID(chatID),
                MessageThreadID: threadID,
                Action:          telego.ChatActionTyping,
        })
        require.NoError(t, err)
        t.Logf("Typing indicator sent in thread %d", threadID)
}

// ---------------------------------------------------------------------------
//  Phase 4: Channel layer — outbound with composite chatID (chatID/threadID)
// ---------------------------------------------------------------------------

func TestThread_Channel_SendToThread(t *testing.T) {
        bot := threadBot(t)
        chatID := threadChatID(t, bot)
        threadID := discoverThreadID(t, bot, chatID)
        ch := threadChannel(t, bot)

        compositeChatID := fmt.Sprintf("%d/%d", chatID, threadID)
        msgIDs, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  compositeChatID,
                Content: fmt.Sprintf("🧵 Channel test: Send to thread %d via composite chatID", threadID),
        })
        require.NoError(t, err)
        require.NotEmpty(t, msgIDs)
        t.Logf("Sent to thread %d via composite chatID %s, IDs: %v", threadID, compositeChatID, msgIDs)
}

func TestThread_Channel_SendViaTopicIDContext(t *testing.T) {
        bot := threadBot(t)
        chatID := threadChatID(t, bot)
        threadID := discoverThreadID(t, bot, chatID)
        ch := threadChannel(t, bot)

        msgIDs, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  fmt.Sprintf("%d", chatID),
                Content: fmt.Sprintf("🧵 Channel test: Send via TopicID context (thread=%d)", threadID),
                Context: bus.InboundContext{
                        TopicID: fmt.Sprintf("%d", threadID),
                },
        })
        require.NoError(t, err)
        require.NotEmpty(t, msgIDs)
        t.Logf("Sent via TopicID context, IDs: %v", msgIDs)
}

func TestThread_Channel_EditInThread(t *testing.T) {
        bot := threadBot(t)
        chatID := threadChatID(t, bot)
        threadID := discoverThreadID(t, bot, chatID)
        ch := threadChannel(t, bot)

        compositeChatID := fmt.Sprintf("%d/%d", chatID, threadID)
        msgIDs, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  compositeChatID,
                Content: "🧵 Channel test: original (will be edited)",
        })
        require.NoError(t, err)
        require.NotEmpty(t, msgIDs)

        err = ch.EditMessage(context.Background(), compositeChatID, msgIDs[0],
                "🧵 Channel test: EDITED ✏️")
        require.NoError(t, err)
        t.Logf("Edited message in thread, ID: %s", msgIDs[0])
}

func TestThread_Channel_DeleteInThread(t *testing.T) {
        bot := threadBot(t)
        chatID := threadChatID(t, bot)
        threadID := discoverThreadID(t, bot, chatID)
        ch := threadChannel(t, bot)

        compositeChatID := fmt.Sprintf("%d/%d", chatID, threadID)
        msgIDs, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  compositeChatID,
                Content: "🧵 Channel test: message to be deleted 🗑️",
        })
        require.NoError(t, err)
        require.NotEmpty(t, msgIDs)

        err = ch.DeleteMessage(context.Background(), compositeChatID, msgIDs[0])
        require.NoError(t, err)
        t.Logf("Deleted message in thread, ID: %s", msgIDs[0])
}

func TestThread_Channel_ReactInThread(t *testing.T) {
        bot := threadBot(t)
        chatID := threadChatID(t, bot)
        threadID := discoverThreadID(t, bot, chatID)
        ch := threadChannel(t, bot)

        compositeChatID := fmt.Sprintf("%d/%d", chatID, threadID)
        msgIDs, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  compositeChatID,
                Content: "🧵 Channel test: react to this message 👆",
        })
        require.NoError(t, err)
        require.NotEmpty(t, msgIDs)

        undo, err := ch.ReactToMessage(context.Background(), compositeChatID, msgIDs[0])
        require.NoError(t, err)
        t.Logf("Reacted in thread, msg ID: %s", msgIDs[0])

        time.Sleep(2 * time.Second)
        undo()
        t.Log("Removed reaction")
}

func TestThread_Channel_TypingInThread(t *testing.T) {
        bot := threadBot(t)
        chatID := threadChatID(t, bot)
        threadID := discoverThreadID(t, bot, chatID)
        ch := threadChannel(t, bot)

        compositeChatID := fmt.Sprintf("%d/%d", chatID, threadID)
        stop, err := ch.StartTyping(context.Background(), compositeChatID)
        require.NoError(t, err)
        t.Logf("Typing indicator started in thread %d", threadID)

        time.Sleep(5 * time.Second)
        stop()
        t.Log("Typing indicator stopped")
}

func TestThread_Channel_ReplyInThread(t *testing.T) {
        bot := threadBot(t)
        chatID := threadChatID(t, bot)
        threadID := discoverThreadID(t, bot, chatID)
        ch := threadChannel(t, bot)

        compositeChatID := fmt.Sprintf("%d/%d", chatID, threadID)
        msgIDs, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  compositeChatID,
                Content: "🧵 Channel test: original message in thread",
        })
        require.NoError(t, err)
        require.NotEmpty(t, msgIDs)

        replyIDs, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:           compositeChatID,
                Content:          "🧵 Channel test: reply in thread ↩️",
                ReplyToMessageID: msgIDs[0],
        })
        require.NoError(t, err)
        require.NotEmpty(t, replyIDs)
        t.Logf("Reply in thread sent, original: %s, reply IDs: %v", msgIDs[0], replyIDs)
}

func TestThread_Channel_LocationInThread(t *testing.T) {
        bot := threadBot(t)
        chatID := threadChatID(t, bot)
        threadID := discoverThreadID(t, bot, chatID)
        ch := threadChannel(t, bot)

        compositeChatID := fmt.Sprintf("%d/%d", chatID, threadID)
        msgIDs, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
                ChatID: compositeChatID,
                Parts: []bus.MediaPart{{
                        Type:      "location",
                        Latitude:  13.7563,
                        Longitude: 100.5018,
                }},
        })
        require.NoError(t, err)
        require.NotEmpty(t, msgIDs)
        t.Logf("Sent location in thread, IDs: %v", msgIDs)
}

func TestThread_Channel_VenueInThread(t *testing.T) {
        bot := threadBot(t)
        chatID := threadChatID(t, bot)
        threadID := discoverThreadID(t, bot, chatID)
        ch := threadChannel(t, bot)

        compositeChatID := fmt.Sprintf("%d/%d", chatID, threadID)
        msgIDs, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
                ChatID: compositeChatID,
                Parts: []bus.MediaPart{{
                        Type:      "venue",
                        Latitude:  13.7563,
                        Longitude: 100.5018,
                        Title:     "🧵 Thread Test Venue",
                        Address:   "Bangkok, Thailand",
                }},
        })
        require.NoError(t, err)
        require.NotEmpty(t, msgIDs)
        t.Logf("Sent venue in thread, IDs: %v", msgIDs)
}

func TestThread_Channel_BatchDeleteInThread(t *testing.T) {
        bot := threadBot(t)
        chatID := threadChatID(t, bot)
        threadID := discoverThreadID(t, bot, chatID)
        ch := threadChannel(t, bot)

        compositeChatID := fmt.Sprintf("%d/%d", chatID, threadID)
        var ids []string
        for i := 0; i < 3; i++ {
                msgIDs, err := ch.Send(context.Background(), bus.OutboundMessage{
                        ChatID:  compositeChatID,
                        Content: fmt.Sprintf("🧵 Batch delete #%d 🗑️", i+1),
                })
                require.NoError(t, err)
                require.NotEmpty(t, msgIDs)
                ids = append(ids, msgIDs...)
        }
        t.Logf("Sent %d messages for batch deletion: %v", len(ids), ids)
        time.Sleep(1 * time.Second)

        err := ch.DeleteMessages(context.Background(), compositeChatID, ids)
        require.NoError(t, err)
        t.Logf("Batch-deleted %d messages in thread", len(ids))
}

// ---------------------------------------------------------------------------
//  Phase 5: Inbound — receiving threaded messages via handleMessage
// ---------------------------------------------------------------------------

func TestThread_Inbound_ThreadedMessage(t *testing.T) {
        bot := threadBot(t)
        chatID := threadChatID(t, bot)
        threadID := discoverThreadID(t, bot, chatID)

        // Set up the inbound harness
        token := os.Getenv("TELEGRAM_BOT_TOKEN")
        secureToken := config.NewSecureString(token)
        tgCfg := &config.TelegramSettings{Token: *secureToken}

        messageBus := bus.NewMessageBus()
        base := channels.NewBaseChannel("telegram", tgCfg, messageBus, nil,
                channels.WithMaxMessageLength(4000),
        )
        store := media.NewFileMediaStore()

        ch := &TelegramChannel{
                BaseChannel: base,
                bot:         bot,
                chatIDs:     make(map[string]int64),
                bc:          &config.Channel{Type: config.ChannelTelegram, Enabled: true},
                tgCfg:       tgCfg,
                progress:    channels.NewToolFeedbackAnimator(nil),
        }
        ch.SetMediaStore(store)

        ctx, cancel := context.WithCancel(context.Background())
        ch.ctx = ctx
        ch.cancel = cancel

        updates, err := bot.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{
                Timeout:        10,
                AllowedUpdates: []string{"message", "edited_message"},
        })
        require.NoError(t, err)

        bh, err := th.NewBotHandler(bot, updates)
        require.NoError(t, err)
        ch.bh = bh

        bh.HandleMessage(func(ctx *th.Context, message telego.Message) error {
                return ch.handleMessage(ctx, &message)
        }, th.AnyMessage())

        bh.HandleEditedMessage(func(ctx *th.Context, message telego.Message) error {
                return ch.handleMessage(ctx, &message, true)
        })

        ch.SetRunning(true)
        go bh.Start()

        cleanup := func() {
                bh.StopWithContext(ctx)
                cancel()
        }
        defer cleanup()

        // Send a prompt in the thread
        promptMsg := tu.Message(tu.ID(chatID),
                fmt.Sprintf("🧵 INBOUND THREAD TEST: Please send a message in thread %d within 90 seconds...", threadID))
        promptMsg.MessageThreadID = threadID
        _, _ = bot.SendMessage(context.Background(), promptMsg)

        // Wait for an inbound message
        inbound := waitForInbound(t, messageBus, 90*time.Second)

        assert.Equal(t, "telegram", inbound.Channel)
        t.Logf("Received inbound message:")
        t.Logf("   ChatID: %s", inbound.ChatID)
        t.Logf("   TopicID: %s", inbound.Context.TopicID)
        t.Logf("   Content: %q", inbound.Content)
        t.Logf("   MessageID: %s", inbound.MessageID)

        // Check if the message has a thread ID
        threadIDStr := inbound.Context.Raw["thread_id"]
        if threadIDStr != "" {
                t.Logf("   thread_id (from raw metadata): %s", threadIDStr)
        }

        // Note: In private chats, the current code only sets composite chatID
        // and TopicID for forum groups (IsForum=true). Reply threads in private
        // chats do NOT get per-thread isolation by design — they share one
        // session per chat. The MessageThreadID is stored in the raw metadata.
        //
        // If this is a forum group, we'd expect:
        //   - ChatID to be composite (e.g., "chatID/threadID")
        //   - TopicID to be set
        //
        // For private chat reply threads:
        //   - ChatID is just the chat ID (no composite)
        //   - TopicID is empty
        //   - thread_id is in the raw metadata

        if inbound.Context.TopicID != "" {
                t.Logf("✅ TopicID is set (forum or topic-aware routing)")
                assert.Contains(t, inbound.ChatID, "/",
                        "TopicID set should mean composite chatID")
        } else {
                t.Log("No TopicID (private chat reply thread — expected behavior)")
                t.Logf("   thread_id in raw: %q", threadIDStr)
        }
}

// ---------------------------------------------------------------------------
//  Phase 6: Inbound callback in thread
// ---------------------------------------------------------------------------

func TestThread_Inbound_CallbackInThread(t *testing.T) {
        bot := threadBot(t)
        chatID := threadChatID(t, bot)
        threadID := discoverThreadID(t, bot, chatID)

        token := os.Getenv("TELEGRAM_BOT_TOKEN")
        secureToken := config.NewSecureString(token)
        tgCfg := &config.TelegramSettings{Token: *secureToken}

        messageBus := bus.NewMessageBus()
        base := channels.NewBaseChannel("telegram", tgCfg, messageBus, nil,
                channels.WithMaxMessageLength(4000),
        )
        store := media.NewFileMediaStore()

        ch := &TelegramChannel{
                BaseChannel: base,
                bot:         bot,
                chatIDs:     make(map[string]int64),
                bc:          &config.Channel{Type: config.ChannelTelegram, Enabled: true},
                tgCfg:       tgCfg,
                progress:    channels.NewToolFeedbackAnimator(nil),
        }
        ch.SetMediaStore(store)

        ctx, cancel := context.WithCancel(context.Background())
        ch.ctx = ctx
        ch.cancel = cancel

        updates, err := bot.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{
                Timeout:        10,
                AllowedUpdates: []string{"message", "callback_query"},
        })
        require.NoError(t, err)

        bh, err := th.NewBotHandler(bot, updates)
        require.NoError(t, err)
        ch.bh = bh

        bh.HandleMessage(func(ctx *th.Context, message telego.Message) error {
                return ch.handleMessage(ctx, &message)
        }, th.AnyMessage())

        bh.HandleCallbackQuery(func(ctx *th.Context, query telego.CallbackQuery) error {
                return ch.handleCallbackQuery(ctx, &query)
        })

        ch.SetRunning(true)
        go bh.Start()

        cleanup := func() {
                bh.StopWithContext(ctx)
                cancel()
        }
        defer cleanup()

        // Send a message with inline keyboard in the thread
        keyboard := tu.InlineKeyboard(
                tu.InlineKeyboardRow(
                        tu.InlineKeyboardButton("🧵 Click Me (Thread Test)").WithCallbackData("thread_test_cb"),
                ),
        )
        msg := tu.Message(tu.ID(chatID), "🧵 CALLBACK THREAD TEST: Click the button below within 90 seconds...")
        msg.MessageThreadID = threadID
        msg.ReplyMarkup = keyboard
        _, err = bot.SendMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound := waitForInbound(t, messageBus, 90*time.Second)

        assert.Equal(t, "callback_query", inbound.Context.Raw["message_kind"])
        assert.Equal(t, "thread_test_cb", inbound.Context.Raw["callback_data"])

        t.Logf("Callback received:")
        t.Logf("   ChatID: %s", inbound.ChatID)
        t.Logf("   TopicID: %s", inbound.Context.TopicID)
        t.Logf("   Callback data: %s", inbound.Context.Raw["callback_data"])

        if inbound.Context.TopicID != "" {
                t.Log("✅ Callback has TopicID (forum topic)")
        } else {
                t.Log("Callback without TopicID (private chat thread — expected)")
        }
}

// ---------------------------------------------------------------------------
//  Phase 7: Comprehensive outbound feature test in thread
// ---------------------------------------------------------------------------

func TestThread_Channel_AllOutboundFeaturesInThread(t *testing.T) {
        bot := threadBot(t)
        chatID := threadChatID(t, bot)
        threadID := discoverThreadID(t, bot, chatID)
        ch := threadChannel(t, bot)
        compositeChatID := fmt.Sprintf("%d/%d", chatID, threadID)

        t.Logf("Running comprehensive test in thread %d of chat %d", threadID, chatID)

        // --- 1. Send text ---
        msgIDs, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  compositeChatID,
                Content: "🧵 Comprehensive thread test (1/8): text message ✅",
        })
        require.NoError(t, err)
        require.NotEmpty(t, msgIDs)
        time.Sleep(500 * time.Millisecond)

        // --- 2. Edit the text ---
        err = ch.EditMessage(context.Background(), compositeChatID, msgIDs[0],
                "🧵 Comprehensive thread test (1/8): text message ✏️ EDITED")
        require.NoError(t, err)
        time.Sleep(500 * time.Millisecond)

        // --- 3. Reply ---
        replyIDs, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:           compositeChatID,
                Content:          "🧵 Comprehensive thread test (2/8): reply ↩️",
                ReplyToMessageID: msgIDs[0],
        })
        require.NoError(t, err)
        require.NotEmpty(t, replyIDs)
        time.Sleep(500 * time.Millisecond)

        // --- 4. React ---
        undo, err := ch.ReactToMessage(context.Background(), compositeChatID, replyIDs[0])
        require.NoError(t, err)
        time.Sleep(2 * time.Second)
        undo()
        t.Log("Reaction added and removed in thread")
        time.Sleep(500 * time.Millisecond)

        // --- 5. Location ---
        _, err = ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
                ChatID: compositeChatID,
                Parts: []bus.MediaPart{{
                        Type:      "location",
                        Latitude:  35.6762,
                        Longitude: 139.6503,
                }},
        })
        require.NoError(t, err)
        time.Sleep(500 * time.Millisecond)

        // --- 6. Venue ---
        _, err = ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
                ChatID: compositeChatID,
                Parts: []bus.MediaPart{{
                        Type:      "venue",
                        Latitude:  35.6762,
                        Longitude: 139.6503,
                        Title:     "🧵 Thread Test — Tokyo Tower",
                        Address:   "4 Chome-2-8 Shibakoen, Minato City, Tokyo",
                }},
        })
        require.NoError(t, err)
        time.Sleep(500 * time.Millisecond)

        // --- 7. Pin + Unpin ---
        pinMsgIDs, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  compositeChatID,
                Content: "🧵 Comprehensive thread test (7/8): pin test 📌",
        })
        require.NoError(t, err)
        require.NotEmpty(t, pinMsgIDs)

        err = ch.PinMessage(context.Background(), compositeChatID, pinMsgIDs[0])
        if err != nil {
                t.Logf("Pin failed (expected in private chat): %v", err)
        } else {
                t.Log("Pin succeeded in thread")
                time.Sleep(1 * time.Second)
                _ = ch.UnpinMessage(context.Background(), compositeChatID, pinMsgIDs[0])
        }

        // --- 8. Batch delete ---
        var batchIDs []string
        for i := 0; i < 2; i++ {
                ids, sendErr := ch.Send(context.Background(), bus.OutboundMessage{
                        ChatID:  compositeChatID,
                        Content: fmt.Sprintf("🧵 Comprehensive thread test (8/8): batch-delete #%d 🗑️", i+1),
                })
                require.NoError(t, sendErr)
                require.NotEmpty(t, ids)
                batchIDs = append(batchIDs, ids...)
        }
        time.Sleep(500 * time.Millisecond)

        err = ch.DeleteMessages(context.Background(), compositeChatID, batchIDs)
        require.NoError(t, err)
        t.Log("Batch delete succeeded in thread")

        // --- Final summary ---
        _, _ = ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  compositeChatID,
                Content: "✅ Comprehensive thread outbound test complete! All features verified.",
        })
}
