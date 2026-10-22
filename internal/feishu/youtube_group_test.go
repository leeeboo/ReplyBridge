package feishu

import (
	"path/filepath"
	"testing"

	"comment-bridge/internal/commentbridge"
)

func TestYouTubeGroupRoutingAndQuotedRepliesSurviveRestart(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "bridge.db")
	store, err := commentbridge.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	client, err := New(Config{AppID: "id", AppSecret: "secret", ChatID: "bili-chat", YouTubeChatID: "youtube-chat", AllowAllGroupMembers: true})
	if err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	client.sender = sender
	bridge := &commentbridge.Bridge{Store: store, Notifier: client, TargetChatID: "bili-chat", YouTubeTargetChatID: "youtube-chat"}
	for _, platform := range []commentbridge.Platform{commentbridge.PlatformBilibili, commentbridge.PlatformYouTube} {
		if err := store.RecordPoll(ctx, platform, "account", []commentbridge.Comment(nil)); err != nil {
			t.Fatal(err)
		}
		comment := commentbridge.Comment{Platform: platform, AccountID: "account", ID: "new", RootID: "new", ResourceID: "video", Text: "comment"}
		if err := store.RecordPoll(ctx, platform, "account", []commentbridge.Comment{comment}); err != nil {
			t.Fatal(err)
		}
		sender.messageID = string(platform) + "-notice"
		if err := bridge.NotifyOnce(ctx); err != nil {
			t.Fatal(err)
		}
		want := "bili-chat"
		if platform == commentbridge.PlatformYouTube {
			want = "youtube-chat"
		}
		if sender.message.ChatID != want {
			t.Fatalf("%s sent to %q", platform, sender.message.ChatID)
		}
		var mappedChat string
		if err := store.DB().QueryRowContext(ctx, `SELECT chat_id FROM feishu_messages WHERE message_id=?`, sender.messageID).Scan(&mappedChat); err != nil || mappedChat != want {
			t.Fatalf("mapping=%q err=%v", mappedChat, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = commentbridge.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	bridge.Store = store
	for _, tc := range []struct{ name, chat, parent string }{
		{"cross-group", "bili-chat", "youtube-notice"},
		{"cross-platform", "youtube-chat", "bilibili-notice"},
		{"unrelated-group", "other-chat", "youtube-notice"},
		{"root-only", "youtube-chat", ""},
		{"old-webhook", "youtube-chat", "webhook-notice"},
	} {
		event := receiveEvent(tc.chat, "user", "external-member", "text", `{"text":"reply"}`, tc.parent, "youtube-notice", nil)
		event.Event.Message.MessageId = ptr(tc.name)
		if err := client.HandleMessage(ctx, event, bridge); err != nil {
			t.Fatal(err)
		}
	}
	counts, err := store.StatusCounts(ctx)
	if err != nil || counts.Replies[commentbridge.JobPending] != 0 {
		t.Fatalf("invalid reply accepted: %#v %v", counts.Replies, err)
	}
	event := receiveEvent("youtube-chat", "user", "external-member", "text", `{"text":"reply"}`, "youtube-notice", "youtube-notice", nil)
	for range 2 {
		if err := client.HandleMessage(ctx, event, bridge); err != nil {
			t.Fatal(err)
		}
	}
	counts, err = store.StatusCounts(ctx)
	if err != nil || counts.Replies[commentbridge.JobPending] != 1 {
		t.Fatalf("reply dedup failed: %#v %v", counts.Replies, err)
	}
	var platform string
	if err := store.DB().QueryRowContext(ctx, `SELECT platform FROM reply_jobs`).Scan(&platform); err != nil || platform != "youtube" {
		t.Fatalf("reply routed to %q: %v", platform, err)
	}
	if err := client.HandleReactionCreated(ctx, reactionEvent("yt-like", "youtube-notice", "user", "external-member", "THUMBSUP", "1700000000123"), bridge); err != nil {
		t.Fatal(err)
	}
	counts, err = store.StatusCounts(ctx)
	if err != nil || counts.ManualEngagementFeedbacks[commentbridge.JobPending] != 1 || counts.Engagements[commentbridge.JobPending] != 0 {
		t.Fatalf("YouTube reaction routing: %#v %v", counts, err)
	}
	client.allowed = map[string]struct{}{"allowed": {}}
	client.config.AllowAllGroupMembers = false
	sink := &fakeSink{}
	if err := client.HandleMessage(ctx, event, sink); err != nil || len(sink.replies) != 0 {
		t.Fatal("YouTube group bypassed user allowlist")
	}
}
