package bili

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"comment-bridge/internal/commentbridge"
)

func riskTestClient(t *testing.T, endpoint string, stop *commentbridge.BilibiliRiskStop) *Client {
	t.Helper()
	c, err := NewClient(Config{BaseURL: endpoint, RiskStop: stop, Gate: NewRequestGate(GateConfig{Disabled: true})})
	if err != nil {
		t.Fatal(err)
	}
	c.keys = WBIKeys{ImgKey: "0123456789abcdef0123456789abcdef", SubKey: "fedcba9876543210fedcba9876543210"}
	c.SetCredential(Credential{Cookies: map[string]string{"SESSDATA": "session", "bili_jct": "csrf", "DedeUserID": "1"}})
	return c
}

func riskRoutes(t *testing.T, c *Client) map[string]func() error {
	t.Helper()
	a := NewCommentAdapter(c, "1")
	comment := commentbridge.Comment{Platform: commentbridge.PlatformBilibili, AccountID: "1", ResourceID: "2", ID: "3", AuthorID: "4"}
	return map[string]func() error{
		"comments":       func() error { _, err := a.ListRecent(t.Context()); return err },
		"child-replies":  func() error { return c.getJSON(t.Context(), "/x/v2/reply/reply", nil, false, nil) },
		"danmaku":        func() error { _, err := a.ListRecentDanmaku(t.Context()); return err },
		"nav":            func() error { _, err := c.Nav(t.Context()); return err },
		"reply":          func() error { _, err := a.Reply(t.Context(), comment, "reply"); return err },
		"like":           func() error { _, err := a.Engage(t.Context(), comment, commentbridge.EngagementLike); return err },
		"delete":         func() error { _, err := a.Moderate(t.Context(), comment, commentbridge.ModerationDelete); return err },
		"ban":            func() error { _, err := a.Moderate(t.Context(), comment, commentbridge.ModerationBan); return err },
		"qr-generate":    func() error { _, err := a.BeginCredentialRefresh(t.Context()); return err },
		"qr-poll":        func() error { _, err := a.PollCredentialRefresh(t.Context(), "key"); return err },
		"login-redirect": func() error { return c.followLoginURL(t.Context(), c.base.String()+"/callback") },
	}
}

func TestRiskStopCoversEveryRouteAndRestart(t *testing.T) {
	for _, first := range []string{"comments", "child-replies", "danmaku", "nav", "reply", "like", "delete", "ban", "qr-generate", "qr-poll", "login-redirect"} {
		t.Run(first, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(429)
				fmt.Fprint(w, `{"code":-352,"message":"SECRET"}`)
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "bridge.db")
			store, err := commentbridge.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { store.Close() }()
			notices := 0
			send := func(ctx context.Context, text, id string) (string, error) {
				notices++
				state, e := store.BilibiliRiskStatus(ctx)
				if e != nil || !strings.Contains(state, "state=stopped") || !strings.Contains(text, "二维码登录") || strings.Contains(text, "SECRET") || id == "" {
					t.Errorf("unsafe or premature alert: %s %v", state, e)
				}
				return "message", nil
			}
			stop, err := commentbridge.NewBilibiliRiskStop(store, send)
			if err != nil {
				t.Fatal(err)
			}
			c := riskTestClient(t, server.URL, stop)
			err = riskRoutes(t, c)[first]()
			if err == nil || commentbridge.IsUnknownOutcome(err) || !stop.Stopped() {
				t.Fatalf("risk not stopped as definite rejection: %v", err)
			}
			for round := range 2 {
				c.now = func() time.Time { return time.Now().Add(365 * 24 * time.Hour) }
				c.ReplaceCredential(Credential{Cookies: map[string]string{"SESSDATA": "new", "bili_jct": "new", "DedeUserID": "1"}})
				for name, call := range riskRoutes(t, c) {
					if call() == nil {
						t.Errorf("%s accepted after stop", name)
					}
				}
				if calls.Load() != 1 || notices != 1 {
					t.Fatalf("leak/spam round=%d requests=%d notices=%d", round, calls.Load(), notices)
				}
				if round == 0 {
					store.Close()
					store, err = commentbridge.Open(path)
					if err != nil {
						t.Fatal(err)
					}
					stop, err = commentbridge.NewBilibiliRiskStop(store, send)
					if err != nil {
						t.Fatal(err)
					}
					c = riskTestClient(t, server.URL, stop)
				}
			}
		})
	}
}

func TestRiskStopCodesDoNotRefreshSignaturesOrRetry(t *testing.T) {
	for _, tc := range []struct{ status, code int }{{403, 0}, {412, 0}, {429, 0}, {200, -352}, {200, -403}, {200, -412}, {200, 412}, {200, -509}, {200, -102}} {
		t.Run(fmt.Sprintf("%d_%d", tc.status, tc.code), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `{"code":%d}`, tc.code)
			}))
			defer server.Close()
			store, err := commentbridge.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			stop, err := commentbridge.NewBilibiliRiskStop(store, func(context.Context, string, string) (string, error) { return "message", nil })
			if err != nil {
				t.Fatal(err)
			}
			c := riskTestClient(t, server.URL, stop)
			err = c.getJSON(t.Context(), "/signed", url.Values{}, true, nil)
			if !errors.Is(err, commentbridge.ErrBilibiliRiskStopped) || requests.Load() != 1 {
				t.Fatalf("err=%v requests=%d", err, requests.Load())
			}
		})
	}
}

func TestRiskStopSerializesConcurrentRequests(t *testing.T) {
	var requests atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(entered)
			<-release
		}
		fmt.Fprint(w, `{"code":-352}`)
	}))
	defer server.Close()
	store, err := commentbridge.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	stop, err := commentbridge.NewBilibiliRiskStop(store, func(context.Context, string, string) (string, error) { return "message", nil })
	if err != nil {
		t.Fatal(err)
	}
	c := riskTestClient(t, server.URL, stop)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_, err := c.Nav(t.Context())
			if !errors.Is(err, commentbridge.ErrBilibiliRiskStopped) {
				t.Errorf("err=%v", err)
			}
		})
	}
	<-entered
	close(release)
	wg.Wait()
	if requests.Load() != 1 {
		t.Fatalf("concurrent requests escaped: %d", requests.Load())
	}
}

func TestRiskStopPreservesNormalAndExpiredCredentialResponses(t *testing.T) {
	for _, code := range []int{0, -101, -111, -400} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"code":%d,"data":{"value":42}}`, code) }))
			defer server.Close()
			store, err := commentbridge.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			stop, err := commentbridge.NewBilibiliRiskStop(store, func(context.Context, string, string) (string, error) {
				t.Error("unexpected risk notice")
				return "", nil
			})
			if err != nil {
				t.Fatal(err)
			}
			c := riskTestClient(t, server.URL, stop)
			for range 2 {
				raw, err := c.Nav(t.Context())
				if code == 0 && (err != nil || string(raw) != `{"value":42}`) {
					t.Fatalf("response changed: %s %v", raw, err)
				}
				if code != 0 {
					api, ok := errors.AsType[*APIError](err)
					if !ok || api.Code != code {
						t.Fatalf("API error changed: %v", err)
					}
				}
			}
			if stop.Stopped() {
				t.Fatal("ordinary response latched risk")
			}
		})
	}
}
