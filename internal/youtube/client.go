// Package youtube implements the YouTube Data API v3 comment adapter.
package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	forceSSLScope   = "https://www.googleapis.com/auth/youtube.force-ssl"
	defaultAPIURL   = "https://www.googleapis.com"
	defaultTokenURL = "https://oauth2.googleapis.com/token"
)

// Config configures a YouTube comment client. APIBaseURL and TokenURL are
// injectable to make the official API contract testable with httptest.
type Config struct {
	HTTPClient   *http.Client
	APIBaseURL   string
	TokenURL     string
	ClientID     string
	ClientSecret string
	RefreshToken string
	ChannelID    string
	AccountID    string
	Scope        string
	MaxPages     int
	Now          func() time.Time
}

// Client is a YouTube Data API v3 CommentSource and CommentReplier.
type Client struct {
	http         *http.Client
	apiBase      *url.URL
	tokenURL     *url.URL
	clientID     string
	clientSecret string
	refreshToken string
	channelID    string
	accountID    string
	maxPages     int
	now          func() time.Time

	tokenMu     sync.Mutex
	accessToken string
	tokenExpiry time.Time

	replyCountsMu sync.Mutex
	replyCounts   map[string]uint64

	videoTitlesMu sync.Mutex
	videoTitles   map[string]string
}

// OwnedChannel is one YouTube channel managed by the authorized identity.
// Only public channel metadata is returned; OAuth credentials stay private.
type OwnedChannel struct {
	ID    string
	Title string
}

// NewClient validates configuration and constructs a client. The exact
// youtube.force-ssl scope is required because the client replies to comments.
func NewClient(cfg Config) (*Client, error) {
	if !hasScope(cfg.Scope, forceSSLScope) {
		return nil, errors.New("YouTube scope must include comment access")
	}
	if strings.TrimSpace(cfg.ClientID) == "" || strings.TrimSpace(cfg.RefreshToken) == "" {
		return nil, errors.New("YouTube OAuth configuration is incomplete")
	}
	if strings.TrimSpace(cfg.ChannelID) == "" {
		return nil, errors.New("YouTube channel ID is required")
	}
	apiBase, err := parseEndpoint(cfg.APIBaseURL, defaultAPIURL)
	if err != nil {
		return nil, errors.New("invalid YouTube API base URL")
	}
	tokenURL, err := parseEndpoint(cfg.TokenURL, defaultTokenURL)
	if err != nil {
		return nil, errors.New("invalid YouTube token URL")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 20 * time.Second}
	}
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = 1
	}
	if cfg.MaxPages > 20 {
		return nil, errors.New("YouTube max pages must not exceed 20")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if strings.TrimSpace(cfg.AccountID) == "" {
		cfg.AccountID = cfg.ChannelID
	}
	return &Client{http: cfg.HTTPClient, apiBase: apiBase, tokenURL: tokenURL, clientID: cfg.ClientID,
		clientSecret: cfg.ClientSecret, refreshToken: cfg.RefreshToken, channelID: cfg.ChannelID,
		accountID: cfg.AccountID, maxPages: cfg.MaxPages, now: cfg.Now, replyCounts: make(map[string]uint64),
		videoTitles: make(map[string]string)}, nil
}

func parseEndpoint(raw, fallback string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		raw = fallback
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("invalid endpoint")
	}
	return u, nil
}

func hasScope(scope, required string) bool {
	for _, candidate := range strings.Fields(scope) {
		if candidate == required {
			return true
		}
	}
	return false
}

func (c *Client) apiURL(path string, query url.Values) string {
	u := *c.apiBase
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimLeft(path, "/")
	u.RawQuery = query.Encode()
	return u.String()
}

func (c *Client) bearer(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.accessToken != "" && c.now().Add(60*time.Second).Before(c.tokenExpiry) {
		return c.accessToken, nil
	}

	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.refreshToken}, "client_id": {c.clientID}}
	if c.clientSecret != "" {
		form.Set("client_secret", c.clientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", errors.New("YouTube token refresh request could not be created")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// A redirect could disclose the refresh token to another endpoint. Token
	// exchange is a single, non-retried POST to the configured authority.
	hc := *c.http
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return "", errors.New("YouTube token refresh failed")
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if readErr != nil {
		return "", errors.New("YouTube token refresh failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("YouTube token refresh failed: status %d", resp.StatusCode)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
	}
	if json.Unmarshal(body, &payload) != nil || strings.TrimSpace(payload.AccessToken) == "" || payload.ExpiresIn <= 0 || (payload.TokenType != "" && !strings.EqualFold(payload.TokenType, "Bearer")) {
		return "", errors.New("YouTube token refresh returned an invalid response")
	}
	if payload.Scope != "" && !hasScope(payload.Scope, forceSSLScope) {
		return "", errors.New("YouTube token refresh lacks required scopes")
	}
	c.accessToken, c.tokenExpiry = payload.AccessToken, c.now().Add(time.Duration(payload.ExpiresIn)*time.Second)
	return c.accessToken, nil
}

func (c *Client) clearToken() {
	c.tokenMu.Lock()
	c.accessToken, c.tokenExpiry = "", time.Time{}
	c.tokenMu.Unlock()
}

// DiscoverOwnedChannels lists channels managed by the OAuth identity. It is a
// read-only onboarding helper used before a channel ID has been configured.
func DiscoverOwnedChannels(ctx context.Context, cfg Config) ([]OwnedChannel, error) {
	// NewClient deliberately requires a channel ID for the live bridge. Supply
	// an internal sentinel only for this read-only discovery request.
	cfg.ChannelID = "youtube-channel-discovery"
	cfg.AccountID = "youtube-channel-discovery"
	c, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}
	token, err := c.bearer(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL("youtube/v3/channels", url.Values{"part": {"id,snippet"}, "mine": {"true"}, "maxResults": {"50"}}), nil)
	if err != nil {
		return nil, errors.New("YouTube channel discovery request could not be created")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	hc := *c.http
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return nil, errors.New("YouTube channel discovery failed")
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if readErr != nil {
		return nil, errors.New("YouTube channel discovery failed")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		c.clearToken()
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("YouTube channel discovery failed: status %d", resp.StatusCode)
	}
	var payload struct {
		Items []struct {
			ID      string `json:"id"`
			Snippet struct {
				Title string `json:"title"`
			} `json:"snippet"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return nil, errors.New("YouTube channel discovery returned an invalid response")
	}
	channels := make([]OwnedChannel, 0, len(payload.Items))
	for _, item := range payload.Items {
		if id := strings.TrimSpace(item.ID); id != "" {
			channels = append(channels, OwnedChannel{ID: id, Title: strings.TrimSpace(item.Snippet.Title)})
		}
	}
	return channels, nil
}

// ValidateChannel verifies that the OAuth identity owns the configured
// channel before any comments are polled or replies can be enqueued. Without
// this preflight, a typo could monitor and reply on somebody else's videos
// using the authenticated creator identity.
func (c *Client) ValidateChannel(ctx context.Context) error {
	token, err := c.bearer(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL("youtube/v3/channels", url.Values{"part": {"id"}, "mine": {"true"}, "maxResults": {"50"}}), nil)
	if err != nil {
		return errors.New("YouTube channel validation request could not be created")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	hc := *c.http
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return errors.New("YouTube channel validation failed")
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if readErr != nil {
		return errors.New("YouTube channel validation failed")
	}
	if resp.StatusCode == http.StatusUnauthorized {
		c.clearToken()
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("YouTube channel validation failed: status %d", resp.StatusCode)
	}
	var payload struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return errors.New("YouTube channel validation returned an invalid response")
	}
	for _, channel := range payload.Items {
		if channel.ID == c.channelID {
			return nil
		}
	}
	return errors.New("YouTube OAuth identity does not own the configured channel")
}

// AccountID returns the configured account identity.
func (c *Client) AccountID() string { return c.accountID }
