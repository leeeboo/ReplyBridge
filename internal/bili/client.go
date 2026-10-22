package bili

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"comment-bridge/internal/commentbridge"
)

const defaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"

// Config is intentionally small. Supply BaseURL (and a disabled Gate) when
// testing with httptest; production defaults target the public web API.
type Config struct {
	RiskStop    *commentbridge.BilibiliRiskStop
	HTTPClient  *http.Client
	BaseURL     string
	PassportURL string
	UserAgent   string
	Gate        *RequestGate
	RetryMax    int
	RetryBase   time.Duration
	Now         func() time.Time
}

type Client struct {
	http      *http.Client
	jar       http.CookieJar
	base      *url.URL
	passport  *url.URL
	userAgent string
	gate      *RequestGate
	retryMax  int
	retryBase time.Duration
	now       func() time.Time

	keysMu sync.Mutex
	keys   WBIKeys
	keysAt time.Time
}

const wbiKeyTTL = 6 * time.Hour

func NewClient(cfg Config) (*Client, error) {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.bilibili.com"
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid Bilibili base URL")
	}
	passportURL := cfg.PassportURL
	if passportURL == "" {
		if cfg.BaseURL != "" {
			passportURL = cfg.BaseURL
		} else {
			passportURL = "https://passport.bilibili.com"
		}
	}
	passport, err := url.Parse(passportURL)
	if err != nil || passport.Scheme == "" || passport.Host == "" {
		return nil, fmt.Errorf("invalid Bilibili passport URL")
	}
	hc := &http.Client{Timeout: 20 * time.Second}
	if cfg.HTTPClient != nil {
		*hc = *cfg.HTTPClient
	}
	if cfg.RiskStop != nil {
		base := hc.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		hc.Transport = &riskTransport{base: base, stop: cfg.RiskStop}
	}
	if hc.Jar == nil {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return nil, err
		}
		hc.Jar = jar
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	if cfg.Gate == nil {
		cfg.Gate = NewRequestGate(DefaultGateConfig())
	}
	if cfg.RetryMax == 0 {
		cfg.RetryMax = 3
	}
	if cfg.RetryBase == 0 {
		cfg.RetryBase = 2 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Client{http: hc, jar: hc.Jar, base: base, passport: passport, userAgent: cfg.UserAgent, gate: cfg.Gate, retryMax: cfg.RetryMax, retryBase: cfg.RetryBase, now: cfg.Now}, nil
}

// Nav returns the raw nav data and updates the WBI key cache when possible.
func (c *Client) Nav(ctx context.Context) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.getJSON(ctx, "/x/web-interface/nav", nil, false, &raw); err != nil {
		return nil, err
	}
	c.cacheKeysFromNav(raw)
	return raw, nil
}

func (c *Client) getJSON(ctx context.Context, endpoint string, params url.Values, signed bool, out any) error {
	return c.getJSONAt(ctx, c.base, endpoint, params, signed, out)
}

func (c *Client) getJSONAt(ctx context.Context, base *url.URL, endpoint string, params url.Values, signed bool, out any) error {
	refreshTried := false
	for {
		values := cloneValues(params)
		if signed {
			keys, err := c.wbiKeys(ctx)
			if err != nil {
				return err
			}
			values = SignWBI(values, keys, c.now())
		}
		body, response, err := c.request(ctx, base, endpoint, values)
		if err != nil {
			return err
		}
		if response.StatusCode == http.StatusPreconditionFailed {
			return &CooldownError{RetryAfter: maxDuration(30*time.Minute, retryAfter(response.Header, c.now()))}
		}
		if response.StatusCode < 200 || response.StatusCode > 299 {
			return fmt.Errorf("bilibili HTTP status %d", response.StatusCode)
		}
		var envelope struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			return fmt.Errorf("decode Bilibili response: %w", err)
		}
		if envelope.Code != 0 {
			apiErr := &APIError{Code: envelope.Code, Message: envelope.Message}
			if signed && !refreshTried && isWBISignatureError(apiErr) {
				refreshTried = true
				c.invalidateWBIKeys()
				continue
			}
			if errors.Is(apiErr, ErrRateLimited) {
				return &CooldownError{RetryAfter: 30 * time.Minute}
			}
			return apiErr
		}
		if out == nil {
			return nil
		}
		if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
			return nil
		}
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("decode Bilibili data: %w", err)
		}
		return nil
	}
}

func (c *Client) request(ctx context.Context, base *url.URL, endpoint string, params url.Values) ([]byte, *http.Response, error) {
	requestURL := *base
	requestURL.Path = strings.TrimRight(base.Path, "/") + "/" + strings.TrimLeft(endpoint, "/")
	requestURL.RawQuery = EncodeWBI(params)
	return c.requestURL(ctx, requestURL.String())
}

// requestURL deliberately does not include the URL in errors: subtitle URLs
// sometimes carry short-lived query credentials.
func (c *Client) requestURL(ctx context.Context, rawURL string) ([]byte, *http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= c.retryMax; attempt++ {
		if err := c.gate.Wait(ctx); err != nil {
			return nil, nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, nil, errors.New("invalid Bilibili request URL")
		}
		req.Header.Set("User-Agent", c.userAgent)
		req.Header.Set("Referer", "https://www.bilibili.com/")
		response, err := c.http.Do(req)
		if err != nil {
			if errors.Is(err, commentbridge.ErrBilibiliRiskStopped) {
				return nil, nil, commentbridge.ErrBilibiliRiskStopped
			}
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			// net/url errors can embed the request URL. Avoid returning those
			// because subtitle links may contain short-lived query credentials.
			lastErr = errors.New("bilibili network request failed")
		} else {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
			response.Body.Close()
			if readErr != nil {
				return nil, response, fmt.Errorf("read Bilibili response: %w", readErr)
			}
			if response.StatusCode == http.StatusTooManyRequests {
				return nil, response, &CooldownError{RetryAfter: maxDuration(30*time.Minute, retryAfter(response.Header, c.now()))}
			}
			if response.StatusCode >= 500 {
				lastErr = httpStatusError{status: response.StatusCode, after: retryAfter(response.Header, c.now())}
				if attempt == c.retryMax {
					return nil, response, lastErr
				}
			} else {
				return body, response, nil
			}
		}
		if attempt == c.retryMax {
			break
		}
		delay := c.retryDelay(attempt, lastErr)
		if err := waitContext(ctx, delay); err != nil {
			return nil, nil, err
		}
	}
	return nil, nil, lastErr
}

type httpStatusError struct {
	status int
	after  time.Duration
}

func (e httpStatusError) Error() string { return fmt.Sprintf("bilibili HTTP status %d", e.status) }

func (c *Client) retryDelay(attempt int, err error) time.Duration {
	if status, ok := err.(httpStatusError); ok && status.status == http.StatusTooManyRequests {
		base := status.after
		if base <= 0 {
			base = time.Minute * time.Duration(math.Pow(2, float64(attempt)))
		}
		if base > 10*time.Minute {
			base = 10 * time.Minute
		}
		// Retry-After is a lower bound; only add positive jitter.
		return base + randomUpTo(base/4)
	}
	multiplier := math.Pow(2, float64(attempt))
	delay := time.Duration(float64(c.retryBase) * multiplier)
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	// Keep transient retries irregular while preserving a non-zero floor.
	return delay/2 + randomUpTo(delay/2)
}

func randomUpTo(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(max) + 1))
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func waitContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryAfter(header http.Header, now time.Time) time.Duration {
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if seconds, err := time.ParseDuration(raw + "s"); err == nil && seconds > 0 {
		return seconds
	}
	if when, err := http.ParseTime(raw); err == nil && when.After(now) {
		return when.Sub(now)
	}
	return 0
}

func cloneValues(values url.Values) url.Values {
	if values == nil {
		return make(url.Values)
	}
	copy := make(url.Values, len(values))
	for k, entries := range values {
		copy[k] = append([]string(nil), entries...)
	}
	return copy
}

func isWBISignatureError(err *APIError) bool {
	// -403 is the common stale w_rid result. -400 is also returned by some
	// edge nodes for malformed/expired WBI signatures.
	return err.Code == -403 || err.Code == -400
}

func (c *Client) wbiKeys(ctx context.Context) (WBIKeys, error) {
	c.keysMu.Lock()
	defer c.keysMu.Unlock()
	now := c.now()
	if c.keys.Valid() && (c.keysAt.IsZero() || now.Before(c.keysAt.Add(wbiKeyTTL))) {
		return c.keys, nil
	}
	c.keys, c.keysAt = WBIKeys{}, time.Time{}
	var nav json.RawMessage
	if err := c.getJSON(ctx, "/x/web-interface/nav", nil, false, &nav); err != nil {
		return WBIKeys{}, err
	}
	c.cacheKeysFromNavLocked(nav)
	if !c.keys.Valid() {
		return WBIKeys{}, errors.New("Bilibili nav response has no WBI keys")
	}
	return c.keys, nil
}

func (c *Client) invalidateWBIKeys() {
	c.keysMu.Lock()
	c.keys, c.keysAt = WBIKeys{}, time.Time{}
	c.keysMu.Unlock()
}

func (c *Client) cacheKeysFromNav(raw json.RawMessage) {
	c.keysMu.Lock()
	defer c.keysMu.Unlock()
	c.cacheKeysFromNavLocked(raw)
}

func (c *Client) cacheKeysFromNavLocked(raw json.RawMessage) {
	var nav struct {
		WBIImg struct {
			ImgURL string `json:"img_url"`
			SubURL string `json:"sub_url"`
		} `json:"wbi_img"`
	}
	if json.Unmarshal(raw, &nav) == nil {
		keys := WBIKeys{ImgKey: WBIKeyFromURL(nav.WBIImg.ImgURL), SubKey: WBIKeyFromURL(nav.WBIImg.SubURL)}
		if keys.Valid() {
			c.keys, c.keysAt = keys, c.now()
		}
	}
}
