package bili

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"comment-bridge/internal/commentbridge"
)

// CommentAdapter adapts Bilibili's non-official web comment APIs to the
// comment bridge. It is experimental and is expected to be disabled by an
// upper-layer feature flag unless a user has explicitly enabled it.
//
// It reads the authenticated creator inbox and posts a direct reply to a
// comment. These endpoints and their browser anti-abuse parameters are not a
// stable public API.
type CommentAdapter struct {
	client          *Client
	accountID       string
	credentialFile  string
	subReplyPages   int
	seenMu          sync.Mutex
	seenIDs         map[string]struct{}
	replyCountsMu   sync.Mutex
	replyCounts     map[string]int
	riskMu          sync.RWMutex
	riskFused       bool
	readRetryAt     time.Time
	readCooldown    time.Duration
	OnSkippedThread func()
}

// CommentAdapterConfig bounds the undocumented child-reply pagination. The
// default and upper limit mirror bilibili-comment-studio so operators can
// trade completeness against account-risk exposure deliberately.
type CommentAdapterConfig struct {
	AccountID      string
	CredentialFile string
	SubReplyPages  int
}

// ErrCommentRiskControl identifies deterministic anti-abuse rejections (for
// example API -352/-412). A human must resolve these; automatic reply retries
// are unsafe.
var ErrCommentRiskControl = errors.New("bilibili comment reply risk control")

// NewCommentAdapter creates an experimental Bilibili comment adapter for the
// supplied creator account ID.
func NewCommentAdapter(client *Client, accountID string) *CommentAdapter {
	adapter, _ := NewConfiguredCommentAdapter(client, CommentAdapterConfig{AccountID: accountID})
	return adapter
}

// NewConfiguredCommentAdapter constructs an adapter with validated polling
// bounds. SubReplyPages defaults to 10 and may be raised to at most 50.
func NewConfiguredCommentAdapter(client *Client, config CommentAdapterConfig) (*CommentAdapter, error) {
	if config.SubReplyPages == 0 {
		config.SubReplyPages = commentSubReplyDefaultPages
	}
	if config.SubReplyPages < 1 || config.SubReplyPages > commentSubReplyMaxPages {
		return nil, errors.New("bilibili sub-reply page limit must be between 1 and 50")
	}
	return &CommentAdapter{
		client: client, accountID: strings.TrimSpace(config.AccountID), credentialFile: strings.TrimSpace(config.CredentialFile), subReplyPages: config.SubReplyPages,
		seenIDs: make(map[string]struct{}), replyCounts: make(map[string]int),
	}, nil
}

func (a *CommentAdapter) Platform() commentbridge.Platform { return commentbridge.PlatformBilibili }
func (a *CommentAdapter) AccountID() string                { return a.accountID }

const (
	commentInboxPageSize        = 50
	commentInboxMaxPages        = 5
	commentSubReplyPageSize     = 20
	commentSubReplyDefaultPages = 10
	commentSubReplyMaxPages     = 50
	qrLoginLifetime             = 180 * time.Second
)

// BeginCredentialRefresh starts one Bilibili QR-login session. The opaque key
// is returned only to the bridge's in-memory refresh coordinator; it is never
// included in adapter errors.
func (a *CommentAdapter) BeginCredentialRefresh(ctx context.Context) (commentbridge.CredentialRefreshSession, error) {
	if a == nil || a.client == nil {
		return commentbridge.CredentialRefreshSession{}, errors.New("bilibili credential refresh is unavailable")
	}
	if strings.TrimSpace(a.accountID) == "" {
		return commentbridge.CredentialRefreshSession{}, errors.New("bilibili credential refresh has no account ID")
	}
	login, err := a.client.generateQRLoginIsolated(ctx)
	if err != nil {
		return commentbridge.CredentialRefreshSession{}, sanitizeQRLoginError(err, "bilibili QR login could not start")
	}
	return commentbridge.CredentialRefreshSession{
		Key: login.Key, URL: login.URL, ExpiresAt: a.client.now().Add(qrLoginLifetime),
	}, nil
}

// PollCredentialRefresh advances a QR-login session by one provider poll.
// Successful credentials are verified against the configured creator account,
// checked online, and persisted before the live client jar or risk fuse is
// changed. The isolated jar used by pollQRLoginIsolated makes failed and
// wrong-account scans observational only.
func (a *CommentAdapter) PollCredentialRefresh(ctx context.Context, key string) (commentbridge.CredentialRefreshState, error) {
	if a == nil || a.client == nil {
		return credentialRefreshState(""), errors.New("bilibili credential refresh is unavailable")
	}
	if strings.TrimSpace(a.accountID) == "" {
		return credentialRefreshState(""), errors.New("bilibili credential refresh has no account ID")
	}
	if strings.TrimSpace(key) == "" {
		return credentialRefreshState(""), errors.New("bilibili QR login key is empty")
	}
	status, err := a.client.pollQRLoginIsolated(ctx, key)
	if err != nil {
		return credentialRefreshState(""), sanitizeQRLoginError(err, "bilibili QR login polling failed")
	}
	switch status.Code {
	case 86101:
		return credentialRefreshState("waiting"), nil
	case 86090:
		return credentialRefreshState("scanned"), nil
	case 86038:
		return credentialRefreshState("expired"), nil
	case 0:
		if !status.Credential.Complete() {
			return credentialRefreshState(""), errors.New("bilibili QR login returned incomplete credential")
		}
		if strings.TrimSpace(status.Credential.Cookies["DedeUserID"]) != a.accountID {
			return credentialRefreshState(""), errors.New("bilibili QR login account mismatch")
		}
		if err := a.client.verifyCredential(ctx, status.Credential, a.accountID); err != nil {
			return credentialRefreshState(""), err
		}
		path := strings.TrimSpace(a.credentialFile)
		if path == "" {
			return credentialRefreshState(""), errors.New("bilibili credential refresh has no writable credential file")
		}
		if err := SaveCredentialFile(path, status.Credential); err != nil {
			return credentialRefreshState(""), errors.New("bilibili credential refresh could not save credential")
		}
		a.client.ReplaceCredential(status.Credential)
		a.clearRiskFuse()
		return credentialRefreshState("succeeded"), nil
	default:
		return credentialRefreshState(""), errors.New("bilibili QR login returned unexpected status")
	}
}

func credentialRefreshState(value string) commentbridge.CredentialRefreshState {
	return commentbridge.CredentialRefreshState(value)
}

// ListRecent walks backward until it reaches a comment seen on the prior poll
// (or a conservative page cap). This follows bilibili-comment-studio's
// pagination contract without paying several undocumented Web requests on
// every quiet poll. A restart scans up to 250 recent items to cover downtime.
func (a *CommentAdapter) ListRecent(ctx context.Context) (result []commentbridge.Comment, resultErr error) {
	if a == nil || a.client == nil {
		return nil, errors.New("bilibili comment adapter has no client")
	}
	if a.accountID == "" {
		return nil, errors.New("bilibili comment adapter has no account ID")
	}
	a.riskMu.RLock()
	blocked := a.riskFused || a.client.now().Before(a.readRetryAt)
	a.riskMu.RUnlock()
	if blocked {
		return nil, ErrCommentRiskControl
	}
	defer func() { a.finishReadAttempt(resultErr) }()
	priorSeen := a.snapshotSeen()
	priorReplyCounts := a.snapshotReplyCounts()
	currentSeen := make(map[string]struct{})
	replyCountUpdates := make(map[string]int)
	byID := make(map[string]commentbridge.Comment)
	for page := 1; page <= commentInboxMaxPages; page++ {
		params := url.Values{
			"order": {"1"}, "filter": {"-1"}, "type": {"1"},
			"pn": {strconv.Itoa(page)}, "ps": {strconv.Itoa(commentInboxPageSize)}, "charge_plus_filter": {"false"},
		}
		var rawData json.RawMessage
		if err := a.client.getJSON(ctx, "/x/v2/reply/up/fulllist", params, false, &rawData); err != nil {
			err = wrapCredentialInvalid(err)
			return nil, err
		}
		decoder := json.NewDecoder(strings.NewReader(string(rawData)))
		decoder.UseNumber()
		var payload any
		if err := decoder.Decode(&payload); err != nil {
			return nil, errors.New("decode Bilibili comment list")
		}
		items := extractTopLevelCommentItems(payload)
		reachedSeen := false
		for _, item := range items {
			id := firstString(item, []string{"rpid"}, []string{"rpid_str"}, []string{"id"}, []string{"id_str"})
			if id != "" {
				currentSeen[id] = struct{}{}
				_, reached := priorSeen[id]
				reachedSeen = reachedSeen || reached
			}
			top, ok := a.parseComment(item)
			if !ok {
				continue
			}
			direct := directReplies(item)
			for _, rawReply := range direct {
				if reply, ok := a.parseSubReply(top, rawReply); ok && reply.ID != top.ID && reply.AuthorID != a.accountID {
					byID[reply.ID] = reply
				}
			}
			replyCount := commentReplyCount(item)
			if replyCount <= len(direct) {
				replyCountUpdates[top.ID] = replyCount
			} else if previous, known := priorReplyCounts[top.ID]; !known || previous != replyCount {
				replies, complete, err := a.fetchSubReplies(ctx, top, replyCount, direct)
				if err != nil {
					err = wrapCredentialInvalid(err)
					return nil, err
				}
				for _, reply := range replies {
					if reply.AuthorID != a.accountID {
						byID[reply.ID] = reply
					}
				}
				if complete {
					replyCountUpdates[top.ID] = replyCount
				}
			}
			if top.AuthorID != a.accountID {
				byID[top.ID] = top
			}
		}
		if len(items) < commentInboxPageSize || reachedSeen {
			break
		}
	}
	a.replaceSeen(currentSeen)
	a.updateReplyCounts(replyCountUpdates)
	comments := make([]commentbridge.Comment, 0, len(byID))
	for _, comment := range byID {
		comments = append(comments, comment)
	}
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].PublishedAt.Before(comments[j].PublishedAt) })
	return comments, nil
}

func (a *CommentAdapter) snapshotReplyCounts() map[string]int {
	a.replyCountsMu.Lock()
	defer a.replyCountsMu.Unlock()
	out := make(map[string]int, len(a.replyCounts))
	for id, count := range a.replyCounts {
		out[id] = count
	}
	return out
}

func (a *CommentAdapter) updateReplyCounts(updates map[string]int) {
	a.replyCountsMu.Lock()
	defer a.replyCountsMu.Unlock()
	for id, count := range updates {
		a.replyCounts[id] = count
	}
}

func (a *CommentAdapter) snapshotSeen() map[string]struct{} {
	a.seenMu.Lock()
	defer a.seenMu.Unlock()
	out := make(map[string]struct{}, len(a.seenIDs))
	for id := range a.seenIDs {
		out[id] = struct{}{}
	}
	return out
}

func (a *CommentAdapter) replaceSeen(ids map[string]struct{}) {
	a.seenMu.Lock()
	a.seenIDs = ids
	a.seenMu.Unlock()
}

func (a *CommentAdapter) parseComment(item map[string]any) (commentbridge.Comment, bool) {
	id := firstString(item, []string{"rpid"}, []string{"rpid_str"}, []string{"id"}, []string{"id_str"})
	oid := firstString(item, []string{"oid"}, []string{"oid_str"}, []string{"subject_oid"}, []string{"subject_id"}, []string{"archive", "aid"}, []string{"aid"})
	if id == "" || id == "0" || oid == "" || oid == "0" {
		return commentbridge.Comment{}, false
	}
	root := firstString(item, []string{"root"}, []string{"root_str"}, []string{"root_rpid"}, []string{"root_rpid_str"})
	if root == "" || root == "0" {
		root = id
	}
	parent := firstString(item, []string{"parent"}, []string{"parent_str"}, []string{"parent_rpid"})
	if parent == "0" {
		parent = ""
	}
	bvid := firstString(item, []string{"archive", "bvid"}, []string{"bvid"}, []string{"video", "bvid"})
	title := firstString(item, []string{"archive", "title"}, []string{"subject", "title"}, []string{"title"}, []string{"video_title"}, []string{"dynamic", "title"}, []string{"video", "title"})
	resourceURL := normalizeBilibiliURL(firstString(item, []string{"archive", "jump_url"}, []string{"url"}))
	if resourceURL == "" && bvid != "" {
		resourceURL = "https://www.bilibili.com/video/" + bvid
	} else if resourceURL == "" {
		resourceURL = "https://www.bilibili.com/video/av" + oid
	}
	ctime, _ := strconv.ParseInt(firstString(item, []string{"ctime"}, []string{"time"}, []string{"timestamp"}), 10, 64)
	if ctime > 10_000_000_000 {
		ctime /= 1000
	}
	message := firstString(item, []string{"content", "message"}, []string{"message"}, []string{"reply_content"}, []string{"text"})
	return commentbridge.Comment{
		Platform: a.Platform(), AccountID: a.accountID, ID: id, RootID: root, ParentID: parent, ResourceID: oid,
		ResourceTitle: title, ResourceURL: resourceURL,
		AuthorID:   firstString(item, []string{"member", "mid"}, []string{"member", "uid"}, []string{"mid"}, []string{"uid"}),
		AuthorName: firstString(item, []string{"member", "uname"}, []string{"member", "name"}, []string{"uname"}, []string{"name"}),
		Text:       message, PublishedAt: time.Unix(ctime, 0), IsChargePlus: isChargePlus(item),
	}, true
}

// isChargePlus reads only the creator-center reply-control flag. The field is
// not part of the public comment contract, so missing or wrongly typed values
// deliberately remain false instead of inferring membership from other fields.
func isChargePlus(item map[string]any) bool {
	value, ok := dig(item, []string{"reply_control", "is_charge_plus"}).(bool)
	return ok && value
}

// parseSubReply carries the enclosing thread's resource context when Bilibili
// omits it from embedded or paginated child replies.
func (a *CommentAdapter) parseSubReply(top commentbridge.Comment, item map[string]any) (commentbridge.Comment, bool) {
	copy := make(map[string]any, len(item)+1)
	for key, value := range item {
		copy[key] = value
	}
	if firstString(copy, []string{"oid"}, []string{"oid_str"}, []string{"subject_oid"}, []string{"subject_id"}, []string{"archive", "aid"}, []string{"aid"}) == "" {
		copy["oid"] = top.ResourceID
	}
	reply, ok := a.parseComment(copy)
	if !ok || reply.ID == top.ID {
		return commentbridge.Comment{}, false
	}
	root := firstString(item, []string{"root"}, []string{"root_str"}, []string{"root_rpid"}, []string{"root_rpid_str"})
	if root == "" || root == "0" || root == reply.ID {
		root = top.ID
	}
	parent := firstString(item, []string{"parent"}, []string{"parent_str"}, []string{"parent_rpid"}, []string{"parent_rpid_str"})
	if parent == "" || parent == "0" {
		parent = top.ID
	}
	reply.RootID, reply.ParentID = root, parent
	if reply.ResourceTitle == "" {
		reply.ResourceTitle = top.ResourceTitle
	}
	if !hasCommentURL(item) {
		reply.ResourceURL = top.ResourceURL
	}
	return reply, true
}

func hasCommentURL(item map[string]any) bool {
	return firstString(item, []string{"archive", "jump_url"}, []string{"url"}, []string{"bvid"}, []string{"archive", "bvid"}, []string{"video", "bvid"}) != ""
}

func directReplies(item map[string]any) []map[string]any {
	value, ok := item["replies"].([]any)
	if !ok {
		return nil
	}
	replies := make([]map[string]any, 0, len(value))
	for _, raw := range value {
		if reply, ok := raw.(map[string]any); ok && looksLikeComment(reply) {
			replies = append(replies, reply)
		}
	}
	return replies
}

func commentReplyCount(item map[string]any) int {
	count, err := strconv.Atoi(firstString(item, []string{"rcount"}, []string{"reply_count"}, []string{"sub_reply_count"}, []string{"count"}))
	if err != nil || count < 0 {
		return 0
	}
	return count
}

// fetchSubReplies returns all normalized replies found during this bounded
// pass and whether the API response establishes that the thread was complete.
func (a *CommentAdapter) fetchSubReplies(ctx context.Context, top commentbridge.Comment, expected int, direct []map[string]any) ([]commentbridge.Comment, bool, error) {
	seen := make(map[string]struct{}, len(direct))
	comments := make([]commentbridge.Comment, 0, expected)
	for _, raw := range direct {
		if reply, ok := a.parseSubReply(top, raw); ok {
			seen[reply.ID] = struct{}{}
		}
	}
	pageLimit := min(a.subReplyPages, max(1, (expected+commentSubReplyPageSize-1)/commentSubReplyPageSize))
	for page := 1; page <= pageLimit; page++ {
		params := url.Values{
			"pn": {strconv.Itoa(page)}, "ps": {strconv.Itoa(commentSubReplyPageSize)}, "type": {"1"}, "oid": {top.ResourceID}, "root": {top.ID},
		}
		var payload any
		if err := a.client.getJSON(ctx, "/x/v2/reply/reply", params, false, &payload); err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.Code == 12006 {
				// The inbox can still list a removed thread. Retain comments
				// already returned and avoid re-querying it until its count changes.
				log.Print("bilibili comment child thread unavailable: api_code=12006")
				if a.OnSkippedThread != nil {
					a.OnSkippedThread()
				}
				return comments, true, nil
			}
			return nil, false, wrapCredentialInvalid(err)
		}
		rawReplies, err := extractSubReplyItems(payload)
		if err != nil {
			return nil, false, err
		}
		newOnPage := 0
		for _, raw := range rawReplies {
			reply, ok := a.parseSubReply(top, raw)
			if !ok {
				continue
			}
			if _, duplicate := seen[reply.ID]; duplicate {
				continue
			}
			seen[reply.ID] = struct{}{}
			comments = append(comments, reply)
			newOnPage++
		}
		total := subReplyPageCount(payload)
		if len(seen) >= expected || (total > 0 && len(seen) >= total) {
			return comments, true, nil
		}
		if len(rawReplies) < commentSubReplyPageSize {
			return comments, len(seen) >= expected || (total > 0 && len(seen) >= total), nil
		}
		if newOnPage == 0 {
			return comments, false, nil
		}
	}
	return comments, false, nil
}

func extractSubReplyItems(payload any) ([]map[string]any, error) {
	object, ok := payload.(map[string]any)
	if !ok {
		return nil, errors.New("decode Bilibili sub-reply list")
	}
	values, ok := object["replies"]
	if !ok || values == nil {
		return nil, nil
	}
	array, ok := values.([]any)
	if !ok {
		return nil, errors.New("decode Bilibili sub-reply list")
	}
	items := make([]map[string]any, 0, len(array))
	for _, value := range array {
		item, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("decode Bilibili sub-reply list")
		}
		if looksLikeComment(item) {
			items = append(items, item)
		}
	}
	return items, nil
}

func subReplyPageCount(payload any) int {
	object, ok := payload.(map[string]any)
	if !ok {
		return 0
	}
	page, ok := object["page"].(map[string]any)
	if !ok {
		return 0
	}
	return commentReplyCount(page)
}

func extractCommentItems(payload any) []map[string]any {
	var candidates [][]map[string]any
	var walk func(any, int)
	walk = func(value any, depth int) {
		if depth > 32 {
			return
		}
		switch value := value.(type) {
		case []any:
			var comments []map[string]any
			for _, child := range value {
				if item, ok := child.(map[string]any); ok && looksLikeComment(item) {
					comments = append(comments, item)
				}
			}
			if len(comments) > 0 {
				candidates = append(candidates, comments)
			}
			for _, child := range value {
				walk(child, depth+1)
			}
		case map[string]any:
			for _, child := range value {
				walk(child, depth+1)
			}
		}
	}
	walk(payload, 0)
	var best []map[string]any
	for _, candidate := range candidates {
		if len(candidate) > len(best) {
			best = candidate
		}
	}
	return best
}

// The creator inbox itself may call its top-level items either list or
// replies. Prefer that envelope over the generic walker: otherwise a thread
// with several embedded replies can be mistaken for the inbox page.
func extractTopLevelCommentItems(payload any) []map[string]any {
	object, ok := payload.(map[string]any)
	if !ok {
		return extractCommentItems(payload)
	}
	for _, key := range []string{"list", "replies"} {
		values, ok := object[key].([]any)
		if !ok {
			continue
		}
		items := make([]map[string]any, 0, len(values))
		for _, value := range values {
			if item, ok := value.(map[string]any); ok && looksLikeComment(item) {
				items = append(items, item)
			}
		}
		return items
	}
	return extractCommentItems(payload)
}

func looksLikeComment(item map[string]any) bool {
	return firstString(item, []string{"rpid"}, []string{"rpid_str"}, []string{"id"}, []string{"id_str"}) != "" &&
		(dig(item, []string{"content"}) != nil || dig(item, []string{"message"}) != nil || dig(item, []string{"member"}) != nil || dig(item, []string{"uname"}) != nil || dig(item, []string{"mid"}) != nil)
}

func firstString(item map[string]any, paths ...[]string) string {
	for _, path := range paths {
		if value := scalarString(dig(item, path)); value != "" {
			return value
		}
	}
	return ""
}

func dig(item map[string]any, path []string) any {
	var current any = item
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[key]
	}
	return current
}

func scalarString(value any) string {
	switch value := value.(type) {
	case string:
		return strings.TrimSpace(value)
	case json.Number:
		return value.String()
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(value), 'f', -1, 32)
	case int:
		return strconv.Itoa(value)
	case int64:
		return strconv.FormatInt(value, 10)
	case uint64:
		return strconv.FormatUint(value, 10)
	default:
		return ""
	}
}

func normalizeBilibiliURL(raw string) string {
	switch {
	case strings.HasPrefix(raw, "//"):
		raw = "https:" + raw
	case strings.HasPrefix(raw, "/"):
		raw = "https://www.bilibili.com" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || (u.Hostname() != "bilibili.com" && !strings.HasSuffix(u.Hostname(), ".bilibili.com")) || u.User != nil {
		return ""
	}
	return u.String()
}

func jsonScalarString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

// Keep the same conservative ceiling as bilibili-comment-studio. The Web API
// is undocumented, so relying on a wider observed limit would be brittle.
const maxCommentReplyRunes = 900

// Reply sends one POST request, except that an explicit WBI-signature
// rejection (-400/-403) permits one freshly signed retry because the server
// has definitively rejected the first write. Errors marked UnknownOutcome are
// never retried because Bilibili may already have accepted the request.
func (a *CommentAdapter) Reply(ctx context.Context, comment commentbridge.Comment, text string) (string, error) {
	if a == nil || a.client == nil {
		return "", errors.New("bilibili comment adapter has no client")
	}
	if comment.Platform != commentbridge.PlatformBilibili ||
		(strings.TrimSpace(comment.AccountID) != "" && strings.TrimSpace(comment.AccountID) != a.accountID) {
		return "", errors.New("bilibili reply target belongs to another account")
	}
	if a.isRiskFused() {
		return "", ErrCommentRiskControl
	}
	message := strings.TrimSpace(text)
	if message == "" || utf8.RuneCountInString(message) > maxCommentReplyRunes {
		return "", errors.New("bilibili reply text is empty or too long")
	}
	oid, rpid := strings.TrimSpace(comment.ResourceID), strings.TrimSpace(comment.ID)
	if oid == "" || rpid == "" {
		return "", errors.New("bilibili reply target is incomplete")
	}
	credential := a.client.Credential()
	csrf := credential.Cookies["bili_jct"]
	if credential.Cookies["SESSDATA"] == "" || csrf == "" {
		return "", errors.New("bilibili reply requires SESSDATA and bili_jct")
	}
	form := url.Values{
		"oid": {oid}, "type": {"1"}, "message": {message}, "plat": {"1"},
		"statistics": {`{"appId":100,"platform":5}`}, "gaia_source": {"main_web"},
		"root": {firstNonEmpty(strings.TrimSpace(comment.RootID), rpid)}, "parent": {rpid}, "csrf": {csrf}, "csrf_token": {csrf},
	}
	for signatureAttempt := 0; signatureAttempt < 2; signatureAttempt++ {
		keys, err := a.client.wbiKeys(ctx)
		if err != nil {
			err = wrapCredentialInvalid(err)
			if isCommentRiskError(err) {
				a.tripRiskFuse()
			}
			return "", err
		}
		status, body, err := a.postReplyOnce(ctx, form, keys)
		if err != nil {
			return "", err
		}
		if status >= 500 || status >= 300 && status < 400 {
			return "", newReplyUnknownError("bilibili reply request outcome is unknown")
		}
		if status < 200 || status > 299 {
			if status == http.StatusPreconditionFailed || status == http.StatusTooManyRequests {
				a.tripRiskFuse()
				return "", fmt.Errorf("%w (HTTP status %d)", ErrCommentRiskControl, status)
			}
			return "", fmt.Errorf("bilibili reply rejected with HTTP status %d", status)
		}
		var envelope commentReplyEnvelope
		if json.Unmarshal(body, &envelope) != nil {
			return "", newReplyUnknownError("bilibili reply request outcome is unknown")
		}
		if envelope.Code == -400 || envelope.Code == -403 {
			a.client.invalidateWBIKeys()
			if signatureAttempt == 0 {
				continue
			}
		}
		if envelope.Code != 0 {
			if envelope.Code == -101 || envelope.Code == -111 {
				a.tripRiskFuse()
				return "", credentialInvalidError(envelope.Code)
			}
			if envelope.Code == -352 || envelope.Code == -412 || envelope.Code == 412 || envelope.Code == -509 {
				a.tripRiskFuse()
				return "", fmt.Errorf("%w (API code %d)", ErrCommentRiskControl, envelope.Code)
			}
			return "", fmt.Errorf("bilibili reply rejected with API code %d", envelope.Code)
		}
		id := strings.TrimSpace(envelope.Data.RPIDStr)
		if id == "" {
			id = jsonScalarString(envelope.Data.RPID)
		}
		if id == "" {
			id = strings.TrimSpace(envelope.Data.Reply.RPIDStr)
		}
		if id == "" {
			id = jsonScalarString(envelope.Data.Reply.RPID)
		}
		if id == "" {
			return "", newReplyUnknownError("bilibili reply request outcome is unknown")
		}
		return id, nil
	}
	return "", errors.New("bilibili reply signature refresh failed")
}

// Engage performs one human-approved engagement action against a Bilibili
// comment. Comment actions are browser form posts, but unlike replies this
// endpoint does not use WBI signing. Writes are deliberately never retried:
// once a request is handed to the network, an indeterminate response might
// still represent a successful like.
func (a *CommentAdapter) Engage(ctx context.Context, comment commentbridge.Comment, kind commentbridge.EngagementKind) (string, error) {
	if a == nil || a.client == nil {
		return "", errors.New("bilibili comment adapter has no client")
	}
	if kind != commentbridge.EngagementLike {
		return "", errors.New("unsupported Bilibili comment engagement")
	}
	if comment.Platform != commentbridge.PlatformBilibili ||
		strings.TrimSpace(a.accountID) == "" ||
		strings.TrimSpace(comment.AccountID) == "" ||
		strings.TrimSpace(comment.AccountID) != a.accountID {
		return "", errors.New("bilibili engagement target belongs to another account")
	}
	if a.isRiskFused() {
		return "", ErrCommentRiskControl
	}
	oid, rpid := strings.TrimSpace(comment.ResourceID), strings.TrimSpace(comment.ID)
	if oid == "" || oid == "0" || rpid == "" || rpid == "0" {
		return "", errors.New("bilibili engagement target is incomplete")
	}
	credential := a.client.Credential()
	csrf := credential.Cookies["bili_jct"]
	if credential.Cookies["SESSDATA"] == "" || csrf == "" {
		return "", errors.New("bilibili engagement requires SESSDATA and bili_jct")
	}
	status, body, err := a.postCommentEngagementOnce(ctx, url.Values{
		"oid":        {oid},
		"type":       {"1"},
		"rpid":       {rpid},
		"action":     {"1"},
		"csrf":       {csrf},
		"csrf_token": {csrf},
	})
	if err != nil {
		return "", err
	}
	if status >= http.StatusMultipleChoices && status < http.StatusBadRequest || status >= http.StatusInternalServerError {
		return "", newReplyUnknownError("bilibili comment engagement outcome is unknown")
	}
	if status < http.StatusOK || status > 299 {
		if status == http.StatusForbidden || status == http.StatusPreconditionFailed || status == http.StatusTooManyRequests {
			a.tripRiskFuse()
			return "", fmt.Errorf("%w (HTTP status %d)", ErrCommentRiskControl, status)
		}
		return "", fmt.Errorf("bilibili comment engagement rejected with HTTP status %d", status)
	}
	var envelope struct {
		Code *int `json:"code"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Code == nil {
		return "", newReplyUnknownError("bilibili comment engagement outcome is unknown")
	}
	switch *envelope.Code {
	case 0:
		// The action response does not have a durable action ID. The comment ID
		// is stable and identifies the sole resource that was engaged.
		return rpid, nil
	case -101, -111:
		a.tripRiskFuse()
		return "", credentialInvalidError(*envelope.Code)
	case -102:
		// -102 is an account-state rejection, not evidence that the stored
		// browser cookie has expired. Preserve the historical authentication
		// classification and fuse behavior, but do not raise the credential
		// invalid sentinel used for operator notifications.
		a.tripRiskFuse()
		return "", fmt.Errorf("%w (API code %d)", ErrAuthentication, *envelope.Code)
	case -352, -403, -412, -509:
		a.tripRiskFuse()
		return "", fmt.Errorf("%w (API code %d)", ErrCommentRiskControl, *envelope.Code)
	default:
		return "", fmt.Errorf("bilibili comment engagement rejected with API code %d", *envelope.Code)
	}
}

// postCommentEngagementOnce is intentionally separate from postReplyOnce so
// a future change to reply signing cannot accidentally add WBI parameters or
// retries to this write endpoint.
func (a *CommentAdapter) postCommentEngagementOnce(ctx context.Context, form url.Values) (int, []byte, error) {
	endpoint := *a.client.base
	endpoint.Path = strings.TrimRight(a.client.base.Path, "/") + "/x/v2/reply/action"
	endpoint.RawQuery = ""
	if err := a.client.gate.Wait(ctx); err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, errors.New("invalid Bilibili comment engagement request")
	}
	req.Header.Set("User-Agent", a.client.userAgent)
	req.Header.Set("Origin", "https://www.bilibili.com")
	req.Header.Set("Referer", "https://www.bilibili.com/")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hc := *a.client.http
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := hc.Do(req)
	if err != nil {
		return 0, nil, newReplyUnknownError("bilibili comment engagement outcome is unknown")
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	response.Body.Close()
	if readErr != nil || ctx.Err() != nil {
		return 0, nil, newReplyUnknownError("bilibili comment engagement outcome is unknown")
	}
	return response.StatusCode, body, nil
}

type commentReplyEnvelope struct {
	Code int `json:"code"`
	Data struct {
		RPID    json.RawMessage `json:"rpid"`
		RPIDStr string          `json:"rpid_str"`
		Reply   struct {
			RPID    json.RawMessage `json:"rpid"`
			RPIDStr string          `json:"rpid_str"`
		} `json:"reply"`
	} `json:"data"`
}

func (a *CommentAdapter) postReplyOnce(ctx context.Context, form url.Values, keys WBIKeys) (int, []byte, error) {
	query := SignWBI(wbi2Fingerprint(), keys, a.client.now())
	endpoint := *a.client.base
	endpoint.Path = strings.TrimRight(a.client.base.Path, "/") + "/x/v2/reply/add"
	endpoint.RawQuery = EncodeWBI(query)
	if err := a.client.gate.Wait(ctx); err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, errors.New("invalid Bilibili reply request")
	}
	req.Header.Set("User-Agent", a.client.userAgent)
	req.Header.Set("Origin", "https://www.bilibili.com")
	req.Header.Set("Referer", "https://www.bilibili.com/")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hc := *a.client.http
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := hc.Do(req)
	if err != nil {
		return 0, nil, newReplyUnknownError("bilibili reply request outcome is unknown")
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	response.Body.Close()
	if readErr != nil || ctx.Err() != nil {
		return 0, nil, newReplyUnknownError("bilibili reply request outcome is unknown")
	}
	return response.StatusCode, body, nil
}

func wbi2Fingerprint() url.Values {
	const (
		width  = 1280
		height = 665
		vendor = "WebGL 1.0 (OpenGL ES 2.0 Chromium)"
		gpu    = "ANGLE (Intel, Intel(R) Iris(R) Xe Graphics (0x00009A49) Direct3D11 vs_5_0 ps_5_0, D3D11)"
	)
	whRandom := rand.IntN(114)
	ofRandom := rand.IntN(514)
	wh := []int{2*width + 2*height + 3*whRandom, 4*width - height + whRandom, whRandom}
	of := []int{ofRandom, 2 * ofRandom, ofRandom}
	return url.Values{
		"dm_img_list":      {"[]"},
		"dm_img_str":       {encodeB64Trim(vendor)},
		"dm_cover_img_str": {encodeB64Trim(gpu)},
		"dm_img_inter":     {fmt.Sprintf(`{"ds":[],"wh":[%d,%d,%d],"of":[%d,%d,%d]}`, wh[0], wh[1], wh[2], of[0], of[1], of[2])},
	}
}

func encodeB64Trim(value string) string {
	return strings.TrimRight(base64.StdEncoding.EncodeToString([]byte(value)), "=")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (a *CommentAdapter) tripRiskFuse() {
	a.riskMu.Lock()
	a.riskFused = true
	a.riskMu.Unlock()
}

func (a *CommentAdapter) clearRiskFuse() {
	a.riskMu.Lock()
	a.riskFused = false
	a.readRetryAt, a.readCooldown = time.Time{}, 0
	a.riskMu.Unlock()
}

func (a *CommentAdapter) isRiskFused() bool {
	a.riskMu.RLock()
	fused := a.riskFused || !a.readRetryAt.IsZero()
	a.riskMu.RUnlock()
	return fused
}

// Only successful reads reopen a read-side fuse. Writes and authentication
// failures still require human intervention; no failed write is retried here.
func (a *CommentAdapter) finishReadAttempt(err error) {
	a.riskMu.Lock()
	defer a.riskMu.Unlock()
	if err == nil {
		if !a.readRetryAt.IsZero() {
			log.Print("bilibili comment read recovered")
		}
		a.readRetryAt, a.readCooldown = time.Time{}, 0
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	var apiErr *APIError
	code := 0
	if errors.As(err, &apiErr) {
		code = apiErr.Code
	}
	if errors.Is(err, ErrAuthentication) || code == -102 {
		a.riskFused = true
		log.Printf("bilibili comment read requires login or account review: api_code=%d", code)
		return
	}
	// Cool down the whole read pass, including child replies and network errors.
	// Retry-After is a lower bound even when it exceeds our 24-hour backoff cap.
	a.readCooldown = min(24*time.Hour, max(time.Hour, 2*a.readCooldown))
	delay := a.readCooldown
	var cooldown *CooldownError
	if errors.As(err, &cooldown) {
		delay = max(delay, cooldown.RetryAfter)
	}
	a.readRetryAt = a.client.now().Add(delay)
	log.Printf("bilibili comment read cooling down: api_code=%d rate_limited=%t retry_at=%s", code, errors.Is(err, ErrRateLimited), a.readRetryAt.UTC().Format(time.RFC3339))
}

func isCommentRiskError(err error) bool {
	if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrAuthentication) {
		return true
	}
	var apiErr *APIError
	return errors.As(err, &apiErr) && (apiErr.Code == -352 || apiErr.Code == -412 || apiErr.Code == 412 || apiErr.Code == -509)
}

// wrapCredentialInvalid adds the bridge-level sentinel only to structured
// Bilibili authentication API failures. Transport errors, malformed JSON,
// WBI/signature failures, rate limits, and anti-abuse responses deliberately
// remain unrelated so an operator is not told to rescan a still-valid cookie.
func wrapCredentialInvalid(err error) error {
	if err == nil || errors.Is(err, commentbridge.ErrCredentialInvalid) || !isCredentialInvalidAPIError(err) {
		return err
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	return fmt.Errorf("%w: %w", commentbridge.NewCredentialInvalidError(credentialInvalidReason(apiErr.Code)), err)
}

func isCredentialInvalidAPIError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && (apiErr.Code == -101 || apiErr.Code == -111)
}

func credentialInvalidError(code int) error {
	return fmt.Errorf("%w: %w (API code %d)", commentbridge.NewCredentialInvalidError(credentialInvalidReason(code)), ErrAuthentication, code)
}

func credentialInvalidReason(code int) commentbridge.CredentialInvalidReason {
	if code == -111 {
		return commentbridge.CredentialInvalidCSRF
	}
	return commentbridge.CredentialInvalidLogin
}

type replyUnknownError struct{ message string }

func newReplyUnknownError(message string) error { return replyUnknownError{message: message} }
func (e replyUnknownError) Error() string       { return e.message }
func (replyUnknownError) OutcomeUnknown() bool  { return true }

var _ commentbridge.CommentSource = (*CommentAdapter)(nil)
var _ commentbridge.CommentReplier = (*CommentAdapter)(nil)
var _ commentbridge.CommentEngager = (*CommentAdapter)(nil)
var _ commentbridge.CredentialRefresher = (*CommentAdapter)(nil)
