package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"comment-bridge/internal/commentbridge"
)

const maxReplyRunes = 10000

const (
	maxVideoIDsPerRequest = 50
	unknownVideoTitle     = "YouTube 视频"
)

var _ commentbridge.CommentSource = (*Client)(nil)
var _ commentbridge.CommentReplier = (*Client)(nil)

func (c *Client) Platform() commentbridge.Platform { return commentbridge.PlatformYouTube }

// ListRecent lists comments and replies ordered from oldest to newest. It does
// not retain page tokens, but remembers reply counts to avoid repeatedly
// expanding already-complete reply threads.
func (c *Client) ListRecent(ctx context.Context) ([]commentbridge.Comment, error) {
	token, err := c.bearer(ctx)
	if err != nil {
		return nil, err
	}
	var all []commentbridge.Comment
	seenComments := make(map[string]struct{})
	updatedReplyCounts := make(map[string]uint64)
	pageToken := ""
	seenPageTokens := make(map[string]struct{})
	for page := 0; page < c.maxPages; page++ {
		q := url.Values{"part": {"snippet,replies"}, "allThreadsRelatedToChannelId": {c.channelID}, "order": {"time"}, "maxResults": {"100"}, "textFormat": {"plainText"}}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL("youtube/v3/commentThreads", q), nil)
		if err != nil {
			return nil, errors.New("YouTube comment list request could not be created")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, errors.New("YouTube comment list request failed")
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, errors.New("YouTube comment list response could not be read")
		}
		if resp.StatusCode == http.StatusUnauthorized {
			c.clearToken()
			return nil, errors.New("YouTube authentication failed")
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, fmt.Errorf("YouTube comment list failed: status %d", resp.StatusCode)
		}
		var response commentThreadsResponse
		if err := json.Unmarshal(body, &response); err != nil {
			return nil, errors.New("YouTube comment list returned an invalid response")
		}
		for _, item := range response.Items {
			top := item.Snippet.TopLevelComment
			if strings.TrimSpace(top.ID) == "" {
				continue
			}
			videoID := item.Snippet.VideoID
			if videoID == "" {
				videoID = top.Snippet.VideoID
			}
			if comment, ok := c.normalizeComment(top, top.ID, "", videoID); ok {
				appendUniqueComment(&all, seenComments, comment)
			}

			for _, reply := range item.Replies.Comments {
				if comment, ok := c.normalizeComment(reply, top.ID, top.ID, videoID); ok {
					appendUniqueComment(&all, seenComments, comment)
				}
			}

			count := item.Snippet.TotalReplyCount
			previousCount, known := c.replyCount(top.ID)
			if (!known || previousCount != count) && count > uint64(len(item.Replies.Comments)) {
				replies, err := c.listAllReplies(ctx, token, top.ID, videoID)
				if err != nil {
					return nil, err
				}
				for _, reply := range replies {
					appendUniqueComment(&all, seenComments, reply)
				}
			}
			updatedReplyCounts[top.ID] = count
		}
		pageToken = response.NextPageToken
		if pageToken == "" {
			break
		}
		if _, duplicate := seenPageTokens[pageToken]; duplicate {
			return nil, errors.New("YouTube comment list returned a repeated page token")
		}
		seenPageTokens[pageToken] = struct{}{}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].PublishedAt.Before(all[j].PublishedAt) })
	if err := c.resolveVideoTitles(ctx, token, all); err != nil {
		return nil, err
	}
	c.updateReplyCounts(updatedReplyCounts)
	return all, nil
}

func (c *Client) resolveVideoTitles(ctx context.Context, token string, comments []commentbridge.Comment) error {
	c.videoTitlesMu.Lock()
	missing := make([]string, 0)
	seen := make(map[string]struct{})
	for _, comment := range comments {
		videoID := strings.TrimSpace(comment.ResourceID)
		if videoID == "" {
			continue
		}
		if _, cached := c.videoTitles[videoID]; cached {
			continue
		}
		if _, duplicate := seen[videoID]; duplicate {
			continue
		}
		seen[videoID] = struct{}{}
		missing = append(missing, videoID)
	}
	c.videoTitlesMu.Unlock()

	fetched := make(map[string]string, len(missing))
	for start := 0; start < len(missing); start += maxVideoIDsPerRequest {
		end := start + maxVideoIDsPerRequest
		if end > len(missing) {
			end = len(missing)
		}
		titles, err := c.listVideoTitles(ctx, token, missing[start:end])
		if err != nil {
			return err
		}
		for _, videoID := range missing[start:end] {
			title := strings.TrimSpace(titles[videoID])
			if title == "" {
				title = unknownVideoTitle
			}
			fetched[videoID] = title
		}
	}

	c.videoTitlesMu.Lock()
	for videoID, title := range fetched {
		c.videoTitles[videoID] = title
	}
	for i := range comments {
		if title := c.videoTitles[strings.TrimSpace(comments[i].ResourceID)]; title != "" {
			comments[i].ResourceTitle = title
		} else {
			comments[i].ResourceTitle = unknownVideoTitle
		}
	}
	c.videoTitlesMu.Unlock()
	return nil
}

func (c *Client) listVideoTitles(ctx context.Context, token string, videoIDs []string) (map[string]string, error) {
	if len(videoIDs) == 0 || len(videoIDs) > maxVideoIDsPerRequest {
		return nil, errors.New("YouTube video title lookup received an invalid batch")
	}
	q := url.Values{"part": {"snippet"}, "id": {strings.Join(videoIDs, ",")}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL("youtube/v3/videos", q), nil)
	if err != nil {
		return nil, errors.New("YouTube video title request could not be created")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	hc := *c.http
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return nil, errors.New("YouTube video title lookup failed")
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()
	if readErr != nil {
		return nil, errors.New("YouTube video title response could not be read")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		c.clearToken()
		return nil, errors.New("YouTube authentication failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("YouTube video title lookup failed: status %d", resp.StatusCode)
	}
	var response videosResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, errors.New("YouTube video title lookup returned an invalid response")
	}
	titles := make(map[string]string, len(response.Items))
	for _, item := range response.Items {
		if id := strings.TrimSpace(item.ID); id != "" {
			titles[id] = strings.TrimSpace(item.Snippet.Title)
		}
	}
	return titles, nil
}

func appendUniqueComment(all *[]commentbridge.Comment, seen map[string]struct{}, comment commentbridge.Comment) {
	if _, exists := seen[comment.ID]; exists {
		return
	}
	seen[comment.ID] = struct{}{}
	*all = append(*all, comment)
}

func (c *Client) normalizeComment(raw youtubeComment, rootID, parentID, videoID string) (commentbridge.Comment, bool) {
	published, err := parsePublished(raw.Snippet.PublishedAt)
	authorID := strings.TrimSpace(raw.Snippet.AuthorChannelID.Value)
	// commentThreads.list includes the authenticated creator's own replies.
	// Suppressing them prevents the bridge from notifying on its own writes.
	if err != nil || strings.TrimSpace(raw.ID) == "" || authorID == strings.TrimSpace(c.channelID) {
		return commentbridge.Comment{}, false
	}
	text := raw.Snippet.TextOriginal
	if text == "" {
		text = raw.Snippet.TextDisplay
	}
	return commentbridge.Comment{Platform: commentbridge.PlatformYouTube, AccountID: c.accountID,
		ID: raw.ID, RootID: rootID, ParentID: parentID, ResourceID: videoID, ResourceTitle: "", ResourceURL: watchURL(videoID, raw.ID),
		AuthorID: authorID, AuthorName: raw.Snippet.AuthorDisplayName, Text: text, PublishedAt: published}, true
}

func (c *Client) replyCount(topLevelID string) (uint64, bool) {
	c.replyCountsMu.Lock()
	defer c.replyCountsMu.Unlock()
	count, ok := c.replyCounts[topLevelID]
	return count, ok
}

func (c *Client) updateReplyCounts(counts map[string]uint64) {
	c.replyCountsMu.Lock()
	defer c.replyCountsMu.Unlock()
	for id, count := range counts {
		c.replyCounts[id] = count
	}
}

const maxReplyPages = 100

func (c *Client) listAllReplies(ctx context.Context, token, topLevelID, videoID string) ([]commentbridge.Comment, error) {
	var replies []commentbridge.Comment
	seenIDs := make(map[string]struct{})
	seenTokens := make(map[string]struct{})
	pageToken := ""
	for page := 0; page < maxReplyPages; page++ {
		q := url.Values{"part": {"snippet"}, "parentId": {topLevelID}, "maxResults": {"100"}, "textFormat": {"plainText"}}
		if pageToken != "" {
			if _, duplicate := seenTokens[pageToken]; duplicate {
				return nil, errors.New("YouTube comment reply list returned a repeated page token")
			}
			seenTokens[pageToken] = struct{}{}
			q.Set("pageToken", pageToken)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL("youtube/v3/comments", q), nil)
		if err != nil {
			return nil, errors.New("YouTube comment reply list request could not be created")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, errors.New("YouTube comment reply list request failed")
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, errors.New("YouTube comment reply list response could not be read")
		}
		if resp.StatusCode == http.StatusUnauthorized {
			c.clearToken()
			return nil, errors.New("YouTube authentication failed")
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, fmt.Errorf("YouTube comment reply list failed: status %d", resp.StatusCode)
		}
		var response commentsResponse
		if err := json.Unmarshal(body, &response); err != nil {
			return nil, errors.New("YouTube comment reply list returned an invalid response")
		}
		for _, raw := range response.Items {
			if comment, ok := c.normalizeComment(raw, topLevelID, topLevelID, videoID); ok {
				appendUniqueComment(&replies, seenIDs, comment)
			}
		}
		pageToken = response.NextPageToken
		if pageToken == "" {
			return replies, nil
		}
	}
	return nil, errors.New("YouTube comment reply list exceeded page limit")
}

func watchURL(videoID, commentID string) string {
	if videoID == "" {
		return ""
	}
	query := url.Values{"v": {videoID}}
	if strings.TrimSpace(commentID) != "" {
		query.Set("lc", commentID)
	}
	return (&url.URL{Scheme: "https", Host: "www.youtube.com", Path: "/watch", RawQuery: query.Encode()}).String()
}

func parsePublished(raw string) (time.Time, error) { return time.Parse(time.RFC3339, raw) }

// Reply creates one direct reply to a top-level YouTube comment. It never
// retries writes: a transport failure, 5xx, or malformed success body is an
// unknown outcome and must be reconciled by a human.
func (c *Client) Reply(ctx context.Context, parent commentbridge.Comment, text string) (string, error) {
	text = strings.TrimSpace(text)
	if parent.Platform != commentbridge.PlatformYouTube || strings.TrimSpace(parent.ID) == "" {
		return "", errors.New("invalid YouTube parent comment")
	}
	if strings.TrimSpace(parent.AccountID) != "" && strings.TrimSpace(parent.AccountID) != c.accountID {
		return "", errors.New("YouTube parent comment belongs to another account")
	}
	if text == "" || utf8.RuneCountInString(text) > maxReplyRunes {
		return "", errors.New("invalid YouTube reply text")
	}
	token, err := c.bearer(ctx)
	if err != nil {
		return "", fmt.Errorf("YouTube reply was not sent: %w", err)
	}
	// Build explicitly rather than accepting caller-controlled JSON.
	var requestBody struct {
		Snippet struct {
			ParentID string `json:"parentId"`
			Text     string `json:"textOriginal"`
		} `json:"snippet"`
	}
	targetID := strings.TrimSpace(parent.RootID)
	if targetID == "" {
		targetID = parent.ID
	}
	requestBody.Snippet.ParentID, requestBody.Snippet.Text = targetID, text
	payload, _ := json.Marshal(requestBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL("youtube/v3/comments", url.Values{"part": {"snippet"}}), bytes.NewReader(payload))
	if err != nil {
		return "", errors.New("YouTube reply was not sent")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	// Never follow a redirect for a write: the original POST may have reached
	// YouTube, so treating it as unknown prevents a duplicate reply.
	hc := *c.http
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return "", unknownError{cause: "YouTube reply outcome is unknown"}
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if readErr != nil {
		return "", unknownError{cause: "YouTube reply outcome is unknown"}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		c.clearToken()
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return "", unknownError{cause: "YouTube reply outcome is unknown"}
	}
	if resp.StatusCode >= 500 {
		return "", unknownError{cause: "YouTube reply outcome is unknown"}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("YouTube reply failed: status %d%s", resp.StatusCode, apiReason(body))
	}
	var response struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &response) != nil || strings.TrimSpace(response.ID) == "" {
		return "", unknownError{cause: "YouTube reply outcome is unknown"}
	}
	return response.ID, nil
}

type unknownError struct{ cause string }

func (e unknownError) Error() string        { return e.cause }
func (e unknownError) OutcomeUnknown() bool { return true }

func apiReason(body []byte) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	if len(payload.Error.Errors) > 0 && payload.Error.Errors[0].Reason != "" {
		if reason := safeAPIReason(payload.Error.Errors[0].Reason); reason != "" {
			return ": " + reason
		}
	}
	return ""
}
func safeAPIReason(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 80 {
		return ""
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != '.' {
			return ""
		}
	}
	return s
}

type commentThreadsResponse struct {
	NextPageToken string `json:"nextPageToken"`
	Items         []struct {
		ID      string `json:"id"`
		Snippet struct {
			VideoID         string         `json:"videoId"`
			TotalReplyCount uint64         `json:"totalReplyCount"`
			TopLevelComment youtubeComment `json:"topLevelComment"`
		} `json:"snippet"`
		Replies struct {
			Comments []youtubeComment `json:"comments"`
		} `json:"replies"`
	} `json:"items"`
}

type commentsResponse struct {
	NextPageToken string           `json:"nextPageToken"`
	Items         []youtubeComment `json:"items"`
}

type videosResponse struct {
	Items []struct {
		ID      string `json:"id"`
		Snippet struct {
			Title string `json:"title"`
		} `json:"snippet"`
	} `json:"items"`
}

type youtubeComment struct {
	ID      string `json:"id"`
	Snippet struct {
		VideoID         string `json:"videoId"`
		AuthorChannelID struct {
			Value string `json:"value"`
		} `json:"authorChannelId"`
		AuthorDisplayName string `json:"authorDisplayName"`
		TextOriginal      string `json:"textOriginal"`
		TextDisplay       string `json:"textDisplay"`
		PublishedAt       string `json:"publishedAt"`
	} `json:"snippet"`
}
