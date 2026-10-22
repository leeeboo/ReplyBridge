package feishu

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"comment-bridge/internal/commentbridge"
)

func TestWebhookDelivery(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantError  bool
		unknown    bool
	}{
		{"success", `{"code":0,"data":{}}`, 200, false, false},
		{"rejected", `{"code":19024,"msg":"private response"}`, 200, true, false},
		{"missing code", `{}`, 200, true, true},
		{"invalid JSON", `private response`, 200, true, true},
		{"server error", `{"code":0}`, 500, true, true},
		{"conflicting server error", `{"code":1}`, 503, true, true},
		{"redirect", `{"code":0}`, 302, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
					t.Error("unexpected request method or content type")
				}
				var payload struct {
					MsgType string                `json:"msg_type"`
					Content struct{ Text string } `json:"content"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				text := payload.Content.Text
				if payload.MsgType != "text" || !strings.Contains(text, "视频：视频") || !strings.Contains(text, "作者：作者") || !strings.Contains(text, "请打开上方链接回复或点赞。") || utf8.RuneCountInString(text) > maxNotificationRunes {
					t.Error("invalid webhook notification content")
				}
				for _, forbidden := range []string{"平台：", "YouTube", "引用回复", "private-hook"} {
					if strings.Contains(text, forbidden) {
						t.Errorf("notification contains %q", forbidden)
					}
				}
				w.Header().Set("Location", "/redirect")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			n, err := NewWebhookNotifier("https://open.feishu.cn/open-apis/bot/v2/hook/private-hook")
			if err != nil {
				t.Fatal(err)
			}
			n.url = server.URL
			id, err := n.NotifyComment(t.Context(), commentbridge.Comment{Platform: commentbridge.PlatformYouTube, ResourceTitle: "视频", ResourceURL: "https://example.test/watch", AuthorName: "作者", Text: strings.Repeat("评论", 4000)}, "action")
			if (err != nil) != tc.wantError || commentbridge.IsUnknownOutcome(err) != tc.unknown || id != "" || calls != 1 {
				t.Fatalf("id=%q error=%v calls=%d", id, err, calls)
			}
			if err != nil && strings.Contains(err.Error(), "private") {
				t.Fatal("response leaked")
			}
		})
	}
}

func TestWebhookValidationAndTransportPrivacy(t *testing.T) {
	for _, raw := range []string{"", "http://open.feishu.cn/open-apis/bot/v2/hook/private-hook", "https://example.test/open-apis/bot/v2/hook/private-hook", "https://open.feishu.cn/open-apis/bot/v2/hook/", "https://open.feishu.cn/open-apis/bot/v2/hook/private-hook/extra", "https://user@open.feishu.cn/open-apis/bot/v2/hook/private-hook", "https://open.feishu.cn/open-apis/bot/v2/hook/private-hook?secret=x"} {
		if _, err := NewWebhookNotifier(raw); err == nil || strings.Contains(err.Error(), "private-hook") {
			t.Fatal("invalid URL accepted or exposed")
		}
	}
	n, err := NewWebhookNotifier("https://open.feishu.cn/open-apis/bot/v2/hook/private-hook")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.NotifyComment(t.Context(), commentbridge.Comment{Platform: commentbridge.PlatformBilibili}, "action"); err == nil {
		t.Fatal("bilibili webhook accepted")
	}
	if _, err := n.NotifyComment(t.Context(), commentbridge.Comment{Platform: commentbridge.PlatformYouTube}, ""); err == nil {
		t.Fatal("empty action accepted")
	}
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	n.url = server.URL + "/private-hook"
	_, err = n.NotifyComment(t.Context(), commentbridge.Comment{Platform: commentbridge.PlatformYouTube}, "action")
	if !commentbridge.IsUnknownOutcome(err) {
		t.Fatal("transport failure must remain unknown")
	}
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if strings.Contains(cause.Error(), "private-hook") {
			t.Fatal("URL leaked in error chain")
		}
	}
}
