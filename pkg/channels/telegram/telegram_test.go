package telegram

import (
        "context"
        "encoding/json"
        "errors"
        "fmt"
        "io"
        "os"
        "path/filepath"
        "strconv"
        "strings"
        "testing"
        "time"

        "github.com/mymmrac/telego"
        ta "github.com/mymmrac/telego/telegoapi"
        "github.com/stretchr/testify/assert"
        "github.com/stretchr/testify/require"

        "github.com/sipeed/picoclaw/pkg/bus"
        "github.com/sipeed/picoclaw/pkg/channels"
        "github.com/sipeed/picoclaw/pkg/config"
        "github.com/sipeed/picoclaw/pkg/media"
)

const testToken = "1234567890:aaaabbbbaaaabbbbaaaabbbbaaaabbbbccc"

// stubCaller implements ta.Caller for testing.
type stubCaller struct {
        calls  []stubCall
        callFn func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error)
}

type stubCall struct {
        URL  string
        Data *ta.RequestData
}

func (s *stubCaller) Call(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
        s.calls = append(s.calls, stubCall{URL: url, Data: data})
        return s.callFn(ctx, url, data)
}

// stubConstructor implements ta.RequestConstructor for testing.
type stubConstructor struct{}

type multipartCall struct {
        Parameters map[string]string
        FileSizes  map[string]int
}

func (s *stubConstructor) JSONRequest(parameters any) (*ta.RequestData, error) {
        b, err := json.Marshal(parameters)
        if err != nil {
                return nil, err
        }
        return &ta.RequestData{
                ContentType: "application/json",
                BodyRaw:     b,
        }, nil
}

func (s *stubConstructor) MultipartRequest(
        parameters map[string]string,
        files map[string]ta.NamedReader,
) (*ta.RequestData, error) {
        return &ta.RequestData{}, nil
}

type multipartRecordingConstructor struct {
        stubConstructor
        calls []multipartCall
}

func (s *multipartRecordingConstructor) MultipartRequest(
        parameters map[string]string,
        files map[string]ta.NamedReader,
) (*ta.RequestData, error) {
        call := multipartCall{
                Parameters: make(map[string]string, len(parameters)),
                FileSizes:  make(map[string]int, len(files)),
        }
        for k, v := range parameters {
                call.Parameters[k] = v
        }
        for field, file := range files {
                if file == nil {
                        continue
                }
                data, err := io.ReadAll(file)
                if err != nil {
                        return nil, err
                }
                call.FileSizes[field] = len(data)
        }
        s.calls = append(s.calls, call)
        return &ta.RequestData{}, nil
}

// successResponse returns a ta.Response that telego will treat as a successful SendMessage.
func successResponse(t *testing.T) *ta.Response {
        return successResponseWithMessageID(t, 1)
}

func successResponseWithMessageID(t *testing.T, messageID int) *ta.Response {
        t.Helper()
        msg := &telego.Message{MessageID: messageID}
        b, err := json.Marshal(msg)
        require.NoError(t, err)
        return &ta.Response{Ok: true, Result: b}
}

func successUserResponse(t *testing.T, user *telego.User) *ta.Response {
        t.Helper()
        b, err := json.Marshal(user)
        require.NoError(t, err)
        return &ta.Response{Ok: true, Result: b}
}

// newTestChannel creates a TelegramChannel with a mocked bot for unit testing.
func newTestChannel(t *testing.T, caller *stubCaller) *TelegramChannel {
        return newTestChannelWithConstructor(t, caller, &stubConstructor{})
}

func newTestChannelWithConstructor(
        t *testing.T,
        caller *stubCaller,
        constructor ta.RequestConstructor,
) *TelegramChannel {
        t.Helper()

        bot, err := telego.NewBot(testToken,
                telego.WithAPICaller(caller),
                telego.WithRequestConstructor(constructor),
                telego.WithDiscardLogger(),
        )
        require.NoError(t, err)

        base := channels.NewBaseChannel("telegram", nil, nil, nil,
                channels.WithMaxMessageLength(4000),
        )
        base.SetRunning(true)

        return &TelegramChannel{
                BaseChannel: base,
                bot:         bot,
                chatIDs:     make(map[string]int64),
                bc:          &config.Channel{Type: config.ChannelTelegram, Enabled: true},
                tgCfg:       &config.TelegramSettings{},
                progress:    channels.NewToolFeedbackAnimator(nil),
        }
}

func TestSendMedia_ImageFallbacksToDocumentOnInvalidDimensions(t *testing.T) {
        constructor := &multipartRecordingConstructor{}
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        switch {
                        case strings.Contains(url, "sendPhoto"):
                                return nil, errors.New(`api: 400 "Bad Request: PHOTO_INVALID_DIMENSIONS"`)
                        case strings.Contains(url, "sendDocument"):
                                return successResponse(t), nil
                        default:
                                t.Fatalf("unexpected API call: %s", url)
                                return nil, nil
                        }
                },
        }
        ch := newTestChannelWithConstructor(t, caller, constructor)

        store := media.NewFileMediaStore()
        ch.SetMediaStore(store)

        tmpDir := t.TempDir()
        localPath := filepath.Join(tmpDir, "woodstock-en-10s.png")
        content := []byte("fake-png-content")
        require.NoError(t, os.WriteFile(localPath, content, 0o644))

        ref, err := store.Store(
                localPath,
                media.MediaMeta{Filename: "woodstock-en-10s.png", ContentType: "image/png"},
                "scope-1",
        )
        require.NoError(t, err)

        _, err = ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
                ChatID: "12345",
                Parts: []bus.MediaPart{{
                        Type:    "image",
                        Ref:     ref,
                        Caption: "caption",
                }},
        })

        require.NoError(t, err)
        require.Len(t, caller.calls, 2)
        assert.Contains(t, caller.calls[0].URL, "sendPhoto")
        assert.Contains(t, caller.calls[1].URL, "sendDocument")
        require.Len(t, constructor.calls, 2)
        assert.Equal(t, len(content), constructor.calls[0].FileSizes["photo"])
        assert.Equal(t, len(content), constructor.calls[1].FileSizes["document"])
        assert.Equal(t, "caption", constructor.calls[1].Parameters["caption"])
}

func TestSendMedia_ImageNonDimensionErrorDoesNotFallback(t *testing.T) {
        constructor := &multipartRecordingConstructor{}
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        return nil, errors.New("api: 500 \"server exploded\"")
                },
        }
        ch := newTestChannelWithConstructor(t, caller, constructor)

        store := media.NewFileMediaStore()
        ch.SetMediaStore(store)

        tmpDir := t.TempDir()
        localPath := filepath.Join(tmpDir, "image.png")
        require.NoError(t, os.WriteFile(localPath, []byte("fake-png-content"), 0o644))

        ref, err := store.Store(localPath, media.MediaMeta{Filename: "image.png", ContentType: "image/png"}, "scope-1")
        require.NoError(t, err)

        _, err = ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
                ChatID: "12345",
                Parts: []bus.MediaPart{{
                        Type: "image",
                        Ref:  ref,
                }},
        })

        require.Error(t, err)
        assert.ErrorIs(t, err, channels.ErrTemporary)
        require.Len(t, caller.calls, 1)
        assert.Contains(t, caller.calls[0].URL, "sendPhoto")
        require.Len(t, constructor.calls, 1)
        assert.NotContains(t, caller.calls[0].URL, "sendDocument")
}

func TestSend_EmptyContent(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        t.Fatal("SendMessage should not be called for empty content")
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)

        _, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  "12345",
                Content: "",
        })

        assert.NoError(t, err)
        assert.Empty(t, caller.calls, "no API calls should be made for empty content")
}

func TestSend_ShortMessage_SingleCall(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        return successResponse(t), nil
                },
        }
        ch := newTestChannel(t, caller)

        _, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  "12345",
                Content: "Hello, world!",
        })

        assert.NoError(t, err)
        assert.Len(t, caller.calls, 1, "short message should result in exactly one SendMessage call")
}

func TestSend_NonToolFeedbackDeletesTrackedProgressMessage(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        switch {
                        case strings.Contains(url, "editMessageText"):
                                return successResponseWithMessageID(t, 1), nil
                        default:
                                t.Fatalf("unexpected API call: %s", url)
                                return nil, nil
                        }
                },
        }
        ch := newTestChannel(t, caller)
        ch.RecordToolFeedbackMessage("12345", "1", "🔧 `read_file`")

        ids, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  "12345",
                Content: "final reply",
        })

        assert.NoError(t, err)
        assert.Equal(t, []string{"1"}, ids)
        require.Len(t, caller.calls, 1)
        assert.Contains(t, caller.calls[0].URL, "editMessageText")
        _, ok := ch.currentToolFeedbackMessage("12345")
        assert.False(t, ok, "tracked tool feedback should be cleared after final reply")
}

func TestSend_ToolFeedbackTrackingIsTopicScoped(t *testing.T) {
        nextMessageID := 0
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        nextMessageID++
                        return successResponseWithMessageID(t, nextMessageID), nil
                },
        }
        ch := newTestChannel(t, caller)

        _, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  "-1001234567890",
                Content: "🔧 `read_file`",
                Context: bus.InboundContext{
                        Channel: "telegram",
                        ChatID:  "-1001234567890",
                        TopicID: "42",
                        Raw: map[string]string{
                                "message_kind": "tool_feedback",
                        },
                },
        })
        require.NoError(t, err)

        _, ok := ch.currentToolFeedbackMessage("-1001234567890")
        assert.False(t, ok, "base chat should not track topic-specific tool feedback")

        msgID, ok := ch.currentToolFeedbackMessage("-1001234567890/42")
        require.True(t, ok, "topic chat should track tool feedback")
        assert.Equal(t, "1", msgID)
}

func TestSend_TopicReplyDoesNotFinalizeDifferentTopicToolFeedback(t *testing.T) {
        nextMessageID := 0
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        nextMessageID++
                        return successResponseWithMessageID(t, nextMessageID), nil
                },
        }
        ch := newTestChannel(t, caller)

        _, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  "-1001234567890",
                Content: "🔧 `read_file`",
                Context: bus.InboundContext{
                        Channel: "telegram",
                        ChatID:  "-1001234567890",
                        TopicID: "42",
                        Raw: map[string]string{
                                "message_kind": "tool_feedback",
                        },
                },
        })
        require.NoError(t, err)

        ids, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  "-1001234567890",
                Content: "final reply in another topic",
                Context: bus.InboundContext{
                        Channel: "telegram",
                        ChatID:  "-1001234567890",
                        TopicID: "43",
                },
        })
        require.NoError(t, err)
        require.Len(t, caller.calls, 2)
        assert.Equal(t, []string{"2"}, ids)
        assert.Contains(t, caller.calls[1].URL, "sendMessage")
        assert.NotContains(t, caller.calls[1].URL, "editMessageText")

        _, ok := ch.currentToolFeedbackMessage("-1001234567890/42")
        assert.True(t, ok, "tool feedback in the original topic should remain tracked")
}

func TestFinalizeTrackedToolFeedbackMessage_StopsTrackingBeforeEdit(t *testing.T) {
        ch := newTestChannel(t, &stubCaller{
                callFn: func(context.Context, string, *ta.RequestData) (*ta.Response, error) {
                        t.Fatal("unexpected API call")
                        return nil, nil
                },
        })
        ch.RecordToolFeedbackMessage("12345", "1", "🔧 `read_file`")

        msgIDs, handled := ch.finalizeTrackedToolFeedbackMessage(
                context.Background(),
                "12345",
                "final reply",
                func(_ context.Context, chatID, messageID, content string) error {
                        _, ok := ch.currentToolFeedbackMessage(chatID)
                        assert.False(t, ok, "tracked tool feedback should be stopped before edit")
                        assert.Equal(t, "12345", chatID)
                        assert.Equal(t, "1", messageID)
                        assert.Equal(t, "final reply", content)
                        return nil
                },
        )

        assert.True(t, handled)
        assert.Equal(t, []string{"1"}, msgIDs)
}

func TestSend_ToolFeedbackStaysSingleMessageAfterHTMLExpansion(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        return successResponse(t), nil
                },
        }
        ch := newTestChannel(t, caller)

        _, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  "12345",
                Content: "🔧 `read_file`\n" + strings.Repeat("<", 2000),
                Context: bus.InboundContext{
                        Channel: "telegram",
                        ChatID:  "12345",
                        Raw: map[string]string{
                                "message_kind": "tool_feedback",
                        },
                },
        })

        assert.NoError(t, err)
        assert.Len(t, caller.calls, 1, "tool feedback should stay a single Telegram message after HTML escaping")
}

func TestFitToolFeedbackForTelegram_ReservesAnimationFrame(t *testing.T) {
        content := "🔧 `read_file`\n" + strings.Repeat("a", 4096)

        fitted := fitToolFeedbackForTelegram(content, false, 4096)
        animated := strings.Replace(
                fitted,
                "`\n",
                strings.Repeat(".", channels.MaxToolFeedbackAnimationFrameLength())+"`\n",
                1,
        )

        if got := len([]rune(parseContent(animated, false))); got > 4096 {
                t.Fatalf("animated parsed length = %d, want <= 4096", got)
        }
}

func TestSend_LongMessage_SingleCall(t *testing.T) {
        // With WithMaxMessageLength(4000), the Manager pre-splits messages before
        // they reach Send(). A message at exactly 4000 chars should go through
        // as a single SendMessage call (no re-split needed since HTML expansion
        // won't exceed 4096 for plain text).
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        return successResponse(t), nil
                },
        }
        ch := newTestChannel(t, caller)

        longContent := strings.Repeat("a", 4000)

        _, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  "12345",
                Content: longContent,
        })

        assert.NoError(t, err)
        assert.Len(t, caller.calls, 1, "pre-split message within limit should result in one SendMessage call")
}

func TestSend_HTMLFallback_PerChunk(t *testing.T) {
        callCount := 0
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        callCount++
                        // Fail on odd calls (HTML attempt), succeed on even calls (plain text fallback)
                        if callCount%2 == 1 {
                                return nil, errors.New("Bad Request: can't parse entities")
                        }
                        return successResponse(t), nil
                },
        }
        ch := newTestChannel(t, caller)

        _, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  "12345",
                Content: "Hello **world**",
        })

        assert.NoError(t, err)
        // One short message → 1 HTML attempt (fail) + 1 plain text fallback (success) = 2 calls
        assert.Equal(t, 2, len(caller.calls), "should have HTML attempt + plain text fallback")
}

func TestSend_HTMLFallback_BothFail(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        return nil, errors.New("send failed")
                },
        }
        ch := newTestChannel(t, caller)

        _, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  "12345",
                Content: "Hello",
        })

        assert.Error(t, err)
        assert.True(t, errors.Is(err, channels.ErrTemporary), "error should wrap ErrTemporary")
        assert.Equal(t, 2, len(caller.calls), "should have HTML attempt + plain text attempt")
}

func TestSend_LongMessage_HTMLFallback_StopsOnError(t *testing.T) {
        // With a long message that gets split into 2 chunks, if both HTML and
        // plain text fail on the first chunk, Send should return early.
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        return nil, errors.New("send failed")
                },
        }
        ch := newTestChannel(t, caller)

        longContent := strings.Repeat("x", 4001)

        _, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  "12345",
                Content: longContent,
        })

        assert.Error(t, err)
        // Should fail on the first chunk (2 calls: HTML + fallback), never reaching the second chunk.
        assert.Equal(t, 2, len(caller.calls), "should stop after first chunk fails both HTML and plain text")
}

func TestSend_MarkdownShortButHTMLLong_MultipleCalls(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        return successResponse(t), nil
                },
        }
        ch := newTestChannel(t, caller)

        // Create markdown whose length is <= 4000 but whose HTML expansion is much longer.
        // "**a** " (6 chars) becomes "<b>a</b> " (9 chars) in HTML, so repeating it many times
        // yields HTML that exceeds Telegram's limit while markdown stays within it.
        markdownContent := strings.Repeat("**a** ", 600) // 3600 chars markdown, HTML ~5400+ chars
        assert.LessOrEqual(t, len([]rune(markdownContent)), 4000, "markdown content must not exceed chunk size")

        htmlExpanded := markdownToTelegramHTML(markdownContent)
        assert.Greater(
                t, len([]rune(htmlExpanded)), 4096,
                "HTML expansion must exceed Telegram limit for this test to be meaningful",
        )

        _, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  "12345",
                Content: markdownContent,
        })

        assert.NoError(t, err)
        assert.Greater(
                t, len(caller.calls), 1,
                "markdown-short but HTML-long message should be split into multiple SendMessage calls",
        )
}

func TestSend_HTMLOverflow_WordBoundary(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        return successResponse(t), nil
                },
        }
        ch := newTestChannel(t, caller)

        // We want to force a split near index ~2600 while keeping markdown length <= 4000.
        // Prefix of 430 bold units (6 chars each) = 2580 chars.
        // Expansion per unit is +3 chars when converted to HTML, so 2580 + 430*3 = 3870.
        prefix := strings.Repeat("**a** ", 430)
        targetWord := "TARGETWORDTHATSTAYSTOGETHER"
        // Suffix of 230 bold units (6 chars each) = 1380 chars.
        // Total markdown length: 2580 (prefix) + 27 (target word) + 1380 (suffix) = 3987 <= 4000.
        // HTML expansion adds ~3 chars per bold unit: (430 + 230)*3 = 1980 extra chars,
        // so total HTML length comfortably exceeds 4096.
        suffix := strings.Repeat(" **b**", 230)
        content := prefix + targetWord + suffix

        // Ensure the test content matches the intended boundary conditions.
        assert.LessOrEqual(t, len([]rune(content)), 4000, "markdown content must not exceed chunk size for this test")

        _, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  "123456",
                Content: content,
        })

        assert.NoError(t, err)

        foundFullWord := false
        for i, call := range caller.calls {
                var params map[string]any
                err := json.Unmarshal(call.Data.BodyRaw, &params)
                require.NoError(t, err)
                text, _ := params["text"].(string)

                hasWord := strings.Contains(text, targetWord)
                t.Logf("Chunk %d length: %d, contains target word: %v", i, len(text), hasWord)

                if hasWord {
                        foundFullWord = true
                        break
                }
        }

        assert.True(t, foundFullWord, "The target word should not be split between chunks")
}

func TestSend_NotRunning(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        t.Fatal("should not be called")
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)
        ch.SetRunning(false)

        _, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  "12345",
                Content: "Hello",
        })

        assert.ErrorIs(t, err, channels.ErrNotRunning)
        assert.Empty(t, caller.calls)
}

func TestSend_InvalidChatID(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        t.Fatal("should not be called")
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)

        _, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  "not-a-number",
                Content: "Hello",
        })

        assert.Error(t, err)
        assert.True(t, errors.Is(err, channels.ErrSendFailed), "error should wrap ErrSendFailed")
        assert.Empty(t, caller.calls)
}

func TestParseTelegramChatID_Plain(t *testing.T) {
        cid, tid, err := parseTelegramChatID("12345")
        assert.NoError(t, err)
        assert.Equal(t, int64(12345), cid)
        assert.Equal(t, 0, tid)
}

func TestParseTelegramChatID_NegativeGroup(t *testing.T) {
        cid, tid, err := parseTelegramChatID("-1001234567890")
        assert.NoError(t, err)
        assert.Equal(t, int64(-1001234567890), cid)
        assert.Equal(t, 0, tid)
}

func TestParseTelegramChatID_WithThreadID(t *testing.T) {
        cid, tid, err := parseTelegramChatID("-1001234567890/42")
        assert.NoError(t, err)
        assert.Equal(t, int64(-1001234567890), cid)
        assert.Equal(t, 42, tid)
}

func TestParseTelegramChatID_GeneralTopic(t *testing.T) {
        cid, tid, err := parseTelegramChatID("-100123/1")
        assert.NoError(t, err)
        assert.Equal(t, int64(-100123), cid)
        assert.Equal(t, 1, tid)
}

func TestParseTelegramChatID_Invalid(t *testing.T) {
        _, _, err := parseTelegramChatID("not-a-number")
        assert.Error(t, err)
}

func TestParseTelegramChatID_InvalidThreadID(t *testing.T) {
        _, _, err := parseTelegramChatID("-100123/not-a-thread")
        assert.Error(t, err)
        assert.Contains(t, err.Error(), "invalid thread ID")
}

func TestSend_WithForumThreadID(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        return successResponse(t), nil
                },
        }
        ch := newTestChannel(t, caller)

        _, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  "-1001234567890/42",
                Content: "Hello from topic",
        })

        assert.NoError(t, err)
        assert.Len(t, caller.calls, 1)
}

func TestSend_UsesContextTopicIDWhenChatIDDoesNotIncludeThread(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        return successResponse(t), nil
                },
        }
        ch := newTestChannel(t, caller)

        _, err := ch.Send(context.Background(), bus.OutboundMessage{
                ChatID:  "-1001234567890",
                Content: "Hello from topic context",
                Context: bus.InboundContext{
                        Channel: "telegram",
                        ChatID:  "-1001234567890",
                        TopicID: "42",
                },
        })

        require.NoError(t, err)
        require.Len(t, caller.calls, 1)

        var params struct {
                ChatID          int64  `json:"chat_id"`
                MessageThreadID int    `json:"message_thread_id"`
                Text            string `json:"text"`
        }
        require.NoError(t, json.Unmarshal(caller.calls[0].Data.BodyRaw, &params))
        assert.Equal(t, int64(-1001234567890), params.ChatID)
        assert.Equal(t, 42, params.MessageThreadID)
        assert.Equal(t, "Hello from topic context", params.Text)
}

func TestBeginStream_UpdateUsesForumThreadID(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        return &ta.Response{Ok: true, Result: []byte("true")}, nil
                },
        }
        ch := newTestChannel(t, caller)
        ch.tgCfg.Streaming.Enabled = true

        streamer, err := ch.BeginStream(context.Background(), "-1001234567890/42")
        require.NoError(t, err)
        require.NoError(t, streamer.Update(context.Background(), "partial"))
        require.Len(t, caller.calls, 1)
        assert.Contains(t, caller.calls[0].URL, "sendMessageDraft")

        var params struct {
                ChatID          int64  `json:"chat_id"`
                MessageThreadID int    `json:"message_thread_id"`
                Text            string `json:"text"`
        }
        require.NoError(t, json.Unmarshal(caller.calls[0].Data.BodyRaw, &params))
        assert.Equal(t, int64(-1001234567890), params.ChatID)
        assert.Equal(t, 42, params.MessageThreadID)
        assert.Equal(t, "partial", params.Text)
}

func TestBeginStream_FinalizeUsesForumThreadID(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        return successResponse(t), nil
                },
        }
        ch := newTestChannel(t, caller)
        ch.tgCfg.Streaming.Enabled = true

        streamer, err := ch.BeginStream(context.Background(), "-1001234567890/42")
        require.NoError(t, err)
        require.NoError(t, streamer.Finalize(context.Background(), "final"))
        require.Len(t, caller.calls, 1)
        assert.Contains(t, caller.calls[0].URL, "sendMessage")

        var params struct {
                ChatID          int64  `json:"chat_id"`
                MessageThreadID int    `json:"message_thread_id"`
                Text            string `json:"text"`
        }
        require.NoError(t, json.Unmarshal(caller.calls[0].Data.BodyRaw, &params))
        assert.Equal(t, int64(-1001234567890), params.ChatID)
        assert.Equal(t, 42, params.MessageThreadID)
        assert.Equal(t, "final", params.Text)
}

func TestHandleMessage_ForumTopic_SetsMetadata(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                Text:            "hello from topic",
                MessageID:       10,
                MessageThreadID: 42,
                Chat: telego.Chat{
                        ID:      -1001234567890,
                        Type:    "supergroup",
                        IsForum: true,
                },
                From: &telego.User{
                        ID:        7,
                        FirstName: "Alice",
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok, "expected inbound message")

        // ChatID remains the parent chat; TopicID isolates the sub-conversation.
        assert.Equal(t, "-1001234567890", inbound.ChatID)
        assert.Equal(t, "group", inbound.Context.ChatType)
        assert.Equal(t, "42", inbound.Context.TopicID)
}

func TestHandleMessage_NoForum_NoThreadMetadata(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                Text:      "regular group message",
                MessageID: 11,
                Chat: telego.Chat{
                        ID:   -100999,
                        Type: "group",
                },
                From: &telego.User{
                        ID:        8,
                        FirstName: "Bob",
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)

        // Plain chatID without thread suffix
        assert.Equal(t, "-100999", inbound.ChatID)

        assert.Equal(t, "group", inbound.Context.ChatType)
        assert.Empty(t, inbound.Context.TopicID)
}

func TestHandleMessage_ReplyThread_NonForum_NoIsolation(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        // In regular groups, reply threads set MessageThreadID to the original
        // message ID. This should NOT trigger per-thread session isolation.
        msg := &telego.Message{
                Text:            "reply in thread",
                MessageID:       20,
                MessageThreadID: 15,
                Chat: telego.Chat{
                        ID:      -100999,
                        Type:    "supergroup",
                        IsForum: false,
                },
                From: &telego.User{
                        ID:        9,
                        FirstName: "Carol",
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)

        // chatID should NOT include thread suffix for non-forum groups
        assert.Equal(t, "-100999", inbound.ChatID)

        assert.Equal(t, "group", inbound.Context.ChatType)
        assert.Empty(t, inbound.Context.TopicID)
}

func assertHandleMessageQuotedUserReply(
        t *testing.T,
        chatID int64,
        messageID int,
        userID int64,
        userName string,
        userText string,
        replyMessageID int,
        replyText string,
        replyCaption string,
        replyAuthorID int64,
        replyAuthorName string,
        expectedContent string,
) {
        t.Helper()

        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                Text:      userText,
                MessageID: messageID,
                Chat: telego.Chat{
                        ID:   chatID,
                        Type: "private",
                },
                From: &telego.User{
                        ID:        userID,
                        FirstName: userName,
                },
                ReplyToMessage: &telego.Message{
                        MessageID: replyMessageID,
                        Text:      replyText,
                        Caption:   replyCaption,
                        From: &telego.User{
                                ID:        replyAuthorID,
                                FirstName: replyAuthorName,
                        },
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)
        assert.Equal(t, strconv.Itoa(replyMessageID), inbound.Context.ReplyToMessageID)
        assert.Equal(t, expectedContent, inbound.Content)
}

func TestHandleMessage_ReplyToMessage_PrependsQuotedTextAndMetadata(t *testing.T) {
        assertHandleMessageQuotedUserReply(
                t,
                456,
                21,
                11,
                "Alice",
                "follow up",
                99,
                "old context",
                "",
                12,
                "Bob",
                "[quoted user message from Bob]: old context\n\nfollow up",
        )
}

func TestHandleMessage_ReplyToMessage_UsesCaptionWhenQuotedTextMissing(t *testing.T) {
        assertHandleMessageQuotedUserReply(
                t,
                789,
                22,
                13,
                "Carol",
                "answer this",
                100,
                "",
                "caption context",
                14,
                "Dave",
                "[quoted user message from Dave]: caption context\n\nanswer this",
        )
}

func TestHandleMessage_ReplyToOwnBotMessage_UsesAssistantRole(t *testing.T) {
        messageBus := bus.NewMessageBus()
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "getMe") {
                                return successUserResponse(t, &telego.User{
                                        ID:        42,
                                        IsBot:     true,
                                        FirstName: "Pico",
                                        Username:  "afjcjsbx_picoclaw_bot",
                                }), nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)
        ch.BaseChannel = channels.NewBaseChannel("telegram", nil, messageBus, nil)
        ch.ctx = context.Background()

        msg := &telego.Message{
                Text:      "ti ricordi questo file?",
                MessageID: 23,
                Chat: telego.Chat{
                        ID:   999,
                        Type: "private",
                },
                From: &telego.User{
                        ID:        15,
                        FirstName: "Eve",
                },
                ReplyToMessage: &telego.Message{
                        MessageID: 101,
                        Text:      "Fatto! Ho creato il file notizie_2026_03_28.md",
                        From: &telego.User{
                                ID:        42,
                                IsBot:     true,
                                FirstName: "Pico",
                                Username:  "afjcjsbx_picoclaw_bot",
                        },
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)
        assert.Equal(t, "101", inbound.Context.ReplyToMessageID)
        assert.Equal(
                t,
                "[quoted assistant message from afjcjsbx_picoclaw_bot]: Fatto! Ho creato il file notizie_2026_03_28.md\n\nti ricordi questo file?",
                inbound.Content,
        )
}

func TestTelegramQuotedContent_IncludesVoiceMarkerAlongsideCaption(t *testing.T) {
        msg := &telego.Message{
                Caption: "listen to this",
                Voice: &telego.Voice{
                        FileID: "voice-file",
                },
        }

        assert.Equal(t, "listen to this\n[voice]", telegramQuotedContent(msg))
}

func TestQuotedTelegramMediaRefs_ResolvesQuotedAudioInOrder(t *testing.T) {
        msg := &telego.Message{
                Voice: &telego.Voice{FileID: "voice-file"},
                Audio: &telego.Audio{FileID: "audio-file"},
        }

        var calls []string
        refs := quotedTelegramMediaRefs(msg, func(fileID, ext, filename string) string {
                calls = append(calls, fileID+"|"+ext+"|"+filename)
                return "ref://" + filename
        })

        assert.Equal(
                t,
                []string{"voice-file|.ogg|voice.ogg", "audio-file|.mp3|audio.mp3"},
                calls,
        )
        assert.Equal(t, []string{"ref://voice.ogg", "ref://audio.mp3"}, refs)
}

func TestHandleMessage_EmptyContent_Ignored(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        // Service message with no text/caption/media (like ForumTopicCreated)
        msg := &telego.Message{
                MessageID: 123,
                Chat: telego.Chat{
                        ID:   456,
                        Type: "group",
                },
                From: &telego.User{
                        ID:        789,
                        FirstName: "User",
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        // Should NOT publish to message bus
        select {
        case <-messageBus.InboundChan():
                t.Fatal("Empty message should not be published to message bus")
        default:
        }
}

// --- Phase 1 Tests: Critical Infrastructure ---

func TestHandleMessage_ReplyToMessage_PopulatesReplyToSenderID(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                Text:      "replying to you",
                MessageID: 50,
                Chat: telego.Chat{
                        ID:   100,
                        Type: "private",
                },
                From: &telego.User{
                        ID:        11,
                        FirstName: "Alice",
                },
                ReplyToMessage: &telego.Message{
                        MessageID: 40,
                        Text:      "original message",
                        From: &telego.User{
                                ID:        22,
                                FirstName: "Bob",
                        },
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok, "expected inbound message")

        assert.Equal(t, "40", inbound.Context.ReplyToMessageID)
        assert.Equal(t, "22", inbound.Context.ReplyToSenderID, "ReplyToSenderID should be populated from ReplyToMessage.From.ID")
}

func TestHandleMessage_ReplyToMessage_NilFrom_SkipsReplyToSenderID(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                Text:      "replying to channel post",
                MessageID: 51,
                Chat: telego.Chat{
                        ID:   -100123,
                        Type: "supergroup",
                },
                From: &telego.User{
                        ID:        11,
                        FirstName: "Alice",
                },
                ReplyToMessage: &telego.Message{
                        MessageID: 40,
                        Text:      "channel message with no From",
                        // From is nil — e.g. a channel post or anonymous admin message
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)

        assert.Equal(t, "40", inbound.Context.ReplyToMessageID)
        assert.Empty(t, inbound.Context.ReplyToSenderID, "ReplyToSenderID should be empty when ReplyToMessage.From is nil")
}

func TestHandleMessage_NoReply_SenderIDEmpty(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                Text:      "standalone message",
                MessageID: 52,
                Chat: telego.Chat{
                        ID:   100,
                        Type: "private",
                },
                From: &telego.User{
                        ID:        11,
                        FirstName: "Alice",
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)

        assert.Empty(t, inbound.Context.ReplyToMessageID)
        assert.Empty(t, inbound.Context.ReplyToSenderID)
}

// --- Phase 2 Tests: ReactionCapable ---

func TestReactToMessage_DefaultEyesEmoji(t *testing.T) {
        var capturedURL string
        var capturedBody []byte

        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        capturedURL = url
                        capturedBody = data.BodyRaw
                        return &ta.Response{Ok: true, Result: json.RawMessage(`true`)}, nil
                },
        }
        ch := newTestChannel(t, caller)

        undo, err := ch.ReactToMessage(context.Background(), "123456", "789")
        require.NoError(t, err)
        require.NotNil(t, undo)

        assert.Contains(t, capturedURL, "setMessageReaction")

        // Verify the reaction payload contains the eyes emoji
        var payload map[string]any
        require.NoError(t, json.Unmarshal(capturedBody, &payload))

        reactions, ok := payload["reaction"].([]any)
        require.True(t, ok, "reaction field should be an array")
        require.Len(t, reactions, 1)

        reaction, ok := reactions[0].(map[string]any)
        require.True(t, ok)
        assert.Equal(t, "emoji", reaction["type"])
        assert.Equal(t, "\U0001F440", reaction["emoji"]) // 👀
}

func TestReactToMessage_CustomEmoji(t *testing.T) {
        var capturedBody []byte

        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        capturedBody = data.BodyRaw
                        return &ta.Response{Ok: true, Result: json.RawMessage(`true`)}, nil
                },
        }
        ch := newTestChannel(t, caller)
        ch.tgCfg.ReactionEmoji = "👍"

        _, err := ch.ReactToMessage(context.Background(), "123456", "789")
        require.NoError(t, err)

        var payload map[string]any
        require.NoError(t, json.Unmarshal(capturedBody, &payload))

        reactions, ok := payload["reaction"].([]any)
        require.True(t, ok)
        reaction := reactions[0].(map[string]any)
        assert.Equal(t, "👍", reaction["emoji"])
}

func TestReactToMessage_UndoRemovesReaction(t *testing.T) {
        var urls []string
        var bodies [][]byte

        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        urls = append(urls, url)
                        bodies = append(bodies, data.BodyRaw)
                        return &ta.Response{Ok: true, Result: json.RawMessage(`true`)}, nil
                },
        }
        ch := newTestChannel(t, caller)

        undo, err := ch.ReactToMessage(context.Background(), "123456", "789")
        require.NoError(t, err)
        require.NotNil(t, undo)

        // Only one API call so far (add reaction)
        assert.Len(t, urls, 1)

        // Call undo — should send another setMessageReaction with empty reaction array
        undo()

        assert.Len(t, urls, 2)
        assert.Contains(t, urls[1], "setMessageReaction")

        var undoPayload map[string]any
        require.NoError(t, json.Unmarshal(bodies[1], &undoPayload))

        // When Reaction is an empty slice, telego's omitempty tag omits the field.
        // Telegram API treats missing "reaction" as removing all reactions.
        reactions, exists := undoPayload["reaction"]
        if exists {
                assert.Empty(t, reactions, "undo should send empty reaction array to remove the reaction")
        }
        // If the field is absent, that's also valid — it clears all reactions.
}

func TestReactToMessage_ApiError_GracefulDegradation(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        return &ta.Response{
                                Ok: false,
                                Error: &ta.Error{
                                        ErrorCode:   400,
                                        Description: "Bad Request: message can't be reacted to",
                                },
                        }, nil
                },
        }
        ch := newTestChannel(t, caller)

        // Should NOT return an error — graceful degradation
        undo, err := ch.ReactToMessage(context.Background(), "123456", "789")
        assert.NoError(t, err)
        assert.NotNil(t, undo)

        // Undo should be safe to call even when the original reaction failed
        undo() // should not panic
}

func TestReactToMessage_InvalidChatID(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        t.Fatal("should not make API call for invalid chatID")
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)

        _, err := ch.ReactToMessage(context.Background(), "not-a-number", "789")
        assert.Error(t, err)
}

func TestReactToMessage_InvalidMessageID(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        t.Fatal("should not make API call for invalid messageID")
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)

        _, err := ch.ReactToMessage(context.Background(), "123456", "not-a-number")
        assert.Error(t, err)
}

func TestTelegramChannel_ImplementsReactionCapable(t *testing.T) {
        // Compile-time interface assertion is in telegram.go;
        // This runtime test confirms the wiring is correct.
        var _ channels.ReactionCapable = (*TelegramChannel)(nil)
}

// --- Phase 3 Tests: Inbound Media Completeness ---

func TestHandleMessage_Sticker_ProducesContentAnnotation(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                MessageID: 60,
                Chat: telego.Chat{
                        ID:   100,
                        Type: "private",
                },
                From: &telego.User{
                        ID:        11,
                        FirstName: "Alice",
                },
                Sticker: &telego.Sticker{
                        FileID:     "sticker_abc",
                        Emoji:      "🎉",
                        IsAnimated: false,
                        IsVideo:    false,
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok, "sticker message should produce inbound content")

        assert.Contains(t, inbound.Content, "[sticker: 🎉]")
        // Static sticker should NOT add media (emoji description is sufficient)
        assert.Empty(t, inbound.Media)
}

func TestHandleMessage_Sticker_Animated_DownloadsMedia(t *testing.T) {
        // Use a mock bot that returns an empty file for getFile requests,
        // so the download attempt doesn't panic but also doesn't produce media.
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "getFile") {
                                file := &telego.File{FileID: "anim_sticker_abc"}
                                b, _ := json.Marshal(file)
                                return &ta.Response{Ok: true, Result: b}, nil
                        }
                        return &ta.Response{Ok: true, Result: json.RawMessage(`true`)}, nil
                },
        }
        messageBus := bus.NewMessageBus()
        ch := newTestChannel(t, caller)
        ch.BaseChannel = channels.NewBaseChannel("telegram", nil, messageBus, nil)
        ch.ctx = context.Background()

        msg := &telego.Message{
                MessageID: 61,
                Chat: telego.Chat{
                        ID:   100,
                        Type: "private",
                },
                From: &telego.User{
                        ID:        11,
                        FirstName: "Alice",
                },
                Sticker: &telego.Sticker{
                        FileID:     "anim_sticker_abc",
                        Emoji:      "👍",
                        IsAnimated: true,
                        IsVideo:    false,
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)

        assert.Contains(t, inbound.Content, "[sticker: 👍]")
        // Animated sticker attempts a download; since the mock returns no FilePath,
        // download fails gracefully and no media ref is added — content is still present.
}

func TestHandleMessage_Sticker_NoEmoji_UsesFallback(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                MessageID: 62,
                Chat: telego.Chat{
                        ID:   100,
                        Type: "private",
                },
                From: &telego.User{
                        ID:        11,
                        FirstName: "Alice",
                },
                Sticker: &telego.Sticker{
                        FileID: "sticker_noemoji",
                        Emoji:  "", // no emoji
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)

        assert.Contains(t, inbound.Content, "[sticker: ?]")
}

func TestHandleMessage_Contact_ProducesContentAnnotation(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                MessageID: 63,
                Chat: telego.Chat{
                        ID:   100,
                        Type: "private",
                },
                From: &telego.User{
                        ID:        11,
                        FirstName: "Alice",
                },
                Contact: &telego.Contact{
                        PhoneNumber: "+1234567890",
                        FirstName:   "John",
                        LastName:    "Doe",
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)

        assert.Contains(t, inbound.Content, "[contact: John Doe, +1234567890]")
}

func TestHandleMessage_Contact_NoLastName(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                MessageID: 64,
                Chat: telego.Chat{
                        ID:   100,
                        Type: "private",
                },
                From: &telego.User{
                        ID:        11,
                        FirstName: "Alice",
                },
                Contact: &telego.Contact{
                        PhoneNumber: "+9876543210",
                        FirstName:   "Jane",
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)

        assert.Contains(t, inbound.Content, "[contact: Jane, +9876543210]")
}

func TestHandleMessage_Location_ProducesContentAnnotation(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                MessageID: 65,
                Chat: telego.Chat{
                        ID:   100,
                        Type: "private",
                },
                From: &telego.User{
                        ID:        11,
                        FirstName: "Alice",
                },
                Location: &telego.Location{
                        Latitude:  13.756331,
                        Longitude: 100.501765,
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)

        assert.Contains(t, inbound.Content, "[location:")
        assert.Contains(t, inbound.Content, "13.756331")
        assert.Contains(t, inbound.Content, "100.501765")
}

func TestHandleMessage_Venue_ProducesContentAnnotation(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                MessageID: 66,
                Chat: telego.Chat{
                        ID:   100,
                        Type: "private",
                },
                From: &telego.User{
                        ID:        11,
                        FirstName: "Alice",
                },
                Venue: &telego.Venue{
                        Location: telego.Location{Latitude: 40.7128, Longitude: -74.0060},
                        Title:    "Central Park",
                        Address:  "New York, NY",
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)

        assert.Contains(t, inbound.Content, "[venue: Central Park, New York, NY]")
}

func TestHandleMessage_Poll_ProducesContentAnnotation(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                MessageID: 67,
                Chat: telego.Chat{
                        ID:   -100999,
                        Type: "supergroup",
                },
                From: &telego.User{
                        ID:        11,
                        FirstName: "Alice",
                },
                Poll: &telego.Poll{
                        ID:       "poll1",
                        Question: "What time?",
                        Options: []telego.PollOption{
                                {Text: "9am"},
                                {Text: "10am"},
                                {Text: "11am"},
                        },
                        Type: "regular",
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)

        assert.Contains(t, inbound.Content, "[poll: What time? (9am / 10am / 11am)]")
}

func TestHandleMessage_Poll_QuizType(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                MessageID: 68,
                Chat: telego.Chat{
                        ID:   -100999,
                        Type: "supergroup",
                },
                From: &telego.User{
                        ID:        11,
                        FirstName: "Alice",
                },
                Poll: &telego.Poll{
                        ID:       "quiz1",
                        Question: "Capital of France?",
                        Options: []telego.PollOption{
                                {Text: "London"},
                                {Text: "Paris"},
                        },
                        Type: "quiz",
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)

        assert.Contains(t, inbound.Content, "[quiz: Capital of France? (London / Paris)]")
}

func TestHandleMessage_Dice_ProducesContentAnnotation(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                MessageID: 69,
                Chat: telego.Chat{
                        ID:   100,
                        Type: "private",
                },
                From: &telego.User{
                        ID:        11,
                        FirstName: "Alice",
                },
                Dice: &telego.Dice{
                        Emoji: "🎲",
                        Value: 5,
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)

        assert.Contains(t, inbound.Content, "[dice: 🎲 5]")
}

func TestHandleMessage_TextAndSticker_BothAnnotations(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                Text:      "nice one!",
                MessageID: 70,
                Chat: telego.Chat{
                        ID:   100,
                        Type: "private",
                },
                From: &telego.User{
                        ID:        11,
                        FirstName: "Alice",
                },
                Sticker: &telego.Sticker{
                        FileID: "sticker_xyz",
                        Emoji:  "🔥",
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)

        assert.Contains(t, inbound.Content, "nice one!")
        assert.Contains(t, inbound.Content, "[sticker: 🔥]")
}

// --- Phase 3: telegramQuotedContent tests ---

func TestTelegramQuotedContent_IncludesSticker(t *testing.T) {
        msg := &telego.Message{
                Sticker: &telego.Sticker{Emoji: "😎"},
        }
        content := telegramQuotedContent(msg)
        assert.Contains(t, content, "[sticker: 😎]")
}

func TestTelegramQuotedContent_IncludesVideo(t *testing.T) {
        msg := &telego.Message{
                Video: &telego.Video{FileID: "vid1"},
        }
        content := telegramQuotedContent(msg)
        assert.Contains(t, content, "[video]")
}

func TestTelegramQuotedContent_IncludesVideoNote(t *testing.T) {
        msg := &telego.Message{
                VideoNote: &telego.VideoNote{FileID: "vn1"},
        }
        content := telegramQuotedContent(msg)
        assert.Contains(t, content, "[video note]")
}

func TestTelegramQuotedContent_IncludesAnimation(t *testing.T) {
        msg := &telego.Message{
                Animation: &telego.Animation{FileID: "anim1"},
        }
        content := telegramQuotedContent(msg)
        assert.Contains(t, content, "[animation]")
}

func TestTelegramQuotedContent_IncludesContact(t *testing.T) {
        msg := &telego.Message{
                Contact: &telego.Contact{FirstName: "Jane", LastName: "Smith", PhoneNumber: "+111"},
        }
        content := telegramQuotedContent(msg)
        assert.Contains(t, content, "[contact: Jane Smith, +111]")
}

func TestTelegramQuotedContent_IncludesLocation(t *testing.T) {
        msg := &telego.Message{
                Location: &telego.Location{Latitude: 1.234, Longitude: 5.678},
        }
        content := telegramQuotedContent(msg)
        assert.Contains(t, content, "[location:")
}

func TestTelegramQuotedContent_IncludesVenue(t *testing.T) {
        msg := &telego.Message{
                Venue: &telego.Venue{Title: "Cafe", Address: "123 St"},
        }
        content := telegramQuotedContent(msg)
        assert.Contains(t, content, "[venue: Cafe, 123 St]")
}

func TestTelegramQuotedContent_IncludesPoll(t *testing.T) {
        msg := &telego.Message{
                Poll: &telego.Poll{
                        Question: "Pick one",
                        Options:  []telego.PollOption{{Text: "A"}, {Text: "B"}},
                        Type:     "regular",
                },
        }
        content := telegramQuotedContent(msg)
        assert.Contains(t, content, "[poll: Pick one (A / B)]")
}

func TestTelegramQuotedContent_IncludesDice(t *testing.T) {
        msg := &telego.Message{
                Dice: &telego.Dice{Emoji: "🎯", Value: 3},
        }
        content := telegramQuotedContent(msg)
        assert.Contains(t, content, "[dice: 🎯 3]")
}

// --- Phase 3: quotedTelegramMediaRefs tests ---

func TestQuotedTelegramMediaRefs_IncludesPhoto(t *testing.T) {
        msg := &telego.Message{
                Photo: []telego.PhotoSize{
                        {FileID: "small", Width: 90, Height: 90},
                        {FileID: "large", Width: 800, Height: 600},
                },
        }
        var resolved []string
        refs := quotedTelegramMediaRefs(msg, func(fileID, ext, filename string) string {
                resolved = append(resolved, fileID)
                return "ref:" + fileID
        })
        assert.Contains(t, resolved, "large", "should download largest photo size")
        assert.Equal(t, "ref:large", refs[0])
}

func TestQuotedTelegramMediaRefs_IncludesDocument(t *testing.T) {
        msg := &telego.Message{
                Document: &telego.Document{FileID: "doc123", FileName: "report.pdf"},
        }
        var resolved []string
        refs := quotedTelegramMediaRefs(msg, func(fileID, ext, filename string) string {
                resolved = append(resolved, fileID)
                return "ref:" + fileID
        })
        assert.Contains(t, resolved, "doc123")
        assert.Equal(t, "ref:doc123", refs[0])
}

func TestQuotedTelegramMediaRefs_IncludesVideo(t *testing.T) {
        msg := &telego.Message{
                Video: &telego.Video{FileID: "vid456", FileName: "clip.mp4"},
        }
        var resolved []string
        refs := quotedTelegramMediaRefs(msg, func(fileID, ext, filename string) string {
                resolved = append(resolved, fileID)
                return "ref:" + fileID
        })
        assert.Contains(t, resolved, "vid456")
        assert.Equal(t, "ref:vid456", refs[0])
}

func TestQuotedTelegramMediaRefs_AllTypesInOrder(t *testing.T) {
        msg := &telego.Message{
                Photo: []telego.PhotoSize{
                        {FileID: "photo1", Width: 800, Height: 600},
                },
                Voice:    &telego.Voice{FileID: "voice1"},
                Audio:    &telego.Audio{FileID: "audio1"},
                Document: &telego.Document{FileID: "doc1"},
                Video:    &telego.Video{FileID: "vid1"},
        }
        var order []string
        refs := quotedTelegramMediaRefs(msg, func(fileID, ext, filename string) string {
                order = append(order, fileID)
                return "ref:" + fileID
        })
        require.Len(t, refs, 5)
        // Order: photo, voice, audio, document, video
        assert.Equal(t, []string{"photo1", "voice1", "audio1", "doc1", "vid1"}, order)
}

// ---------------------------------------------------------------------------
// Phase 4: Callback Query tests
// ---------------------------------------------------------------------------

// helperCallbackMessage creates a telego.Message that satisfies
// MaybeInaccessibleMessage for use in callback query tests.
func helperCallbackMessage(chatID int64, chatType string, messageID int, isForum bool, threadID int) *telego.Message {
        msg := &telego.Message{
                MessageID: messageID,
                Chat: telego.Chat{
                        ID:      chatID,
                        Type:    chatType,
                        IsForum: isForum,
                },
        }
        if threadID != 0 {
                msg.MessageThreadID = threadID
        }
        return msg
}

// successTrueResponse returns a ta.Response with `true` result (used by answerCallbackQuery).
func successTrueResponse(t *testing.T) *ta.Response {
        t.Helper()
        return &ta.Response{Ok: true, Result: []byte("true")}
}

func TestHandleCallbackQuery_PrivateChat_PublishesInbound(t *testing.T) {
        messageBus := bus.NewMessageBus()
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "answerCallbackQuery") {
                                return successTrueResponse(t), nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)
        ch.BaseChannel = channels.NewBaseChannel("telegram", nil, messageBus, nil)
        ch.ctx = context.Background()

        query := &telego.CallbackQuery{
                ID:           "cb123",
                From:         telego.User{ID: 42, FirstName: "Alice", Username: "alice42"},
                ChatInstance: "123456789",
                Data:         "confirm_yes",
        }
        // Set the Message field — use a regular message (accessible).
        callbackMsg := helperCallbackMessage(999, "private", 50, false, 0)
        query.Message = callbackMsg

        err := ch.handleCallbackQuery(context.Background(), query)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok, "expected inbound message on bus")

        assert.Equal(t, "[callback: confirm_yes]", inbound.Content)
        assert.Equal(t, "callback_query", inbound.Context.Raw["message_kind"])
        assert.Equal(t, "cb123", inbound.Context.Raw["callback_query_id"])
        assert.Equal(t, "confirm_yes", inbound.Context.Raw["callback_data"])
        assert.Equal(t, "123456789", inbound.Context.Raw["chat_instance"])
        assert.Equal(t, "999", inbound.Context.ChatID)
        assert.Equal(t, "50", inbound.Context.MessageID)
        assert.Equal(t, "direct", inbound.Context.ChatType)
        assert.True(t, inbound.Context.Mentioned, "callback queries should always be treated as mentioned")
}

func TestHandleCallbackQuery_AnswerCallbackQuery_Called(t *testing.T) {
        messageBus := bus.NewMessageBus()
        var answerCalled bool
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "answerCallbackQuery") {
                                answerCalled = true
                                return successTrueResponse(t), nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)
        ch.BaseChannel = channels.NewBaseChannel("telegram", nil, messageBus, nil)
        ch.ctx = context.Background()

        query := &telego.CallbackQuery{
                ID:   "cb_ack",
                From: telego.User{ID: 10, FirstName: "Bob"},
                Data: "btn_click",
        }
        query.Message = helperCallbackMessage(100, "private", 1, false, 0)

        err := ch.handleCallbackQuery(context.Background(), query)
        require.NoError(t, err)
        assert.True(t, answerCalled, "AnswerCallbackQuery should have been called")
}

func TestHandleCallbackQuery_AnswerCallbackQuery_Failure_NonFatal(t *testing.T) {
        messageBus := bus.NewMessageBus()
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "answerCallbackQuery") {
                                return nil, errors.New("network timeout")
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)
        ch.BaseChannel = channels.NewBaseChannel("telegram", nil, messageBus, nil)
        ch.ctx = context.Background()

        query := &telego.CallbackQuery{
                ID:   "cb_fail",
                From: telego.User{ID: 10, FirstName: "Bob"},
                Data: "btn_ok",
        }
        query.Message = helperCallbackMessage(100, "private", 1, false, 0)

        // Should NOT return an error — acknowledgment failure is non-fatal.
        err := ch.handleCallbackQuery(context.Background(), query)
        require.NoError(t, err)

        // The inbound message should still be published.
        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok, "inbound message should still be published despite ack failure")
        assert.Equal(t, "[callback: btn_ok]", inbound.Content)
}

func TestHandleCallbackQuery_GroupChat_SetsGroupChatType(t *testing.T) {
        messageBus := bus.NewMessageBus()
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "answerCallbackQuery") {
                                return successTrueResponse(t), nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)
        ch.BaseChannel = channels.NewBaseChannel("telegram", nil, messageBus, nil)
        ch.ctx = context.Background()

        query := &telego.CallbackQuery{
                ID:   "cb_group",
                From: telego.User{ID: 20, FirstName: "Carol"},
                Data: "vote_yes",
        }
        query.Message = helperCallbackMessage(-1001234567890, "supergroup", 200, false, 0)

        err := ch.handleCallbackQuery(context.Background(), query)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)
        assert.Equal(t, "group", inbound.Context.ChatType)
        assert.Equal(t, "-1001234567890", inbound.Context.ChatID)
}

func TestHandleCallbackQuery_ForumTopic_CompositeChatID(t *testing.T) {
        messageBus := bus.NewMessageBus()
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "answerCallbackQuery") {
                                return successTrueResponse(t), nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)
        ch.BaseChannel = channels.NewBaseChannel("telegram", nil, messageBus, nil)
        ch.ctx = context.Background()

        query := &telego.CallbackQuery{
                ID:   "cb_forum",
                From: telego.User{ID: 30, FirstName: "Dave"},
                Data: "topic_action",
        }
        query.Message = helperCallbackMessage(-100999, "supergroup", 300, true, 42)

        err := ch.handleCallbackQuery(context.Background(), query)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)
        assert.Equal(t, "-100999/42", inbound.Context.ChatID, "forum callback should use composite chat ID")
        assert.Equal(t, "42", inbound.Context.TopicID)
}

func TestHandleCallbackQuery_NilQuery_NoOp(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        err := ch.handleCallbackQuery(context.Background(), nil)
        require.NoError(t, err)

        // No message should be published.
        select {
        case <-messageBus.InboundChan():
                t.Fatal("no inbound message should be published for nil query")
        default:
        }
}

func TestHandleCallbackQuery_AllowlistRejected_NoInboundMessage(t *testing.T) {
        messageBus := bus.NewMessageBus()
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "answerCallbackQuery") {
                                return successTrueResponse(t), nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)
        ch.BaseChannel = channels.NewBaseChannel("telegram", nil, messageBus, []string{"99999"})
        ch.ctx = context.Background()

        query := &telego.CallbackQuery{
                ID:   "cb_rejected",
                From: telego.User{ID: 42, FirstName: "Blocked"},
                Data: "btn",
        }
        query.Message = helperCallbackMessage(100, "private", 1, false, 0)

        err := ch.handleCallbackQuery(context.Background(), query)
        require.NoError(t, err)

        // Ack should still be called even for rejected users.
        assert.Len(t, caller.calls, 1, "AnswerCallbackQuery should still be called")
        assert.Contains(t, caller.calls[0].URL, "answerCallbackQuery")

        // No inbound message should be published.
        select {
        case <-messageBus.InboundChan():
                t.Fatal("no inbound message for allowlisted-rejected user")
        default:
        }
}

func TestHandleCallbackQuery_NoMessageAndNoInlineMessage_SkipsPublish(t *testing.T) {
        messageBus := bus.NewMessageBus()
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "answerCallbackQuery") {
                                return successTrueResponse(t), nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)
        ch.BaseChannel = channels.NewBaseChannel("telegram", nil, messageBus, nil)
        ch.ctx = context.Background()

        query := &telego.CallbackQuery{
                ID:           "cb_nomsg",
                From:         telego.User{ID: 10, FirstName: "Eve"},
                ChatInstance: "abc",
                Data:         "orphan_btn",
                // Message is nil and InlineMessageID is empty → can't determine chat.
        }

        err := ch.handleCallbackQuery(context.Background(), query)
        require.NoError(t, err)

        // Ack should still be called.
        assert.Len(t, caller.calls, 1)

        // No inbound message — can't route without a chat.
        select {
        case <-messageBus.InboundChan():
                t.Fatal("no inbound message should be published when chat is unidentifiable")
        default:
        }
}

func TestHandleCallbackQuery_InlineMessage_SetsInlineMetadata(t *testing.T) {
        messageBus := bus.NewMessageBus()
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "answerCallbackQuery") {
                                return successTrueResponse(t), nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)
        ch.BaseChannel = channels.NewBaseChannel("telegram", nil, messageBus, nil)
        ch.ctx = context.Background()

        query := &telego.CallbackQuery{
                ID:              "cb_inline",
                From:            telego.User{ID: 55, FirstName: "Frank"},
                ChatInstance:    "987654321",
                InlineMessageID: "inline_msg_42",
                Data:            "inline_action",
        }

        err := ch.handleCallbackQuery(context.Background(), query)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok, "expected inbound message for inline callback")
        assert.Equal(t, "[callback: inline_action]", inbound.Content)
        assert.Equal(t, "inline", inbound.Context.ChatType)
        assert.Equal(t, "inline_msg_42", inbound.Context.MessageID)
        assert.Equal(t, "inline_msg_42", inbound.Context.Raw["inline_message_id"])
}

func TestHandleCallbackQuery_GameShortName_Recorded(t *testing.T) {
        messageBus := bus.NewMessageBus()
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "answerCallbackQuery") {
                                return successTrueResponse(t), nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)
        ch.BaseChannel = channels.NewBaseChannel("telegram", nil, messageBus, nil)
        ch.ctx = context.Background()

        query := &telego.CallbackQuery{
                ID:           "cb_game",
                From:         telego.User{ID: 60, FirstName: "Gamer"},
                ChatInstance: "111",
                Data:         "",
                GameShortName: "my_cool_game",
        }
        query.Message = helperCallbackMessage(200, "private", 5, false, 0)

        err := ch.handleCallbackQuery(context.Background(), query)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)
        assert.Equal(t, "my_cool_game", inbound.Context.Raw["game_short_name"])
        // Content for empty data should still be produced.
        assert.Equal(t, "[callback: ]", inbound.Content)
}

func TestHandleCallbackQuery_SenderInfo_Populated(t *testing.T) {
        messageBus := bus.NewMessageBus()
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "answerCallbackQuery") {
                                return successTrueResponse(t), nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)
        ch.BaseChannel = channels.NewBaseChannel("telegram", nil, messageBus, nil)
        ch.ctx = context.Background()

        query := &telego.CallbackQuery{
                ID:   "cb_sender",
                From: telego.User{ID: 777, FirstName: "Grace", Username: "grace777"},
                Data: "click_me",
        }
        query.Message = helperCallbackMessage(300, "private", 10, false, 0)

        err := ch.handleCallbackQuery(context.Background(), query)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)
        assert.Equal(t, "telegram", inbound.Sender.Platform)
        assert.Equal(t, "777", inbound.Sender.PlatformID)
        assert.Equal(t, "telegram:777", inbound.Sender.CanonicalID)
        assert.Equal(t, "grace777", inbound.Sender.Username)
        assert.Equal(t, "Grace", inbound.Sender.DisplayName)
}

func TestHandleCallbackQuery_ChatIDStored(t *testing.T) {
        messageBus := bus.NewMessageBus()
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "answerCallbackQuery") {
                                return successTrueResponse(t), nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)
        ch.BaseChannel = channels.NewBaseChannel("telegram", nil, messageBus, nil)
        ch.ctx = context.Background()

        query := &telego.CallbackQuery{
                ID:   "cb_store",
                From: telego.User{ID: 888, FirstName: "Heidi"},
                Data: "store_test",
        }
        query.Message = helperCallbackMessage(555, "private", 20, false, 0)

        err := ch.handleCallbackQuery(context.Background(), query)
        require.NoError(t, err)

        // Drain the inbound message.
        <-messageBus.InboundChan()

        // Verify the chatID was stored for potential outbound use.
        assert.Equal(t, int64(555), ch.chatIDs["888"])
}

func TestAnswerCallbackQuery_PublicAPIMethod(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "answerCallbackQuery") {
                                var params map[string]any
                                require.NoError(t, json.Unmarshal(data.BodyRaw, &params))
                                assert.Equal(t, "query123", params["callback_query_id"])
                                assert.Equal(t, "Processing...", params["text"])
                                assert.Equal(t, true, params["show_alert"])
                                return successTrueResponse(t), nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)

        err := ch.AnswerCallbackQuery(context.Background(), "query123", "Processing...", true)
        require.NoError(t, err)
        assert.Len(t, caller.calls, 1)
}

func TestTelegramChannel_ImplementsCallbackQueryCapable(t *testing.T) {
        // Compile-time assertion is in telegram.go; this runtime test is a safety net.
        var _ channels.CallbackQueryCapable = (*TelegramChannel)(nil)
}

func TestHandleCallbackQuery_InaccessibleMessage_StillExtractsChatAndID(t *testing.T) {
        messageBus := bus.NewMessageBus()
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "answerCallbackQuery") {
                                return successTrueResponse(t), nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)
        ch.BaseChannel = channels.NewBaseChannel("telegram", nil, messageBus, nil)
        ch.ctx = context.Background()

        // Simulate an inaccessible message (e.g., message was deleted).
        inaccessible := &telego.InaccessibleMessage{
                Chat:      telego.Chat{ID: -100555, Type: "supergroup"},
                MessageID: 400,
                Date:      0,
        }

        query := &telego.CallbackQuery{
                ID:        "cb_inaccessible",
                From:      telego.User{ID: 90, FirstName: "Ivan"},
                Data:      "old_btn",
        }
        query.Message = inaccessible

        err := ch.handleCallbackQuery(context.Background(), query)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)
        assert.Equal(t, "-100555", inbound.Context.ChatID)
        assert.Equal(t, "400", inbound.Context.MessageID)
        assert.Equal(t, "group", inbound.Context.ChatType)
        assert.Equal(t, "[callback: old_btn]", inbound.Content)
}

func TestHandleCallbackQuery_ForumWithoutThread_NoCompositeChatID(t *testing.T) {
        messageBus := bus.NewMessageBus()
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "answerCallbackQuery") {
                                return successTrueResponse(t), nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)
        ch.BaseChannel = channels.NewBaseChannel("telegram", nil, messageBus, nil)
        ch.ctx = context.Background()

        query := &telego.CallbackQuery{
                ID:   "cb_forum_nothread",
                From: telego.User{ID: 99, FirstName: "Judy"},
                Data: "general_topic_btn",
        }
        // Forum chat but no thread ID (General topic or thread ID = 0).
        query.Message = helperCallbackMessage(-100777, "supergroup", 500, true, 0)

        err := ch.handleCallbackQuery(context.Background(), query)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)
        // Without a thread ID, no composite chat ID.
        assert.Equal(t, "-100777", inbound.Context.ChatID)
        assert.Empty(t, inbound.Context.TopicID)
}

// --- Inline Query Tests ---

func TestHandleInlineQuery_Disabled_ReturnsEmptyResults(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "answerInlineQuery") {
                                return &ta.Response{Ok: true, Result: []byte("true")}, nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)
        // EnableInline is false by default

        query := &telego.InlineQuery{
                ID:   "test-query-1",
                From: telego.User{ID: 42, FirstName: "Alice", Username: "alice"},
                Query: "hello",
        }

        err := ch.handleInlineQuery(context.Background(), query)
        require.NoError(t, err)

        // Should have called answerInlineQuery with empty results
        require.Len(t, caller.calls, 1)
        assert.Contains(t, caller.calls[0].URL, "answerInlineQuery")

        var params struct {
                InlineQueryID string `json:"inline_query_id"`
                Results       []any  `json:"results"`
        }
        require.NoError(t, json.Unmarshal(caller.calls[0].Data.BodyRaw, &params))
        assert.Equal(t, "test-query-1", params.InlineQueryID)
        assert.Empty(t, params.Results, "disabled inline mode should return empty results")
}

func TestHandleInlineQuery_Enabled_NilQuery_ReturnsNil(t *testing.T) {
        ch := newTestChannel(t, &stubCaller{})
        ch.tgCfg.EnableInline = true

        err := ch.handleInlineQuery(context.Background(), nil)
        assert.NoError(t, err)
}

func TestHandleInlineQuery_Enabled_EmptyQuery_ReturnsHelpArticle(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "answerInlineQuery") {
                                return &ta.Response{Ok: true, Result: []byte("true")}, nil
                        }
                        if strings.Contains(url, "getMe") {
                                return successUserResponse(t, &telego.User{
                                        ID:       123,
                                        IsBot:    true,
                                        Username: "testbot",
                                }), nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)
        ch.tgCfg.EnableInline = true

        query := &telego.InlineQuery{
                ID:    "test-query-2",
                From:  telego.User{ID: 42, FirstName: "Alice"},
                Query: "",
        }

        err := ch.handleInlineQuery(context.Background(), query)
        require.NoError(t, err)

        // getMe is called for bot.Username(), then answerInlineQuery
        require.Len(t, caller.calls, 2)
        assert.Contains(t, caller.calls[0].URL, "getMe")
        assert.Contains(t, caller.calls[1].URL, "answerInlineQuery")

        var params struct {
                InlineQueryID string `json:"inline_query_id"`
                Results       []struct {
                        Type  string `json:"type"`
                        ID    string `json:"id"`
                        Title string `json:"title"`
                } `json:"results"`
        }
        require.NoError(t, json.Unmarshal(caller.calls[1].Data.BodyRaw, &params))
        assert.Equal(t, "test-query-2", params.InlineQueryID)
        require.Len(t, params.Results, 1)
        assert.Equal(t, "article", params.Results[0].Type)
        assert.Equal(t, "help", params.Results[0].ID)
        assert.Equal(t, "Ask me anything", params.Results[0].Title)
}

func TestHandleInlineQuery_Enabled_NonEmptyQuery_ReturnsQueryArticle(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "answerInlineQuery") {
                                return &ta.Response{Ok: true, Result: []byte("true")}, nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        messageBus := bus.NewMessageBus()
        ch := newTestChannel(t, caller)
        ch.tgCfg.EnableInline = true
        ch.BaseChannel = channels.NewBaseChannel("telegram", nil, messageBus, nil)

        query := &telego.InlineQuery{
                ID:    "test-query-3",
                From:  telego.User{ID: 42, FirstName: "Alice", Username: "alice"},
                Query: "what is the weather?",
        }

        err := ch.handleInlineQuery(context.Background(), query)
        require.NoError(t, err)

        require.Len(t, caller.calls, 1)
        assert.Contains(t, caller.calls[0].URL, "answerInlineQuery")

        var params struct {
                InlineQueryID string `json:"inline_query_id"`
                IsPersonal    bool   `json:"is_personal"`
                Results       []struct {
                        Type  string `json:"type"`
                        ID    string `json:"id"`
                        Title string `json:"title"`
                } `json:"results"`
        }
        require.NoError(t, json.Unmarshal(caller.calls[0].Data.BodyRaw, &params))
        assert.Equal(t, "test-query-3", params.InlineQueryID)
        assert.True(t, params.IsPersonal)
        require.Len(t, params.Results, 1)
        assert.Equal(t, "article", params.Results[0].Type)
        assert.Equal(t, "query", params.Results[0].ID)
        assert.Equal(t, "what is the weather?", params.Results[0].Title)
}

func TestHandleInlineQuery_Enabled_NonEmptyQuery_PublishesInboundMessage(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "answerInlineQuery") {
                                return &ta.Response{Ok: true, Result: []byte("true")}, nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        messageBus := bus.NewMessageBus()
        ch := newTestChannel(t, caller)
        ch.tgCfg.EnableInline = true
        ch.BaseChannel = channels.NewBaseChannel("telegram", nil, messageBus, nil)

        query := &telego.InlineQuery{
                ID:    "test-query-4",
                From:  telego.User{ID: 42, FirstName: "Alice"},
                Query: "tell me a joke",
        }

        err := ch.handleInlineQuery(context.Background(), query)
        require.NoError(t, err)

        // Wait for the async goroutine to publish the inbound message
        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok, "expected inbound message from inline query")

        assert.Equal(t, "inline", inbound.Context.ChatType)
        assert.Equal(t, "inline:42", inbound.Context.ChatID)
        assert.Equal(t, "inline_query", inbound.Context.Raw["message_kind"])
        assert.Equal(t, "test-query-4", inbound.Context.Raw["inline_query_id"])
        assert.Equal(t, "tell me a joke", inbound.Context.Raw["query_text"])
        assert.Contains(t, inbound.Content, "[inline_query:")
        assert.Contains(t, inbound.Content, "tell me a joke")
}

func TestAnswerInlineQuery_ConvertsResultsToArticles(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "answerInlineQuery") {
                                return &ta.Response{Ok: true, Result: []byte("true")}, nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)

        results := []channels.InlineQueryResult{
                {
                        ID:          "1",
                        Title:       "First result",
                        Description: "Description 1",
                        Content:     "Content of first result",
                        URL:         "https://example.com/1",
                },
                {
                        ID:          "2",
                        Title:       "Second result",
                        Description: "Description 2",
                        Content:     "Content of second result",
                },
        }

        err := ch.AnswerInlineQuery(context.Background(), "query-123", results)
        require.NoError(t, err)

        require.Len(t, caller.calls, 1)
        assert.Contains(t, caller.calls[0].URL, "answerInlineQuery")

        var params struct {
                InlineQueryID string `json:"inline_query_id"`
                CacheTime     int    `json:"cache_time"`
                IsPersonal    bool   `json:"is_personal"`
                Results       []struct {
                        Type  string `json:"type"`
                        ID    string `json:"id"`
                        Title string `json:"title"`
                } `json:"results"`
        }
        require.NoError(t, json.Unmarshal(caller.calls[0].Data.BodyRaw, &params))
        assert.Equal(t, "query-123", params.InlineQueryID)
        assert.Equal(t, 30, params.CacheTime)
        assert.True(t, params.IsPersonal)
        require.Len(t, params.Results, 2)
        assert.Equal(t, "article", params.Results[0].Type)
        assert.Equal(t, "1", params.Results[0].ID)
        assert.Equal(t, "First result", params.Results[0].Title)
        assert.Equal(t, "2", params.Results[1].ID)
}

func TestHandleInlineQuery_WithLocation_RecordsLocationInRaw(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "answerInlineQuery") {
                                return &ta.Response{Ok: true, Result: []byte("true")}, nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        messageBus := bus.NewMessageBus()
        ch := newTestChannel(t, caller)
        ch.tgCfg.EnableInline = true
        ch.BaseChannel = channels.NewBaseChannel("telegram", nil, messageBus, nil)

        query := &telego.InlineQuery{
                ID:    "test-query-loc",
                From:  telego.User{ID: 42, FirstName: "Alice"},
                Query: "nearby restaurants",
                Location: &telego.Location{
                        Latitude:  40.7128,
                        Longitude: -74.0060,
                },
        }

        err := ch.handleInlineQuery(context.Background(), query)
        require.NoError(t, err)

        // Wait for async inbound message
        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok, "expected inbound message with location data")

        assert.Contains(t, inbound.Context.Raw, "location_lat")
        assert.Contains(t, inbound.Context.Raw, "location_lng")
}

// --- Phase 6 Tests: GAP-8 (Edited Messages), GAP-14 (Channel Posts), GAP-22 (Chat Member) ---

func TestHandleMessage_EditedMessage_SetsIsEditAndEditDate(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                Text:      "edited text",
                MessageID: 100,
                EditDate:  1700000000,
                Chat: telego.Chat{
                        ID:   12345,
                        Type: "private",
                },
                From: &telego.User{
                        ID:        7,
                        FirstName: "Alice",
                },
        }

        err := ch.handleMessage(context.Background(), msg, true)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok, "expected inbound message")

        assert.True(t, inbound.Context.IsEdit, "IsEdit should be true for edited messages")
        assert.Equal(t, int64(1700000000), inbound.Context.EditDate, "EditDate should match message EditDate")
        assert.Equal(t, "edited text", inbound.Content)
}

func TestHandleMessage_RegularMessage_IsEditFalse(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                Text:      "normal message",
                MessageID: 101,
                Chat: telego.Chat{
                        ID:   12345,
                        Type: "private",
                },
                From: &telego.User{
                        ID:        8,
                        FirstName: "Bob",
                },
        }

        err := ch.handleMessage(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok, "expected inbound message")

        assert.False(t, inbound.Context.IsEdit, "IsEdit should be false for regular messages")
        assert.Equal(t, int64(0), inbound.Context.EditDate, "EditDate should be 0 for non-edited messages")
}

func TestHandleMessage_EditedMessage_NoEditDate_ZeroEditDate(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                Text:      "edited without date",
                MessageID: 102,
                EditDate:  0, // No edit date set
                Chat: telego.Chat{
                        ID:   12345,
                        Type: "private",
                },
                From: &telego.User{
                        ID:        9,
                        FirstName: "Carol",
                },
        }

        err := ch.handleMessage(context.Background(), msg, true)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok, "expected inbound message")

        assert.True(t, inbound.Context.IsEdit, "IsEdit should be true even without EditDate")
        assert.Equal(t, int64(0), inbound.Context.EditDate, "EditDate should be 0 when message.EditDate is 0")
}

func TestHandleChannelPost_WithSenderChat_PublishesAsChannelType(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                Text:      "Hello from the channel!",
                MessageID: 200,
                Chat: telego.Chat{
                        ID:   -1001234567890,
                        Type: "channel",
                },
                SenderChat: &telego.Chat{
                        ID:       -1001234567890,
                        Title:    "My Channel",
                        Username: "my_channel",
                },
        }

        err := ch.handleChannelPost(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok, "expected inbound message from channel post")

        assert.Equal(t, "channel", inbound.Context.ChatType)
        assert.Equal(t, "-1001234567890", inbound.Context.ChatID)
        assert.Equal(t, "200", inbound.Context.MessageID)
        assert.Equal(t, "Hello from the channel!", inbound.Content)
        assert.Equal(t, "chat_-1001234567890", inbound.Context.SenderID)
        assert.Equal(t, "My Channel", inbound.Sender.DisplayName)
        assert.Equal(t, "my_channel", inbound.Sender.Username)
        assert.Equal(t, "-1001234567890", inbound.Context.Raw["sender_chat_id"])
        assert.Equal(t, "My Channel", inbound.Context.Raw["sender_chat_title"])
}

func TestHandleChannelPost_EmptyText_SkipsPublish(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                MessageID: 201,
                Chat: telego.Chat{
                        ID:   -1001234567890,
                        Type: "channel",
                },
        }

        err := ch.handleChannelPost(context.Background(), msg)
        require.NoError(t, err)

        // Use a non-blocking read: since nothing was published, the channel
        // should be empty. We use a short timeout to verify this.
        select {
        case <-messageBus.InboundChan():
                t.Fatal("should not publish for empty channel post")
        case <-time.After(50 * time.Millisecond):
                // Expected: no message published within the timeout window.
        }
}

func TestHandleChannelPost_WithCaption_UsesCaptionAsContent(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        msg := &telego.Message{
                Caption:   "Photo from the channel",
                MessageID: 202,
                Chat: telego.Chat{
                        ID:   -1001234567890,
                        Type: "channel",
                },
                SenderChat: &telego.Chat{
                        ID:    -1001234567890,
                        Title: "My Channel",
                },
        }

        err := ch.handleChannelPost(context.Background(), msg)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok, "expected inbound message")

        assert.Equal(t, "Photo from the channel", inbound.Content)
}

func TestHandleChannelPost_NilMessage_ReturnsNil(t *testing.T) {
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, nil, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        err := ch.handleChannelPost(context.Background(), nil)
        assert.NoError(t, err)
}

func TestHandleChatMemberUpdated_MyChatMember_PublishesEvent(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        update := &telego.ChatMemberUpdated{
                Chat: telego.Chat{
                        ID:   -100999,
                        Type: "supergroup",
                },
                From: telego.User{
                        ID:        1,
                        FirstName: "Admin",
                },
                Date: 1700000000,
                OldChatMember: &telego.ChatMemberLeft{
                        Status: "left",
                        User:   telego.User{ID: 42, FirstName: "Bot"},
                },
                NewChatMember: &telego.ChatMemberMember{
                        Status: "member",
                        User:   telego.User{ID: 42, FirstName: "Bot"},
                },
        }

        err := ch.handleChatMemberUpdated(context.Background(), update, true)
        require.NoError(t, err)

        event, ok := <-messageBus.ChatMemberEventsChan()
        require.True(t, ok, "expected chat member event")

        assert.Equal(t, "telegram", event.Channel)
        assert.Equal(t, "-100999", event.ChatID)
        assert.Equal(t, "supergroup", event.ChatType)
        assert.Equal(t, "1", event.ActorID)
        assert.Equal(t, "42", event.UserID)
        assert.Equal(t, "left", event.OldStatus)
        assert.Equal(t, "member", event.NewStatus)
        assert.True(t, event.IsMyChatMember)
        assert.Equal(t, int64(1700000000), event.Date)
}

func TestHandleChatMemberUpdated_OtherMember_NotMyChatMember(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        update := &telego.ChatMemberUpdated{
                Chat: telego.Chat{
                        ID:   -100888,
                        Type: "group",
                },
                From: telego.User{
                        ID:        1,
                        FirstName: "Admin",
                },
                Date: 1700000001,
                OldChatMember: &telego.ChatMemberMember{
                        Status: "member",
                        User:   telego.User{ID: 99, FirstName: "Alice"},
                },
                NewChatMember: &telego.ChatMemberAdministrator{
                        Status: "administrator",
                        User:   telego.User{ID: 99, FirstName: "Alice"},
                },
        }

        err := ch.handleChatMemberUpdated(context.Background(), update, false)
        require.NoError(t, err)

        event, ok := <-messageBus.ChatMemberEventsChan()
        require.True(t, ok, "expected chat member event")

        assert.Equal(t, "-100888", event.ChatID)
        assert.Equal(t, "99", event.UserID)
        assert.Equal(t, "member", event.OldStatus)
        assert.Equal(t, "administrator", event.NewStatus)
        assert.False(t, event.IsMyChatMember)
}

func TestHandleChatMemberUpdated_BotAddedToGroup_StoresChatID(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        update := &telego.ChatMemberUpdated{
                Chat: telego.Chat{
                        ID:   -100777,
                        Type: "supergroup",
                },
                From: telego.User{
                        ID:        1,
                        FirstName: "Admin",
                },
                Date: 1700000002,
                OldChatMember: &telego.ChatMemberLeft{
                        Status: "left",
                        User:   telego.User{ID: 42, FirstName: "Bot"},
                },
                NewChatMember: &telego.ChatMemberMember{
                        Status: "member",
                        User:   telego.User{ID: 42, FirstName: "Bot"},
                },
        }

        err := ch.handleChatMemberUpdated(context.Background(), update, true)
        require.NoError(t, err)

        // Drain the event
        <-messageBus.ChatMemberEventsChan()

        // Verify the chat ID was stored for the bot
        assert.Equal(t, int64(-100777), ch.chatIDs["bot"])
}

func TestHandleChatMemberUpdated_MemberBanned_KickedStatus(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        update := &telego.ChatMemberUpdated{
                Chat: telego.Chat{
                        ID:   -100666,
                        Type: "supergroup",
                },
                From: telego.User{
                        ID:        1,
                        FirstName: "Admin",
                },
                Date: 1700000003,
                OldChatMember: &telego.ChatMemberMember{
                        Status: "member",
                        User:   telego.User{ID: 55, FirstName: "Troll"},
                },
                NewChatMember: &telego.ChatMemberBanned{
                        Status: "kicked",
                        User:   telego.User{ID: 55, FirstName: "Troll"},
                },
        }

        err := ch.handleChatMemberUpdated(context.Background(), update, false)
        require.NoError(t, err)

        event, ok := <-messageBus.ChatMemberEventsChan()
        require.True(t, ok)

        assert.Equal(t, "member", event.OldStatus)
        assert.Equal(t, "kicked", event.NewStatus)
        assert.Equal(t, "55", event.UserID)
}

func TestHandleChatMemberUpdated_NilUpdate_ReturnsNil(t *testing.T) {
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, nil, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        err := ch.handleChatMemberUpdated(context.Background(), nil, false)
        assert.NoError(t, err)
}

func TestTelegramChatMemberStatus_NilMember(t *testing.T) {
        assert.Equal(t, "unknown", telegramChatMemberStatus(nil))
}

func TestTelegramChatMemberStatus_Owner(t *testing.T) {
        member := &telego.ChatMemberOwner{Status: "creator"}
        assert.Equal(t, "creator", telegramChatMemberStatus(member))
}

func TestTelegramChatMemberStatus_Restricted(t *testing.T) {
        member := &telego.ChatMemberRestricted{Status: "restricted"}
        assert.Equal(t, "restricted", telegramChatMemberStatus(member))
}

func TestHandleMessage_EditedMessage_SameSessionAsOriginal(t *testing.T) {
        messageBus := bus.NewMessageBus()
        ch := &TelegramChannel{
                BaseChannel: channels.NewBaseChannel("telegram", nil, messageBus, nil),
                chatIDs:     make(map[string]int64),
                ctx:         context.Background(),
        }

        // Edited message should use the same chat ID / session as the original.
        msg := &telego.Message{
                Text:      "corrected text",
                MessageID: 103,
                EditDate:  1700000100,
                Chat: telego.Chat{
                        ID:   456,
                        Type: "private",
                },
                From: &telego.User{
                        ID:        10,
                        FirstName: "Dave",
                },
        }

        err := ch.handleMessage(context.Background(), msg, true)
        require.NoError(t, err)

        inbound, ok := <-messageBus.InboundChan()
        require.True(t, ok)

        // Same session key as a non-edited message from the same chat
        assert.Equal(t, "456", inbound.Context.ChatID)
        assert.Equal(t, "103", inbound.Context.MessageID)
        assert.True(t, inbound.Context.IsEdit)
}

func TestInboundContext_IsEditEditDate_Fields(t *testing.T) {
        ctx := bus.InboundContext{
                Channel:  "telegram",
                ChatID:   "123",
                SenderID: "456",
                IsEdit:   true,
                EditDate: 1700000000,
        }

        assert.True(t, ctx.IsEdit)
        assert.Equal(t, int64(1700000000), ctx.EditDate)

        // Verify omitempty behavior
        ctx2 := bus.InboundContext{
                Channel:  "telegram",
                ChatID:   "123",
                SenderID: "456",
        }
        assert.False(t, ctx2.IsEdit)
        assert.Equal(t, int64(0), ctx2.EditDate)
}

func TestChatMemberEvent_StructFields(t *testing.T) {
        event := bus.ChatMemberEvent{
                Channel:        "telegram",
                ChatID:         "-100999",
                ChatType:       "supergroup",
                ActorID:        "1",
                UserID:         "42",
                OldStatus:      "left",
                NewStatus:      "member",
                IsMyChatMember: true,
                Date:           1700000000,
                Raw: map[string]string{
                        "actor_id":          "1",
                        "user_id":           "42",
                        "old_status":        "left",
                        "new_status":        "member",
                        "is_my_chat_member": "true",
                },
        }

        assert.Equal(t, "telegram", event.Channel)
        assert.Equal(t, "-100999", event.ChatID)
        assert.Equal(t, "supergroup", event.ChatType)
        assert.Equal(t, "1", event.ActorID)
        assert.Equal(t, "42", event.UserID)
        assert.Equal(t, "left", event.OldStatus)
        assert.Equal(t, "member", event.NewStatus)
        assert.True(t, event.IsMyChatMember)
        assert.Equal(t, int64(1700000000), event.Date)
}

func TestPublishChatMemberEvent_ClosedBus(t *testing.T) {
        messageBus := bus.NewMessageBus()
        messageBus.Close()

        err := messageBus.PublishChatMemberEvent(bus.ChatMemberEvent{
                Channel:   "telegram",
                ChatID:    "-100",
                OldStatus: "left",
                NewStatus: "member",
        })
        assert.ErrorIs(t, err, bus.ErrBusClosed)
}

// ============================================================================
// Phase 7: Enhanced Media Features (GAP-9, GAP-10, GAP-11, GAP-12, GAP-13)
// ============================================================================

// --- GAP-9: Media Group (Album) Support ---

func TestSendMedia_MediaGroup_TwoImages_SendsAsAlbum(t *testing.T) {
        constructor := &multipartRecordingConstructor{}
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "sendMediaGroup") {
                                // Return two messages (album of 2 items).
                                msgs := []telego.Message{
                                        {MessageID: 101},
                                        {MessageID: 102},
                                }
                                b, err := json.Marshal(msgs)
                                require.NoError(t, err)
                                return &ta.Response{Ok: true, Result: b}, nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannelWithConstructor(t, caller, constructor)
        store := media.NewFileMediaStore()
        ch.SetMediaStore(store)

        tmpDir := t.TempDir()
        var refs []string
        for i := 0; i < 2; i++ {
                path := filepath.Join(tmpDir, fmt.Sprintf("photo%d.jpg", i))
                require.NoError(t, os.WriteFile(path, []byte("fake-img"), 0o644))
                ref, err := store.Store(path, media.MediaMeta{Filename: fmt.Sprintf("photo%d.jpg", i)}, "")
                require.NoError(t, err)
                refs = append(refs, ref)
        }

        ids, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
                ChatID: "-100200",
                Parts: []bus.MediaPart{
                        {Type: "image", Ref: refs[0], Caption: "Album caption"},
                        {Type: "image", Ref: refs[1]},
                },
        })
        require.NoError(t, err)
        assert.Equal(t, []string{"101", "102"}, ids)

        // Verify sendMediaGroup was called — the multipart constructor
        // captures the media field counts. An album of 2 images should
        // produce at least 2 file uploads in a single call.
        assert.Equal(t, 1, len(constructor.calls), "expected exactly 1 multipart call (sendMediaGroup)")
        assert.GreaterOrEqual(t, len(constructor.calls[0].FileSizes), 2, "album should contain 2 files")
}

func TestSendMedia_MediaGroup_MixedImageAndVideo(t *testing.T) {
        constructor := &multipartRecordingConstructor{}
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "sendMediaGroup") {
                                msgs := []telego.Message{
                                        {MessageID: 201},
                                        {MessageID: 202},
                                        {MessageID: 203},
                                }
                                b, err := json.Marshal(msgs)
                                require.NoError(t, err)
                                return &ta.Response{Ok: true, Result: b}, nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannelWithConstructor(t, caller, constructor)
        store := media.NewFileMediaStore()
        ch.SetMediaStore(store)

        tmpDir := t.TempDir()
        var refs []string
        for i, ext := range []string{".jpg", ".mp4", ".jpg"} {
                path := filepath.Join(tmpDir, fmt.Sprintf("media%d%s", i, ext))
                require.NoError(t, os.WriteFile(path, []byte("fake"), 0o644))
                ref, err := store.Store(path, media.MediaMeta{Filename: fmt.Sprintf("media%d%s", i, ext)}, "")
                require.NoError(t, err)
                refs = append(refs, ref)
        }

        ids, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
                ChatID: "-100200",
                Parts: []bus.MediaPart{
                        {Type: "image", Ref: refs[0]},
                        {Type: "video", Ref: refs[1]},
                        {Type: "image", Ref: refs[2]},
                },
        })
        require.NoError(t, err)
        assert.Equal(t, []string{"201", "202", "203"}, ids)
}

func TestSendMedia_MediaGroup_AlbumFailure_FallbackToIndividual(t *testing.T) {
        constructor := &multipartRecordingConstructor{}
        callCount := 0
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        callCount++
                        if strings.Contains(url, "sendMediaGroup") {
                                // Album call fails.
                                return nil, errors.New("media group not supported")
                        }
                        if strings.Contains(url, "sendPhoto") {
                                return successResponseWithMessageID(t, 300+callCount), nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannelWithConstructor(t, caller, constructor)
        store := media.NewFileMediaStore()
        ch.SetMediaStore(store)

        tmpDir := t.TempDir()
        var refs []string
        for i := 0; i < 2; i++ {
                path := filepath.Join(tmpDir, fmt.Sprintf("photo%d.jpg", i))
                require.NoError(t, os.WriteFile(path, []byte("fake-img"), 0o644))
                ref, err := store.Store(path, media.MediaMeta{Filename: fmt.Sprintf("photo%d.jpg", i)}, "")
                require.NoError(t, err)
                refs = append(refs, ref)
        }

        ids, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
                ChatID: "-100200",
                Parts: []bus.MediaPart{
                        {Type: "image", Ref: refs[0]},
                        {Type: "image", Ref: refs[1]},
                },
        })
        require.NoError(t, err)
        assert.Len(t, ids, 2)
}

func TestSendMedia_SingleImage_NotBatchedAsAlbum(t *testing.T) {
        constructor := &multipartRecordingConstructor{}
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "sendPhoto") {
                                return successResponseWithMessageID(t, 400), nil
                        }
                        t.Fatalf("unexpected API call: %s (expected sendPhoto, not sendMediaGroup)", url)
                        return nil, nil
                },
        }
        ch := newTestChannelWithConstructor(t, caller, constructor)
        store := media.NewFileMediaStore()
        ch.SetMediaStore(store)

        tmpDir := t.TempDir()
        path := filepath.Join(tmpDir, "single.jpg")
        require.NoError(t, os.WriteFile(path, []byte("fake-img"), 0o644))
        ref, err := store.Store(path, media.MediaMeta{Filename: "single.jpg"}, "")
        require.NoError(t, err)

        ids, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
                ChatID: "-100200",
                Parts: []bus.MediaPart{
                        {Type: "image", Ref: ref, Caption: "Just one photo"},
                },
        })
        require.NoError(t, err)
        assert.Equal(t, []string{"400"}, ids)
        // Ensure no sendMediaGroup call was made (single image should
        // go through sendPhoto, not sendMediaGroup).
        // The caller function already verified sendPhoto was called.
        // If sendMediaGroup had been used, the caller would have fatalf'd.
}

func TestSendMedia_ImageThenAudioThenImage_OnlyConsecutiveImagesBatched(t *testing.T) {
        constructor := &multipartRecordingConstructor{}
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "sendPhoto") {
                                return successResponseWithMessageID(t, 501), nil
                        }
                        if strings.Contains(url, "sendAudio") {
                                return successResponseWithMessageID(t, 502), nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannelWithConstructor(t, caller, constructor)
        store := media.NewFileMediaStore()
        ch.SetMediaStore(store)

        tmpDir := t.TempDir()
        imgPath := filepath.Join(tmpDir, "photo.jpg")
        require.NoError(t, os.WriteFile(imgPath, []byte("fake-img"), 0o644))
        imgRef, err := store.Store(imgPath, media.MediaMeta{Filename: "photo.jpg"}, "")
        require.NoError(t, err)

        audioPath := filepath.Join(tmpDir, "audio.mp3")
        require.NoError(t, os.WriteFile(audioPath, []byte("fake-audio"), 0o644))
        audioRef, err := store.Store(audioPath, media.MediaMeta{Filename: "audio.mp3"}, "")
        require.NoError(t, err)

        ids, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
                ChatID: "-100200",
                Parts: []bus.MediaPart{
                        {Type: "image", Ref: imgRef},
                        {Type: "audio", Ref: audioRef},
                        {Type: "image", Ref: imgRef},
                },
        })
        require.NoError(t, err)
        assert.Equal(t, []string{"501", "502", "501"}, ids)
}

// --- GAP-10: Message Pinning (PinnableCapable) ---

func TestPinMessage_Success(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        assert.Contains(t, url, "pinChatMessage")
                        return &ta.Response{Ok: true, Result: json.RawMessage(`true`)}, nil
                },
        }
        ch := newTestChannel(t, caller)

        err := ch.PinMessage(context.Background(), "-100200", "42")
        assert.NoError(t, err)
}

func TestUnpinMessage_Success(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        assert.Contains(t, url, "unpinChatMessage")
                        return &ta.Response{Ok: true, Result: json.RawMessage(`true`)}, nil
                },
        }
        ch := newTestChannel(t, caller)

        err := ch.UnpinMessage(context.Background(), "-100200", "42")
        assert.NoError(t, err)
}

func TestPinMessage_InvalidChatID(t *testing.T) {
        caller := &stubCaller{callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                return &ta.Response{Ok: true, Result: json.RawMessage(`true`)}, nil
        }}
        ch := newTestChannel(t, caller)

        err := ch.PinMessage(context.Background(), "not-a-number", "42")
        assert.Error(t, err)
}

func TestPinMessage_InvalidMessageID(t *testing.T) {
        caller := &stubCaller{callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                return &ta.Response{Ok: true, Result: json.RawMessage(`true`)}, nil
        }}
        ch := newTestChannel(t, caller)

        err := ch.PinMessage(context.Background(), "-100200", "abc")
        assert.Error(t, err)
}

func TestPinnableCapable_InterfaceAssertion(t *testing.T) {
        // Verify TelegramChannel implements PinnableCapable at compile time.
        var _ channels.PinnableCapable = (*TelegramChannel)(nil)
}

// --- GAP-11: Outbound Sticker Support ---

func TestSendMedia_StickerWithFileID(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        assert.Contains(t, url, "sendSticker")
                        return successResponseWithMessageID(t, 600), nil
                },
        }
        ch := newTestChannel(t, caller)

        ids, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
                ChatID: "-100200",
                Parts: []bus.MediaPart{
                        {Type: "sticker", Ref: "CAACAgIAAxkBAAEB"},
                },
        })
        require.NoError(t, err)
        assert.Equal(t, []string{"600"}, ids)
}

func TestSendMedia_StickerWithURL(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        assert.Contains(t, url, "sendSticker")
                        return successResponseWithMessageID(t, 601), nil
                },
        }
        ch := newTestChannel(t, caller)

        ids, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
                ChatID: "-100200",
                Parts: []bus.MediaPart{
                        {Type: "sticker", Ref: "https://example.com/sticker.webp"},
                },
        })
        require.NoError(t, err)
        assert.Equal(t, []string{"601"}, ids)
}

func TestSendMedia_StickerWithMediaStoreRef(t *testing.T) {
        constructor := &multipartRecordingConstructor{}
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        assert.Contains(t, url, "sendSticker")
                        return successResponseWithMessageID(t, 602), nil
                },
        }
        ch := newTestChannelWithConstructor(t, caller, constructor)
        store := media.NewFileMediaStore()
        ch.SetMediaStore(store)

        tmpDir := t.TempDir()
        path := filepath.Join(tmpDir, "sticker.webp")
        require.NoError(t, os.WriteFile(path, []byte("fake-webp"), 0o644))
        ref, err := store.Store(path, media.MediaMeta{Filename: "sticker.webp"}, "")
        require.NoError(t, err)

        ids, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
                ChatID: "-100200",
                Parts: []bus.MediaPart{
                        {Type: "sticker", Ref: ref},
                },
        })
        require.NoError(t, err)
        assert.Equal(t, []string{"602"}, ids)
}

// --- GAP-12: Location/Venue Outbound Support ---

func TestSendMedia_Location(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        assert.Contains(t, url, "sendLocation")
                        return successResponseWithMessageID(t, 700), nil
                },
        }
        ch := newTestChannel(t, caller)

        ids, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
                ChatID: "-100200",
                Parts: []bus.MediaPart{
                        {Type: "location", Latitude: 13.7563, Longitude: 100.5018},
                },
        })
        require.NoError(t, err)
        assert.Equal(t, []string{"700"}, ids)
}

func TestSendMedia_Venue(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        assert.Contains(t, url, "sendVenue")
                        return successResponseWithMessageID(t, 701), nil
                },
        }
        ch := newTestChannel(t, caller)

        ids, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
                ChatID: "-100200",
                Parts: []bus.MediaPart{
                        {
                                Type:      "venue",
                                Latitude:  13.7563,
                                Longitude: 100.5018,
                                Title:     "Bangkok Office",
                                Address:   "123 Sukhumvit Rd",
                        },
                },
        })
        require.NoError(t, err)
        assert.Equal(t, []string{"701"}, ids)
}

func TestSendMedia_MixedLocationAndImage(t *testing.T) {
        constructor := &multipartRecordingConstructor{}
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        if strings.Contains(url, "sendLocation") {
                                return successResponseWithMessageID(t, 710), nil
                        }
                        if strings.Contains(url, "sendPhoto") {
                                return successResponseWithMessageID(t, 711), nil
                        }
                        t.Fatalf("unexpected API call: %s", url)
                        return nil, nil
                },
        }
        ch := newTestChannelWithConstructor(t, caller, constructor)
        store := media.NewFileMediaStore()
        ch.SetMediaStore(store)

        tmpDir := t.TempDir()
        path := filepath.Join(tmpDir, "photo.jpg")
        require.NoError(t, os.WriteFile(path, []byte("fake-img"), 0o644))
        ref, err := store.Store(path, media.MediaMeta{Filename: "photo.jpg"}, "")
        require.NoError(t, err)

        ids, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
                ChatID: "-100200",
                Parts: []bus.MediaPart{
                        {Type: "location", Latitude: 13.7563, Longitude: 100.5018},
                        {Type: "image", Ref: ref, Caption: "See this place!"},
                },
        })
        require.NoError(t, err)
        assert.Equal(t, []string{"710", "711"}, ids)
}

// --- GAP-13: Batch Message Deletion ---

func TestDeleteMessages_BatchSuccess(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        assert.Contains(t, url, "deleteMessages")
                        return &ta.Response{Ok: true, Result: json.RawMessage(`true`)}, nil
                },
        }
        ch := newTestChannel(t, caller)

        err := ch.DeleteMessages(context.Background(), "-100200", []string{"10", "20", "30"})
        assert.NoError(t, err)
}

func TestDeleteMessages_EmptyList_NoAPICall(t *testing.T) {
        called := false
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        called = true
                        return nil, nil
                },
        }
        ch := newTestChannel(t, caller)

        err := ch.DeleteMessages(context.Background(), "-100200", []string{})
        assert.NoError(t, err)
        assert.False(t, called, "no API call should be made for empty message list")
}

func TestDeleteMessages_InvalidMessageID(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        return &ta.Response{Ok: true, Result: json.RawMessage(`true`)}, nil
                },
        }
        ch := newTestChannel(t, caller)

        err := ch.DeleteMessages(context.Background(), "-100200", []string{"10", "not-a-number"})
        assert.Error(t, err)
        assert.Contains(t, err.Error(), "invalid message ID")
}

func TestDeleteMessages_InvalidChatID(t *testing.T) {
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        return &ta.Response{Ok: true, Result: json.RawMessage(`true`)}, nil
                },
        }
        ch := newTestChannel(t, caller)

        err := ch.DeleteMessages(context.Background(), "not-a-number", []string{"10", "20"})
        assert.Error(t, err)
}

func TestBatchMessageDeleter_InterfaceAssertion(t *testing.T) {
        // Verify TelegramChannel implements BatchMessageDeleter at compile time.
        var _ channels.BatchMessageDeleter = (*TelegramChannel)(nil)
}

// --- MediaPart Extended Fields ---

func TestMediaPart_LocationFields(t *testing.T) {
        part := bus.MediaPart{
                Type:      "location",
                Latitude:  13.7563,
                Longitude: 100.5018,
        }
        assert.Equal(t, "location", part.Type)
        assert.InDelta(t, 13.7563, part.Latitude, 0.0001)
        assert.InDelta(t, 100.5018, part.Longitude, 0.0001)
}

func TestMediaPart_VenueFields(t *testing.T) {
        part := bus.MediaPart{
                Type:      "venue",
                Latitude:  13.7563,
                Longitude: 100.5018,
                Title:     "Office",
                Address:   "123 Main St",
        }
        assert.Equal(t, "venue", part.Type)
        assert.Equal(t, "Office", part.Title)
        assert.Equal(t, "123 Main St", part.Address)
}

func TestMediaPart_StickerType(t *testing.T) {
        part := bus.MediaPart{
                Type: "sticker",
                Ref:  "CAACAgIAAxkBAAEB",
        }
        assert.Equal(t, "sticker", part.Type)
        assert.Equal(t, "CAACAgIAAxkBAAEB", part.Ref)
}

func TestSendMedia_StickerLocationAndImage_Combined(t *testing.T) {
        constructor := &multipartRecordingConstructor{}
        caller := &stubCaller{
                callFn: func(ctx context.Context, url string, data *ta.RequestData) (*ta.Response, error) {
                        switch {
                        case strings.Contains(url, "sendSticker"):
                                return successResponseWithMessageID(t, 800), nil
                        case strings.Contains(url, "sendLocation"):
                                return successResponseWithMessageID(t, 801), nil
                        case strings.Contains(url, "sendPhoto"):
                                return successResponseWithMessageID(t, 802), nil
                        default:
                                t.Fatalf("unexpected API call: %s", url)
                                return nil, nil
                        }
                },
        }
        ch := newTestChannelWithConstructor(t, caller, constructor)
        store := media.NewFileMediaStore()
        ch.SetMediaStore(store)

        tmpDir := t.TempDir()
        imgPath := filepath.Join(tmpDir, "photo.jpg")
        require.NoError(t, os.WriteFile(imgPath, []byte("fake-img"), 0o644))
        imgRef, err := store.Store(imgPath, media.MediaMeta{Filename: "photo.jpg"}, "")
        require.NoError(t, err)

        ids, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
                ChatID: "-100200",
                Parts: []bus.MediaPart{
                        {Type: "sticker", Ref: "CAACAgIAAxkBAAEB"},
                        {Type: "location", Latitude: 13.7563, Longitude: 100.5018},
                        {Type: "image", Ref: imgRef, Caption: "Here we are!"},
                },
        })
        require.NoError(t, err)
        assert.Equal(t, []string{"800", "801", "802"}, ids)
}
