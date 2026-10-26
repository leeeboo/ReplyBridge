package feishu

import (
	"comment-bridge/internal/commentbridge"
	"context"
	"fmt"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

type moderationRecorder struct{ calls int }

func (*moderationRecorder) Platform() commentbridge.Platform { return commentbridge.PlatformBilibili }
func (m *moderationRecorder) Moderate(_ context.Context, c commentbridge.Comment, k commentbridge.ModerationKind) (string, error) {
	m.calls++
	return c.ID, nil
}

func TestBothGroupHelpAndModerationEvents(t *testing.T) {
	for _, p := range []commentbridge.Platform{commentbridge.PlatformBilibili, commentbridge.PlatformYouTube} {
		for _, emoji := range []string{"ThumbsDown", "MinusOne"} {
			t.Run(string(p)+emoji, func(t *testing.T) {
				ctx := t.Context()
				s, err := commentbridge.Open(":memory:")
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				client, err := New(Config{AppID: "app", AppSecret: "secret", ChatID: "bili", YouTubeChatID: "youtube", AllowAllGroupMembers: true})
				if err != nil {
					t.Fatal(err)
				}
				client.botOpenID = "bot-self"
				sender := &fakeSender{messageID: "notice"}
				client.sender = sender
				transport := &fakeReplyTransport{messageID: "prompt"}
				client.replies = transport
				moderator := &moderationRecorder{}
				bridge := &commentbridge.Bridge{Store: s, Notifier: client, MessageReplier: client, TargetChatID: "bili", YouTubeTargetChatID: "youtube", Moderators: map[commentbridge.Platform]commentbridge.CommentModerator{p: moderator}}
				chat := "bili"
				if p == commentbridge.PlatformYouTube {
					chat = "youtube"
				}
				if err = s.RecordPoll(ctx, p, "account", []commentbridge.Comment(nil)); err != nil {
					t.Fatal(err)
				}
				c := commentbridge.Comment{Platform: p, AccountID: "account", ID: "child", RootID: "root", ResourceID: "video", AuthorID: "author"}
				if err = s.RecordPoll(ctx, p, "account", []commentbridge.Comment{c}); err != nil {
					t.Fatal(err)
				}
				if err = bridge.NotifyOnce(ctx); err != nil {
					t.Fatal(err)
				}
				reaction := reactionEvent("event", "notice", "user", "member", emoji, strconv.FormatInt(time.Now().UnixMilli(), 10))
				for range 2 {
					if err = client.HandleReactionCreated(ctx, reaction, bridge); err != nil {
						t.Fatal(err)
					}
				}
				if err = bridge.ModerationOnce(ctx); err != nil || moderator.calls != 0 {
					t.Fatal("premature action", err)
				}
				if err = bridge.BotCommandOnce(ctx); err != nil {
					t.Fatal(err)
				}
				if transport.reply.ParentMessageID != "notice" || !strings.Contains(transport.reply.Content, "二次确认") {
					t.Fatal("missing prompt")
				}
				for i, user := range []string{"stranger", "member", "member"} {
					event := receiveEvent(chat, "user", user, "text", `{"text":"确认"}`, "prompt", "notice", nil)
					event.Event.Message.MessageId = ptr(fmt.Sprint("confirm", i))
					event.Event.Message.CreateTime = ptr(strconv.FormatInt(time.Now().UnixMilli(), 10))
					if err = client.HandleMessage(ctx, event, bridge); err != nil {
						t.Fatal(err)
					}
					if err = bridge.ModerationOnce(ctx); err != nil {
						t.Fatal(err)
					}
					if i == 0 && moderator.calls != 0 {
						t.Fatal("stranger confirmed")
					}
				}
				if moderator.calls != 1 {
					t.Fatalf("moderation calls=%d", moderator.calls)
				}
				// Help must mention this bot, in either configured group, and never creates a platform reply.
				for i, bot := range []string{"other-bot", "bot-self"} {
					mentions := []*larkim.MentionEvent{{Key: ptr("@_user_1"), MentionedType: ptr("bot"), Id: &larkim.UserId{OpenId: ptr(bot)}}}
					event := receiveEvent(chat, "user", "member", "text", `{"text":"@_user_1 HeLp"}`, "", "", mentions)
					event.Event.Message.MessageId = ptr(fmt.Sprint("help", i))
					for range 2 {
						if err = client.HandleMessage(ctx, event, bridge); err != nil {
							t.Fatal(err)
						}
					}
				}
				var helpCount, replies int
				if err = s.DB().QueryRow(`SELECT count(*) FROM bot_command_jobs WHERE text=?`, commentbridge.HelpText).Scan(&helpCount); err != nil || helpCount != 1 {
					t.Fatal("help count", helpCount, err)
				}
				if err = s.DB().QueryRow(`SELECT count(*) FROM reply_jobs`).Scan(&replies); err != nil || replies != 0 {
					t.Fatal("command leaked to platform", err)
				}
			})
		}
	}
}

func TestBotIdentityRejectsMalformedOrMissingIdentity(t *testing.T) {
	for i, body := range []string{`{"code":0,"bot":{"open_id":"bot-self"}}`, `{"bot":{"open_id":"bot-self"}}`, `{"code":1,"bot":{"open_id":"bot-self"}}`, `{"code":0}`, `bad`} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			client := newTestClient(t)
			client.sdkClient = lark.NewClient(fmt.Sprint("identity-test-", i), "secret", lark.WithHttpClient(&http.Client{Transport: identityRoundTrip(func(req *http.Request) (*http.Response, error) {
				response := body
				if strings.Contains(req.URL.Path, "tenant_access_token") {
					response = `{"code":0,"tenant_access_token":"test-token","expire":7200}`
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
			})}))
			err := client.loadBotIdentity(t.Context())
			if (err == nil) != (i == 0) || (client.botOpenID == "bot-self") != (i == 0) {
				t.Fatalf("identity accepted incorrectly: %v", err)
			}
		})
	}
}

type identityRoundTrip func(*http.Request) (*http.Response, error)

func (f identityRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
