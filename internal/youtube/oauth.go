package youtube

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultAuthorizationURL = "https://accounts.google.com/o/oauth2/v2/auth"
	desktopCallbackPath     = "/oauth2/callback"
	maxOAuthJSONBytes       = 1 << 20
)

// ClientCredentials are the public client details from a Google installed-app
// OAuth client JSON file. A desktop client secret is retained only for the
// authorization-code exchange and is never included in returned errors.
type ClientCredentials struct {
	ClientID     string
	ClientSecret string
}

// ParseDesktopClientCredentials parses Google installed-app client JSON.
// Web-client JSON is intentionally rejected: it has different redirect-URI
// security properties and must not be used by the loopback desktop flow.
func ParseDesktopClientCredentials(data []byte) (ClientCredentials, error) {
	return ParseDesktopClientCredentialsReader(bytes.NewReader(data))
}

// ParseDesktopClientCredentialsReader is the streaming counterpart to
// ParseDesktopClientCredentials. The caller owns all filesystem access.
func ParseDesktopClientCredentialsReader(r io.Reader) (ClientCredentials, error) {
	if r == nil {
		return ClientCredentials{}, errors.New("YouTube OAuth client JSON is required")
	}
	payload, err := io.ReadAll(io.LimitReader(r, maxOAuthJSONBytes+1))
	if err != nil || len(payload) > maxOAuthJSONBytes {
		return ClientCredentials{}, errors.New("YouTube OAuth client JSON could not be read")
	}
	var document struct {
		Installed *struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		} `json:"installed"`
		Web json.RawMessage `json:"web"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		return ClientCredentials{}, errors.New("invalid YouTube OAuth client JSON")
	}
	if len(document.Web) != 0 && string(document.Web) != "null" {
		return ClientCredentials{}, errors.New("YouTube web OAuth clients are not supported")
	}
	if document.Installed == nil {
		return ClientCredentials{}, errors.New("YouTube installed OAuth client is required")
	}
	if strings.TrimSpace(document.Installed.ClientID) == "" {
		return ClientCredentials{}, errors.New("YouTube installed OAuth client ID is required")
	}
	return ClientCredentials{ClientID: strings.TrimSpace(document.Installed.ClientID), ClientSecret: document.Installed.ClientSecret}, nil
}

// TokenFile is the private, durable output of a completed desktop OAuth flow.
// CreatedAt is UTC when the authorization code was successfully exchanged.
type TokenFile struct {
	RefreshToken string    `json:"refresh_token"`
	Scope        string    `json:"scope"`
	CreatedAt    time.Time `json:"created_at"`
}

// DesktopAuthorizationOptions controls endpoint injection for tests. With no
// overrides, StartDesktopAuthorization always uses Google's fixed endpoints.
// HTTP endpoints are accepted only for loopback hosts, to keep test injection
// from accidentally permitting cleartext remote token exchanges.
type DesktopAuthorizationOptions struct {
	HTTPClient            *http.Client
	AuthorizationEndpoint string
	TokenEndpoint         string
	Now                   func() time.Time
}

// DesktopAuthorization holds one pending loopback OAuth authorization. Print
// AuthorizationURL for the user to open; this package never opens a browser.
// Wait exchanges exactly one callback code, and Close releases the listener.
type DesktopAuthorization struct {
	AuthorizationURL string

	httpClient    *http.Client
	tokenEndpoint string
	credentials   ClientCredentials
	redirectURI   string
	verifier      string
	state         string
	callbackPath  string
	callbacks     chan string
	server        *http.Server
	listener      net.Listener
	now           func() time.Time
	closeOnce     sync.Once
}

// StartDesktopAuthorization starts a callback listener on an ephemeral
// 127.0.0.1 port, builds a S256 PKCE authorization URL, and returns it. It
// does not open a browser or log any OAuth values.
func StartDesktopAuthorization(ctx context.Context, credentials ClientCredentials, options DesktopAuthorizationOptions) (*DesktopAuthorization, error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.New("YouTube OAuth authorization was cancelled")
	}
	if strings.TrimSpace(credentials.ClientID) == "" {
		return nil, errors.New("YouTube OAuth client ID is required")
	}
	authorizationEndpoint, err := parseDesktopOAuthEndpoint(options.AuthorizationEndpoint, defaultAuthorizationURL)
	if err != nil {
		return nil, errors.New("invalid YouTube OAuth authorization endpoint")
	}
	tokenEndpoint, err := parseDesktopOAuthEndpoint(options.TokenEndpoint, defaultTokenURL)
	if err != nil {
		return nil, errors.New("invalid YouTube OAuth token endpoint")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("could not start YouTube OAuth callback listener")
	}
	cleanup := func() { _ = listener.Close() }
	state, err := oauthRandomURLValue(32)
	if err != nil {
		cleanup()
		return nil, errors.New("could not generate YouTube OAuth state")
	}
	verifier, err := oauthRandomURLValue(64)
	if err != nil {
		cleanup()
		return nil, errors.New("could not generate YouTube OAuth PKCE verifier")
	}
	redirectURI := "http://" + listener.Addr().String() + desktopCallbackPath
	if options.Now == nil {
		options.Now = time.Now
	}
	a := &DesktopAuthorization{
		httpClient:    oauthHTTPClient(options.HTTPClient),
		tokenEndpoint: tokenEndpoint.String(), credentials: credentials, redirectURI: redirectURI,
		verifier: verifier, state: state, callbackPath: desktopCallbackPath, callbacks: make(chan string, 1),
		listener: listener, now: options.Now,
	}
	a.server = &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(a.handleCallback)}
	authorizationURL := *authorizationEndpoint
	query := authorizationURL.Query()
	query.Set("response_type", "code")
	query.Set("client_id", credentials.ClientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("scope", forceSSLScope)
	query.Set("state", state)
	query.Set("code_challenge", oauthPKCEChallenge(verifier))
	query.Set("code_challenge_method", "S256")
	query.Set("access_type", "offline")
	query.Set("prompt", "consent")
	authorizationURL.RawQuery = query.Encode()
	a.AuthorizationURL = authorizationURL.String()
	go func() { _ = a.server.Serve(listener) }()
	return a, nil
}

// Wait waits for a valid callback and exchanges its authorization code once.
// It closes the callback listener before returning in every case.
func (a *DesktopAuthorization) Wait(ctx context.Context) (TokenFile, error) {
	if a == nil {
		return TokenFile{}, errors.New("YouTube OAuth authorization is required")
	}
	defer a.Close()
	select {
	case code := <-a.callbacks:
		return a.exchangeCode(ctx, code)
	case <-ctx.Done():
		return TokenFile{}, errors.New("waiting for YouTube OAuth callback was cancelled")
	}
}

// Close stops the loopback callback server. It is safe to call more than once.
func (a *DesktopAuthorization) Close() {
	if a == nil {
		return
	}
	a.closeOnce.Do(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if a.server != nil {
			_ = a.server.Shutdown(shutdownCtx)
		}
		if a.listener != nil {
			_ = a.listener.Close()
		}
	})
}

func (a *DesktopAuthorization) handleCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != a.callbackPath || r.URL.EscapedPath() != a.callbackPath {
		http.NotFound(w, r)
		return
	}
	code, valid := validDesktopCallback(r.URL.Query(), a.state)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if !valid {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "<!doctype html><title>Authorization failed</title><p>Authorization failed. You may close this window.</p>")
		return
	}
	_, _ = io.WriteString(w, "<!doctype html><title>Authorization complete</title><p>Authorization complete. You may close this window.</p>")
	select {
	case a.callbacks <- code:
	default:
	}
}

func validDesktopCallback(values url.Values, expectedState string) (string, bool) {
	states, codes, oauthErrors := values["state"], values["code"], values["error"]
	if len(oauthErrors) != 0 || len(states) != 1 || len(codes) != 1 || states[0] != expectedState || strings.TrimSpace(codes[0]) == "" {
		return "", false
	}
	return codes[0], true
}

func (a *DesktopAuthorization) exchangeCode(ctx context.Context, code string) (TokenFile, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {a.credentials.ClientID},
		"redirect_uri":  {a.redirectURI},
		"code_verifier": {a.verifier},
	}
	if a.credentials.ClientSecret != "" {
		form.Set("client_secret", a.credentials.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenFile{}, errors.New("could not create YouTube OAuth token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.httpClient.Do(req) // Deliberately exactly one token POST; no retry or redirect follow.
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return TokenFile{}, errors.New("YouTube OAuth token exchange failed")
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthJSONBytes+1))
	if err != nil || len(payload) > maxOAuthJSONBytes {
		return TokenFile{}, errors.New("invalid YouTube OAuth token response")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return TokenFile{}, fmt.Errorf("YouTube OAuth token exchange failed: HTTP status %d", resp.StatusCode)
	}
	var response struct {
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(payload, &response); err != nil || strings.TrimSpace(response.RefreshToken) == "" || !hasScope(response.Scope, forceSSLScope) {
		return TokenFile{}, errors.New("invalid YouTube OAuth token response")
	}
	return TokenFile{RefreshToken: response.RefreshToken, Scope: response.Scope, CreatedAt: a.now().UTC()}, nil
}

func parseDesktopOAuthEndpoint(raw, fallback string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		raw = fallback
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("invalid endpoint")
	}
	if u.Scheme == "https" {
		return u, nil
	}
	if u.Scheme == "http" && net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback() {
		return u, nil
	}
	return nil, errors.New("insecure endpoint")
}

func oauthHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		base = &http.Client{Timeout: 30 * time.Second}
	}
	copy := *base
	// Returning the redirect response lets exchangeCode reject all 3xx status
	// codes without issuing a request to its Location.
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}

func oauthRandomURLValue(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func oauthPKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// SaveToken atomically writes token to path as a private 0600 JSON file. It
// creates the explicitly named parent directory with mode 0700 if necessary.
// The target must not be a symbolic link.
func SaveToken(path string, token TokenFile) error {
	if err := validateTokenFile(token); err != nil {
		return err
	}
	if strings.TrimSpace(path) == "" {
		return errors.New("YouTube token path is required")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return errors.New("could not create YouTube token directory")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("YouTube token target must not be a symbolic link")
		}
		if !info.Mode().IsRegular() {
			return errors.New("YouTube token target must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("could not inspect YouTube token target")
	}
	payload, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return errors.New("could not encode YouTube token")
	}
	payload = append(payload, '\n')
	tmp, err := os.CreateTemp(directory, ".youtube-token-*")
	if err != nil {
		return errors.New("could not create YouTube token temporary file")
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
		return errors.New("could not write YouTube token")
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("YouTube token target must not be a symbolic link")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("could not inspect YouTube token target")
	}
	if err := os.Rename(tmpName, path); err != nil {
		return errors.New("could not replace YouTube token")
	}
	dir, err := os.Open(directory)
	if err != nil {
		return errors.New("could not sync YouTube token directory")
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return errors.New("could not sync YouTube token directory")
	}
	return nil
}

// LoadToken loads a private token file created by SaveToken. It rejects
// symlinks, non-regular files, and files accessible by group or other users.
func LoadToken(path string) (TokenFile, error) {
	if strings.TrimSpace(path) == "" {
		return TokenFile{}, errors.New("YouTube token path is required")
	}
	before, err := os.Lstat(path)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Mode().Perm() != 0o600 {
		return TokenFile{}, errors.New("YouTube token file must be a regular 0600 file")
	}
	f, err := os.Open(path)
	if err != nil {
		return TokenFile{}, errors.New("could not open YouTube token file")
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0o600 {
		return TokenFile{}, errors.New("YouTube token file changed while opening")
	}
	payload, err := io.ReadAll(io.LimitReader(f, maxOAuthJSONBytes+1))
	if err != nil || len(payload) > maxOAuthJSONBytes {
		return TokenFile{}, errors.New("could not read YouTube token file")
	}
	var token TokenFile
	if err := json.Unmarshal(payload, &token); err != nil {
		return TokenFile{}, errors.New("invalid YouTube token file")
	}
	if err := validateTokenFile(token); err != nil {
		return TokenFile{}, err
	}
	return token, nil
}

func validateTokenFile(token TokenFile) error {
	if strings.TrimSpace(token.RefreshToken) == "" || !hasScope(token.Scope, forceSSLScope) || token.CreatedAt.IsZero() {
		return errors.New("invalid YouTube token")
	}
	return nil
}
