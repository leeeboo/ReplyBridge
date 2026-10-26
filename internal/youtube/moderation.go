package youtube

import (
	"comment-bridge/internal/commentbridge"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

var _ commentbridge.CommentModerator = (*Client)(nil)

// Reject the exact viewer comment; comments.delete only supports one's own comments.
func (c *Client) Moderate(ctx context.Context, comment commentbridge.Comment, kind commentbridge.ModerationKind) (string, error) {
	if comment.Platform != c.Platform() || comment.AccountID != c.AccountID() || strings.TrimSpace(comment.ID) == "" || strings.ContainsAny(comment.ID, ", \r\n\t") {
		return "", errors.New("invalid YouTube moderation target")
	}
	q := url.Values{"id": {comment.ID}, "moderationStatus": {"rejected"}}
	switch kind {
	case commentbridge.ModerationDelete:
	case commentbridge.ModerationBan:
		if comment.AuthorID == "" || comment.AuthorID == c.channelID {
			return "", errors.New("invalid YouTube author target")
		}
		q.Set("banAuthor", "true")
	default:
		return "", errors.New("unsupported YouTube moderation")
	}
	token, err := c.bearer(ctx)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL("youtube/v3/comments/setModerationStatus", q), nil)
	if err != nil {
		return "", errors.New("invalid YouTube moderation request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	hc := *c.http
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return "", unknownError{cause: "YouTube moderation outcome is unknown"}
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if readErr != nil || ctx.Err() != nil {
		return "", unknownError{cause: "YouTube moderation outcome is unknown"}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		c.clearToken()
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return "", fmt.Errorf("YouTube moderation rejected: status %d%s", resp.StatusCode, apiReason(body))
	}
	if resp.StatusCode != http.StatusNoContent {
		return "", unknownError{cause: "YouTube moderation outcome is unknown"}
	}
	return comment.ID, nil
}
