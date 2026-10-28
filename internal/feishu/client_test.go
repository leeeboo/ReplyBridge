package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	qrcode "github.com/skip2/go-qrcode"
	_ "image/png"

	"comment-bridge/internal/commentbridge"
)

type fakeSender struct {
	message   outgoingMessage
	messageID string
	definite  bool
	err       error
}

func TestClientRequestsHaveTimeoutAndPreserveUnknownOutcome(t *testing.T) {
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	var tokenRequests, replyRequests int
	http.DefaultTransport = identityRoundTrip(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 20*time.Second {
			t.Error("SDK request must have a bounded 20s deadline")
		}
		if strings.Contains(r.URL.Path, "/auth/") {
			tokenRequests++
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":0,"tenant_access_token":"test-token","expire":7200}`))}, nil
		}
		replyRequests++
		return nil, context.DeadlineExceeded
	})
	c, err := New(Config{AppID: "timeout-regression-app", AppSecret: "test-secret", ChatID: "test-chat", AllowAllGroupMembers: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ReplyToMessage(t.Context(), "test-parent", "test text", "test-action")
	if !commentbridge.IsUnknownOutcome(err) || tokenRequests != 1 || replyRequests != 1 {
		t.Fatalf("timeout must stay unknown without retry: token=%d reply=%d err=%v", tokenRequests, replyRequests, err)
	}
}

func TestSendErrorAlertUsesGroupAndIdempotency(t *testing.T) {
	fake := &fakeSender{messageID: "alert-id"}
	c := &Client{config: Config{ChatID: "chat"}, sender: fake}
	id, err := c.SendErrorAlert(context.Background(), "⚠️ 抓取异常", "incident")
	if err != nil || id != "alert-id" || fake.message.ChatID != "chat" || fake.message.UUID != feishuUUID("incident") || !strings.Contains(fake.message.Content, "抓取异常") {
		t.Fatalf("invalid alert transport: %v", err)
	}
	fake.err = errors.New("timeout")
	_, err = c.SendErrorAlert(context.Background(), "⚠️ 抓取异常", "incident")
	if !commentbridge.IsUnknownOutcome(err) {
		t.Fatal("uncertain send must remain unknown")
	}
}

func (f *fakeSender) Send(_ context.Context, message outgoingMessage) (string, bool, error) {
	f.message = message
	return f.messageID, f.definite, f.err
}

type fakeReactionTransport struct {
	reaction   messageReaction
	reactionID string
	definite   bool
	err        error
}

type fakeImageTransport struct {
	image    imageUpload
	imageKey string
	definite bool
	err      error
	called   bool
}

func (f *fakeImageTransport) Upload(_ context.Context, image imageUpload) (string, bool, error) {
	f.called = true
	f.image = image
	return f.imageKey, f.definite, f.err
}

func (f *fakeReactionTransport) Create(_ context.Context, reaction messageReaction) (string, bool, error) {
	f.reaction = reaction
	return f.reactionID, f.definite, f.err
}

type fakeReplyTransport struct {
	reply     outgoingReply
	messageID string
	definite  bool
	err       error
}

func (f *fakeReplyTransport) Reply(_ context.Context, reply outgoingReply) (string, bool, error) {
	f.reply = reply
	return f.messageID, f.definite, f.err
}

func TestNotifyComment(t *testing.T) {
	client := newTestClient(t)
	fake := &fakeSender{messageID: "om_123"}
	client.sender = fake
	got, err := client.NotifyComment(context.Background(), commentbridge.Comment{Platform: commentbridge.PlatformBilibili, ResourceTitle: "视频", ResourceURL: "https://example.test/v", AuthorName: "作者", Text: "评论"}, "action-1")
	if err != nil || got != "om_123" {
		t.Fatalf("NotifyComment() = %q, %v", got, err)
	}
	if fake.message.ChatID != "chat-1" || fake.message.UUID != "action-1" || fake.message.MsgType != "text" {
		t.Fatalf("unexpected request: %#v", fake.message)
	}
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(fake.message.Content), &content); err != nil {
		t.Fatalf("decode notification: %v", err)
	}
	want := "💬 新评论\n视频：视频\n链接：https://example.test/v\n作者：作者\n评论：评论\n\n请引用回复本消息。回复内容将公开发布到对应平台。\n给本消息添加 👍 可点赞评论；移除不会撤销。"
	if content.Text != want {
		t.Fatalf("notification text = %q, want %q", content.Text, want)
	}
}

func TestNotificationTextYouTubeFooter(t *testing.T) {
	got := notificationText(commentbridge.Comment{Platform: commentbridge.PlatformYouTube, ResourceTitle: "视频", ResourceURL: "https://example.test/watch", AuthorName: "作者", Text: "评论"})
	want := "💬 新评论\n视频：视频\n链接：https://example.test/watch\n作者：作者\n评论：评论\n\n请引用回复本消息。回复内容将公开发布到对应平台。\n官方 API 不支持自动点赞，请打开链接手动操作。"
	if got != want {
		t.Fatalf("notification text = %q, want %q", got, want)
	}
}

func TestNotificationTextChargeMemberLabel(t *testing.T) {
	base := commentbridge.Comment{
		ResourceTitle: "视频",
		ResourceURL:   "https://example.test/watch",
		AuthorName:    "作者",
		Text:          "评论",
	}
	tests := []struct {
		name    string
		comment commentbridge.Comment
		want    string
	}{
		{
			name:    "bilibili charge member only",
			comment: withPlatformAndCharge(base, commentbridge.PlatformBilibili, true),
			want:    "💬 新评论\n视频：视频\n链接：https://example.test/watch\n作者：作者 ⚡充电会员\n评论：评论\n\n请引用回复本消息。回复内容将公开发布到对应平台。\n给本消息添加 👍 可点赞评论；移除不会撤销。",
		},
		{
			name:    "bilibili non-member",
			comment: withPlatformAndCharge(base, commentbridge.PlatformBilibili, false),
			want:    "💬 新评论\n视频：视频\n链接：https://example.test/watch\n作者：作者\n评论：评论\n\n请引用回复本消息。回复内容将公开发布到对应平台。\n给本消息添加 👍 可点赞评论；移除不会撤销。",
		},
		{
			name:    "youtube",
			comment: withPlatformAndCharge(base, commentbridge.PlatformYouTube, true),
			want:    "💬 新评论\n视频：视频\n链接：https://example.test/watch\n作者：作者\n评论：评论\n\n请引用回复本消息。回复内容将公开发布到对应平台。\n官方 API 不支持自动点赞，请打开链接手动操作。",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := notificationText(tt.comment); got != tt.want {
				t.Fatalf("notification text = %q, want %q", got, tt.want)
			}
		})
	}
}

func withPlatformAndCharge(comment commentbridge.Comment, platform commentbridge.Platform, chargePlus bool) commentbridge.Comment {
	comment.Platform = platform
	comment.IsChargePlus = chargePlus
	return comment
}

func TestNotificationAuthorClampsMemberLabelWithinLimit(t *testing.T) {
	author := strings.Repeat("名", 100)
	for _, tc := range []struct {
		name     string
		platform commentbridge.Platform
		charge   bool
		label    string
	}{
		{name: "bilibili", platform: commentbridge.PlatformBilibili, charge: true, label: " ⚡充电会员"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := notificationAuthor(withPlatformAndCharge(commentbridge.Comment{AuthorName: author}, tc.platform, tc.charge))
			if utf8.RuneCountInString(got) != 100 {
				t.Fatalf("author length = %d, want 100: %q", utf8.RuneCountInString(got), got)
			}
			if !strings.HasSuffix(got, tc.label) || !strings.Contains(got, "…") {
				t.Fatalf("author = %q, want clipped label %q", got, tc.label)
			}
		})
	}
}

func TestNotifyCommentUnknownOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		messageID string
		err       error
	}{
		{"network", "", errors.New("dial")},
		{"empty response", "", nil},
		{"missing message id", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t)
			client.sender = &fakeSender{messageID: tc.messageID, err: tc.err}
			_, err := client.NotifyComment(context.Background(), commentbridge.Comment{}, "a")
			if !commentbridge.IsUnknownOutcome(err) {
				t.Fatalf("error is not unknown: %v", err)
			}
		})
	}
}

func TestNotifyCommentBusinessRejectionIsDefinite(t *testing.T) {
	client := newTestClient(t)
	client.sender = &fakeSender{definite: true, err: errors.New("rejected")}
	_, err := client.NotifyComment(context.Background(), commentbridge.Comment{}, "a")
	if err == nil || commentbridge.IsUnknownOutcome(err) {
		t.Fatalf("expected definite error, got %v", err)
	}
}

func TestNotifyCredentialInvalid(t *testing.T) {
	client := newTestClient(t)
	fake := &fakeSender{messageID: "om_alert"}
	client.sender = fake
	got, err := client.NotifyCredentialInvalid(context.Background(), commentbridge.PlatformBilibili, "credential-alert-1")
	if err != nil || got != "om_alert" {
		t.Fatalf("NotifyCredentialInvalid() = %q, %v", got, err)
	}
	if fake.message.ChatID != "chat-1" || fake.message.MsgType != "text" || fake.message.UUID != "credential-alert-1" {
		t.Fatalf("unexpected request: %#v", fake.message)
	}
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(fake.message.Content), &content); err != nil {
		t.Fatalf("decode credential alert: %v", err)
	}
	if content.Text != credentialInvalidText {
		t.Fatalf("credential alert text = %q, want %q", content.Text, credentialInvalidText)
	}
	if !strings.Contains(content.Text, "⚠️") || !strings.Contains(content.Text, "B站") {
		t.Fatalf("credential alert lost Unicode text: %q", content.Text)
	}
	for _, forbidden := range []string{"account", "cookie", "path", "error"} {
		if strings.Contains(strings.ToLower(content.Text), forbidden) {
			t.Fatalf("credential alert exposed %q: %q", forbidden, content.Text)
		}
	}
}

func TestNotifyCredentialInvalidUnknownOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		messageID string
		err       error
	}{
		{name: "network", err: errors.New("dial")},
		{name: "empty response"},
		{name: "missing message id", messageID: "  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t)
			client.sender = &fakeSender{messageID: tc.messageID, err: tc.err}
			_, err := client.NotifyCredentialInvalid(context.Background(), commentbridge.PlatformBilibili, "alert")
			if !commentbridge.IsUnknownOutcome(err) {
				t.Fatalf("error is not unknown: %v", err)
			}
		})
	}
}

func TestNotifyCredentialInvalidBusinessRejectionIsDefinite(t *testing.T) {
	client := newTestClient(t)
	client.sender = &fakeSender{definite: true, err: errors.New("rejected")}
	_, err := client.NotifyCredentialInvalid(context.Background(), commentbridge.PlatformBilibili, "alert")
	if err == nil || commentbridge.IsUnknownOutcome(err) {
		t.Fatalf("expected definite error, got %v", err)
	}
}

func TestUploadCredentialQRGenerates512PNGAndUploadsMessageImage(t *testing.T) {
	client := newTestClient(t)
	fake := &fakeImageTransport{imageKey: "img_qr_1"}
	client.images = fake
	got, err := client.UploadCredentialQR(context.Background(), commentbridge.PlatformBilibili, "https://passport.bilibili.com/qr/login?key=opaque", "qr-action-1")
	if err != nil || got != "img_qr_1" {
		t.Fatalf("UploadCredentialQR() = %q, %v", got, err)
	}
	if !fake.called || fake.image.ImageType != "message" {
		t.Fatalf("unexpected image upload request: called=%v type=%q", fake.called, fake.image.ImageType)
	}
	decoded, format, err := image.Decode(bytes.NewReader(fake.image.Content))
	if err != nil {
		t.Fatalf("decode generated QR PNG: %v", err)
	}
	if format != "png" || decoded.Bounds().Dx() != credentialQRSize || decoded.Bounds().Dy() != credentialQRSize {
		t.Fatalf("generated image = format %q bounds %v, want png %dx%d", format, decoded.Bounds(), credentialQRSize, credentialQRSize)
	}
}

func TestUploadCredentialQRRejectsInvalidInputWithoutNetwork(t *testing.T) {
	for _, tc := range []struct {
		name     string
		platform commentbridge.Platform
		qrURL    string
		actionID string
	}{
		{name: "unsupported platform", platform: commentbridge.PlatformYouTube, qrURL: "https://example.test/qr", actionID: "action"},
		{name: "empty action", platform: commentbridge.PlatformBilibili, qrURL: "https://example.test/qr"},
		{name: "empty URL", platform: commentbridge.PlatformBilibili, actionID: "action"},
		{name: "URL too long", platform: commentbridge.PlatformBilibili, qrURL: strings.Repeat("x", maxCredentialQRURLBytes+1), actionID: "action"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t)
			fake := &fakeImageTransport{imageKey: "img_should_not_send"}
			client.images = fake
			_, err := client.UploadCredentialQR(context.Background(), tc.platform, tc.qrURL, tc.actionID)
			if err == nil {
				t.Fatal("UploadCredentialQR() error = nil")
			}
			if fake.called {
				t.Fatalf("image upload was called: %#v", fake.image)
			}
			if strings.Contains(err.Error(), tc.qrURL) && tc.qrURL != "" {
				t.Fatalf("validation error exposed QR URL")
			}
		})
	}
}

func TestUploadCredentialQRClassifiesTransportFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		imageKey string
		definite bool
		err      error
		unknown  bool
	}{
		{name: "business rejection", definite: true, err: errors.New("rejected"), unknown: false},
		{name: "network error", err: errors.New("network"), unknown: true},
		{name: "empty response", unknown: true},
		{name: "missing image key", imageKey: "  ", unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t)
			client.images = &fakeImageTransport{imageKey: tc.imageKey, definite: tc.definite, err: tc.err}
			_, err := client.UploadCredentialQR(context.Background(), commentbridge.PlatformBilibili, "https://example.test/qr", "action")
			if err == nil || commentbridge.IsUnknownOutcome(err) != tc.unknown {
				t.Fatalf("UploadCredentialQR() err=%v unknown=%v", err, commentbridge.IsUnknownOutcome(err))
			}
			if strings.Contains(err.Error(), "https://example.test/qr") || strings.Contains(err.Error(), "img_secret") {
				t.Fatalf("upload error exposed sensitive data: %v", err)
			}
		})
	}
}

func TestSendCredentialQRUsesGroupImageContentAndStableUUID(t *testing.T) {
	client := newTestClient(t)
	fake := &fakeSender{messageID: "om_qr"}
	client.sender = fake
	actionID := strings.Repeat("qr-action-", 8)
	got, err := client.SendCredentialQR(context.Background(), commentbridge.PlatformBilibili, "img_qr_1", actionID)
	if err != nil || got != "om_qr" {
		t.Fatalf("SendCredentialQR() = %q, %v", got, err)
	}
	if fake.message.ChatID != "chat-1" || fake.message.MsgType != "image" || fake.message.UUID != feishuUUID(actionID) {
		t.Fatalf("unexpected image message: %#v", fake.message)
	}
	var content map[string]string
	if err := json.Unmarshal([]byte(fake.message.Content), &content); err != nil {
		t.Fatalf("decode image content: %v", err)
	}
	if content["image_key"] != "img_qr_1" || len(content) != 1 {
		t.Fatalf("image content = %#v", content)
	}
}

func TestSendCredentialQRRejectsInvalidInputWithoutSending(t *testing.T) {
	for _, tc := range []struct {
		name     string
		platform commentbridge.Platform
		imageKey string
		actionID string
	}{
		{name: "unsupported platform", platform: commentbridge.PlatformYouTube, imageKey: "img", actionID: "action"},
		{name: "empty image key", platform: commentbridge.PlatformBilibili, actionID: "action"},
		{name: "empty action", platform: commentbridge.PlatformBilibili, imageKey: "img"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t)
			fake := &fakeSender{messageID: "om_should_not_send"}
			client.sender = fake
			if _, err := client.SendCredentialQR(context.Background(), tc.platform, tc.imageKey, tc.actionID); err == nil {
				t.Fatal("SendCredentialQR() error = nil")
			}
			if fake.message != (outgoingMessage{}) {
				t.Fatalf("image message was sent: %#v", fake.message)
			}
		})
	}
}

func TestSendCredentialQRClassifiesSenderFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		messageID string
		definite  bool
		err       error
		unknown   bool
	}{
		{name: "business rejection", definite: true, err: errors.New("rejected")},
		{name: "network error", err: errors.New("network"), unknown: true},
		{name: "missing message id", unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t)
			client.sender = &fakeSender{messageID: tc.messageID, definite: tc.definite, err: tc.err}
			_, err := client.SendCredentialQR(context.Background(), commentbridge.PlatformBilibili, "img_qr_1", "action")
			if err == nil || commentbridge.IsUnknownOutcome(err) != tc.unknown {
				t.Fatalf("SendCredentialQR() err=%v unknown=%v", err, commentbridge.IsUnknownOutcome(err))
			}
			if strings.Contains(err.Error(), "img_qr_1") {
				t.Fatalf("send error exposed image key: %v", err)
			}
		})
	}
}

func TestNotifyCredentialInvalidRejectsUnsupportedPlatformAndEmptyActionWithoutSending(t *testing.T) {
	for _, tc := range []struct {
		name     string
		platform commentbridge.Platform
		actionID string
	}{
		{name: "unsupported platform", platform: commentbridge.PlatformYouTube, actionID: "alert"},
		{name: "empty action", platform: commentbridge.PlatformBilibili, actionID: "  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t)
			fake := &fakeSender{messageID: "om_should_not_send"}
			client.sender = fake
			if _, err := client.NotifyCredentialInvalid(context.Background(), tc.platform, tc.actionID); err == nil {
				t.Fatal("NotifyCredentialInvalid() error = nil")
			}
			if fake.message != (outgoingMessage{}) {
				t.Fatalf("credential alert was sent: %#v", fake.message)
			}
		})
	}
}

func TestNotifyCommentRejectsEmptyActionWithoutSending(t *testing.T) {
	client := newTestClient(t)
	fake := &fakeSender{messageID: "om_123"}
	client.sender = fake
	if _, err := client.NotifyComment(context.Background(), commentbridge.Comment{}, "  "); err == nil || fake.message.ChatID != "" {
		t.Fatalf("err=%v message=%#v", err, fake.message)
	}
}

func TestNotifyCommentCompressesOverlongActionID(t *testing.T) {
	client := newTestClient(t)
	fake := &fakeSender{messageID: "om_123"}
	client.sender = fake
	longAction := strings.Repeat("a", 64)
	if _, err := client.NotifyComment(context.Background(), commentbridge.Comment{}, longAction); err != nil {
		t.Fatal(err)
	}
	if fake.message.UUID == longAction || len(fake.message.UUID) > 50 || fake.message.UUID != feishuUUID(longAction) {
		t.Fatalf("uuid=%q (%d)", fake.message.UUID, len(fake.message.UUID))
	}
}

func TestAddReaction(t *testing.T) {
	client := newTestClient(t)
	fake := &fakeReactionTransport{reactionID: "reaction-1"}
	client.reactions = fake
	got, err := client.AddReaction(context.Background(), "om_123", "OK", "action-1")
	if err != nil || got != "reaction-1" {
		t.Fatalf("AddReaction() = %q, %v", got, err)
	}
	if fake.reaction != (messageReaction{MessageID: "om_123", EmojiType: "OK"}) {
		t.Fatalf("unexpected reaction: %#v", fake.reaction)
	}
}

func TestAddReactionRejectsEmptyInputWithoutSending(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		messageID, emojiType string
		actionID             string
	}{
		{name: "message id", emojiType: "OK", actionID: "action"},
		{name: "emoji type", messageID: "om_123", actionID: "action"},
		{name: "action id", messageID: "om_123", emojiType: "OK"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t)
			fake := &fakeReactionTransport{reactionID: "reaction-1"}
			client.reactions = fake
			if _, err := client.AddReaction(context.Background(), tc.messageID, tc.emojiType, tc.actionID); err == nil {
				t.Fatal("AddReaction() error = nil")
			}
			if fake.reaction != (messageReaction{}) {
				t.Fatalf("reaction was sent: %#v", fake.reaction)
			}
		})
	}
}

func TestAddReactionClassifiesTransportFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reactionID string
		definite   bool
		err        error
		unknown    bool
	}{
		{name: "business rejection", definite: true, err: errors.New("rejected"), unknown: false},
		{name: "network error", err: errors.New("dial"), unknown: true},
		{name: "server error", err: errors.New("status 500"), unknown: true},
		{name: "nil response", unknown: true},
		{name: "success response without reaction id", reactionID: "  ", unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t)
			client.reactions = &fakeReactionTransport{reactionID: tc.reactionID, definite: tc.definite, err: tc.err}
			_, err := client.AddReaction(context.Background(), "om_sensitive", "OK", "action")
			if err == nil || commentbridge.IsUnknownOutcome(err) != tc.unknown {
				t.Fatalf("err=%v unknown=%v", err, commentbridge.IsUnknownOutcome(err))
			}
			if strings.Contains(err.Error(), "om_sensitive") {
				t.Fatalf("reaction error exposed message id: %v", err)
			}
		})
	}
}

func TestReplyToMessageCompressesOverlongActionID(t *testing.T) {
	client := newTestClient(t)
	fake := &fakeReplyTransport{messageID: "om_reply"}
	client.replies = fake
	longAction := strings.Repeat("a", 64)
	if _, err := client.ReplyToMessage(context.Background(), "om_parent", "text", longAction); err != nil {
		t.Fatal(err)
	}
	if fake.reply.UUID == longAction || len(fake.reply.UUID) > 50 || fake.reply.UUID != feishuUUID(longAction) {
		t.Fatalf("uuid=%q (%d)", fake.reply.UUID, len(fake.reply.UUID))
	}
}

func TestReplyToMessage(t *testing.T) {
	client := newTestClient(t)
	fake := &fakeReplyTransport{messageID: "om_reply"}
	client.replies = fake
	text := "点赞需在 YouTube 手动完成：\"示例\"\\下一行 👍"
	got, err := client.ReplyToMessage(context.Background(), "om_parent", text, "action-1")
	if err != nil || got != "om_reply" {
		t.Fatalf("ReplyToMessage() = %q, %v", got, err)
	}
	if fake.reply.ParentMessageID != "om_parent" || fake.reply.MsgType != "text" || fake.reply.UUID != "action-1" {
		t.Fatalf("unexpected request: %#v", fake.reply)
	}
	if !strings.Contains(fake.reply.Content, `\"`) || !strings.Contains(fake.reply.Content, `\\`) {
		t.Fatalf("reply content is not JSON-escaped: %q", fake.reply.Content)
	}
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(fake.reply.Content), &content); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if content.Text != text {
		t.Fatalf("reply text = %q, want %q", content.Text, text)
	}
}

func TestReplyToMessageRejectsEmptyRequiredInputWithoutSending(t *testing.T) {
	for _, tc := range []struct {
		name, parentMessageID, actionID string
	}{
		{name: "parent message id", actionID: "action"},
		{name: "action id", parentMessageID: "om_parent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t)
			fake := &fakeReplyTransport{messageID: "om_reply"}
			client.replies = fake
			if _, err := client.ReplyToMessage(context.Background(), tc.parentMessageID, "text", tc.actionID); err == nil {
				t.Fatal("ReplyToMessage() error = nil")
			}
			if fake.reply != (outgoingReply{}) {
				t.Fatalf("reply was sent: %#v", fake.reply)
			}
		})
	}
}

func TestReplyToMessageClassifiesTransportFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		messageID string
		definite  bool
		err       error
		unknown   bool
	}{
		{name: "business rejection", definite: true, err: errors.New("rejected")},
		{name: "network error", err: errors.New("dial"), unknown: true},
		{name: "server error", err: errors.New("status 500"), unknown: true},
		{name: "nil response", unknown: true},
		{name: "success response without message id", messageID: "  ", unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t)
			client.replies = &fakeReplyTransport{messageID: tc.messageID, definite: tc.definite, err: tc.err}
			_, err := client.ReplyToMessage(context.Background(), "om_parent", "text", "action")
			if err == nil || commentbridge.IsUnknownOutcome(err) != tc.unknown {
				t.Fatalf("err=%v unknown=%v", err, commentbridge.IsUnknownOutcome(err))
			}
			if strings.Contains(err.Error(), "om_parent") {
				t.Fatalf("reply error exposed parent message id: %v", err)
			}
		})
	}
}

func TestSDKReactionTransportBuildsTenantTokenRequest(t *testing.T) {
	httpClient := &reactionHTTPClient{t: t, status: http.StatusOK, body: `{"code":0,"data":{"reaction_id":"reaction-1"}}`}
	transport := sdkReactionTransport{client: lark.NewClient("app-id", "app-secret", lark.WithHttpClient(httpClient))}
	reactionID, definite, err := transport.Create(context.Background(), messageReaction{MessageID: "om_123", EmojiType: "OK"})
	if err != nil || definite || reactionID != "reaction-1" {
		t.Fatalf("Create() = %q, definite=%v, err=%v", reactionID, definite, err)
	}
	if !httpClient.sawTenantToken || !httpClient.sawReaction {
		t.Fatalf("expected tenant token and reaction requests, got token=%v reaction=%v", httpClient.sawTenantToken, httpClient.sawReaction)
	}
}

func TestSDKReactionTransportClassifiesResponse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		definite bool
	}{
		{name: "business code", status: http.StatusOK, body: `{"code":999}`, definite: true},
		{name: "client status", status: http.StatusBadRequest, body: `{"code":999}`, definite: true},
		{name: "server status", status: http.StatusInternalServerError, body: `{"code":0,"data":{"reaction_id":"reaction-1"}}`},
		{name: "non json", status: http.StatusOK, body: "not json"},
		{name: "missing reaction id", status: http.StatusOK, body: `{"code":0,"data":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			httpClient := &reactionHTTPClient{t: t, status: tc.status, body: tc.body}
			transport := sdkReactionTransport{client: lark.NewClient("app-id", "app-secret", lark.WithHttpClient(httpClient))}
			_, definite, err := transport.Create(context.Background(), messageReaction{MessageID: "om_123", EmojiType: "OK"})
			if err == nil || definite != tc.definite {
				t.Fatalf("Create() definite=%v err=%v", definite, err)
			}
			if strings.Contains(err.Error(), "om_123") || strings.Contains(err.Error(), "reaction-1") {
				t.Fatalf("reaction error exposed response data: %v", err)
			}
		})
	}
}

func TestSDKImageTransportBuildsTenantTokenMultipartRequest(t *testing.T) {
	httpClient := &imageHTTPClient{t: t, status: http.StatusOK, body: `{"code":0,"data":{"image_key":"img_qr_1"}}`}
	transport := sdkImageTransport{client: lark.NewClient("image-upload-build-app", "app-secret", lark.WithHttpClient(httpClient))}
	imageKey, definite, err := transport.Upload(context.Background(), imageUpload{ImageType: "message", Content: testPNG(t)})
	if err != nil || definite || imageKey != "img_qr_1" {
		t.Fatalf("Upload() = %q, definite=%v, err=%v", imageKey, definite, err)
	}
	if !httpClient.sawTenantToken || !httpClient.sawImage {
		t.Fatalf("expected tenant token and image requests, got token=%v image=%v", httpClient.sawTenantToken, httpClient.sawImage)
	}
}

func TestSDKImageTransportClassifiesResponse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		definite bool
	}{
		{name: "business code", status: http.StatusOK, body: `{"code":999}`, definite: true},
		{name: "client status", status: http.StatusBadRequest, body: `{"code":999}`, definite: true},
		{name: "server status", status: http.StatusInternalServerError, body: `{"code":0,"data":{"image_key":"img_qr_1"}}`},
		{name: "non json", status: http.StatusOK, body: "not json"},
		{name: "missing image key", status: http.StatusOK, body: `{"code":0,"data":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			httpClient := &imageHTTPClient{t: t, status: tc.status, body: tc.body}
			transport := sdkImageTransport{client: lark.NewClient("image-upload-"+tc.name, "app-secret", lark.WithHttpClient(httpClient))}
			_, definite, err := transport.Upload(context.Background(), imageUpload{ImageType: "message", Content: testPNG(t)})
			if err == nil || definite != tc.definite {
				t.Fatalf("Upload() definite=%v err=%v", definite, err)
			}
			if strings.Contains(err.Error(), "img_qr_1") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("image upload error exposed sensitive data: %v", err)
			}
		})
	}
}

func TestSDKImageTransportNetworkErrorIsUnknown(t *testing.T) {
	httpClient := &imageHTTPClient{t: t, err: errors.New("network contains secret")}
	transport := sdkImageTransport{client: lark.NewClient("image-upload-network-app", "app-secret", lark.WithHttpClient(httpClient))}
	_, definite, err := transport.Upload(context.Background(), imageUpload{ImageType: "message", Content: testPNG(t)})
	if err == nil || definite || strings.Contains(err.Error(), "secret") {
		t.Fatalf("Upload() definite=%v err=%v", definite, err)
	}
}

func TestSDKReplyTransportBuildsTenantTokenRequest(t *testing.T) {
	httpClient := &replyHTTPClient{t: t, status: http.StatusOK, body: `{"code":0,"data":{"message_id":"om_reply"}}`, expectedUUID: "action-1", expectedText: `手动点赞 "quoted" 👍`}
	transport := sdkReplyTransport{client: lark.NewClient("reply-app-id", "app-secret", lark.WithHttpClient(httpClient))}
	replyID, definite, err := transport.Reply(context.Background(), outgoingReply{
		ParentMessageID: "om_parent",
		MsgType:         "text",
		Content:         `{"text":"手动点赞 \"quoted\" 👍"}`,
		UUID:            "action-1",
	})
	if err != nil || definite || replyID != "om_reply" {
		t.Fatalf("Reply() = %q, definite=%v, err=%v", replyID, definite, err)
	}
	if !httpClient.sawTenantToken || !httpClient.sawReply {
		t.Fatalf("expected tenant token and reply requests, got token=%v reply=%v", httpClient.sawTenantToken, httpClient.sawReply)
	}
}

func TestSDKReplyTransportClassifiesResponse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		definite bool
	}{
		{name: "business code", status: http.StatusOK, body: `{"code":999}`, definite: true},
		{name: "client status", status: http.StatusBadRequest, body: `{"code":999}`, definite: true},
		{name: "server status", status: http.StatusInternalServerError, body: `{"code":0,"data":{"message_id":"om_reply"}}`},
		{name: "gateway timeout", status: http.StatusGatewayTimeout, body: `{"code":0}`},
		{name: "redirect", status: http.StatusFound, body: `{"code":0}`},
		{name: "non json", status: http.StatusOK, body: "not json"},
		{name: "missing message id", status: http.StatusOK, body: `{"code":0,"data":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			httpClient := &replyHTTPClient{t: t, status: tc.status, body: tc.body}
			transport := sdkReplyTransport{client: lark.NewClient("app-id", "app-secret", lark.WithHttpClient(httpClient))}
			_, definite, err := transport.Reply(context.Background(), outgoingReply{ParentMessageID: "om_parent", MsgType: "text", Content: `{"text":"secret reply"}`, UUID: "action"})
			if err == nil || definite != tc.definite {
				t.Fatalf("Reply() definite=%v err=%v", definite, err)
			}
			if strings.Contains(err.Error(), "om_parent") || strings.Contains(err.Error(), "secret reply") || strings.Contains(err.Error(), "om_reply") {
				t.Fatalf("reply error exposed sensitive request or response data: %v", err)
			}
		})
	}
}

type reactionHTTPClient struct {
	t              *testing.T
	status         int
	body           string
	err            error
	sawTenantToken bool
	sawReaction    bool
}

type imageHTTPClient struct {
	t              *testing.T
	status         int
	body           string
	err            error
	sawTenantToken bool
	sawImage       bool
}

type replyHTTPClient struct {
	t              *testing.T
	status         int
	body           string
	sawTenantToken bool
	sawReply       bool
	expectedUUID   string
	expectedText   string
}

func (c *replyHTTPClient) Do(req *http.Request) (*http.Response, error) {
	c.t.Helper()
	if req.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
		c.sawTenantToken = true
		return jsonResponse(http.StatusOK, `{"code":0,"tenant_access_token":"tenant-token","expire":7200}`), nil
	}
	if req.URL.Path != "/open-apis/im/v1/messages/om_parent/reply" {
		c.t.Fatalf("unexpected request path: %s", req.URL.Path)
	}
	c.sawReply = true
	if got := req.Header.Get("Authorization"); got != "Bearer tenant-token" {
		c.t.Fatalf("authorization = %q", got)
	}
	var body struct {
		Content string `json:"content"`
		MsgType string `json:"msg_type"`
		UUID    string `json:"uuid"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		c.t.Fatalf("decode reply request: %v", err)
	}
	if body.MsgType != "text" {
		c.t.Fatalf("reply body = %#v", body)
	}
	if c.expectedUUID != "" && body.UUID != c.expectedUUID {
		c.t.Fatalf("reply uuid = %q, want %q", body.UUID, c.expectedUUID)
	}
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(body.Content), &content); err != nil {
		c.t.Fatalf("decode reply content: %v", err)
	}
	if c.expectedText != "" && content.Text != c.expectedText {
		c.t.Fatalf("reply text = %q", content.Text)
	}
	return jsonResponse(c.status, c.body), nil
}

func (c *reactionHTTPClient) Do(req *http.Request) (*http.Response, error) {
	c.t.Helper()
	if req.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
		c.sawTenantToken = true
		return jsonResponse(http.StatusOK, `{"code":0,"tenant_access_token":"tenant-token","expire":7200}`), nil
	}
	if req.URL.Path != "/open-apis/im/v1/messages/om_123/reactions" {
		c.t.Fatalf("unexpected request path: %s", req.URL.Path)
	}
	c.sawReaction = true
	if got := req.Header.Get("Authorization"); got != "Bearer tenant-token" {
		c.t.Fatalf("authorization = %q", got)
	}
	var body struct {
		ReactionType struct {
			EmojiType string `json:"emoji_type"`
		} `json:"reaction_type"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		c.t.Fatalf("decode reaction request: %v", err)
	}
	if body.ReactionType.EmojiType != "OK" {
		c.t.Fatalf("emoji_type = %q", body.ReactionType.EmojiType)
	}
	return jsonResponse(c.status, c.body), nil
}

func (c *imageHTTPClient) Do(req *http.Request) (*http.Response, error) {
	c.t.Helper()
	if c.err != nil {
		return nil, c.err
	}
	if req.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
		c.sawTenantToken = true
		return jsonResponse(http.StatusOK, `{"code":0,"tenant_access_token":"tenant-token","expire":7200}`), nil
	}
	if req.URL.Path != "/open-apis/im/v1/images" {
		c.t.Fatalf("unexpected request path: %s", req.URL.Path)
	}
	c.sawImage = true
	if got := req.Header.Get("Authorization"); got != "Bearer tenant-token" {
		c.t.Fatalf("authorization = %q", got)
	}
	if err := req.ParseMultipartForm(2 << 20); err != nil {
		c.t.Fatalf("parse image multipart request: %v", err)
	}
	if got := req.FormValue("image_type"); got != "message" {
		c.t.Fatalf("image_type = %q", got)
	}
	file, _, err := req.FormFile("image")
	if err != nil {
		c.t.Fatalf("image part missing: %v", err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		c.t.Fatalf("read image part: %v", err)
	}
	if _, format, err := image.Decode(bytes.NewReader(data)); err != nil || format != "png" {
		c.t.Fatalf("image part is not PNG: format=%q err=%v", format, err)
	}
	return jsonResponse(c.status, c.body), nil
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	data, err := qrcode.Encode("test", qrcode.Medium, 8)
	if err != nil {
		t.Fatalf("generate test PNG: %v", err)
	}
	return data
}

func TestNotificationTextClampsUntrustedFields(t *testing.T) {
	client := newTestClient(t)
	fake := &fakeSender{messageID: "om_123"}
	client.sender = fake
	long := strings.Repeat("评", 4000)
	if _, err := client.NotifyComment(context.Background(), commentbridge.Comment{Platform: commentbridge.PlatformBilibili, Text: long, ResourceTitle: long, AuthorName: long}, "action"); err != nil {
		t.Fatal(err)
	}
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(fake.message.Content), &content); err != nil {
		t.Fatalf("decode notification: %v", err)
	}
	if len([]rune(content.Text)) > maxNotificationRunes || !strings.Contains(content.Text, "…") || !strings.HasSuffix(content.Text, "给本消息添加 👍 可点赞评论；移除不会撤销。") {
		t.Fatalf("notification was not clamped with its footer: %d runes: %q", len([]rune(content.Text)), content.Text)
	}
}

func TestNewValidatesMutuallyExclusiveReplyPolicies(t *testing.T) {
	base := Config{AppID: "id", AppSecret: "secret", ChatID: "chat"}
	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{name: "neither policy", config: base, wantErr: true},
		{name: "both policies", config: Config{AppID: "id", AppSecret: "secret", ChatID: "chat", AllowAllGroupMembers: true, AllowedOpenIDs: []string{"ou_allowed"}}, wantErr: true},
		{name: "allow all", config: Config{AppID: "id", AppSecret: "secret", ChatID: "chat", AllowAllGroupMembers: true}, wantErr: false},
		{name: "allowlist", config: Config{AppID: "id", AppSecret: "secret", ChatID: "chat", AllowedOpenIDs: []string{" ou_allowed "}}, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := New(tt.config)
			if (err != nil) != tt.wantErr {
				t.Fatalf("New() client=%#v err=%v wantErr=%v", client, err, tt.wantErr)
			}
		})
	}
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(Config{AppID: "id", AppSecret: "secret", ChatID: "chat-1", AllowedOpenIDs: []string{"ou_allowed"}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func ptr(s string) *string { return &s }

func TestReplyHelpUsesNativeFeishuEmoji(t *testing.T) {
	client := newTestClient(t)
	fake := &fakeReplyTransport{messageID: "om_help"}
	client.replies = fake
	if _, err := client.ReplyToMessage(t.Context(), "om_parent", commentbridge.HelpText, "help-action"); err != nil {
		t.Fatal(err)
	}
	if fake.reply.MsgType != "post" || fake.reply.ParentMessageID != "om_parent" || fake.reply.UUID != "help-action" {
		t.Fatalf("wrong help envelope: %+v", fake.reply)
	}
	var content struct {
		ZhCN struct {
			Content [][]map[string]string `json:"content"`
		} `json:"zh_cn"`
	}
	if err := json.Unmarshal([]byte(fake.reply.Content), &content); err != nil {
		t.Fatal(err)
	}
	var emoji []string
	var rebuilt []string
	for _, row := range content.ZhCN.Content {
		var line strings.Builder
		for _, element := range row {
			switch element["tag"] {
			case "text":
				line.WriteString(element["text"])
			case "emotion":
				emoji = append(emoji, element["emoji_type"])
				line.WriteString("[" + element["emoji_type"] + "]")
			default:
				t.Fatalf("unexpected help element: %+v", element)
			}
		}
		rebuilt = append(rebuilt, line.String())
	}
	if strings.Join(emoji, ",") != "THUMBSUP,HEART,ThumbsDown,MinusOne" {
		t.Fatalf("wrong native emoji: %v", emoji)
	}
	if strings.Join(rebuilt, "\n") != commentbridge.HelpText {
		t.Fatal("help wording or line breaks changed")
	}
}
