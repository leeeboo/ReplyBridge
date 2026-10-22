package bili

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"comment-bridge/internal/commentbridge"
)

// ListRecentDanmaku shares the comment adapter's gate, login and cooldown.
// One request is sufficient: the creator inbox accepts the full 100-item cap.
func (a *CommentAdapter) ListRecentDanmaku(ctx context.Context) (items []commentbridge.Danmaku, resultErr error) {
	if a.client.riskStopped() {
		return nil, commentbridge.ErrBilibiliRiskStopped
	}
	a.riskMu.RLock()
	blocked := a.riskFused || a.client.now().Before(a.readRetryAt)
	a.riskMu.RUnlock()
	if blocked {
		return nil, ErrCommentRiskControl
	}
	defer func() { a.finishReadAttempt(resultErr) }()
	endpoint := *a.client.base
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/x/v2/dm/recent"
	endpoint.RawQuery = url.Values{"pn": {"1"}, "ps": {strconv.Itoa(commentbridge.DanmakuLimit)}}.Encode()
	if err := a.client.gate.Wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errors.New("invalid danmaku request")
	}
	req.Header.Set("User-Agent", a.client.userAgent)
	req.Header.Set("Referer", "https://member.bilibili.com/platform/inter-active/danmu")
	hc := *a.client.http
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		if errors.Is(err, commentbridge.ErrBilibiliRiskStopped) {
			return nil, commentbridge.ErrBilibiliRiskStopped
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("danmaku read request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusPreconditionFailed {
		return nil, &CooldownError{RetryAfter: retryAfter(resp.Header, a.client.now())}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("danmaku read HTTP status %d", resp.StatusCode)
	}
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			Result *[]struct {
				ID       json.RawMessage `json:"id"`
				IDString string          `json:"id_str"`
				AID      json.RawMessage `json:"aid"`
				Title    string          `json:"title"`
				Message  string          `json:"msg"`
				Progress int64           `json:"progress"`
				CTime    int64           `json:"ctime"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&envelope) != nil {
		return nil, errors.New("invalid danmaku response")
	}
	if envelope.Code != 0 {
		return nil, wrapCredentialInvalid(&APIError{Code: envelope.Code})
	}
	if envelope.Data.Result == nil || len(*envelope.Data.Result) > commentbridge.DanmakuLimit {
		return nil, errors.New("invalid danmaku inbox")
	}
	for _, raw := range *envelope.Data.Result {
		id := firstNonEmpty(raw.IDString, jsonScalarString(raw.ID))
		aid := jsonScalarString(raw.AID)
		if !positiveDecimal(id) || !positiveDecimal(aid) || raw.Message == "" || raw.Progress < 0 {
			return nil, errors.New("invalid danmaku record")
		}
		items = append(items, commentbridge.Danmaku{ID: id, VideoTitle: raw.Title, Text: raw.Message,
			VideoURL:   "https://www.bilibili.com/video/av" + aid + "?t=" + strconv.FormatInt(raw.Progress/1000, 10),
			PositionMS: raw.Progress, CreatedAt: raw.CTime})
	}
	return items, nil
}

func positiveDecimal(value string) bool {
	if value == "" || value[0] == '0' {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
