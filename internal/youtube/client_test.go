package youtube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testConfig(url string) Config {
	return Config{HTTPClient: http.DefaultClient, APIBaseURL: url, TokenURL: url + "/token", ClientID: "client", ClientSecret: "client-secret", RefreshToken: "refresh-secret", ChannelID: "channel", AccountID: "account", Scope: forceSSLScope}
}

func TestScopeFailsClosed(t *testing.T) {
	cfg := testConfig("https://example.test")
	cfg.Scope = "https://www.googleapis.com/auth/youtube.readonly"
	if _, err := NewClient(cfg); err == nil {
		t.Fatal("accepted readonly scope")
	}
	cfg.Scope = "openid " + forceSSLScope
	if _, err := NewClient(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.AccountID = ""
	c, err := NewClient(cfg)
	if err != nil || c.AccountID() != cfg.ChannelID {
		t.Fatalf("default account id = %q, err = %v", c.AccountID(), err)
	}
}

func TestMaxPagesFailsClosed(t *testing.T) {
	cfg := testConfig("https://example.test")
	cfg.MaxPages = 21
	if _, err := NewClient(cfg); err == nil {
		t.Fatal("accepted excessive max pages")
	}
}

func TestRefreshCachedAndSingleFlight(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "refresh-secret" {
			t.Error("bad form")
		}
		time.Sleep(time.Millisecond)
		_, _ = w.Write([]byte(`{"access_token":"access","expires_in":3600,"scope":"https://www.googleapis.com/auth/youtube.force-ssl"}`))
	}))
	defer s.Close()
	c, _ := NewClient(testConfig(s.URL))
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := c.bearer(context.Background()); err != nil || got != "access" {
				t.Errorf("%q %v", got, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh calls %d", calls.Load())
	}
}

func TestRefreshErrorDoesNotLeakSecret(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "refresh-secret client-secret", 400) }))
	defer s.Close()
	c, _ := NewClient(testConfig(s.URL))
	_, err := c.bearer(context.Background())
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe error %v", err)
	}
}

func TestRefreshRejectsRedirectAndScopeDowngrade(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		var redirected atomic.Int32
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/elsewhere" {
				redirected.Add(1)
			}
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}))
		defer s.Close()
		c, _ := NewClient(testConfig(s.URL))
		if _, err := c.bearer(context.Background()); err == nil || redirected.Load() != 0 {
			t.Fatalf("err=%v redirected=%d", err, redirected.Load())
		}
	})

	t.Run("scope downgrade", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"access_token":"access","expires_in":3600,"scope":"https://www.googleapis.com/auth/youtube.readonly"}`))
		}))
		defer s.Close()
		c, _ := NewClient(testConfig(s.URL))
		if _, err := c.bearer(context.Background()); err == nil {
			t.Fatal("accepted downgraded token scope")
		}
	})
}

func TestValidateChannelOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantErr    bool
	}{
		{"owned", `{"items":[{"id":"other"},{"id":"channel"}]}`, 200, false},
		{"mismatch", `{"items":[{"id":"other"}]}`, 200, true},
		{"bad json", `{`, 200, true},
		{"forbidden", `{}`, 403, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					_, _ = w.Write([]byte(`{"access_token":"access","expires_in":3600,"scope":"https://www.googleapis.com/auth/youtube.force-ssl"}`))
					return
				}
				if r.URL.Path != "/youtube/v3/channels" || r.URL.Query().Get("mine") != "true" || r.URL.Query().Get("part") != "id" || r.Header.Get("Authorization") != "Bearer access" {
					t.Errorf("unexpected validation request: %s?%s", r.URL.Path, r.URL.RawQuery)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()
			client, err := NewClient(testConfig(s.URL))
			if err != nil {
				t.Fatal(err)
			}
			err = client.ValidateChannel(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateChannel() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestDiscoverOwnedChannels(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"access_token":"access","expires_in":3600,"scope":"https://www.googleapis.com/auth/youtube.force-ssl"}`))
			return
		}
		if r.URL.Path != "/youtube/v3/channels" || r.URL.Query().Get("mine") != "true" || r.URL.Query().Get("part") != "id,snippet" || r.Header.Get("Authorization") != "Bearer access" {
			t.Fatalf("unexpected discovery request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"items":[{"id":"channel-1","snippet":{"title":"Creator Channel"}},{"id":"","snippet":{"title":"ignored"}}]}`))
	}))
	defer s.Close()
	cfg := testConfig(s.URL)
	cfg.ChannelID = ""
	channels, err := DiscoverOwnedChannels(context.Background(), cfg)
	if err != nil || len(channels) != 1 || channels[0].ID != "channel-1" || channels[0].Title != "Creator Channel" {
		t.Fatalf("channels=%#v err=%v", channels, err)
	}
}
