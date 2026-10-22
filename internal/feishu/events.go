package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/larksuite/oapi-sdk-go/v3/ws"

	"comment-bridge/internal/commentbridge"
)

// Start opens the official Feishu long connection and blocks while it runs.
func (c *Client) Start(ctx context.Context, sink EventSink) error {
	d := c.Dispatcher(sink)
	return ws.NewClient(c.config.AppID, c.config.AppSecret, ws.WithEventHandler(d), ws.WithLogLevel(larkcore.LogLevelWarn)).Start(ctx)
}

// Run owns a long connection until ctx is cancelled. The upstream SDK's
// Start method blocks forever even after Close, so Run isolates it in a
// goroutine and lets the process shut down promptly after closing the socket.
func (c *Client) Run(ctx context.Context, sink EventSink) error {
	d := c.Dispatcher(sink)
	client := ws.NewClient(c.config.AppID, c.config.AppSecret, ws.WithEventHandler(d), ws.WithLogLevel(larkcore.LogLevelWarn))
	errCh := make(chan error, 1)
	go func() { errCh <- client.Start(ctx) }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		client.Close()
		return nil
	}
}

// Dispatcher returns the official SDK dispatcher used by the long connection.
func (c *Client) Dispatcher(sink EventSink) *dispatcher.EventDispatcher {
	return dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
			err := c.HandleMessage(ctx, event, sink)
			if c.OnEventError != nil {
				c.OnEventError(ctx, err)
			}
			return err
		}).
		OnP2MessageReactionCreatedV1(func(ctx context.Context, event *larkim.P2MessageReactionCreatedV1) error {
			err := c.HandleReactionCreated(ctx, event, sink)
			if c.OnEventError != nil {
				c.OnEventError(ctx, err)
			}
			return err
		})
}

// EventSink receives both human replies and explicit reaction approvals.
type EventSink interface {
	commentbridge.ReplySink
	commentbridge.EngagementSink
}

func (c *Client) acceptsChat(chatID string) bool {
	return chatID != "" && (chatID == c.config.ChatID || chatID == c.config.YouTubeChatID)
}

// HandleMessage validates and normalizes one SDK message-receive event.
func (c *Client) HandleMessage(ctx context.Context, event *larkim.P2MessageReceiveV1, sink commentbridge.ReplySink) error {
	if event == nil || event.Event == nil || event.Event.Message == nil || event.Event.Sender == nil || sink == nil {
		return nil
	}
	m, sender := event.Event.Message, event.Event.Sender
	if value(sender.SenderType) != "user" || value(m.ChatType) != "group" || !c.acceptsChat(value(m.ChatId)) || value(m.MessageType) != "text" {
		return nil
	}
	if value(m.MessageId) == "" || value(m.ParentId) == "" || sender.SenderId == nil {
		return nil
	}
	openID := strings.TrimSpace(value(sender.SenderId.OpenId))
	if openID == "" {
		return nil
	}
	if !c.config.AllowAllGroupMembers {
		if _, ok := c.allowed[openID]; !ok {
			return nil
		}
	}
	text, ok := parseText(value(m.Content), m.Mentions)
	if !ok || text == "" {
		return nil
	}
	_, err := sink.AcceptReply(ctx, commentbridge.IncomingReply{
		MessageID: value(m.MessageId), ParentMessageID: value(m.ParentId), RootMessageID: value(m.RootId),
		ChatID: value(m.ChatId), UserID: openID, Text: text, ReceivedAt: eventTime(value(m.CreateTime)),
	})
	if errors.Is(err, commentbridge.ErrUnmappedReply) || errors.Is(err, commentbridge.ErrInvalidReply) {
		return nil
	}
	return err
}

// HandleReactionCreated validates and normalizes one SDK message-reaction event.
func (c *Client) HandleReactionCreated(ctx context.Context, event *larkim.P2MessageReactionCreatedV1, sink commentbridge.EngagementSink) error {
	if event == nil || event.EventV2Base == nil || event.EventV2Base.Header == nil || event.Event == nil || event.Event.ReactionType == nil || event.Event.UserId == nil || sink == nil {
		return nil
	}
	e := event.Event
	eventID := strings.TrimSpace(event.EventV2Base.Header.EventID)
	messageID := strings.TrimSpace(value(e.MessageId))
	openID := strings.TrimSpace(value(e.UserId.OpenId))
	if eventID == "" || messageID == "" || openID == "" || value(e.OperatorType) != "user" || value(e.ReactionType.EmojiType) != "THUMBSUP" {
		return nil
	}
	if !c.config.AllowAllGroupMembers {
		if _, ok := c.allowed[openID]; !ok {
			return nil
		}
	}
	chatIDs := []string{c.config.ChatID}
	if c.config.YouTubeChatID != "" && c.config.YouTubeChatID != c.config.ChatID {
		chatIDs = append(chatIDs, c.config.YouTubeChatID)
	}
	for _, chatID := range chatIDs {
		_, err := sink.AcceptEngagement(ctx, commentbridge.IncomingEngagement{
			EventID: eventID, MessageID: messageID, UserID: openID, ExpectedChatID: chatID,
			ReactionType: "THUMBSUP", EmojiType: "THUMBSUP", ReceivedAt: eventTime(value(e.ActionTime)),
		})
		if errors.Is(err, commentbridge.ErrUnmappedEngagement) {
			continue
		}
		if errors.Is(err, commentbridge.ErrUnsupportedEngagement) || errors.Is(err, commentbridge.ErrInvalidEngagement) {
			return nil
		}
		return err
	}
	return nil
}

var mention = regexp.MustCompile(`<at\b[^>]*>.*?</at>`)

func parseText(content string, mentions []*larkim.MentionEvent) (string, bool) {
	var body struct {
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(content), &body) != nil {
		return "", false
	}
	text := mention.ReplaceAllString(body.Text, "")
	for _, item := range mentions {
		if item == nil || value(item.Key) == "" {
			continue
		}
		replacement := ""
		if value(item.MentionedType) == "user" && value(item.Name) != "" {
			replacement = "@" + value(item.Name)
		}
		text = strings.ReplaceAll(text, value(item.Key), replacement)
	}
	return strings.TrimSpace(text), true
}

func eventTime(raw string) time.Time {
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func value(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
