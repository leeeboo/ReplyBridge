package commentbridge

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeNotifier struct {
	calls int
	err   error
	after func()
}

func (f *fakeNotifier) NotifyComment(context.Context, Comment, string) (string, error) {
	f.calls++
	if f.after != nil {
		f.after()
	}
	return "f1", f.err
}

type unknownErr struct{ error }

func (unknownErr) OutcomeUnknown() bool { return true }

func TestYouTubeWebhookRoutingAndDeduplication(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status JobStatus
	}{
		{"sent", nil, JobSent},
		{"unknown", unknownErr{errors.New("timeout")}, JobUnknown},
		{"rejected", errors.New("rejected"), JobFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := t.Context()
			bot, webhook := &fakeNotifier{}, &fakeNotifier{err: tc.err}
			b := Bridge{Store: s, Notifier: bot, YouTubeWebhook: webhook, TargetChatID: "chat-1"}
			for _, platform := range []Platform{PlatformBilibili, PlatformYouTube} {
				c := testComment()
				c.Platform = platform
				if err := s.RecordPoll(ctx, platform, "a", []Comment{c}); err != nil {
					t.Fatal(err)
				}
				if err := b.NotifyOnce(ctx); err != nil {
					t.Fatal(err)
				}
				if (platform == PlatformBilibili && bot.calls != 0) || webhook.calls != 0 {
					t.Fatal("baseline notified")
				}
				c.ID = "new"
				if err := s.RecordPoll(ctx, platform, "a", []Comment{c}); err != nil {
					t.Fatal(err)
				}
				if err := b.NotifyOnce(ctx); err != nil {
					t.Fatal(err)
				}
				if err := s.RecordPoll(ctx, platform, "a", []Comment{c}); err != nil {
					t.Fatal(err)
				}
				if err := b.NotifyOnce(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if bot.calls != 1 || webhook.calls != 1 {
				t.Fatalf("bot=%d webhook=%d", bot.calls, webhook.calls)
			}
			var status JobStatus
			if err := s.DB().QueryRowContext(ctx, `SELECT status FROM notification_jobs WHERE platform=?`, PlatformYouTube).Scan(&status); err != nil || status != tc.status {
				t.Fatalf("status=%s err=%v", status, err)
			}
			var mappings int
			if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM feishu_messages WHERE platform=?`, PlatformYouTube).Scan(&mappings); err != nil || mappings != 0 {
				t.Fatalf("webhook mappings=%d err=%v", mappings, err)
			}
			ok, err := b.AcceptReply(ctx, IncomingReply{MessageID: "reply", ParentMessageID: "f1", ChatID: "chat-1", UserID: "user", Text: "reply"})
			if err != nil || !ok {
				t.Fatalf("bilibili quote mapping lost: %v", err)
			}
			var platform Platform
			if err := s.DB().QueryRowContext(ctx, `SELECT platform FROM reply_jobs`).Scan(&platform); err != nil || platform != PlatformBilibili {
				t.Fatalf("reply platform=%s err=%v", platform, err)
			}
		})
	}
}

func TestNotifyUnknownIsNeverRetried(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c := testComment()
	if e := s.RecordPoll(ctx, PlatformBilibili, "a", []Comment{c}); e != nil {
		t.Fatal(e)
	}
	c.ID = "new"
	if e := s.RecordPoll(ctx, PlatformBilibili, "a", []Comment{c}); e != nil {
		t.Fatal(e)
	}
	n := &fakeNotifier{err: unknownErr{errors.New("timeout")}}
	b := Bridge{Store: s, Notifier: n, TargetChatID: "chat-1"}
	if e := b.NotifyOnce(ctx); e != nil {
		t.Fatal(e)
	}
	if e := b.NotifyOnce(ctx); e != nil {
		t.Fatal(e)
	}
	if n.calls != 1 {
		t.Fatalf("calls=%d", n.calls)
	}
	counts, e := s.StatusCounts(ctx)
	if e != nil || counts.Notifications[JobUnknown] != 1 {
		t.Fatalf("counts=%#v err=%v", counts, e)
	}
	jobs, e := s.ListTerminalJobs(ctx, JobUnknown)
	if e != nil || len(jobs) != 1 || jobs[0].Kind != "notification" || jobs[0].FeishuMessageID != "" {
		t.Fatalf("jobs=%#v err=%v", jobs, e)
	}
	ok, e := s.ResetUnknownToPending(ctx, jobs[0].ActionID)
	if e != nil || !ok {
		t.Fatalf("reset=%v err=%v", ok, e)
	}
	if e := b.NotifyOnce(ctx); e != nil {
		t.Fatal(e)
	}
	if n.calls != 2 {
		t.Fatalf("calls=%d", n.calls)
	}
}

func TestCancelledTerminalWriteFallsBackToUnknown(t *testing.T) {
	s := testStore(t)
	base := context.Background()
	c := testComment()
	if err := s.RecordPoll(base, PlatformBilibili, "a", []Comment{c}); err != nil {
		t.Fatal(err)
	}
	c.ID = "new"
	if err := s.RecordPoll(base, PlatformBilibili, "a", []Comment{c}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(base)
	n := &fakeNotifier{after: cancel}
	b := Bridge{Store: s, Notifier: n, TargetChatID: "chat-1"}
	if err := b.NotifyOnce(ctx); err == nil {
		t.Fatal("expected cancelled terminal persistence error")
	}
	counts, err := s.StatusCounts(base)
	if err != nil || counts.Notifications[JobUnknown] != 1 || counts.Notifications[JobSending] != 0 {
		t.Fatalf("counts=%#v err=%v", counts, err)
	}
}

type mutableSource struct{ comments []Comment }

func (s *mutableSource) Platform() Platform { return PlatformYouTube }
func (s *mutableSource) AccountID() string  { return "channel" }
func (s *mutableSource) ListRecent(context.Context) ([]Comment, error) {
	return append([]Comment(nil), s.comments...), nil
}

type fakeReplier struct {
	calls   int
	comment Comment
	text    string
	err     error
}

type fakeEngager struct {
	calls   int
	comment Comment
	kind    EngagementKind
	err     error
	after   func()
}

func (e *fakeEngager) Platform() Platform { return PlatformBilibili }
func (e *fakeEngager) Engage(_ context.Context, comment Comment, kind EngagementKind) (string, error) {
	e.calls++
	e.comment, e.kind = comment, kind
	if e.after != nil {
		e.after()
	}
	return "platform-engagement", e.err
}

type fakeReactor struct {
	calls int
	err   error
	after func()
}

type fakeMessageReplier struct {
	calls  int
	parent string
	text   string
	action string
	err    error
	after  func()
}

func (r *fakeMessageReplier) ReplyToMessage(_ context.Context, parentMessageID, text, actionID string) (string, error) {
	r.calls++
	r.parent, r.text, r.action = parentMessageID, text, actionID
	if r.after != nil {
		r.after()
	}
	return "manual-feedback-message", r.err
}

func (r *fakeReactor) AddReaction(context.Context, string, string, string) (string, error) {
	r.calls++
	if r.after != nil {
		r.after()
	}
	return "reaction-id", r.err
}

func (r *fakeReplier) Platform() Platform { return PlatformYouTube }
func (r *fakeReplier) Reply(_ context.Context, comment Comment, text string) (string, error) {
	r.calls++
	r.comment, r.text = comment, text
	return "platform-reply", r.err
}

func TestBridgeEndToEndAndReplayDeduplication(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	source := &mutableSource{comments: []Comment{{Platform: PlatformYouTube, AccountID: "channel", ID: "baseline", Text: "old"}}}
	notifier := &fakeNotifier{}
	replier := &fakeReplier{}
	bridge := &Bridge{Store: store, Sources: []CommentSource{source}, Notifier: notifier, Repliers: map[Platform]CommentReplier{PlatformYouTube: replier}, TargetChatID: "chat-1"}
	if err := bridge.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := bridge.NotifyOnce(ctx); err != nil || notifier.calls != 0 {
		t.Fatalf("baseline notified: calls=%d err=%v", notifier.calls, err)
	}
	newComment := Comment{Platform: PlatformYouTube, AccountID: "channel", ID: "new", RootID: "new", ResourceID: "video", AuthorName: "viewer", Text: "hello"}
	source.comments = append(source.comments, newComment)
	if err := bridge.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := bridge.NotifyOnce(ctx); err != nil || notifier.calls != 1 {
		t.Fatalf("notification calls=%d err=%v", notifier.calls, err)
	}
	incoming := IncomingReply{MessageID: "human-message", ParentMessageID: "f1", RootMessageID: "f1", ChatID: "chat-1", UserID: "user", Text: "thanks"}
	added, err := bridge.AcceptReply(ctx, incoming)
	if err != nil || !added {
		t.Fatalf("accept reply=%v err=%v", added, err)
	}
	if added, err = bridge.AcceptReply(ctx, incoming); err != nil || added {
		t.Fatalf("duplicate reply=%v err=%v", added, err)
	}
	if err := bridge.ReplyOnce(ctx); err != nil || replier.calls != 1 || replier.comment.ID != "new" || replier.text != "thanks" {
		t.Fatalf("reply calls=%d comment=%#v text=%q err=%v", replier.calls, replier.comment, replier.text, err)
	}
	if err := bridge.ReplyOnce(ctx); err != nil || replier.calls != 1 {
		t.Fatalf("reply replay calls=%d err=%v", replier.calls, err)
	}
	counts, err := store.StatusCounts(ctx)
	if err != nil || counts.Notifications[JobSent] != 1 || counts.Replies[JobSent] != 1 {
		t.Fatalf("counts=%#v err=%v", counts, err)
	}
}

func TestReactionFailureDoesNotChangeSentReply(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	prepareMappedInbound(t, store, "in")
	replier := &fakeReplier{}
	bridge := &Bridge{Store: store, Repliers: map[Platform]CommentReplier{PlatformBilibili: replier}}
	if err := bridge.ReplyOnce(ctx); err != nil || replier.calls != 1 {
		t.Fatalf("reply = %v, calls=%d", err, replier.calls)
	}
	reactor := &fakeReactor{err: errors.New("reaction rejected")}
	bridge.Reactor = reactor
	if err := bridge.ReactionOnce(ctx); err != nil { // read reaction
		t.Fatal(err)
	}
	if err := bridge.ReactionOnce(ctx); err != nil { // ok reaction
		t.Fatal(err)
	}
	if reactor.calls != 2 {
		t.Fatalf("reaction calls = %d", reactor.calls)
	}
	counts, err := store.StatusCounts(ctx)
	if err != nil || counts.Replies[JobSent] != 1 || counts.Reactions[JobFailed] != 2 {
		t.Fatalf("counts = %#v, %v", counts, err)
	}
	if err := bridge.ReplyOnce(ctx); err != nil || replier.calls != 1 {
		t.Fatalf("reply replay = %v, calls=%d", err, replier.calls)
	}
}

func TestReactionUnknownDoesNotChangeSentReply(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	prepareMappedInbound(t, store, "in")
	replier := &fakeReplier{}
	bridge := &Bridge{Store: store, Repliers: map[Platform]CommentReplier{PlatformBilibili: replier}}
	if err := bridge.ReplyOnce(ctx); err != nil || replier.calls != 1 {
		t.Fatalf("reply = %v, calls=%d", err, replier.calls)
	}
	bridge.Reactor = &fakeReactor{err: unknownErr{errors.New("timeout")}}
	if err := bridge.ReactionOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := bridge.ReactionOnce(ctx); err != nil {
		t.Fatal(err)
	}
	counts, err := store.StatusCounts(ctx)
	if err != nil || counts.Replies[JobSent] != 1 || counts.Reactions[JobUnknown] != 2 {
		t.Fatalf("counts = %#v, %v", counts, err)
	}
}

func TestReactionTerminalPersistenceFailureDoesNotChangeSentReply(t *testing.T) {
	store := testStore(t)
	base := context.Background()
	prepareMappedInbound(t, store, "in")
	replier := &fakeReplier{}
	bridge := &Bridge{Store: store, Repliers: map[Platform]CommentReplier{PlatformBilibili: replier}}
	if err := bridge.ReplyOnce(base); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(base)
	defer cancel()
	bridge.Reactor = &fakeReactor{after: cancel}
	if err := bridge.ReactionOnce(ctx); err == nil {
		t.Fatal("expected terminal persistence error")
	}
	counts, err := store.StatusCounts(base)
	if err != nil || counts.Replies[JobSent] != 1 || counts.Reactions[JobUnknown] != 1 || counts.Reactions[JobPending] != 1 {
		t.Fatalf("counts = %#v, %v", counts, err)
	}
}

func TestEngagementOnceDeduplicatesAndCreatesIndependentOKFeedback(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	prepareMappedNotification(t, store, PlatformBilibili, "a", "f1")
	first := IncomingEngagement{EventID: "reaction-event-1", MessageID: "f1", UserID: "user-a", ExpectedChatID: "chat-1", ReactionType: "THUMBSUP"}
	added, err := store.AcceptEngagement(ctx, first)
	if err != nil || !added {
		t.Fatalf("first engagement = %v, %v", added, err)
	}
	if added, err = store.AcceptEngagement(ctx, first); err != nil || added {
		t.Fatalf("replayed event = %v, %v", added, err)
	}
	if added, err = store.AcceptEngagement(ctx, IncomingEngagement{EventID: "reaction-event-2", MessageID: "f1", UserID: "user-b", ExpectedChatID: "chat-1", EmojiType: "THUMBSUP"}); err != nil || added {
		t.Fatalf("different user duplicated action = %v, %v", added, err)
	}
	engager := &fakeEngager{}
	bridge := Bridge{Store: store, Engagers: map[Platform]CommentEngager{PlatformBilibili: engager}}
	if err := bridge.EngagementOnce(ctx); err != nil || engager.calls != 1 || engager.comment.ID != "new" || engager.kind != EngagementLike {
		t.Fatalf("engage = %v calls=%d comment=%#v kind=%q", err, engager.calls, engager.comment, engager.kind)
	}
	if err := bridge.EngagementOnce(ctx); err != nil || engager.calls != 1 {
		t.Fatalf("engagement replay = %v calls=%d", err, engager.calls)
	}
	counts, err := store.StatusCounts(ctx)
	if err != nil || counts.Engagements[JobSent] != 1 || counts.EngagementFeedbacks[JobPending] != 1 || counts.Reactions[JobPending] != 1 {
		t.Fatalf("counts = %#v, %v", counts, err)
	}
	reactor := &fakeReactor{err: errors.New("feedback unavailable")}
	bridge.Reactor = reactor
	if err := bridge.EngagementFeedbackOnce(ctx); err != nil || reactor.calls != 1 {
		t.Fatalf("feedback = %v calls=%d", err, reactor.calls)
	}
	counts, err = store.StatusCounts(ctx)
	if err != nil || counts.Engagements[JobSent] != 1 || counts.EngagementFeedbacks[JobFailed] != 1 {
		t.Fatalf("failed feedback changed engagement: %#v, %v", counts, err)
	}
}

func TestEngagementUnknownNeverRetriesAndCanBeExplicitlyReset(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	prepareMappedNotification(t, store, PlatformBilibili, "a", "f1")
	if added, err := store.AcceptEngagement(ctx, IncomingEngagement{EventID: "reaction-event", MessageID: "f1", UserID: "user", ExpectedChatID: "chat-1", ReactionType: "THUMBSUP"}); err != nil || !added {
		t.Fatalf("accept = %v, %v", added, err)
	}
	engager := &fakeEngager{err: unknownErr{errors.New("timeout")}}
	bridge := Bridge{Store: store, Engagers: map[Platform]CommentEngager{PlatformBilibili: engager}}
	if err := bridge.EngagementOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := bridge.EngagementOnce(ctx); err != nil || engager.calls != 1 {
		t.Fatalf("unknown automatically retried: %v calls=%d", err, engager.calls)
	}
	jobs, err := store.ListTerminalJobs(ctx, JobUnknown)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "engagement" || jobs[0].FeishuMessageID != "f1" {
		t.Fatalf("terminal jobs = %#v, %v", jobs, err)
	}
	if ok, err := store.ResetUnknownToPending(ctx, jobs[0].ActionID); err != nil || !ok {
		t.Fatalf("reset = %v, %v", ok, err)
	}
	engager.err = nil
	if err := bridge.EngagementOnce(ctx); err != nil || engager.calls != 2 {
		t.Fatalf("explicit retry = %v calls=%d", err, engager.calls)
	}
}

func TestManualEngagementFeedbackOnce(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		store := testStore(t)
		ctx := context.Background()
		prepareMappedNotification(t, store, PlatformYouTube, "channel", "youtube-message")
		if added, err := store.AcceptEngagement(ctx, IncomingEngagement{EventID: "youtube-event", MessageID: "youtube-message", UserID: "user", ExpectedChatID: "chat-1", ReactionType: "THUMBSUP"}); err != nil || !added {
			t.Fatalf("accept = %v, %v", added, err)
		}
		replier := &fakeMessageReplier{}
		bridge := Bridge{Store: store, MessageReplier: replier}
		if err := bridge.ManualEngagementFeedbackOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if replier.calls != 1 || replier.parent != "youtube-message" || replier.text != ManualEngagementFeedbackText || replier.action == "" {
			t.Fatalf("reply = %#v", replier)
		}
		if err := bridge.ManualEngagementFeedbackOnce(ctx); err != nil || replier.calls != 1 {
			t.Fatalf("duplicate instruction sent: err=%v calls=%d", err, replier.calls)
		}
		counts, err := store.StatusCounts(ctx)
		if err != nil || counts.ManualEngagementFeedbacks[JobSent] != 1 {
			t.Fatalf("counts = %#v, %v", counts, err)
		}
	})

	t.Run("explicit failure", func(t *testing.T) {
		store := testStore(t)
		ctx := context.Background()
		prepareMappedNotification(t, store, PlatformYouTube, "channel", "youtube-message")
		if added, err := store.AcceptEngagement(ctx, IncomingEngagement{EventID: "youtube-event", MessageID: "youtube-message", UserID: "user", ExpectedChatID: "chat-1", ReactionType: "THUMBSUP"}); err != nil || !added {
			t.Fatalf("accept = %v, %v", added, err)
		}
		replier := &fakeMessageReplier{err: errors.New("reply rejected")}
		bridge := Bridge{Store: store, MessageReplier: replier}
		if err := bridge.ManualEngagementFeedbackOnce(ctx); err != nil {
			t.Fatal(err)
		}
		counts, err := store.StatusCounts(ctx)
		if err != nil || counts.ManualEngagementFeedbacks[JobFailed] != 1 {
			t.Fatalf("counts = %#v, %v", counts, err)
		}
	})

	t.Run("unknown never retries", func(t *testing.T) {
		store := testStore(t)
		ctx := context.Background()
		prepareMappedNotification(t, store, PlatformYouTube, "channel", "youtube-message")
		if added, err := store.AcceptEngagement(ctx, IncomingEngagement{EventID: "youtube-event", MessageID: "youtube-message", UserID: "user", ExpectedChatID: "chat-1", ReactionType: "THUMBSUP"}); err != nil || !added {
			t.Fatalf("accept = %v, %v", added, err)
		}
		replier := &fakeMessageReplier{err: unknownErr{errors.New("timeout")}}
		bridge := Bridge{Store: store, MessageReplier: replier}
		if err := bridge.ManualEngagementFeedbackOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if err := bridge.ManualEngagementFeedbackOnce(ctx); err != nil || replier.calls != 1 {
			t.Fatalf("unknown automatically retried: err=%v calls=%d", err, replier.calls)
		}
		counts, err := store.StatusCounts(ctx)
		if err != nil || counts.ManualEngagementFeedbacks[JobUnknown] != 1 {
			t.Fatalf("counts = %#v, %v", counts, err)
		}
	})
}

type credentialSource struct {
	err      error
	comments []Comment
}

func (s *credentialSource) Platform() Platform { return PlatformBilibili }
func (s *credentialSource) AccountID() string  { return "a" }
func (s *credentialSource) ListRecent(context.Context) ([]Comment, error) {
	if s.err != nil {
		return nil, s.err
	}
	return append([]Comment(nil), s.comments...), nil
}

type fakeCredentialAlertNotifier struct {
	calls int
	err   error
}

func (n *fakeCredentialAlertNotifier) NotifyCredentialInvalid(context.Context, Platform, string) (string, error) {
	n.calls++
	if n.err != nil {
		return "", n.err
	}
	return "alert-message", nil
}

func TestBridgeCredentialInvalidPollAlertAndRearm(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	source := &credentialSource{err: NewCredentialInvalidError(CredentialInvalidLogin)}
	alerts := &fakeCredentialAlertNotifier{}
	bridge := Bridge{Store: store, Sources: []CommentSource{source}, CredentialAlertNotifier: alerts}
	if err := bridge.PollOnce(ctx); !IsCredentialInvalid(err) {
		t.Fatalf("poll error = %v", err)
	}
	if err := bridge.CredentialAlertOnce(ctx); err != nil || alerts.calls != 1 {
		t.Fatalf("alert = %v calls=%d", err, alerts.calls)
	}
	if err := bridge.PollOnce(ctx); !IsCredentialInvalid(err) {
		t.Fatalf("repeat poll error = %v", err)
	}
	if err := bridge.CredentialAlertOnce(ctx); err != nil || alerts.calls != 1 {
		t.Fatalf("duplicate alert = %v calls=%d", err, alerts.calls)
	}
	source.err = nil
	if err := bridge.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	source.err = NewCredentialInvalidError(CredentialInvalidLogin)
	if err := bridge.PollOnce(ctx); !IsCredentialInvalid(err) {
		t.Fatalf("rearmed poll error = %v", err)
	}
	if err := bridge.CredentialAlertOnce(ctx); err != nil || alerts.calls != 2 {
		t.Fatalf("rearmed alert = %v calls=%d", err, alerts.calls)
	}
}

func TestBridgeCredentialInvalidReplyAndEngagement(t *testing.T) {
	t.Run("reply", func(t *testing.T) {
		store := testStore(t)
		ctx := context.Background()
		prepareMappedInbound(t, store, "in")
		replier := &fakeReplier{err: NewCredentialInvalidError(CredentialInvalidCSRF)}
		bridge := Bridge{Store: store, Repliers: map[Platform]CommentReplier{PlatformBilibili: replier}}
		if err := bridge.ReplyOnce(ctx); err != nil {
			t.Fatalf("reply = %v", err)
		}
		counts, err := store.StatusCounts(ctx)
		if err != nil || counts.CredentialAlerts[JobPending] != 1 {
			t.Fatalf("reply alert counts = %#v, %v", counts, err)
		}
	})

	t.Run("engagement", func(t *testing.T) {
		store := testStore(t)
		ctx := context.Background()
		prepareMappedNotification(t, store, PlatformBilibili, "a", "f1")
		if added, err := store.AcceptEngagement(ctx, IncomingEngagement{EventID: "event", MessageID: "f1", UserID: "user", ExpectedChatID: "chat-1", ReactionType: "THUMBSUP"}); err != nil || !added {
			t.Fatalf("accept = %v, %v", added, err)
		}
		engager := &fakeEngager{err: NewCredentialInvalidError(CredentialInvalidLogin)}
		bridge := Bridge{Store: store, Engagers: map[Platform]CommentEngager{PlatformBilibili: engager}}
		if err := bridge.EngagementOnce(ctx); err != nil {
			t.Fatalf("engagement = %v", err)
		}
		counts, err := store.StatusCounts(ctx)
		if err != nil || counts.CredentialAlerts[JobPending] != 1 {
			t.Fatalf("engagement alert counts = %#v, %v", counts, err)
		}
	})
}

func TestBridgeSuccessfulBilibiliReplyRearmsCSRFIncident(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	prepareMappedInbound(t, store, "in")
	if created, err := store.RecordCredentialInvalid(ctx, PlatformBilibili, "a", CredentialInvalidCSRF); err != nil || !created {
		t.Fatalf("record csrf = %v, %v", created, err)
	}
	replier := &fakeReplier{}
	bridge := Bridge{Store: store, Repliers: map[Platform]CommentReplier{PlatformBilibili: replier}}
	if err := bridge.ReplyOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if created, err := store.RecordCredentialInvalid(ctx, PlatformBilibili, "a", CredentialInvalidCSRF); err != nil || !created {
		t.Fatalf("csrf was not rearmed after reply: %v, %v", created, err)
	}
}

func TestCredentialAlertUnknownNeverRetriesAndCanBeExplicitlyReset(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if created, err := store.RecordCredentialInvalid(ctx, PlatformBilibili, "a", CredentialInvalidLogin); err != nil || !created {
		t.Fatalf("record = %v, %v", created, err)
	}
	notifier := &fakeCredentialAlertNotifier{err: unknownErr{errors.New("timeout")}}
	bridge := Bridge{Store: store, CredentialAlertNotifier: notifier}
	if err := bridge.CredentialAlertOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := bridge.CredentialAlertOnce(ctx); err != nil || notifier.calls != 1 {
		t.Fatalf("unknown alert retried: err=%v calls=%d", err, notifier.calls)
	}
	counts, err := store.StatusCounts(ctx)
	if err != nil || counts.CredentialAlerts[JobUnknown] != 1 {
		t.Fatalf("counts = %#v, %v", counts, err)
	}
	j, err := store.ListTerminalJobs(ctx, JobUnknown)
	if err != nil || len(j) != 1 {
		t.Fatalf("terminal jobs = %#v, %v", j, err)
	}
	if ok, err := store.ResetUnknownToPending(ctx, j[0].ActionID); err != nil || !ok {
		t.Fatalf("reset = %v, %v", ok, err)
	}
	notifier.err = nil
	if err := bridge.CredentialAlertOnce(ctx); err != nil || notifier.calls != 2 {
		t.Fatalf("explicit retry = %v calls=%d", err, notifier.calls)
	}
}

type fakeCredentialRefresher struct {
	platform Platform
	begin    int
	states   []CredentialRefreshState
	polled   int
	err      error
	cancel   func()
}

func (r *fakeCredentialRefresher) Platform() Platform { return r.platform }
func (r *fakeCredentialRefresher) BeginCredentialRefresh(context.Context) (CredentialRefreshSession, error) {
	r.begin++
	if r.err != nil {
		return CredentialRefreshSession{}, r.err
	}
	return CredentialRefreshSession{Key: "refresh-key", URL: "https://qr.test/login", ExpiresAt: time.Now().Add(time.Minute)}, nil
}
func (r *fakeCredentialRefresher) PollCredentialRefresh(context.Context, string) (CredentialRefreshState, error) {
	r.polled++
	if r.err != nil {
		return "", r.err
	}
	if len(r.states) == 0 {
		return CredentialRefreshWaiting, nil
	}
	state := r.states[0]
	r.states = r.states[1:]
	if state == CredentialRefreshSucceeded && r.cancel != nil {
		r.cancel()
	}
	return state, nil
}

type fakeCredentialQRNotifier struct {
	uploads       int
	sends         int
	uploadActions []string
	sendActions   []string
	image         string
	message       string
	err           error
}

func (n *fakeCredentialQRNotifier) UploadCredentialQR(_ context.Context, _ Platform, _ string, action string) (string, error) {
	n.uploads++
	n.uploadActions = append(n.uploadActions, action)
	if n.err != nil {
		return "", n.err
	}
	return n.image, nil
}
func (n *fakeCredentialQRNotifier) SendCredentialQR(_ context.Context, _ Platform, _ string, action string) (string, error) {
	n.sends++
	n.sendActions = append(n.sendActions, action)
	if n.err != nil {
		return "", n.err
	}
	return n.message, nil
}

func TestBridgeCredentialRefreshSuccessAndTerminalRetry(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if created, err := store.RecordCredentialInvalid(ctx, PlatformBilibili, "a", CredentialInvalidLogin); err != nil || !created {
		t.Fatalf("record invalid = %v, %v", created, err)
	}
	refresher := &fakeCredentialRefresher{platform: PlatformBilibili, states: []CredentialRefreshState{CredentialRefreshWaiting, CredentialRefreshSucceeded}}
	qr := &fakeCredentialQRNotifier{image: "image", message: "qr-message"}
	replier := &fakeMessageReplier{}
	bridge := Bridge{Store: store, Refreshers: map[Platform]CredentialRefresher{PlatformBilibili: refresher}, QRNotifier: qr, MessageReplier: replier, TargetChatID: "chat"}

	if err := bridge.CredentialRefreshOnce(ctx); err != nil { // generate
		t.Fatal(err)
	}
	if err := bridge.CredentialRefreshOnce(ctx); err != nil { // upload
		t.Fatal(err)
	}
	if err := bridge.CredentialRefreshOnce(ctx); err != nil { // send
		t.Fatal(err)
	}
	if err := bridge.CredentialRefreshOnce(ctx); err != nil { // waiting
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE credential_refresh_jobs SET next_poll_at=0`); err != nil {
		t.Fatal(err)
	}
	if err := bridge.CredentialRefreshOnce(ctx); err != nil { // success
		t.Fatal(err)
	}
	if refresher.begin != 1 || refresher.polled != 2 || qr.uploads != 1 || qr.sends != 1 {
		t.Fatalf("refresh calls begin=%d poll=%d upload=%d send=%d", refresher.begin, refresher.polled, qr.uploads, qr.sends)
	}
	if replier.calls != 1 || replier.parent != "qr-message" || replier.text != CredentialRefreshSuccessText {
		t.Fatalf("success feedback = %#v", replier)
	}
	var state string
	if err := store.DB().QueryRowContext(ctx, `SELECT credential_state FROM source_credential_health WHERE platform=? AND account_id=?`, PlatformBilibili, "a").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != credentialStateHealthy {
		t.Fatalf("credential state after QR success = %q", state)
	}
}

func TestBridgeCredentialRefreshExpiredReplyResetsOnlyQRJob(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if created, err := store.RecordCredentialInvalid(ctx, PlatformBilibili, "a", CredentialInvalidLogin); err != nil || !created {
		t.Fatalf("record invalid = %v, %v", created, err)
	}
	refresher := &fakeCredentialRefresher{platform: PlatformBilibili, states: []CredentialRefreshState{CredentialRefreshExpired}}
	qr := &fakeCredentialQRNotifier{image: "image", message: "qr-message"}
	replier := &fakeMessageReplier{}
	bridge := Bridge{Store: store, Refreshers: map[Platform]CredentialRefresher{PlatformBilibili: refresher}, QRNotifier: qr, MessageReplier: replier, TargetChatID: "chat"}
	for i := 0; i < 3; i++ {
		if err := bridge.CredentialRefreshOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := bridge.CredentialRefreshOnce(ctx); err != nil { // expired poll
		t.Fatal(err)
	}
	if replier.calls != 1 || replier.parent != "qr-message" || replier.text != CredentialRefreshFailureText {
		t.Fatalf("expired feedback = %#v", replier)
	}
	if added, err := bridge.AcceptReply(ctx, IncomingReply{MessageID: "wrong-chat", ParentMessageID: "qr-message", ChatID: "other-chat", UserID: "operator", Text: CredentialRefreshRetryText}); !errors.Is(err, ErrUnmappedReply) || added {
		t.Fatalf("cross-chat QR retry accepted=%v err=%v", added, err)
	}
	added, err := bridge.AcceptReply(ctx, IncomingReply{MessageID: "retry", ParentMessageID: "qr-message", ChatID: "chat", UserID: "operator", Text: CredentialRefreshRetryText})
	if err != nil || !added {
		t.Fatalf("retry reply accepted=%v err=%v", added, err)
	}
	var stage CredentialRefreshStage
	var status JobStatus
	var attempt int
	if err := store.DB().QueryRowContext(ctx, `SELECT stage,status,attempt FROM credential_refresh_jobs`).Scan(&stage, &status, &attempt); err != nil {
		t.Fatal(err)
	}
	if stage != CredentialRefreshStageGenerate || status != JobPending || attempt != 2 {
		t.Fatalf("reset state = stage=%q status=%q attempt=%d", stage, status, attempt)
	}
	for i := 0; i < 3; i++ {
		if err := bridge.CredentialRefreshOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(qr.uploadActions) != 2 || len(qr.sendActions) != 2 || qr.uploadActions[0] == qr.uploadActions[1] || qr.sendActions[0] == qr.sendActions[1] {
		t.Fatalf("attempt-specific QR action IDs not separated: uploads=%#v sends=%#v", qr.uploadActions, qr.sendActions)
	}
	if added, err = bridge.AcceptReply(ctx, IncomingReply{MessageID: "retry", ParentMessageID: "qr-message", ChatID: "chat", UserID: "operator", Text: CredentialRefreshRetryText}); err != nil || added {
		t.Fatalf("duplicate retry accepted=%v err=%v", added, err)
	}
}

func TestBridgeCredentialRefreshHealthWritePrecedesTerminalMark(t *testing.T) {
	store := testStore(t)
	base := context.Background()
	if created, err := store.RecordCredentialInvalid(base, PlatformBilibili, "a", CredentialInvalidLogin); err != nil || !created {
		t.Fatalf("record invalid = %v, %v", created, err)
	}
	ctx, cancel := context.WithCancel(base)
	refresher := &fakeCredentialRefresher{platform: PlatformBilibili, states: []CredentialRefreshState{CredentialRefreshSucceeded}, cancel: cancel}
	qr := &fakeCredentialQRNotifier{image: "image", message: "qr-message"}
	bridge := Bridge{Store: store, Refreshers: map[Platform]CredentialRefresher{PlatformBilibili: refresher}, QRNotifier: qr, TargetChatID: "chat"}
	for i := 0; i < 3; i++ {
		if err := bridge.CredentialRefreshOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := bridge.CredentialRefreshOnce(ctx); err == nil {
		t.Fatal("expected health persistence failure")
	}
	var status JobStatus
	if err := store.DB().QueryRowContext(base, `SELECT status FROM credential_refresh_jobs`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != JobSending {
		t.Fatalf("refresh job was terminal before health recovery succeeded: %q", status)
	}
	var health string
	if err := store.DB().QueryRowContext(base, `SELECT credential_state FROM source_credential_health WHERE platform=? AND account_id=?`, PlatformBilibili, "a").Scan(&health); err != nil {
		t.Fatal(err)
	}
	if health != credentialStateInvalid {
		t.Fatalf("credential health changed despite cancelled write: %q", health)
	}
}
