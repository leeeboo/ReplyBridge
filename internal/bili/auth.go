package bili

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Credential is an in-memory cookie collection. It is deliberately separate
// from CLI flags so callers never need to accept a plaintext cookie argument.
type Credential struct{ Cookies map[string]string }

func (c Credential) Valid() bool { return c.Cookies["SESSDATA"] != "" }

// Complete reports whether a QR-login result contains the three cookies
// required by the comment bridge.  Valid intentionally remains a weaker
// compatibility check for callers which only need to inspect a stored
// credential.
func (c Credential) Complete() bool {
	return c.Cookies["SESSDATA"] != "" && c.Cookies["bili_jct"] != "" && c.Cookies["DedeUserID"] != ""
}

func (c Credential) clone() Credential {
	copy := make(map[string]string, len(c.Cookies))
	for k, v := range c.Cookies {
		copy[k] = v
	}
	return Credential{Cookies: copy}
}

func (c Credential) MarshalJSON() ([]byte, error) { return json.Marshal(c.Cookies) }

func (c *Credential) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	parsed, err := credentialFromMap(raw)
	if err != nil {
		return err
	}
	*c = parsed
	return nil
}

// DefaultCredentialPath is intentionally distinct from bilibili-cli's path.
func DefaultCredentialPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".bili-sync", "credential.json"), nil
}

func LoadCredential() (Credential, error) {
	path, err := DefaultCredentialPath()
	if err != nil {
		return Credential{}, err
	}
	return LoadCredentialFile(path)
}

// LoadCredentialFile accepts the lower-case bilibili-cli credential format as
// well as a JSON object with normal cookie names.
func LoadCredentialFile(path string) (Credential, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Credential{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Credential{}, errors.New("bilibili credential must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Credential{}, fmt.Errorf("bilibili credential %s must not be group/world-readable", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return Credential{}, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0o077 != 0 {
		return Credential{}, errors.New("bilibili credential changed while opening")
	}
	return ImportCredentialJSON(f)
}

func SaveCredential(credential Credential) error {
	path, err := DefaultCredentialPath()
	if err != nil {
		return err
	}
	return SaveCredentialFile(path, credential)
}

func SaveCredentialFile(path string, credential Credential) error {
	if !credential.Valid() {
		return errors.New("bilibili credential has no SESSDATA")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(credential, "", "  ")
	if err != nil {
		return err
	}
	// Rename is atomic and the restrictive mode applies before any data is
	// written. Chmod also repairs an existing file's mode.
	directory := filepath.Dir(path)
	tmp, err := os.CreateTemp(directory, ".credential-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(payload)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmpName, path); err != nil {
		return err
	}
	if err = os.Chmod(path, 0o600); err != nil {
		return err
	}
	// Persist the rename itself.  A successful return therefore means both
	// the file contents and the directory entry have reached stable storage.
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func ImportCredentialJSON(r io.Reader) (Credential, error) {
	var raw map[string]any
	decoder := json.NewDecoder(io.LimitReader(r, 1<<20))
	if err := decoder.Decode(&raw); err != nil {
		return Credential{}, err
	}
	return credentialFromMap(raw)
}

// ImportCookieFile detects JSON credentials, Netscape cookies, and a raw
// Cookie-header-style `name=value; name2=value2` file. It intentionally takes
// a file, not a command-line string, to keep cookies out of shell history.
func ImportCookieFile(path string) (Credential, error) {
	f, err := os.Open(path)
	if err != nil {
		return Credential{}, err
	}
	defer f.Close()
	contents, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil {
		return Credential{}, err
	}
	trimmed := strings.TrimSpace(string(contents))
	if strings.HasPrefix(trimmed, "{") {
		return ImportCredentialJSON(strings.NewReader(trimmed))
	}
	if strings.Contains(trimmed, "\t") || strings.HasPrefix(trimmed, "# Netscape") || strings.HasPrefix(trimmed, "#HttpOnly_") {
		return ImportNetscapeCookies(strings.NewReader(trimmed))
	}
	return ImportRawCookieFile(strings.NewReader(trimmed))
}

// ImportRawCookieFile imports a raw Cookie header stored in a local file.
func ImportRawCookieFile(r io.Reader) (Credential, error) {
	contents, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return Credential{}, err
	}
	cookies := make(map[string]string)
	for _, pair := range strings.Split(string(contents), ";") {
		name, value, found := strings.Cut(strings.TrimSpace(pair), "=")
		if !found || name == "" || value == "" {
			continue
		}
		cookies[name] = value
	}
	if cookies["SESSDATA"] == "" {
		return Credential{}, errors.New("raw cookie file has no SESSDATA")
	}
	return Credential{Cookies: cookies}, nil
}

func credentialFromMap(raw map[string]any) (Credential, error) {
	cookies := make(map[string]string)
	aliases := map[string]string{
		"sessdata": "SESSDATA", "SESSDATA": "SESSDATA",
		"bili_jct": "bili_jct", "BILI_JCT": "bili_jct",
		"dedeuserid": "DedeUserID", "DedeUserID": "DedeUserID",
		"buvid3": "buvid3", "buvid4": "buvid4", "ac_time_value": "ac_time_value",
	}
	for source, destination := range aliases {
		if value, ok := raw[source].(string); ok && value != "" {
			cookies[destination] = value
		}
	}
	// A credential exported as {"cookies": {...}} is also useful for tools.
	if nested, ok := raw["cookies"].(map[string]any); ok {
		for key, value := range nested {
			if s, ok := value.(string); ok && s != "" {
				cookies[key] = s
			}
		}
	}
	if cookies["SESSDATA"] == "" {
		return Credential{}, errors.New("credential JSON has no SESSDATA")
	}
	return Credential{Cookies: cookies}, nil
}

// ImportNetscapeCookies imports a Netscape cookie file without exposing a raw
// cookie string. Only bilibili.com rows are admitted.
func ImportNetscapeCookies(r io.Reader) (Credential, error) {
	cookies := make(map[string]string)
	scanner := bufio.NewScanner(io.LimitReader(r, 4<<20))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "#HttpOnly_") {
			continue
		}
		if strings.HasPrefix(line, "#HttpOnly_") {
			line = strings.TrimPrefix(line, "#HttpOnly_")
		}
		columns := strings.Split(line, "\t")
		if len(columns) != 7 {
			continue
		}
		domain := strings.TrimPrefix(strings.ToLower(columns[0]), ".")
		if domain != "bilibili.com" && !strings.HasSuffix(domain, ".bilibili.com") {
			continue
		}
		if columns[5] != "" && columns[6] != "" {
			cookies[columns[5]] = columns[6]
		}
	}
	if err := scanner.Err(); err != nil {
		return Credential{}, err
	}
	if cookies["SESSDATA"] == "" {
		return Credential{}, errors.New("cookie file has no bilibili SESSDATA")
	}
	return Credential{Cookies: cookies}, nil
}

// SetCredential installs cookies in the client's jar for Bilibili endpoints.
func (c *Client) SetCredential(credential Credential) {
	if c.jar == nil {
		return
	}
	for _, base := range c.cookieBases() {
		cookies := make([]*http.Cookie, 0, len(credential.Cookies))
		for name, value := range credential.Cookies {
			cookies = append(cookies, &http.Cookie{Name: name, Value: value, Path: "/"})
		}
		c.jar.SetCookies(base, cookies)
	}
}

// ReplaceCredential removes every cookie currently visible to the client's
// Bilibili hosts before installing the supplied credential.  This is used
// after a verified QR login so an old cookie can never survive alongside the
// newly persisted credential.
func (c *Client) ReplaceCredential(credential Credential) {
	if c == nil || c.jar == nil {
		return
	}
	for _, base := range c.cookieBases() {
		cookies := c.jar.Cookies(base)
		if len(cookies) == 0 {
			continue
		}
		removed := make([]*http.Cookie, 0, len(cookies)*3)
		for _, cookie := range cookies {
			removed = append(removed,
				&http.Cookie{Name: cookie.Name, Path: "/", MaxAge: -1},
				&http.Cookie{Name: cookie.Name, Domain: base.Hostname(), Path: "/", MaxAge: -1},
				&http.Cookie{Name: cookie.Name, Domain: ".bilibili.com", Path: "/", MaxAge: -1},
			)
		}
		c.jar.SetCookies(base, removed)
	}
	c.SetCredential(credential)
}

func (c *Client) Credential() Credential {
	if c.jar == nil {
		return Credential{}
	}
	values := make(map[string]string)
	for _, cookie := range c.jar.Cookies(c.base) {
		values[cookie.Name] = cookie.Value
	}
	return Credential{Cookies: values}
}

func (c *Client) cookieBases() []*url.URL {
	result := []*url.URL{c.base}
	for _, raw := range []string{"https://www.bilibili.com", "https://api.bilibili.com", "https://passport.bilibili.com"} {
		if u, err := url.Parse(raw); err == nil {
			result = append(result, u)
		}
	}
	return result
}

// QRLogin contains the URL to render as a QR code and the opaque polling key.
type QRLogin struct{ URL, Key string }

type QRLoginStatus struct {
	Code       int
	Message    string
	Credential Credential
}

func (s QRLoginStatus) Done() bool { return s.Code == 0 && s.Credential.Complete() }

func (c *Client) GenerateQRLogin(ctx context.Context) (QRLogin, error) {
	var data struct {
		URL string `json:"url"`
		Key string `json:"qrcode_key"`
	}
	if err := c.getJSONAt(ctx, c.passport, "/x/passport-login/web/qrcode/generate", nil, false, &data); err != nil {
		return QRLogin{}, err
	}
	if data.URL == "" || data.Key == "" {
		return QRLogin{}, errors.New("bilibili QR response missing URL or key")
	}
	return QRLogin{URL: data.URL, Key: data.Key}, nil
}

func (c *Client) generateQRLoginIsolated(ctx context.Context) (QRLogin, error) {
	temporary, err := c.cloneWithCredential(Credential{})
	if err != nil {
		return QRLogin{}, err
	}
	login, err := temporary.GenerateQRLogin(ctx)
	if err != nil {
		return QRLogin{}, sanitizeQRLoginError(err, "bilibili QR login could not start")
	}
	return login, nil
}

// PollQRLogin polls once. Code 86101 means waiting to scan, 86090 means
// scanned but awaiting confirmation, and 86038 means expired.
func (c *Client) PollQRLogin(ctx context.Context, key string) (QRLoginStatus, error) {
	if key == "" {
		return QRLoginStatus{}, errors.New("empty QR login key")
	}
	var data struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		URL     string `json:"url"`
	}
	err := c.getJSONAt(ctx, c.passport, "/x/passport-login/web/qrcode/poll", url.Values{"qrcode_key": {key}}, false, &data)
	if err != nil {
		return QRLoginStatus{}, err
	}
	status := QRLoginStatus{Code: data.Code, Message: data.Message}
	if data.Code != 0 {
		return status, nil
	}
	if data.URL != "" {
		// The success URL sets SESSDATA through redirects. Do not log it.
		if err := c.followLoginURL(ctx, data.URL); err != nil {
			return status, err
		}
	}
	status.Credential = c.Credential()
	return status, nil
}

// pollQRLoginIsolated performs the browser callback in a fresh cookie jar.
// The normal PollQRLogin API preserves its historical in-place behavior for
// the CLI, while the comment bridge must not let an unverified or wrong-user
// QR result contaminate the live credential jar.
func (c *Client) pollQRLoginIsolated(ctx context.Context, key string) (QRLoginStatus, error) {
	temporary, err := c.cloneWithCredential(Credential{})
	if err != nil {
		return QRLoginStatus{}, err
	}
	status, err := temporary.PollQRLogin(ctx, key)
	if err != nil {
		return QRLoginStatus{}, sanitizeQRLoginError(err, "bilibili QR login polling failed")
	}
	return status, nil
}

func (c *Client) cloneWithCredential(credential Credential) (*Client, error) {
	if c == nil || c.http == nil || c.jar == nil {
		return nil, errors.New("bilibili credential client unavailable")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, errors.New("bilibili credential client unavailable")
	}
	hc := *c.http
	hc.Jar = jar
	temporary := &Client{
		http: &hc, jar: jar, base: c.base, passport: c.passport,
		userAgent: c.userAgent, gate: c.gate, retryMax: c.retryMax,
		retryBase: c.retryBase, now: c.now,
	}
	temporary.SetCredential(credential)
	return temporary, nil
}

func (c *Client) verifyCredential(ctx context.Context, credential Credential, accountID string) error {
	temporary, err := c.cloneWithCredential(credential)
	if err != nil {
		return err
	}
	raw, err := temporary.Nav(ctx)
	if err != nil {
		return errors.New("bilibili QR login online verification failed")
	}
	var data struct {
		IsLogin bool            `json:"isLogin"`
		Mid     json.RawMessage `json:"mid"`
	}
	if err := json.Unmarshal(raw, &data); err != nil || !data.IsLogin || jsonScalarString(data.Mid) != strings.TrimSpace(accountID) {
		return errors.New("bilibili QR login online account verification failed")
	}
	return nil
}

func (c *Client) followLoginURL(ctx context.Context, raw string) error {
	if err := c.gate.Wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return errors.New("bilibili QR login request failed")
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Referer", "https://www.bilibili.com/")
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("bilibili QR login request failed")
	}
	response.Body.Close()
	return nil
}

func sanitizeQRLoginError(err error, message string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.New(message)
}

// CookieNames returns names only, suitable for status reporting without leaks.
func (c Credential) CookieNames() []string {
	names := make([]string, 0, len(c.Cookies))
	for name := range c.Cookies {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

var _ = time.Second // keeps time available to package docs built with old Go versions.
