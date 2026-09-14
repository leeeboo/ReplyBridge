package youtube

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseDesktopClientCredentials(t *testing.T) {
	credentials, err := ParseDesktopClientCredentials([]byte(`{"installed":{"client_id":"desktop-client.apps.googleusercontent.com","client_secret":"private-value"}}`))
	if err != nil {
		t.Fatalf("ParseDesktopClientCredentials() error = %v", err)
	}
	if credentials.ClientID != "desktop-client.apps.googleusercontent.com" || credentials.ClientSecret != "private-value" {
		t.Fatalf("unexpected credentials: %#v", credentials)
	}
	_, err = ParseDesktopClientCredentials([]byte(`{"web":{"client_id":"web-client"}}`))
	if err == nil || !strings.Contains(err.Error(), "web") {
		t.Fatalf("web client error = %v, want rejection", err)
	}
}

func TestDesktopAuthorizationRejectsWrongState(t *testing.T) {
	var tokenPosts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenPosts.Add(1)
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	a := newTestAuthorization(t, server.URL, server.URL+"/token")
	defer a.Close()
	redirect := authorizationRedirectURL(t, a.AuthorizationURL)
	response, err := http.Get(redirect + "?state=wrong-state&code=authorization-code")
	if err != nil {
		t.Fatalf("callback request: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := a.Wait(ctx); err == nil {
		t.Fatal("Wait() accepted a callback with the wrong state")
	}
	if got := tokenPosts.Load(); got != 0 {
		t.Fatalf("token POST count = %d, want 0", got)
	}
}

func TestDesktopAuthorizationRejectsTokenRedirect(t *testing.T) {
	var tokenPosts, redirectRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenPosts.Add(1)
			http.Redirect(w, r, "/redirect-target", http.StatusFound)
		case "/redirect-target":
			redirectRequests.Add(1)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	a := newTestAuthorization(t, server.URL, server.URL+"/token")
	defer a.Close()
	_, err := a.exchangeCode(context.Background(), "authorization-code")
	if err == nil || !strings.Contains(err.Error(), "HTTP status 302") {
		t.Fatalf("exchangeCode() error = %v, want redirect rejection", err)
	}
	if got := tokenPosts.Load(); got != 1 {
		t.Fatalf("token POST count = %d, want 1", got)
	}
	if got := redirectRequests.Load(); got != 0 {
		t.Fatalf("redirect target request count = %d, want 0", got)
	}
}

func TestDesktopAuthorizationExchangesPKCECode(t *testing.T) {
	formCh := make(chan url.Values, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read token form: %v", err)
			return
		}
		form, err := url.ParseQuery(string(payload))
		if err != nil {
			t.Errorf("parse token form: %v", err)
			return
		}
		formCh <- form
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"refresh_token":"refresh-token","scope":"openid https://www.googleapis.com/auth/youtube.force-ssl"}`))
	}))
	defer server.Close()
	a := newTestAuthorization(t, server.URL, server.URL+"/token")
	defer a.Close()
	authorizationURL, err := url.Parse(a.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	query := authorizationURL.Query()
	if query.Get("code_challenge_method") != "S256" || !hasScope(query.Get("scope"), forceSSLScope) || query.Get("redirect_uri") == "" {
		t.Fatalf("unsafe authorization URL: %s", a.AuthorizationURL)
	}
	callback := query.Get("redirect_uri") + "?" + url.Values{"state": {query.Get("state")}, "code": {"authorization-code"}}.Encode()
	response, err := http.Get(callback)
	if err != nil {
		t.Fatalf("callback request: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d, want 200", response.StatusCode)
	}
	got, err := a.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if got.RefreshToken != "refresh-token" || !hasScope(got.Scope, forceSSLScope) {
		t.Fatalf("Wait() token = %#v", got)
	}
	form := <-formCh
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "authorization-code" || form.Get("code_verifier") == "" || form.Get("redirect_uri") != query.Get("redirect_uri") {
		t.Fatalf("unexpected token form: %#v", form)
	}
	if oauthPKCEChallenge(form.Get("code_verifier")) != query.Get("code_challenge") {
		t.Fatal("token request verifier does not match authorization challenge")
	}
}

func TestDesktopAuthorizationRejectsMissingRefreshTokenOrScope(t *testing.T) {
	for name, response := range map[string]string{
		"refresh token": `{"scope":"https://www.googleapis.com/auth/youtube.force-ssl"}`,
		"scope":         `{"refresh_token":"refresh-token","scope":"https://www.googleapis.com/auth/youtube.readonly"}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/token" || r.Method != http.MethodPost {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(response))
			}))
			defer server.Close()
			a := newTestAuthorization(t, server.URL, server.URL+"/token")
			defer a.Close()
			if _, err := a.exchangeCode(context.Background(), "authorization-code"); err == nil {
				t.Fatal("exchangeCode() accepted an incomplete token response")
			}
		})
	}
}

func TestSaveTokenPrivateAndLoadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "youtube-token.json")
	want := TokenFile{RefreshToken: "refresh-token", Scope: forceSSLScope + " openid", CreatedAt: time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)}
	if err := SaveToken(path, want); err != nil {
		t.Fatalf("SaveToken() error = %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat token: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %04o, want 0600", info.Mode().Perm())
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Lstat parent: %v", err)
	}
	if parent.Mode().Perm() != 0o700 {
		t.Fatalf("new parent mode = %04o, want 0700", parent.Mode().Perm())
	}
	got, err := LoadToken(path)
	if err != nil {
		t.Fatalf("LoadToken() error = %v", err)
	}
	if got != want {
		t.Fatalf("LoadToken() = %#v, want %#v", got, want)
	}
}

func TestSaveTokenRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "elsewhere.json")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "youtube-token.json")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	err := SaveToken(path, validTestToken())
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("SaveToken() error = %v, want symbolic-link rejection", err)
	}
	payload, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "unchanged" {
		t.Fatalf("symlink target changed: %q", payload)
	}
}

func TestOAuthErrorsDoNotContainClientSecret(t *testing.T) {
	const secret = "super-secret-client-value"
	_, err := ParseDesktopClientCredentials([]byte(`{"installed":{"client_id":"client","client_secret":"` + secret + `"}`))
	if err == nil {
		t.Fatal("invalid JSON unexpectedly accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("parse error leaked client secret: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	a, err := StartDesktopAuthorization(context.Background(), ClientCredentials{ClientID: "client", ClientSecret: secret}, DesktopAuthorizationOptions{AuthorizationEndpoint: server.URL, TokenEndpoint: server.URL})
	if err != nil {
		t.Fatalf("StartDesktopAuthorization() error = %v", err)
	}
	defer a.Close()
	_, err = a.exchangeCode(context.Background(), "authorization-code")
	if err == nil {
		t.Fatal("exchangeCode() unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("token exchange error leaked client secret: %v", err)
	}
}

func newTestAuthorization(t *testing.T, authorizationEndpoint, tokenEndpoint string) *DesktopAuthorization {
	t.Helper()
	a, err := StartDesktopAuthorization(context.Background(), ClientCredentials{ClientID: "test-client", ClientSecret: "test-secret"}, DesktopAuthorizationOptions{
		AuthorizationEndpoint: authorizationEndpoint,
		TokenEndpoint:         tokenEndpoint,
		Now:                   func() time.Time { return time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("StartDesktopAuthorization() error = %v", err)
	}
	return a
}

func authorizationRedirectURL(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	redirectURI := u.Query().Get("redirect_uri")
	if redirectURI == "" {
		t.Fatal("authorization URL lacks redirect_uri")
	}
	return redirectURI
}

func validTestToken() TokenFile {
	return TokenFile{RefreshToken: "refresh-token", Scope: forceSSLScope, CreatedAt: time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)}
}

func TestTokenFileJSONContainsOnlyDurableFields(t *testing.T) {
	payload, err := json.Marshal(validTestToken())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "access_token") || strings.Contains(string(payload), "client_secret") {
		t.Fatalf("unexpected token-file fields: %s", payload)
	}
}

func TestLoadTokenRejectsWrongPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	payload, err := json.Marshal(validTestToken())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadToken(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("LoadToken() error = %v, want permissions rejection", err)
	}
}
