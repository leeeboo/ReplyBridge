package commentbridge

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

type moderationMock struct {
	calls   int
	comment Comment
	kind    ModerationKind
	err     error
}

func (m *moderationMock) Platform() Platform { return m.comment.Platform }
func (m *moderationMock) Moderate(_ context.Context, c Comment, k ModerationKind) (string, error) {
	m.calls++
	m.comment = c
	m.kind = k
	return c.ID, m.err
}

type commandMock struct {
	calls int
	texts []string
	err   error
}

func (m *commandMock) ReplyToMessage(_ context.Context, parent, text, action string) (string, error) {
	m.calls++
	m.texts = append(m.texts, text)
	return fmt.Sprint("prompt-", m.calls), m.err
}

func moderationFixture(t *testing.T, p Platform) (*Bridge, *moderationMock, *commandMock, string) {
	t.Helper()
	s := testStore(t)
	ctx := t.Context()
	c := testComment()
	c.Platform = p
	c.ID = "child"
	c.RootID = "root"
	c.AuthorID = "author"
	if err := s.RecordPoll(ctx, p, c.AccountID, []Comment{c}); err != nil {
		t.Fatal(err)
	}
	chat := "bili-chat"
	if p == PlatformYouTube {
		chat = "youtube-chat"
	}
	if _, err := s.db.Exec(`INSERT INTO feishu_messages(message_id,chat_id,platform,account_id,comment_id) VALUES('notice',?,?,?,'child')`, chat, p, c.AccountID); err != nil {
		t.Fatal(err)
	}
	m, n := &moderationMock{}, &commandMock{}
	return &Bridge{Store: s, TargetChatID: "bili-chat", YouTubeTargetChatID: "youtube-chat", Moderators: map[Platform]CommentModerator{p: m}, MessageReplier: n}, m, n, chat
}
func requestModeration(t *testing.T, b *Bridge, chat, emoji, event string) {
	t.Helper()
	ok, err := b.AcceptEngagement(t.Context(), IncomingEngagement{EventID: event, MessageID: "notice", UserID: "user", ExpectedChatID: chat, ReactionType: emoji, ReceivedAt: time.Now()})
	if err != nil || !ok {
		t.Fatalf("request: %v %v", ok, err)
	}
}
func confirmModeration(b *Bridge, ctx context.Context, chat, user, parent, id, text string) (bool, error) {
	return b.AcceptReply(ctx, IncomingReply{MessageID: id, ParentMessageID: parent, ChatID: chat, UserID: user, Text: text, ReceivedAt: time.Now()})
}

func TestModerationRequiresSameInitiatorExactPromptAndDeduplicates(t *testing.T) {
	for _, p := range []Platform{PlatformBilibili, PlatformYouTube} {
		for _, emoji := range []string{"ThumbsDown", "MinusOne"} {
			t.Run(string(p)+emoji, func(t *testing.T) {
				b, m, _, chat := moderationFixture(t, p)
				ctx := t.Context()
				requestModeration(t, b, chat, emoji, "event")
				if err := b.ModerationOnce(ctx); err != nil || m.calls != 0 {
					t.Fatalf("unconfirmed write: %v", err)
				}
				if err := b.BotCommandOnce(ctx); err != nil {
					t.Fatal(err)
				}
				for _, tc := range []struct{ chat, user, parent string }{{chat, "other", "prompt-1"}, {"wrong", "user", "prompt-1"}, {chat, "user", "unmapped"}} {
					ok, err := confirmModeration(b, ctx, tc.chat, tc.user, tc.parent, "bad", "确认")
					if ok || err != nil && !errors.Is(err, ErrUnmappedReply) {
						t.Fatalf("unsafe confirm: %v %v", ok, err)
					}
				}
				ok, err := b.AcceptReply(ctx, IncomingReply{MessageID: "root-only", RootMessageID: "prompt-1", ChatID: chat, UserID: "user", Text: "确认", ReceivedAt: time.Now()})
				if ok || !errors.Is(err, ErrUnmappedReply) {
					t.Fatalf("root accepted: %v %v", ok, err)
				}
				ok, err = confirmModeration(b, ctx, chat, "user", "prompt-1", "confirm", "确认")
				if !ok || err != nil {
					t.Fatalf("confirm %v %v", ok, err)
				}
				for range 2 {
					if ok, err := confirmModeration(b, ctx, chat, "user", "prompt-1", "confirm", "确认"); ok || err != nil {
						t.Fatal("duplicate accepted")
					}
					if err := b.ModerationOnce(ctx); err != nil {
						t.Fatal(err)
					}
				}
				if m.calls != 1 || m.comment.ID != "child" || m.comment.RootID != "root" || m.kind != moderationKind(emoji) {
					t.Fatalf("wrong action: %+v", m)
				}
				requestModeration(t, b, chat, emoji, "new-event")
				for range 3 {
					if err := b.BotCommandOnce(ctx); err != nil {
						t.Fatal(err)
					}
				}
				ok, err = confirmModeration(b, ctx, chat, "user", "prompt-4", "new-confirm", "确认")
				if ok || err != nil {
					t.Fatalf("same platform action repeated %v %v", ok, err)
				}
				if err := b.ModerationOnce(ctx); err != nil || m.calls != 1 {
					t.Fatal("duplicate write")
				}
				counts, err := b.Store.StatusCounts(ctx)
				if err != nil || counts.Replies[JobPending] != 0 || counts.Moderations[JobSent] != 1 {
					t.Fatalf("counts %#v %v", counts, err)
				}
			})
		}
	}
}

func TestModerationCancelledExpiredAndOldEvents(t *testing.T) {
	for _, mode := range []string{"cancel", "expire", "prompt-unknown"} {
		t.Run(mode, func(t *testing.T) {
			b, m, n, chat := moderationFixture(t, PlatformBilibili)
			ctx := t.Context()
			for _, in := range []IncomingEngagement{
				{EventID: "wrong", MessageID: "notice", UserID: "user", ExpectedChatID: "youtube-chat", ReactionType: "ThumbsDown", ReceivedAt: time.Now()},
				{EventID: "old", MessageID: "notice", UserID: "user", ExpectedChatID: chat, ReactionType: "ThumbsDown", ReceivedAt: time.Now().Add(-6 * time.Minute)},
				{EventID: "missing-time", MessageID: "notice", UserID: "user", ExpectedChatID: chat, ReactionType: "ThumbsDown"},
			} {
				if ok, _ := b.AcceptEngagement(ctx, in); ok {
					t.Fatal("invalid request accepted")
				}
			}
			requestModeration(t, b, chat, "ThumbsDown", "event")
			if mode == "prompt-unknown" {
				n.err = unknownErr{errors.New("timeout")}
			}
			if err := b.BotCommandOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if mode == "expire" {
				if _, err := b.Store.db.Exec(`UPDATE moderation_requests SET expires_at=?`, time.Now().Add(-time.Second).UnixNano()); err != nil {
					t.Fatal(err)
				}
			}
			text := "确认"
			if mode == "cancel" {
				text = "取消"
			}
			ok, err := confirmModeration(b, ctx, chat, "user", "prompt-1", "confirm", text)
			if ok || err != nil && !errors.Is(err, ErrUnmappedReply) {
				t.Fatalf("unexpected confirmation %v %v", ok, err)
			}
			if err := b.ModerationOnce(ctx); err != nil || m.calls != 0 {
				t.Fatalf("unapproved action %v", err)
			}
			if mode != "prompt-unknown" {
				requestModeration(t, b, chat, "ThumbsDown", "retry-event")
			}
		})
	}
}

func TestModerationUnknownAndRestartNeverReplay(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprint(interrupted), func(t *testing.T) {
			b, m, _, chat := moderationFixture(t, PlatformYouTube)
			ctx := t.Context()
			requestModeration(t, b, chat, "MinusOne", "event")
			if err := b.BotCommandOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if ok, err := confirmModeration(b, ctx, chat, "user", "prompt-1", "confirm", "确认"); !ok || err != nil {
				t.Fatal(err)
			}
			m.err = unknownErr{errors.New("timeout")}
			if interrupted {
				if _, err := b.Store.db.Exec(`UPDATE moderation_jobs SET status='sending'`); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := b.ModerationOnce(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.Store.RecoverSending(ctx); err != nil {
				t.Fatal(err)
			}
			before := m.calls
			for range 3 {
				if err := b.ModerationOnce(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if m.calls != before {
				t.Fatal("unknown replayed")
			}
			counts, err := b.Store.StatusCounts(ctx)
			if err != nil || counts.Moderations[JobUnknown] != 1 {
				t.Fatalf("unknown not preserved %#v %v", counts, err)
			}
		})
	}
}

func TestModerationPersistentPromptAndMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	b := &Bridge{Store: s, TargetChatID: "chat", MessageReplier: &commandMock{}}
	c := testComment()
	c.AuthorID = "author"
	if err = s.RecordPoll(t.Context(), c.Platform, c.AccountID, []Comment{c}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO feishu_messages VALUES('notice','chat',?,?,?)`, c.Platform, c.AccountID, c.ID); err != nil {
		t.Fatal(err)
	}
	// Simulate the previous schema and verify additive migration preserves the mapping.
	if _, err = s.db.Exec(`DROP TABLE moderation_jobs; DROP TABLE bot_command_jobs; DROP TABLE moderation_requests;`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	b.Store = s
	requestModeration(t, b, "chat", "ThumbsDown", "event")
	if err = b.BotCommandOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b.Store = s
	if ok, err := confirmModeration(b, t.Context(), "chat", "user", "prompt-1", "confirm", "确认"); !ok || err != nil {
		t.Fatalf("restart lost prompt %v %v", ok, err)
	}
	var integrity string
	if err = s.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal("integrity", err)
	}
}
