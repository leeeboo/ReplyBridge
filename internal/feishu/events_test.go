package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"comment-bridge/internal/commentbridge"
)

type fakeSink struct {
	replies       []commentbridge.IncomingReply
	engagements   []commentbridge.IncomingEngagement
	err           error
	engagementErr error
}

func (s *fakeSink) AcceptReply(_ context.Context, reply commentbridge.IncomingReply) (bool, error) {
	s.replies = append(s.replies, reply)
	return s.err == nil, s.err
}

func (s *fakeSink) AcceptEngagement(_ context.Context, engagement commentbridge.IncomingEngagement) (bool, error) {
	s.engagements = append(s.engagements, engagement)
	return s.engagementErr == nil, s.engagementErr
}

func TestHandleMessageAcceptsQuotedAllowedUser(t *testing.T) {
	c := newTestClient(t)
	sink := &fakeSink{}
	err := c.HandleMessage(context.Background(), receiveEvent("chat-1", "user", "ou_allowed", "text", `{"text":"@_user_1  你好"}`, "parent", "root", []*larkim.MentionEvent{{Key: ptr("@_user_1"), MentionedType: ptr("bot")}}), sink)
	if err != nil || len(sink.replies) != 1 {
		t.Fatalf("err=%v replies=%#v", err, sink.replies)
	}
	r := sink.replies[0]
	if r.Text != "你好" || r.MessageID != "message" || r.ParentMessageID != "parent" || r.RootMessageID != "root" || r.UserID != "ou_allowed" {
		t.Fatalf("reply=%#v", r)
	}
}

func TestHandleMessageAllowAllGroupMembersAcceptsExternalOpenID(t *testing.T) {
	c, err := New(Config{AppID: "id", AppSecret: "secret", ChatID: "chat-1", AllowAllGroupMembers: true})
	if err != nil {
		t.Fatal(err)
	}
	sink := &fakeSink{}
	err = c.HandleMessage(context.Background(), receiveEvent("chat-1", "user", "ou_external_member", "text", `{"text":"reply"}`, "parent", "root", nil), sink)
	if err != nil || len(sink.replies) != 1 || sink.replies[0].UserID != "ou_external_member" {
		t.Fatalf("err=%v replies=%#v", err, sink.replies)
	}
}

func TestHandleMessageAllowAllGroupMembersRejectsEmptyOpenID(t *testing.T) {
	c, err := New(Config{AppID: "id", AppSecret: "secret", ChatID: "chat-1", AllowAllGroupMembers: true})
	if err != nil {
		t.Fatal(err)
	}
	sink := &fakeSink{}
	err = c.HandleMessage(context.Background(), receiveEvent("chat-1", "user", "  ", "text", `{"text":"reply"}`, "parent", "root", nil), sink)
	if err != nil || len(sink.replies) != 0 {
		t.Fatalf("err=%v replies=%#v", err, sink.replies)
	}
}

func TestHandleMessageRequiresDirectParentReference(t *testing.T) {
	c := newTestClient(t)
	sink := &fakeSink{}
	if err := c.HandleMessage(context.Background(), receiveEvent("chat-1", "user", "ou_allowed", "text", `{"text":"reply"}`, "parent", "", nil), sink); err != nil || len(sink.replies) != 1 {
		t.Fatalf("direct parent reference was rejected: err=%v replies=%#v", err, sink.replies)
	}
	sink = &fakeSink{}
	if err := c.HandleMessage(context.Background(), receiveEvent("chat-1", "user", "ou_allowed", "text", `{"text":"reply"}`, "", "root", nil), sink); err != nil || len(sink.replies) != 0 {
		t.Fatalf("root-only thread message was accepted: err=%v replies=%#v", err, sink.replies)
	}
}

func TestHandleMessagePreservesHumanMentionName(t *testing.T) {
	c := newTestClient(t)
	sink := &fakeSink{}
	mentions := []*larkim.MentionEvent{
		{Key: ptr("@_user_1"), MentionedType: ptr("bot"), Name: ptr("bot")},
		{Key: ptr("@_user_2"), MentionedType: ptr("user"), Name: ptr("小明")},
	}
	err := c.HandleMessage(context.Background(), receiveEvent("chat-1", "user", "ou_allowed", "text", `{"text":"@_user_1 谢谢 @_user_2"}`, "parent", "root", mentions), sink)
	if err != nil || len(sink.replies) != 1 || sink.replies[0].Text != "谢谢 @小明" {
		t.Fatalf("err=%v replies=%#v", err, sink.replies)
	}
}

func TestHandleMessageRejectsInvalidEvents(t *testing.T) {
	c := newTestClient(t)
	p2p := receiveEvent("chat-1", "user", "ou_allowed", "text", `{"text":"reply"}`, "p", "r", nil)
	p2p.Event.Message.ChatType = ptr("p2p")
	p2pSink := &fakeSink{}
	if err := c.HandleMessage(context.Background(), p2p, p2pSink); err != nil || len(p2pSink.replies) != 0 {
		t.Fatalf("p2p event was accepted: err=%v replies=%#v", err, p2pSink.replies)
	}
	cases := []struct{ name, chat, senderType, openID, messageType, content, parent, root string }{
		{"wrong group", "other", "user", "ou_allowed", "text", `{"text":"reply"}`, "p", "r"},
		{"bot", "chat-1", "bot", "ou_allowed", "text", `{"text":"reply"}`, "p", "r"},
		{"unauthorized", "chat-1", "user", "ou_other", "text", `{"text":"reply"}`, "p", "r"},
		{"non text", "chat-1", "user", "ou_allowed", "image", `{"text":"reply"}`, "p", "r"},
		{"no reference", "chat-1", "user", "ou_allowed", "text", `{"text":"reply"}`, "", ""},
		{"empty", "chat-1", "user", "ou_allowed", "text", `{"text":" <at user_id=\"x\">bot</at> "}`, "p", "r"},
		{"bad json", "chat-1", "user", "ou_allowed", "text", `not json`, "p", "r"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &fakeSink{}
			if err := c.HandleMessage(context.Background(), receiveEvent(tc.chat, tc.senderType, tc.openID, tc.messageType, tc.content, tc.parent, tc.root, nil), sink); err != nil {
				t.Fatal(err)
			}
			if len(sink.replies) != 0 {
				t.Fatalf("unexpected reply: %#v", sink.replies)
			}
		})
	}
}

func TestHandleMessagePropagatesSinkError(t *testing.T) {
	c := newTestClient(t)
	want := errors.New("store unavailable")
	err := c.HandleMessage(context.Background(), receiveEvent("chat-1", "user", "ou_allowed", "text", `{"text":"reply"}`, "p", "r", nil), &fakeSink{err: want})
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func TestHandleMessageIgnoresUnmappedParent(t *testing.T) {
	c := newTestClient(t)
	err := c.HandleMessage(context.Background(), receiveEvent("chat-1", "user", "ou_allowed", "text", `{"text":"reply"}`, "unmapped", "", nil), &fakeSink{err: commentbridge.ErrUnmappedReply})
	if err != nil {
		t.Fatalf("unmapped parent should be acknowledged and ignored: %v", err)
	}
}

func TestHandleReactionCreatedAcceptsAllowedUser(t *testing.T) {
	c := newTestClient(t)
	sink := &fakeSink{}
	err := c.HandleReactionCreated(context.Background(), reactionEvent("event-1", "message-1", "user", "ou_allowed", "THUMBSUP", "1700000000123"), sink)
	if err != nil || len(sink.engagements) != 1 {
		t.Fatalf("err=%v engagements=%#v", err, sink.engagements)
	}
	got := sink.engagements[0]
	if got.EventID != "event-1" || got.MessageID != "message-1" || got.UserID != "ou_allowed" || got.ExpectedChatID != "chat-1" || got.ReactionType != "THUMBSUP" || got.EmojiType != "THUMBSUP" || got.ReceivedAt.Location().String() != "UTC" || got.ReceivedAt.UnixMilli() != 1700000000123 {
		t.Fatalf("engagement=%#v", got)
	}
}

func TestHandleReactionCreatedAllowAllGroupMembersAcceptsExternalOpenID(t *testing.T) {
	c, err := New(Config{AppID: "id", AppSecret: "secret", ChatID: "chat-1", AllowAllGroupMembers: true})
	if err != nil {
		t.Fatal(err)
	}
	sink := &fakeSink{}
	err = c.HandleReactionCreated(context.Background(), reactionEvent("event-1", "message-1", "user", "ou_external_member", "THUMBSUP", "bad"), sink)
	if err != nil || len(sink.engagements) != 1 || !sink.engagements[0].ReceivedAt.IsZero() {
		t.Fatalf("err=%v engagements=%#v", err, sink.engagements)
	}
}

func TestHandleReactionCreatedIgnoresInvalidEvents(t *testing.T) {
	c := newTestClient(t)
	cases := []struct {
		name, eventID, messageID, operator, openID, emoji string
	}{
		{"app operator", "event", "message", "app", "ou_allowed", "THUMBSUP"},
		{"other emoji", "event", "message", "user", "ou_allowed", "OK"},
		{"missing event id", "", "message", "user", "ou_allowed", "THUMBSUP"},
		{"missing message id", "event", "", "user", "ou_allowed", "THUMBSUP"},
		{"missing open id", "event", "message", "user", "", "THUMBSUP"},
		{"unauthorized", "event", "message", "user", "ou_other", "THUMBSUP"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &fakeSink{}
			if err := c.HandleReactionCreated(context.Background(), reactionEvent(tc.eventID, tc.messageID, tc.operator, tc.openID, tc.emoji, "1700000000123"), sink); err != nil {
				t.Fatal(err)
			}
			if len(sink.engagements) != 0 {
				t.Fatalf("unexpected engagements=%#v", sink.engagements)
			}
		})
	}
}

func TestHandleReactionCreatedIgnoresExpectedSinkErrors(t *testing.T) {
	c := newTestClient(t)
	for _, want := range []error{commentbridge.ErrUnmappedEngagement, commentbridge.ErrUnsupportedEngagement, commentbridge.ErrInvalidEngagement} {
		t.Run(want.Error(), func(t *testing.T) {
			err := c.HandleReactionCreated(context.Background(), reactionEvent("event", "message", "user", "ou_allowed", "THUMBSUP", "1700000000123"), &fakeSink{engagementErr: want})
			if err != nil {
				t.Fatalf("expected safe ignore, got %v", err)
			}
		})
	}
}

func TestHandleReactionCreatedPropagatesSinkError(t *testing.T) {
	c := newTestClient(t)
	want := errors.New("store unavailable")
	err := c.HandleReactionCreated(context.Background(), reactionEvent("event", "message", "user", "ou_allowed", "THUMBSUP", "1700000000123"), &fakeSink{engagementErr: want})
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func TestDispatcherRegistersMessageAndReactionHandlers(t *testing.T) {
	c := newTestClient(t)
	sink := &fakeSink{}
	d := c.Dispatcher(sink)
	messageEvent := receiveEvent("chat-1", "user", "ou_allowed", "text", `{"text":"reply"}`, "parent", "root", nil)
	messageEvent.EventV2Base = &larkevent.EventV2Base{Header: &larkevent.EventHeader{EventType: "im.message.receive_v1"}}
	message, err := json.Marshal(messageEvent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Do(context.Background(), message); err != nil {
		t.Fatalf("message dispatch: %v", err)
	}
	reaction := []byte(`{"schema":"2.0","header":{"event_id":"event-1","event_type":"im.message.reaction.created_v1"},"event":{"message_id":"message-1","reaction_type":{"emoji_type":"THUMBSUP"},"operator_type":"user","user_id":{"open_id":"ou_allowed"},"action_time":"1700000000123"}}`)
	if _, err = d.Do(context.Background(), reaction); err != nil {
		t.Fatalf("reaction dispatch: %v", err)
	}
	if len(sink.replies) != 1 || len(sink.engagements) != 1 {
		t.Fatalf("replies=%#v engagements=%#v", sink.replies, sink.engagements)
	}
}

func receiveEvent(chat, senderType, openID, messageType, content, parent, root string, mentions []*larkim.MentionEvent) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{
		Sender:  &larkim.EventSender{SenderType: ptr(senderType), SenderId: &larkim.UserId{OpenId: ptr(openID)}},
		Message: &larkim.EventMessage{MessageId: ptr("message"), ChatId: ptr(chat), ChatType: ptr("group"), MessageType: ptr(messageType), Content: ptr(content), ParentId: ptr(parent), RootId: ptr(root), Mentions: mentions, CreateTime: ptr("1700000000123")},
	}}
}

func reactionEvent(eventID, messageID, operator, openID, emoji, actionTime string) *larkim.P2MessageReactionCreatedV1 {
	return &larkim.P2MessageReactionCreatedV1{
		EventV2Base: &larkevent.EventV2Base{Header: &larkevent.EventHeader{EventID: eventID}},
		Event: &larkim.P2MessageReactionCreatedV1Data{
			MessageId: ptr(messageID), ReactionType: &larkim.Emoji{EmojiType: ptr(emoji)}, OperatorType: ptr(operator),
			UserId: &larkim.UserId{OpenId: ptr(openID)}, ActionTime: ptr(actionTime),
		},
	}
}
