package bili

import (
	"comment-bridge/internal/commentbridge"
	"context"
	"errors"
	"net/url"
	"strconv"
)

var _ commentbridge.CommentModerator = (*CommentAdapter)(nil)

func (a *CommentAdapter) Moderate(ctx context.Context, c commentbridge.Comment, kind commentbridge.ModerationKind) (string, error) {
	if a == nil || a.client == nil || a.accountID == "" || c.Platform != a.Platform() || c.AccountID != a.accountID {
		return "", errors.New("invalid Bilibili moderation account")
	}
	if a.client.riskStopped() {
		return "", commentbridge.ErrBilibiliRiskStopped
	}
	if a.isRiskFused() {
		return "", ErrCommentRiskControl
	}
	validID := func(id string) bool { n, err := strconv.ParseUint(id, 10, 64); return err == nil && n > 0 }
	if !validID(c.ID) || !validID(c.ResourceID) {
		return "", errors.New("invalid Bilibili moderation target")
	}
	credential := a.client.Credential()
	csrf := credential.Cookies["bili_jct"]
	if credential.Cookies["SESSDATA"] == "" || csrf == "" {
		return "", errors.New("Bilibili moderation requires login")
	}
	form := url.Values{"csrf": {csrf}, "csrf_token": {csrf}}
	path, target := "", c.ID
	switch kind {
	case commentbridge.ModerationDelete:
		path = "/x/v2/reply/del"
		form.Set("oid", c.ResourceID)
		form.Set("type", "1")
		form.Set("rpid", c.ID)
	case commentbridge.ModerationBan:
		if !validID(c.AuthorID) || c.AuthorID == a.accountID {
			return "", errors.New("invalid Bilibili author target")
		}
		path, target = "/x/relation/modify", c.AuthorID
		form.Set("fid", c.AuthorID)
		form.Set("act", "5")
		form.Set("re_src", "111")
	default:
		return "", errors.New("unsupported Bilibili moderation")
	}
	status, body, err := a.postCommentActionOnce(ctx, path, form)
	if err != nil {
		return "", err
	}
	return a.commentActionResult(status, body, target)
}
