package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"comment-bridge/internal/commentbridge"
)

func TestParseRunConfigFailsClosed(t *testing.T) {
	secret := writePrivateSecret(t, "secret")
	base := requiredFeishuEnv(secret)
	base["SHADOWBRAIN_YOUTUBE_ENABLED"] = "not-a-bool"
	if _, err := parseRunConfig(mapLookup(base)); err == nil {
		t.Fatal("invalid enabled value was accepted")
	}
	base = requiredFeishuEnv(secret)
	base["SHADOWBRAIN_YOUTUBE_ENABLED"] = "true"
	if _, err := parseRunConfig(mapLookup(base)); err == nil {
		t.Fatal("enabled YouTube without OAuth files was accepted")
	}
}

func TestParseYouTubeWebhookSecretFile(t *testing.T) {
	values := requiredFeishuEnv(writePrivateSecret(t, "secret"))
	values["SHADOWBRAIN_YOUTUBE_ENABLED"] = "true"
	values["SHADOWBRAIN_YOUTUBE_CHANNEL_ID"] = "channel"
	values["SHADOWBRAIN_YOUTUBE_CLIENT_FILE"] = "client.json"
	values["SHADOWBRAIN_YOUTUBE_TOKEN_FILE"] = "token.json"
	if cfg, err := parseRunConfig(mapLookup(values)); err != nil || cfg.youtube.webhookURL != "" {
		t.Fatal("legacy route changed")
	}
	for _, tc := range []struct {
		name, content string
		mode          os.FileMode
		wantError     bool
	}{
		{"valid", "https://open.feishu.cn/open-apis/bot/v2/hook/test\n", 0600, false},
		{"empty", " \n", 0600, true},
		{"public", "https://open.feishu.cn/open-apis/bot/v2/hook/test", 0644, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writePrivateSecret(t, tc.content)
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			values["SHADOWBRAIN_YOUTUBE_FEISHU_WEBHOOK_FILE"] = path
			cfg, err := parseRunConfig(mapLookup(values))
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v", err)
			}
			if !tc.wantError && cfg.youtube.webhookURL != strings.TrimSpace(tc.content) {
				t.Fatal("webhook not loaded")
			}
		})
	}
	values["SHADOWBRAIN_YOUTUBE_FEISHU_WEBHOOK_FILE"] = filepath.Join(t.TempDir(), "missing")
	if _, err := parseRunConfig(mapLookup(values)); err == nil {
		t.Fatal("missing webhook silently fell back")
	}
}

func TestParseRunConfigBiliDoubleGate(t *testing.T) {
	secret := writePrivateSecret(t, "secret")
	values := requiredFeishuEnv(secret)
	values["SHADOWBRAIN_BILIBILI_EXPERIMENTAL_ENABLED"] = "true"
	if _, err := parseRunConfig(mapLookup(values)); err == nil {
		t.Fatal("Bilibili enabled without acknowledgement was accepted")
	}
	values["SHADOWBRAIN_BILIBILI_EXPERIMENTAL_RISK_ACK"] = biliRiskAcknowledgement
	cfg, err := parseRunConfig(mapLookup(values))
	if err != nil || !cfg.bilibili.enabled || cfg.bilibili.subReplyPages != 10 {
		t.Fatalf("double-gated Bilibili config = %#v, %v", cfg.bilibili, err)
	}
	values["SHADOWBRAIN_BILIBILI_MAX_SUB_REPLY_PAGES"] = "51"
	if _, err := parseRunConfig(mapLookup(values)); err == nil {
		t.Fatal("Bilibili sub-reply page limit above maximum was accepted")
	}
}

func TestParseYouTubeReplyGroup(t *testing.T) {
	values := requiredFeishuEnv(writePrivateSecret(t, "secret"))
	values["SHADOWBRAIN_YOUTUBE_FEISHU_CHAT_ID"] = "youtube-chat"
	if _, err := parseRunConfig(mapLookup(values)); err == nil {
		t.Fatal("accepted YouTube group while platform disabled")
	}
	values["SHADOWBRAIN_YOUTUBE_ENABLED"] = "true"
	values["SHADOWBRAIN_YOUTUBE_CHANNEL_ID"] = "channel"
	values["SHADOWBRAIN_YOUTUBE_CLIENT_FILE"] = "client.json"
	values["SHADOWBRAIN_YOUTUBE_TOKEN_FILE"] = "token.json"
	cfg, err := parseRunConfig(mapLookup(values))
	if err != nil || cfg.feishu.YouTubeChatID != "youtube-chat" || cfg.feishu.ChatID == "youtube-chat" {
		t.Fatalf("YouTube group configuration: %v", err)
	}
	values["SHADOWBRAIN_YOUTUBE_FEISHU_WEBHOOK_FILE"] = writePrivateSecret(t, "https://open.feishu.cn/open-apis/bot/v2/hook/test")
	if _, err := parseRunConfig(mapLookup(values)); err == nil {
		t.Fatal("accepted both one-way and reply-capable YouTube routes")
	}
}

func TestMakeBiliAdapterRejectsAccountOverrideMismatch(t *testing.T) {
	store, err := commentbridge.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	send := func(context.Context, string, string) (string, error) { return "message", nil }
	credential := writePrivateSecret(t, `{"SESSDATA":"session","bili_jct":"csrf","DedeUserID":"42"}`)
	if _, err := makeBiliAdapter(biliConfig{credentialFile: credential, accountID: "99", subReplyPages: 10}, store, send); err == nil {
		t.Fatal("Bilibili account override did not match Cookie but was accepted")
	}
	adapter, err := makeBiliAdapter(biliConfig{credentialFile: credential, accountID: "42", subReplyPages: 10}, store, send)
	if err != nil || adapter.AccountID() != "42" {
		t.Fatalf("adapter account=%q err=%v", adapter.AccountID(), err)
	}
}

func TestMakeBiliAdapterAlwaysUsesDurableRiskStop(t *testing.T) {
	store, err := commentbridge.Open(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	send := func(context.Context, string, string) (string, error) { return "message", nil }
	stop, err := commentbridge.NewBilibiliRiskStop(store, send)
	if err != nil {
		t.Fatal(err)
	}
	if err = stop.BeginRequest(t.Context()); err != nil {
		t.Fatal(err)
	}
	_ = stop.EndRequest(true, 429, 0)
	cfg := biliConfig{credentialFile: writePrivateSecret(t, `{"SESSDATA":"session","bili_jct":"csrf","DedeUserID":"42"}`), subReplyPages: 10}
	if _, err = makeBiliAdapter(cfg, nil, send); err == nil {
		t.Fatal("accepted missing durable store")
	}
	adapter, err := makeBiliAdapter(cfg, store, send)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = adapter.ListRecent(t.Context()); !errors.Is(err, commentbridge.ErrBilibiliRiskStopped) {
		t.Fatalf("factory omitted guard: %v", err)
	}
	if _, err = adapter.BeginCredentialRefresh(t.Context()); !errors.Is(err, commentbridge.ErrBilibiliRiskStopped) {
		t.Fatalf("factory omitted QR guard: %v", err)
	}
}

func TestParseRunConfigAllowlistAndIntervals(t *testing.T) {
	secret := writePrivateSecret(t, "secret")
	values := requiredFeishuEnv(secret)
	values["SHADOWBRAIN_BILIBILI_EXPERIMENTAL_ENABLED"] = "true"
	values["SHADOWBRAIN_BILIBILI_EXPERIMENTAL_RISK_ACK"] = biliRiskAcknowledgement
	values["SHADOWBRAIN_FEISHU_ALLOWED_OPEN_IDS"] = " a, ,b,a "
	values["SHADOWBRAIN_COMMENT_POLL_INTERVAL"] = "14s"
	if _, err := parseRunConfig(mapLookup(values)); err == nil {
		t.Fatal("poll interval below minimum was accepted")
	}
	values["SHADOWBRAIN_COMMENT_POLL_INTERVAL"] = "15s"
	values["SHADOWBRAIN_COMMENT_WORK_INTERVAL"] = "100ms"
	cfg, err := parseRunConfig(mapLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(cfg.feishu.AllowedOpenIDs, ","), "a,b"; got != want {
		t.Fatalf("allowlist = %q, want %q", got, want)
	}
	if cfg.pollInterval != 15*time.Second || cfg.workInterval != 100*time.Millisecond {
		t.Fatalf("intervals = %s / %s", cfg.pollInterval, cfg.workInterval)
	}
}

func TestNextCommentPollDelayUsesJapanQuietHours(t *testing.T) {
	jst := time.FixedZone("Asia/Tokyo", 9*60*60)
	normal := time.Minute
	tests := []struct {
		name string
		now  time.Time
		want time.Duration
	}{
		{name: "before quiet hours", now: time.Date(2026, 8, 24, 20, 58, 0, 0, jst), want: normal},
		{name: "cap at quiet start", now: time.Date(2026, 8, 24, 20, 59, 30, 0, jst), want: 30 * time.Second},
		{name: "quiet start inclusive", now: time.Date(2026, 8, 24, 21, 0, 0, 0, jst), want: 20 * time.Minute},
		{name: "UTC converted to Japan time", now: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC), want: 20 * time.Minute},
		{name: "after midnight", now: time.Date(2026, 8, 25, 2, 0, 0, 0, jst), want: 20 * time.Minute},
		{name: "cap at quiet end", now: time.Date(2026, 8, 25, 3, 29, 30, 0, jst), want: 30 * time.Second},
		{name: "quiet end exclusive", now: time.Date(2026, 8, 25, 3, 30, 0, 0, jst), want: normal},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextCommentPollDelay(tc.now, normal); got != tc.want {
				t.Fatalf("nextCommentPollDelay(%s) = %s, want %s", tc.now, got, tc.want)
			}
		})
	}
}

func TestParseRunConfigFeishuAuthorizationModes(t *testing.T) {
	secret := writePrivateSecret(t, "secret")

	allowAll := requiredFeishuEnv(secret)
	allowAll["SHADOWBRAIN_BILIBILI_EXPERIMENTAL_ENABLED"] = "true"
	allowAll["SHADOWBRAIN_BILIBILI_EXPERIMENTAL_RISK_ACK"] = biliRiskAcknowledgement
	allowAll["SHADOWBRAIN_FEISHU_ALLOWED_OPEN_IDS"] = ""
	allowAll["SHADOWBRAIN_FEISHU_ALLOW_ALL_GROUP_MEMBERS"] = "true"
	cfg, err := parseRunConfig(mapLookup(allowAll))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.feishu.AllowAllGroupMembers || len(cfg.feishu.AllowedOpenIDs) != 0 {
		t.Fatalf("allow-all config = %#v", cfg.feishu)
	}

	defaultAllowlist := requiredFeishuEnv(secret)
	defaultAllowlist["SHADOWBRAIN_BILIBILI_EXPERIMENTAL_ENABLED"] = "true"
	defaultAllowlist["SHADOWBRAIN_BILIBILI_EXPERIMENTAL_RISK_ACK"] = biliRiskAcknowledgement
	cfg, err = parseRunConfig(mapLookup(defaultAllowlist))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.feishu.AllowAllGroupMembers || len(cfg.feishu.AllowedOpenIDs) != 1 {
		t.Fatalf("default allowlist config = %#v", cfg.feishu)
	}

	none := requiredFeishuEnv(secret)
	none["SHADOWBRAIN_BILIBILI_EXPERIMENTAL_ENABLED"] = "true"
	none["SHADOWBRAIN_BILIBILI_EXPERIMENTAL_RISK_ACK"] = biliRiskAcknowledgement
	none["SHADOWBRAIN_FEISHU_ALLOWED_OPEN_IDS"] = ""
	if _, err := parseRunConfig(mapLookup(none)); err == nil {
		t.Fatal("missing Feishu authorization mode was accepted")
	}

	both := requiredFeishuEnv(secret)
	both["SHADOWBRAIN_BILIBILI_EXPERIMENTAL_ENABLED"] = "true"
	both["SHADOWBRAIN_BILIBILI_EXPERIMENTAL_RISK_ACK"] = biliRiskAcknowledgement
	both["SHADOWBRAIN_FEISHU_ALLOW_ALL_GROUP_MEMBERS"] = "true"
	if _, err := parseRunConfig(mapLookup(both)); err == nil {
		t.Fatal("both Feishu authorization modes were accepted")
	}

	invalid := requiredFeishuEnv(secret)
	invalid["SHADOWBRAIN_BILIBILI_EXPERIMENTAL_ENABLED"] = "true"
	invalid["SHADOWBRAIN_BILIBILI_EXPERIMENTAL_RISK_ACK"] = biliRiskAcknowledgement
	invalid["SHADOWBRAIN_FEISHU_ALLOW_ALL_GROUP_MEMBERS"] = "definitely-not-a-bool"
	if _, err := parseRunConfig(mapLookup(invalid)); err == nil {
		t.Fatal("invalid allow-all value was accepted")
	}
}

func TestStatusDoesNotExposeBodies(t *testing.T) {
	db := filepath.Join(t.TempDir(), "bridge.db")
	store := createUnknownNotification(t, db, "comment body must stay private")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runStatus(context.Background(), []string{"--db", db}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "comment body must stay private") {
		t.Fatalf("status exposed body: %q", out.String())
	}
	if !strings.Contains(out.String(), "status=unknown") || !strings.Contains(out.String(), "comment=c1") {
		t.Fatalf("status did not identify unknown action: %q", out.String())
	}
	for _, status := range []string{"pending", "sending", "sent", "unknown", "failed"} {
		if !strings.Contains(out.String(), "credential-alerts "+status+"=0") {
			t.Fatalf("status did not report credential alert %s count: %q", status, out.String())
		}
		if !strings.Contains(out.String(), "credential-refreshes "+status+"=0") {
			t.Fatalf("status did not report credential refresh %s count: %q", status, out.String())
		}
		if !strings.Contains(out.String(), "engagements "+status+"=0") {
			t.Fatalf("status did not report engagement %s count: %q", status, out.String())
		}
		if !strings.Contains(out.String(), "reactions "+status+"=0") {
			t.Fatalf("status did not report reaction %s count: %q", status, out.String())
		}
		if !strings.Contains(out.String(), "manual-engagement-feedbacks "+status+"=0") {
			t.Fatalf("status did not report manual engagement feedback %s count: %q", status, out.String())
		}
	}
}

func TestStatusSupportsDatabaseBeforeCredentialAlertAndRefreshMigrations(t *testing.T) {
	db := filepath.Join(t.TempDir(), "bridge.db")
	store, err := commentbridge.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(context.Background(), "DROP TABLE credential_alert_jobs"); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(context.Background(), "DROP TABLE source_credential_health"); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(context.Background(), "DROP TABLE credential_refresh_jobs"); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runStatus(context.Background(), []string{"--db", db}, &out); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"pending", "sending", "sent", "unknown", "failed"} {
		if want := "credential-alerts " + status + "=0"; !strings.Contains(out.String(), want) {
			t.Fatalf("legacy status missing %q: %s", want, out.String())
		}
		if want := "credential-refreshes " + status + "=0"; !strings.Contains(out.String(), want) {
			t.Fatalf("legacy status missing %q: %s", want, out.String())
		}
	}
}

func TestVersion(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"version"}, &out); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "shadowbrain-comment-bridge 0.1.21\n"; got != want {
		t.Fatalf("version = %q, want %q", got, want)
	}
}

func TestRetryUnknownRequiresConfirmationWithoutMutating(t *testing.T) {
	db := filepath.Join(t.TempDir(), "bridge.db")
	store := createUnknownNotification(t, db, "private")
	jobs, err := store.ListTerminalJobs(context.Background(), commentbridge.JobUnknown)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("unknown jobs = %#v, %v", jobs, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runRetryUnknown(context.Background(), []string{"--db", db, "--action", jobs[0].ActionID}, &out); err == nil {
		t.Fatal("retry without confirmation was accepted")
	}
	readonly, err := commentbridge.OpenReadOnly(db)
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	after, err := readonly.ListTerminalJobs(context.Background(), commentbridge.JobUnknown)
	if err != nil || len(after) != 1 || after[0].ActionID != jobs[0].ActionID {
		t.Fatalf("retry without confirmation changed state: %#v, %v", after, err)
	}
}

func TestRetryUnknownManualEngagementFeedback(t *testing.T) {
	db := filepath.Join(t.TempDir(), "bridge.db")
	store, actionID := createUnknownManualEngagementFeedback(t, db)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var status bytes.Buffer
	if err := runStatus(context.Background(), []string{"--db", db}, &status); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.String(), "manual-engagement-feedbacks unknown=1") || !strings.Contains(status.String(), "action="+actionID+" kind=manual_engagement_feedback status=unknown") {
		t.Fatalf("status did not report manual feedback terminal job: %q", status.String())
	}

	var out bytes.Buffer
	if err := runRetryUnknown(context.Background(), []string{"--db", db, "--action", actionID, "--confirm-possible-duplicate"}, &out); err != nil {
		t.Fatal(err)
	}
	readonly, err := commentbridge.OpenReadOnly(db)
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	counts, err := readonly.StatusCounts(context.Background())
	if err != nil || counts.ManualEngagementFeedbacks[commentbridge.JobPending] != 1 {
		t.Fatalf("manual feedback retry state = %#v, %v", counts.ManualEngagementFeedbacks, err)
	}
}

func TestAuthYoutubeArgumentValidation(t *testing.T) {
	for _, args := range [][]string{
		{}, {"youtube"}, {"youtube", "--client-file", "x"},
		{"youtube", "--client-file", "x", "--token-file", "y", "extra"},
		{"bilibili"},
	} {
		if err := runAuth(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Fatalf("runAuth(%q) succeeded", args)
		}
	}
}

func TestDiscoverFeishuArgumentValidation(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--app-id", "id"},
		{"--secret-file", "secret"},
		{"--app-id", "id", "--secret-file", "secret", "extra"},
	} {
		if err := runDiscoverFeishu(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Fatalf("runDiscoverFeishu(%q) succeeded", args)
		}
	}
}

func TestDiscoverYouTubeArgumentValidation(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--client-file", "client"},
		{"--token-file", "token"},
		{"--client-file", "client", "--token-file", "token", "extra"},
	} {
		if err := runDiscoverYouTube(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Fatalf("runDiscoverYouTube(%q) succeeded", args)
		}
	}
}

func requiredFeishuEnv(secret string) map[string]string {
	return map[string]string{
		"SHADOWBRAIN_FEISHU_APP_ID":           "app-id",
		"SHADOWBRAIN_FEISHU_APP_SECRET_FILE":  secret,
		"SHADOWBRAIN_FEISHU_CHAT_ID":          "chat-id",
		"SHADOWBRAIN_FEISHU_ALLOWED_OPEN_IDS": "user-a",
	}
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) { value, ok := values[key]; return value, ok }
}

func writePrivateSecret(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func createUnknownNotification(t *testing.T, db, body string) *commentbridge.Store {
	t.Helper()
	store, err := commentbridge.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	source := testSource{}
	if err := store.RecordPoll(ctx, source, []commentbridge.Comment{}); err != nil {
		t.Fatal(err)
	}
	comment := commentbridge.Comment{Platform: source.Platform(), AccountID: source.AccountID(), ID: "c1", ResourceID: "r1", Text: body}
	if err := store.RecordPoll(ctx, source, []commentbridge.Comment{comment}); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimNotification(ctx)
	if err != nil || job == nil {
		t.Fatalf("claim notification = %#v, %v", job, err)
	}
	if err := store.MarkNotificationFailed(ctx, job.ActionID, commentbridge.JobUnknown, nil); err != nil {
		t.Fatal(err)
	}
	return store
}

func createUnknownManualEngagementFeedback(t *testing.T, db string) (*commentbridge.Store, string) {
	t.Helper()
	store, err := commentbridge.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	source := testSource{}
	if err := store.RecordPoll(ctx, source, []commentbridge.Comment{}); err != nil {
		t.Fatal(err)
	}
	comment := commentbridge.Comment{Platform: source.Platform(), AccountID: source.AccountID(), ID: "c1", ResourceID: "r1", Text: "private"}
	if err := store.RecordPoll(ctx, source, []commentbridge.Comment{comment}); err != nil {
		t.Fatal(err)
	}
	notification, err := store.ClaimNotification(ctx)
	if err != nil || notification == nil {
		t.Fatalf("claim notification = %#v, %v", notification, err)
	}
	if err := store.MarkNotificationSent(ctx, notification.ActionID, "feishu-message", "chat-1"); err != nil {
		t.Fatal(err)
	}
	added, err := store.AcceptEngagement(ctx, commentbridge.IncomingEngagement{EventID: "event-1", MessageID: "feishu-message", UserID: "user", ExpectedChatID: "chat-1", ReactionType: "THUMBSUP"})
	if err != nil || !added {
		t.Fatalf("accept manual engagement = %v, %v", added, err)
	}
	feedback, err := store.ClaimManualEngagementFeedback(ctx)
	if err != nil || feedback == nil {
		t.Fatalf("claim manual feedback = %#v, %v", feedback, err)
	}
	if err := store.MarkManualEngagementFeedbackFailed(ctx, feedback.ActionID, commentbridge.JobUnknown, nil); err != nil {
		t.Fatal(err)
	}
	return store, feedback.ActionID
}

type testSource struct{}

func (testSource) Platform() commentbridge.Platform                            { return commentbridge.PlatformYouTube }
func (testSource) AccountID() string                                           { return "account" }
func (testSource) ListRecent(context.Context) ([]commentbridge.Comment, error) { return nil, nil }
