// Package feishu adapts the official Feishu SDK to commentbridge.
package feishu

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	qrcode "github.com/skip2/go-qrcode"

	"comment-bridge/internal/commentbridge"
)

// Config contains the credentials and policy for one Feishu bot.
// AppSecret is intentionally never logged or returned by this package.
type Config struct {
	AppID          string
	AppSecret      string
	ChatID         string
	YouTubeChatID  string
	AllowedOpenIDs []string
	// AllowAllGroupMembers permits human members of the configured groups to reply by
	// directly quoting a bot notification. It is mutually exclusive with
	// AllowedOpenIDs so operators cannot mistake it for an additional filter.
	AllowAllGroupMembers bool
}

// Client sends notifications and receives approved quoted replies.
type Client struct {
	config       Config
	sdkClient    *lark.Client
	botOpenID    string
	allowed      map[string]struct{}
	sender       messageSender
	images       imageTransport
	reactions    reactionTransport
	replies      replyTransport
	OnEventError func(context.Context, error)
}

// messageSender keeps notification tests independent of the SDK transport.
type messageSender interface {
	Send(context.Context, outgoingMessage) (messageID string, definiteRejection bool, err error)
}

type sdkSender struct{ client *lark.Client }

// imageTransport keeps credential-QR upload tests independent of the SDK
// transport. Content is the generated PNG; the transport must never persist
// it or include it in an error.
type imageTransport interface {
	Upload(context.Context, imageUpload) (imageKey string, definiteRejection bool, err error)
}

type sdkImageTransport struct{ client *lark.Client }

type imageUpload struct {
	ImageType string
	Content   []byte
}

// reactionTransport keeps reaction tests independent of the SDK transport.
// definiteRejection is true only when Feishu has definitely rejected the
// request; all other failures could have reached the API and must not retry.
type reactionTransport interface {
	Create(context.Context, messageReaction) (reactionID string, definiteRejection bool, err error)
}

type sdkReactionTransport struct{ client *lark.Client }

// replyTransport keeps message-reply tests independent of the SDK transport.
// definiteRejection is true only when Feishu has definitely rejected the
// request; all other failures could have reached the API and must not retry.
type replyTransport interface {
	Reply(context.Context, outgoingReply) (messageID string, definiteRejection bool, err error)
}

type sdkReplyTransport struct{ client *lark.Client }

type messageReaction struct {
	MessageID string
	EmojiType string
}

type outgoingMessage struct {
	ChatID  string
	MsgType string
	Content string
	UUID    string
}

type outgoingReply struct {
	ParentMessageID string
	MsgType         string
	Content         string
	UUID            string
}

func (s sdkSender) Send(ctx context.Context, message outgoingMessage) (string, bool, error) {
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(message.ChatID).
			MsgType(message.MsgType).
			Content(message.Content).
			Uuid(message.UUID).
			Build()).
		Build()
	resp, err := s.client.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return "", false, err
	}
	if resp == nil {
		return "", false, errors.New("empty feishu response")
	}
	if !resp.Success() {
		return "", true, fmt.Errorf("feishu rejected message (code %d)", resp.Code)
	}
	if resp.Data == nil || resp.Data.MessageId == nil || strings.TrimSpace(*resp.Data.MessageId) == "" {
		return "", false, errors.New("feishu success response has no message id")
	}
	return *resp.Data.MessageId, false, nil
}

func (s sdkImageTransport) Upload(ctx context.Context, image imageUpload) (string, bool, error) {
	req := larkim.NewCreateImageReqBuilder().
		Body(larkim.NewCreateImageReqBodyBuilder().
			ImageType(image.ImageType).
			Image(bytes.NewReader(image.Content)).
			Build()).
		Build()
	resp, err := s.client.Im.V1.Image.Create(ctx, req)
	if err != nil {
		// SDK errors can include the request URL or response body. Do not
		// surface either because this upload is part of a credential flow.
		return "", false, errors.New("feishu image upload request failed")
	}
	if resp == nil || resp.ApiResp == nil {
		return "", false, errors.New("empty feishu image upload response")
	}
	status := resp.StatusCode
	if status >= 500 || status < 200 || (status >= 300 && status < 400) {
		return "", false, fmt.Errorf("unexpected feishu image upload status %d", status)
	}
	if status >= 400 {
		return "", true, fmt.Errorf("feishu rejected image upload (status %d)", status)
	}
	if !resp.Success() {
		return "", true, fmt.Errorf("feishu rejected image upload (code %d)", resp.Code)
	}
	if resp.Data == nil || resp.Data.ImageKey == nil || strings.TrimSpace(*resp.Data.ImageKey) == "" {
		return "", false, errors.New("feishu image upload response has no image key")
	}
	return strings.TrimSpace(*resp.Data.ImageKey), false, nil
}

func (s sdkReactionTransport) Create(ctx context.Context, reaction messageReaction) (string, bool, error) {
	req := larkim.NewCreateMessageReactionReqBuilder().
		MessageId(reaction.MessageID).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().
			ReactionType(larkim.NewEmojiBuilder().EmojiType(reaction.EmojiType).Build()).
			Build()).
		Build()
	resp, err := s.client.Im.V1.MessageReaction.Create(ctx, req)
	if err != nil {
		// SDK errors can include request paths or response content. Do not
		// surface either because paths contain the Feishu message ID.
		return "", false, errors.New("feishu reaction request failed")
	}
	if resp == nil || resp.ApiResp == nil {
		return "", false, errors.New("empty feishu reaction response")
	}
	status := resp.StatusCode
	if status >= 500 || status < 200 || (status >= 300 && status < 400) {
		return "", false, fmt.Errorf("unexpected feishu reaction status %d", status)
	}
	if status >= 400 {
		return "", true, fmt.Errorf("feishu rejected reaction (status %d)", status)
	}
	if !resp.Success() {
		return "", true, fmt.Errorf("feishu rejected reaction (code %d)", resp.Code)
	}
	if resp.Data == nil || resp.Data.ReactionId == nil || strings.TrimSpace(*resp.Data.ReactionId) == "" {
		return "", false, errors.New("feishu success response has no reaction id")
	}
	return *resp.Data.ReactionId, false, nil
}

func (s sdkReplyTransport) Reply(ctx context.Context, reply outgoingReply) (string, bool, error) {
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(reply.ParentMessageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			Content(reply.Content).
			MsgType(reply.MsgType).
			Uuid(reply.UUID).
			Build()).
		Build()
	resp, err := s.client.Im.V1.Message.Reply(ctx, req)
	if err != nil {
		// SDK errors may include response content. Do not surface it because it
		// can contain the reply text.
		return "", false, errors.New("feishu reply request failed")
	}
	if resp == nil || resp.ApiResp == nil {
		return "", false, errors.New("empty feishu reply response")
	}
	status := resp.StatusCode
	if status >= 500 || status < 200 || (status >= 300 && status < 400) {
		return "", false, fmt.Errorf("unexpected feishu reply status %d", status)
	}
	if status >= 400 {
		return "", true, fmt.Errorf("feishu rejected reply (status %d)", status)
	}
	if !resp.Success() {
		return "", true, fmt.Errorf("feishu rejected reply (code %d)", resp.Code)
	}
	if resp.Data == nil || resp.Data.MessageId == nil || strings.TrimSpace(*resp.Data.MessageId) == "" {
		return "", false, errors.New("feishu success response has no reply message id")
	}
	return *resp.Data.MessageId, false, nil
}

// New creates a bot client. The reply policy must be exactly one of a
// non-empty allowlist or all human members of the configured group.
func New(config Config) (*Client, error) {
	if strings.TrimSpace(config.AppID) == "" {
		return nil, errors.New("feishu app id is required")
	}
	if strings.TrimSpace(config.AppSecret) == "" {
		return nil, errors.New("feishu app secret is required")
	}
	if strings.TrimSpace(config.ChatID) == "" {
		return nil, errors.New("feishu chat id is required")
	}
	allowed := make(map[string]struct{}, len(config.AllowedOpenIDs))
	for _, openID := range config.AllowedOpenIDs {
		if openID = strings.TrimSpace(openID); openID != "" {
			allowed[openID] = struct{}{}
		}
	}
	if config.AllowAllGroupMembers && len(allowed) != 0 {
		return nil, errors.New("feishu allow all group members cannot be combined with allowed open ids")
	}
	if !config.AllowAllGroupMembers && len(allowed) == 0 {
		return nil, errors.New("feishu allowed open ids are required")
	}
	sdkClient := lark.NewClient(config.AppID, config.AppSecret, lark.WithLogLevel(larkcore.LogLevelWarn))
	return &Client{
		config:    config,
		sdkClient: sdkClient,
		allowed:   allowed,
		sender:    sdkSender{client: sdkClient},
		images:    sdkImageTransport{client: sdkClient},
		reactions: sdkReactionTransport{client: sdkClient},
		replies:   sdkReplyTransport{client: sdkClient},
	}, nil
}

func (c *Client) commentChatID(platform commentbridge.Platform) string {
	if platform == commentbridge.PlatformYouTube && c.config.YouTubeChatID != "" {
		return c.config.YouTubeChatID
	}
	return c.config.ChatID
}

// NotifyComment posts a human-readable notification to the configured group.
// actionID is passed to Feishu as its one-hour request de-duplication UUID.
func (c *Client) NotifyComment(ctx context.Context, comment commentbridge.Comment, actionID string) (string, error) {
	if strings.TrimSpace(actionID) == "" {
		return "", errors.New("feishu notification action id is required")
	}
	content, err := json.Marshal(map[string]string{"text": notificationText(comment)})
	if err != nil {
		return "", fmt.Errorf("encode feishu notification: %w", err)
	}
	messageID, definiteRejection, err := c.sender.Send(ctx, outgoingMessage{ChatID: c.commentChatID(comment.Platform), MsgType: "text", Content: string(content), UUID: feishuUUID(actionID)})
	if err != nil {
		if definiteRejection {
			return "", err
		}
		return "", unknownOutcome{cause: err}
	}
	if strings.TrimSpace(messageID) == "" {
		return "", unknownOutcome{cause: errors.New("feishu sender returned no message id")}
	}
	return messageID, nil
}

const credentialInvalidText = "⚠️ B站登录凭据已失效或不完整\n评论监控、回复和点赞可能无法正常执行。\n请重新扫码登录 B站，更新服务器凭据后重启服务。"

const (
	credentialQRSize        = 512
	maxCredentialQRURLBytes = 2048
)

// UploadCredentialQR renders the Bilibili login URL in memory and uploads it
// as a Feishu message image. The URL is deliberately bounded before QR
// generation so malformed operator input cannot trigger unbounded work.
func (c *Client) UploadCredentialQR(ctx context.Context, platform commentbridge.Platform, qrURL, actionID string) (string, error) {
	if platform != commentbridge.PlatformBilibili {
		return "", fmt.Errorf("unsupported credential QR platform %q", platform)
	}
	if strings.TrimSpace(actionID) == "" {
		return "", errors.New("feishu credential QR action id is required")
	}
	qrURL = strings.TrimSpace(qrURL)
	if qrURL == "" {
		return "", errors.New("feishu credential QR URL is required")
	}
	if len([]byte(qrURL)) > maxCredentialQRURLBytes {
		return "", errors.New("feishu credential QR URL is too long")
	}
	png, err := qrcode.Encode(qrURL, qrcode.Medium, credentialQRSize)
	if err != nil {
		// Do not return the QR URL because it is an active login credential.
		return "", errors.New("generate credential QR failed")
	}
	if c.images == nil {
		return "", unknownOutcome{cause: errors.New("feishu image transport is unavailable")}
	}
	imageKey, definiteRejection, err := c.images.Upload(ctx, imageUpload{ImageType: "message", Content: png})
	if err != nil {
		if definiteRejection {
			return "", errors.New("feishu credential QR upload was rejected")
		}
		return "", unknownOutcome{cause: err}
	}
	if strings.TrimSpace(imageKey) == "" {
		return "", unknownOutcome{cause: errors.New("feishu image transport returned no image key")}
	}
	return strings.TrimSpace(imageKey), nil
}

// SendCredentialQR posts the uploaded credential image to the configured
// group. The image key is sent only as Feishu image content; actionID is the
// stable one-hour Feishu de-duplication UUID.
func (c *Client) SendCredentialQR(ctx context.Context, platform commentbridge.Platform, imageKey, actionID string) (string, error) {
	if platform != commentbridge.PlatformBilibili {
		return "", fmt.Errorf("unsupported credential QR platform %q", platform)
	}
	if strings.TrimSpace(imageKey) == "" {
		return "", errors.New("feishu credential QR image key is required")
	}
	if strings.TrimSpace(actionID) == "" {
		return "", errors.New("feishu credential QR action id is required")
	}
	content, err := json.Marshal(map[string]string{"image_key": strings.TrimSpace(imageKey)})
	if err != nil {
		return "", errors.New("encode feishu credential QR message failed")
	}
	messageID, definiteRejection, err := c.sender.Send(ctx, outgoingMessage{
		ChatID:  c.config.ChatID,
		MsgType: "image",
		Content: string(content),
		UUID:    feishuUUID(actionID),
	})
	if err != nil {
		if definiteRejection {
			return "", errors.New("feishu credential QR message was rejected")
		}
		return "", unknownOutcome{cause: err}
	}
	if strings.TrimSpace(messageID) == "" {
		return "", unknownOutcome{cause: errors.New("feishu sender returned no credential QR message id")}
	}
	return messageID, nil
}

// NotifyCredentialInvalid alerts the configured Feishu group that the
// Bilibili credential has stopped working. The alert intentionally contains
// no account, credential, path, or upstream error details.
func (c *Client) NotifyCredentialInvalid(ctx context.Context, platform commentbridge.Platform, actionID string) (string, error) {
	if strings.TrimSpace(actionID) == "" {
		return "", errors.New("feishu credential alert action id is required")
	}
	if platform != commentbridge.PlatformBilibili {
		return "", fmt.Errorf("unsupported credential alert platform %q", platform)
	}
	return c.SendErrorAlert(ctx, credentialInvalidText, actionID)
}

// SendErrorAlert sends fixed operator text supplied by the durable alert collector.
func (c *Client) SendErrorAlert(ctx context.Context, text, actionID string) (string, error) {
	if strings.TrimSpace(actionID) == "" || strings.TrimSpace(text) == "" {
		return "", errors.New("feishu error alert content and action id are required")
	}
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return "", fmt.Errorf("encode feishu credential alert: %w", err)
	}
	messageID, definiteRejection, err := c.sender.Send(ctx, outgoingMessage{
		ChatID:  c.config.ChatID,
		MsgType: "text",
		Content: string(content),
		UUID:    feishuUUID(actionID),
	})
	if err != nil {
		if definiteRejection {
			return "", err
		}
		return "", unknownOutcome{cause: err}
	}
	if strings.TrimSpace(messageID) == "" {
		return "", unknownOutcome{cause: errors.New("feishu sender returned no message id")}
	}
	return messageID, nil
}

// AddReaction adds emojiType to the specified Feishu message. Feishu does not
// expose an idempotency field for this endpoint; actionID is still required so
// callers use the same durable action contract as other external writes.
func (c *Client) AddReaction(ctx context.Context, messageID, emojiType, actionID string) (string, error) {
	if strings.TrimSpace(messageID) == "" {
		return "", errors.New("feishu reaction message id is required")
	}
	if strings.TrimSpace(emojiType) == "" {
		return "", errors.New("feishu reaction emoji type is required")
	}
	if strings.TrimSpace(actionID) == "" {
		return "", errors.New("feishu reaction action id is required")
	}
	reactionID, definiteRejection, err := c.reactions.Create(ctx, messageReaction{MessageID: messageID, EmojiType: emojiType})
	if err != nil {
		if definiteRejection {
			return "", err
		}
		return "", unknownOutcome{cause: err}
	}
	if strings.TrimSpace(reactionID) == "" {
		return "", unknownOutcome{cause: errors.New("feishu reaction transport returned no reaction id")}
	}
	return reactionID, nil
}

// ReplyToMessage posts text as a direct reply to parentMessageID. actionID is
// supplied to Feishu as its one-hour request de-duplication UUID.
func (c *Client) ReplyToMessage(ctx context.Context, parentMessageID, text, actionID string) (string, error) {
	if strings.TrimSpace(parentMessageID) == "" {
		return "", errors.New("feishu reply parent message id is required")
	}
	if strings.TrimSpace(actionID) == "" {
		return "", errors.New("feishu reply action id is required")
	}
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return "", fmt.Errorf("encode feishu reply: %w", err)
	}
	messageID, definiteRejection, err := c.replies.Reply(ctx, outgoingReply{
		ParentMessageID: parentMessageID,
		MsgType:         "text",
		Content:         string(content),
		UUID:            feishuUUID(actionID),
	})
	if err != nil {
		if definiteRejection {
			return "", err
		}
		return "", unknownOutcome{cause: err}
	}
	if strings.TrimSpace(messageID) == "" {
		return "", unknownOutcome{cause: errors.New("feishu reply transport returned no message id")}
	}
	return messageID, nil
}

// Feishu limits uuid to 50 characters. Store action IDs intentionally remain
// full audit identifiers; overlong values are deterministically compressed to
// a 43-character base64url SHA-256 key before crossing the API boundary.
func feishuUUID(actionID string) string {
	if len(actionID) <= 50 {
		return actionID
	}
	sum := sha256.Sum256([]byte(actionID))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

const maxNotificationRunes = 3400

func notificationText(comment commentbridge.Comment) string {
	suffix := "\n\n请引用回复本消息。回复内容将公开发布到对应平台。"
	if footer := notificationFooter(comment.Platform); footer != "" {
		suffix += "\n" + footer
	}
	return formatNotification(comment, suffix)
}

func formatNotification(comment commentbridge.Comment, suffix string) string {
	prefix := fmt.Sprintf("💬 新评论\n视频：%s\n链接：%s\n作者：%s\n评论：",
		singleLine(comment.ResourceTitle, 200), singleLine(comment.ResourceURL, 1000), notificationAuthor(comment))
	remaining := maxNotificationRunes - utf8.RuneCountInString(prefix) - utf8.RuneCountInString(suffix)
	return prefix + clampRunesWithin(comment.Text, remaining) + suffix
}

func notificationAuthor(comment commentbridge.Comment) string {
	author := singleLine(comment.AuthorName, 100)
	if comment.Platform != commentbridge.PlatformBilibili || !comment.IsChargePlus {
		return author
	}
	memberLabel := " ⚡充电会员"
	return clampRunesWithin(author, 100-utf8.RuneCountInString(memberLabel)) + memberLabel
}

func notificationFooter(platform commentbridge.Platform) string {
	switch platform {
	case commentbridge.PlatformBilibili:
		return "给本消息添加 👍 可点赞评论；移除不会撤销。"
	case commentbridge.PlatformYouTube:
		return "官方 API 不支持自动点赞，请打开链接手动操作。"
	default:
		return ""
	}
}

func singleLine(value string, max int) string {
	value = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(value)
	return clampRunes(value, max)
}

func clampRunes(value string, max int) string {
	value = strings.TrimSpace(value)
	if max <= 0 || utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max]) + "…"
}

func clampRunesWithin(value string, max int) string {
	value = strings.TrimSpace(value)
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(value) <= max {
		return value
	}
	if max == 1 {
		return "…"
	}
	runes := []rune(value)
	return string(runes[:max-1]) + "…"
}

type unknownOutcome struct{ cause error }

func (e unknownOutcome) Error() string        { return "feishu message delivery outcome unknown" }
func (e unknownOutcome) Unwrap() error        { return e.cause }
func (e unknownOutcome) OutcomeUnknown() bool { return true }

var _ commentbridge.Notifier = (*Client)(nil)
var _ commentbridge.CredentialAlertNotifier = (*Client)(nil)
var _ commentbridge.CredentialQRNotifier = (*Client)(nil)
var _ commentbridge.Reactor = (*Client)(nil)
var _ commentbridge.MessageReplier = (*Client)(nil)
