package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"comment-bridge/internal/commentbridge"
)

type WebhookNotifier struct {
	url    string
	client *http.Client
}

func NewWebhookNotifier(rawURL string) (*WebhookNotifier, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host != "open.feishu.cn" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
		return nil, errors.New("invalid feishu webhook URL")
	}
	token, ok := strings.CutPrefix(u.Path, "/open-apis/bot/v2/hook/")
	if !ok || token == "" || strings.Contains(token, "/") {
		return nil, errors.New("invalid feishu webhook path")
	}
	return &WebhookNotifier{url: rawURL, client: &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// NotifyComment returns no message ID: custom bots only support one-way delivery.
func (w *WebhookNotifier) NotifyComment(ctx context.Context, comment commentbridge.Comment, actionID string) (string, error) {
	if comment.Platform != commentbridge.PlatformYouTube || strings.TrimSpace(actionID) == "" {
		return "", errors.New("invalid youtube webhook notification")
	}
	payload, err := json.Marshal(struct {
		MsgType string            `json:"msg_type"`
		Content map[string]string `json:"content"`
	}{"text", map[string]string{"text": formatNotification(comment, "\n\n请打开上方链接回复或点赞。")}})
	if err != nil {
		return "", errors.New("encode feishu webhook notification failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(payload))
	if err != nil {
		return "", errors.New("create feishu webhook request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		// HTTP errors can contain the secret URL; never retain them in the error chain.
		return "", unknownOutcome{cause: errors.New("feishu webhook transport failed")}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var result struct {
		Code *int `json:"code"`
	}
	if err != nil || json.Unmarshal(body, &result) != nil || result.Code == nil || resp.StatusCode >= 500 {
		return "", unknownOutcome{cause: errors.New("invalid feishu webhook response")}
	}
	if *result.Code != 0 {
		return "", fmt.Errorf("feishu webhook rejected notification (code %d)", *result.Code)
	}
	if resp.StatusCode != http.StatusOK {
		return "", unknownOutcome{cause: errors.New("unexpected feishu webhook HTTP status")}
	}
	return "", nil
}
