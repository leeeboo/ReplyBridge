package bili

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"comment-bridge/internal/commentbridge"
)

func newCommentTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c, err := NewClient(Config{
		BaseURL: baseURL, Gate: NewRequestGate(GateConfig{Disabled: true}), RetryMax: 0,
		Now: func() time.Time { return time.Unix(1702204169, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	c.keys = WBIKeys{ImgKey: "0123456789abcdef0123456789abcdef", SubKey: "fedcba9876543210fedcba9876543210"}
	c.SetCredential(Credential{Cookies: map[string]string{"SESSDATA": "session", "bili_jct": "csrf", "DedeUserID": "1", "buvid3": "device"}})
	return c
}

func TestCommentAdapterCredentialRefreshStatesAndSuccess(t *testing.T) {
	var pollCode atomic.Int32
	var callbackCalls, navCalls atomic.Int32
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/x/passport-login/web/qrcode/generate":
			writeJSON(w, `{"code":0,"data":{"url":"`+serverURL+`/qr-callback","qrcode_key":"qr-key"}}`)
		case "/x/passport-login/web/qrcode/poll":
			if r.URL.Query().Get("qrcode_key") != "qr-key" {
				t.Fatalf("unexpected QR key")
			}
			code := pollCode.Load()
			if code == 0 {
				_, _ = fmt.Fprintf(w, `{"code":0,"data":{"code":0,"url":"%s/qr-callback"}}`, serverURL)
				return
			}
			_, _ = fmt.Fprintf(w, `{"code":0,"data":{"code":%d,"message":"state"}}`, code)
		case "/qr-callback":
			callbackCalls.Add(1)
			for name, value := range map[string]string{"SESSDATA": "new", "bili_jct": "new-csrf", "DedeUserID": "42"} {
				http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/"})
			}
			writeJSON(w, `{"ok":true}`)
		case "/x/web-interface/nav":
			navCalls.Add(1)
			if cookie, err := r.Cookie("SESSDATA"); err != nil || cookie.Value != "new" {
				t.Fatalf("online verification did not use candidate credential")
			}
			writeJSON(w, `{"code":0,"data":{"isLogin":true,"mid":42}}`)
		default:
			t.Fatalf("unexpected QR path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	serverURL = server.URL
	credentialPath := filepath.Join(t.TempDir(), "bilibili-credential.json")
	client, err := NewClient(Config{BaseURL: server.URL, PassportURL: server.URL, HTTPClient: server.Client(), Gate: NewRequestGate(GateConfig{Disabled: true}), RetryMax: 0, Now: func() time.Time { return time.Unix(1702204169, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	client.SetCredential(Credential{Cookies: map[string]string{"SESSDATA": "old", "bili_jct": "old-csrf", "DedeUserID": "42", "legacy": "stale"}})
	adapter, err := NewConfiguredCommentAdapter(client, CommentAdapterConfig{AccountID: "42", CredentialFile: credentialPath})
	if err != nil {
		t.Fatal(err)
	}
	adapter.tripRiskFuse()
	session, err := adapter.BeginCredentialRefresh(context.Background())
	if err != nil || session.Key != "qr-key" || session.URL == "" || session.ExpiresAt.IsZero() {
		t.Fatalf("session=%#v err=%v", session, err)
	}
	if got := client.Credential().Cookies["SESSDATA"]; got != "old" {
		t.Fatalf("begin polluted active credential: %q", got)
	}
	for _, tc := range []struct {
		code int32
		want commentbridge.CredentialRefreshState
	}{
		{86101, commentbridge.CredentialRefreshWaiting},
		{86090, commentbridge.CredentialRefreshScanned},
		{86038, commentbridge.CredentialRefreshExpired},
	} {
		pollCode.Store(tc.code)
		got, err := adapter.PollCredentialRefresh(context.Background(), session.Key)
		if err != nil || got != tc.want {
			t.Fatalf("code=%d state=%q err=%v", tc.code, got, err)
		}
		if client.Credential().Cookies["SESSDATA"] != "old" || !adapter.isRiskFused() {
			t.Fatalf("non-success QR state changed active auth: state=%d credential=%#v fused=%v", tc.code, client.Credential().CookieNames(), adapter.isRiskFused())
		}
	}
	pollCode.Store(0)
	state, err := adapter.PollCredentialRefresh(context.Background(), session.Key)
	if err != nil || state != commentbridge.CredentialRefreshSucceeded {
		t.Fatalf("success state=%q err=%v", state, err)
	}
	if callbackCalls.Load() != 1 || navCalls.Load() != 1 {
		t.Fatalf("callback=%d nav=%d", callbackCalls.Load(), navCalls.Load())
	}
	if adapter.isRiskFused() {
		t.Fatal("successful credential refresh left risk fuse tripped")
	}
	credential, err := LoadCredentialFile(credentialPath)
	if err != nil || !credential.Complete() || credential.Cookies["SESSDATA"] != "new" {
		t.Fatalf("saved credential=%#v err=%v", credential.CookieNames(), err)
	}
	if info, err := os.Stat(credentialPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved credential mode=%v err=%v", info.Mode().Perm(), err)
	}
	active := client.Credential()
	if active.Cookies["SESSDATA"] != "new" || active.Cookies["legacy"] != "" {
		t.Fatalf("active credential was not completely replaced: %#v", active.CookieNames())
	}
}

func TestCommentAdapterCredentialRefreshRejectsCandidateWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cookies  map[string]string
		navLogin bool
		navMid   string
	}{
		{name: "missing cookie", cookies: map[string]string{"SESSDATA": "new", "DedeUserID": "42"}},
		{name: "wrong account", cookies: map[string]string{"SESSDATA": "new", "bili_jct": "new-csrf", "DedeUserID": "99"}},
		{name: "nav mismatch", cookies: map[string]string{"SESSDATA": "new", "bili_jct": "new-csrf", "DedeUserID": "42"}, navLogin: true, navMid: "99"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pollCode atomic.Int32
			pollCode.Store(0)
			var serverURL string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/x/passport-login/web/qrcode/generate":
					_, _ = fmt.Fprintf(w, `{"code":0,"data":{"url":"%s/qr-callback","qrcode_key":"key"}}`, serverURL)
				case "/x/passport-login/web/qrcode/poll":
					_, _ = fmt.Fprintf(w, `{"code":0,"data":{"code":0,"url":"%s/qr-callback"}}`, serverURL)
				case "/qr-callback":
					for name, value := range tc.cookies {
						http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/"})
					}
					writeJSON(w, `{}`)
				case "/x/web-interface/nav":
					_, _ = fmt.Fprintf(w, `{"code":0,"data":{"isLogin":%t,"mid":%s}}`, tc.navLogin, strconv.Quote(tc.navMid))
				default:
					t.Fatalf("unexpected path %s", r.URL.Path)
				}
			}))
			defer server.Close()
			serverURL = server.URL
			client, err := NewClient(Config{BaseURL: server.URL, PassportURL: server.URL, HTTPClient: server.Client(), Gate: NewRequestGate(GateConfig{Disabled: true}), RetryMax: 0})
			if err != nil {
				t.Fatal(err)
			}
			client.SetCredential(Credential{Cookies: map[string]string{"SESSDATA": "old", "bili_jct": "old-csrf", "DedeUserID": "42", "legacy": "stale"}})
			adapter, err := NewConfiguredCommentAdapter(client, CommentAdapterConfig{AccountID: "42", CredentialFile: filepath.Join(t.TempDir(), "credential.json")})
			if err != nil {
				t.Fatal(err)
			}
			adapter.tripRiskFuse()
			session, err := adapter.BeginCredentialRefresh(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.PollCredentialRefresh(context.Background(), session.Key)
			if err == nil || strings.Contains(err.Error(), session.Key) || strings.Contains(err.Error(), session.URL) {
				t.Fatalf("candidate failure leaked QR data: %v", err)
			}
			active := client.Credential()
			if active.Cookies["SESSDATA"] != "old" || active.Cookies["legacy"] != "stale" || !adapter.isRiskFused() {
				t.Fatalf("candidate failure mutated active state: %#v fused=%v", active.CookieNames(), adapter.isRiskFused())
			}
		})
	}
}

func TestCommentAdapterPaginatesUntilPreviouslySeen(t *testing.T) {
	var phase atomic.Int32
	var laterCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("pn")
		items := []map[string]any{}
		if phase.Load() == 0 {
			items = append(items, biliCommentFixture("old", 1))
		} else {
			laterCalls.Add(1)
			if page == "1" {
				for i := 0; i < commentInboxPageSize; i++ {
					items = append(items, biliCommentFixture(fmt.Sprintf("new-%d", i), int64(1000+i)))
				}
			} else if page == "2" {
				items = append(items, biliCommentFixture("old", 1))
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"list": items}})
	}))
	defer server.Close()
	adapter := NewCommentAdapter(newCommentTestClient(t, server.URL), "42")
	if comments, err := adapter.ListRecent(context.Background()); err != nil || len(comments) != 1 {
		t.Fatalf("baseline comments=%d err=%v", len(comments), err)
	}
	phase.Store(1)
	if comments, err := adapter.ListRecent(context.Background()); err != nil || len(comments) != commentInboxPageSize+1 || laterCalls.Load() != 2 {
		t.Fatalf("new comments=%d calls=%d err=%v", len(comments), laterCalls.Load(), err)
	}
	if comments, err := adapter.ListRecent(context.Background()); err != nil || len(comments) != commentInboxPageSize || laterCalls.Load() != 3 {
		t.Fatalf("quiet comments=%d calls=%d err=%v", len(comments), laterCalls.Load(), err)
	}
}

func biliCommentFixture(id string, ctime int64) map[string]any {
	return map[string]any{"rpid_str": id, "oid": 2, "ctime": ctime, "member": map[string]any{"mid": 3, "uname": "u"}, "content": map[string]any{"message": "m"}}
}

func TestCommentAdapterListRecentParsesAndSorts(t *testing.T) {
	for _, field := range []string{"list", "replies"} {
		t.Run(field, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/x/v2/reply/up/fulllist" {
					t.Fatalf("path = %s", r.URL.Path)
				}
				if got := r.URL.Query(); got.Get("mid") != "" || got.Get("w_rid") != "" || got.Get("order") != "1" || got.Get("filter") != "-1" || got.Get("type") != "1" || got.Get("pn") != "1" || got.Get("ps") != "50" || got.Get("charge_plus_filter") != "false" {
					t.Fatal("unexpected list query")
				}
				_, _ = w.Write([]byte(`{"code":0,"data":{"` + field + `":[` +
					`{"rpid_str":"new","oid":12,"root_str":"root","parent_str":"parent","ctime":20000,"archive":{"bvid":"BV1x","title":"New","jump_url":"//www.bilibili.com/video/BV1x"},"member":{"mid":"7","uname":"u"},"content":{"message":"later"}},` +
					`{"rpid":11,"oid":"10","ctime":10,"member":{"mid":8,"uname":"v"},"content":{"message":"earlier"}},` +
					`{"rpid":0,"oid":9,"ctime":1}` + `]}}`))
			}))
			defer server.Close()
			adapter := NewCommentAdapter(newCommentTestClient(t, server.URL), "42")
			comments, err := adapter.ListRecent(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(comments) != 2 || comments[0].ID != "11" || comments[0].RootID != "11" || comments[0].ResourceID != "10" || comments[1].ID != "new" || comments[1].RootID != "root" || comments[1].ParentID != "parent" || comments[1].ResourceTitle != "New" || comments[1].ResourceURL != "https://www.bilibili.com/video/BV1x" {
				t.Fatalf("unexpected comments: %#v", comments)
			}
		})
	}
}

func TestCommentAdapterParsesChargePlusConservatively(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/x/v2/reply/up/fulllist" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		root := biliCommentFixture("charge-root", 1)
		root["reply_control"] = map[string]any{"is_charge_plus": true}
		root["replies"] = []any{map[string]any{
			"rpid_str": "charge-child", "root_str": "charge-root", "parent_str": "charge-root", "ctime": 2,
			"reply_control": map[string]any{"is_charge_plus": true},
			"member":        map[string]any{"mid": "9", "uname": "reply"}, "content": map[string]any{"message": "child"},
		}}
		missing := biliCommentFixture("charge-missing", 3)
		wrongType := biliCommentFixture("charge-wrong-type", 4)
		wrongType["reply_control"] = map[string]any{"is_charge_plus": "true"}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"list": []any{root, missing, wrongType}}})
	}))
	defer server.Close()

	comments, err := NewCommentAdapter(newCommentTestClient(t, server.URL), "42").ListRecent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	chargePlus := make(map[string]bool, len(comments))
	for _, comment := range comments {
		chargePlus[comment.ID] = comment.IsChargePlus
	}
	want := map[string]bool{
		"charge-root":       true,
		"charge-child":      true,
		"charge-missing":    false,
		"charge-wrong-type": false,
	}
	if len(chargePlus) != len(want) {
		t.Fatalf("comments=%#v", comments)
	}
	for id, expected := range want {
		if got := chargePlus[id]; got != expected {
			t.Errorf("comment %q IsChargePlus=%v, want %v", id, got, expected)
		}
	}
}

func TestCommentAdapterDirectRepliesAreNormalizedWithoutSubRequest(t *testing.T) {
	var subCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/x/v2/reply/reply" {
			subCalls.Add(1)
			t.Fatal("complete direct replies must not fetch the sub-reply API")
		}
		root := biliCommentFixture("root", 10)
		root["oid"] = "12"
		root["rcount"] = 1
		root["archive"] = map[string]any{"title": "Video", "bvid": "BV1x"}
		root["replies"] = []any{map[string]any{
			"rpid_str": "child", "parent_str": "", "root_str": "child", "ctime": 20,
			"member": map[string]any{"mid": "9", "uname": "child"}, "content": map[string]any{"message": "reply"},
		}}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"list": []any{root}}})
	}))
	defer server.Close()
	comments, err := NewCommentAdapter(newCommentTestClient(t, server.URL), "42").ListRecent(context.Background())
	if err != nil || subCalls.Load() != 0 || len(comments) != 2 {
		t.Fatalf("comments=%#v subCalls=%d err=%v", comments, subCalls.Load(), err)
	}
	child := comments[1]
	if child.ID != "child" || child.RootID != "root" || child.ParentID != "root" || child.ResourceID != "12" || child.ResourceTitle != "Video" || child.ResourceURL != "https://www.bilibili.com/video/BV1x" {
		t.Fatalf("child was not normalized: %#v", child)
	}
}

func TestConfiguredCommentAdapterValidatesSubReplyPageLimit(t *testing.T) {
	client := newCommentTestClient(t, "http://127.0.0.1:1")
	for _, pages := range []int{-1, commentSubReplyMaxPages + 1} {
		if _, err := NewConfiguredCommentAdapter(client, CommentAdapterConfig{AccountID: "42", SubReplyPages: pages}); err == nil {
			t.Fatalf("accepted sub-reply page limit %d", pages)
		}
	}
	adapter, err := NewConfiguredCommentAdapter(client, CommentAdapterConfig{AccountID: "42"})
	if err != nil || adapter.subReplyPages != commentSubReplyDefaultPages {
		t.Fatalf("default adapter=%#v err=%v", adapter, err)
	}
}

func TestCommentAdapterFetchesIncompleteSubRepliesAndCachesCount(t *testing.T) {
	var subCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/x/v2/reply/up/fulllist":
			root := biliCommentFixture("root", 1)
			root["rcount"] = 22
			root["replies"] = []any{subReplyFixture("direct", "root", 2)}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"list": []any{root}}})
		case "/x/v2/reply/reply":
			subCalls.Add(1)
			if r.URL.Query().Get("ps") != "20" || r.URL.Query().Get("type") != "1" || r.URL.Query().Get("oid") != "2" || r.URL.Query().Get("root") != "root" {
				t.Fatal("unexpected sub-reply query")
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("pn"))
			replies := make([]any, 0, 20)
			if page == 1 {
				replies = append(replies, subReplyFixture("direct", "root", 2))
				for i := 1; i < 20; i++ {
					replies = append(replies, subReplyFixture(fmt.Sprintf("p1-%d", i), "root", int64(10+i)))
				}
			} else {
				for i := 0; i < 2; i++ {
					replies = append(replies, subReplyFixture(fmt.Sprintf("p2-%d", i), "root", int64(40+i)))
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"replies": replies, "page": map[string]any{"count": 22}}})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	adapter := NewCommentAdapter(newCommentTestClient(t, server.URL), "42")
	comments, err := adapter.ListRecent(context.Background())
	if err != nil || len(comments) != 23 || subCalls.Load() != 2 {
		t.Fatalf("comments=%d subCalls=%d err=%v", len(comments), subCalls.Load(), err)
	}
	if _, err := adapter.ListRecent(context.Background()); err != nil || subCalls.Load() != 2 {
		t.Fatalf("same count unexpectedly fetched: calls=%d err=%v", subCalls.Load(), err)
	}
}

func TestCommentAdapterRefetchesWhenReplyCountChangesAndDoesNotCacheIncompleteRun(t *testing.T) {
	var replyCount atomic.Int32
	var subCalls atomic.Int32
	replyCount.Store(1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/x/v2/reply/up/fulllist" {
			root := biliCommentFixture("root", 1)
			root["rcount"] = replyCount.Load()
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"list": []any{root}}})
			return
		}
		if r.URL.Path != "/x/v2/reply/reply" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		subCalls.Add(1)
		page, _ := strconv.Atoi(r.URL.Query().Get("pn"))
		count := int(replyCount.Load())
		replies := make([]any, 0, 20)
		for i := 0; i < 20 && (page-1)*20+i < count; i++ {
			replies = append(replies, subReplyFixture(fmt.Sprintf("%d-%d", count, (page-1)*20+i), "root", int64(i+1)))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"replies": replies, "page": map[string]any{"count": count}}})
	}))
	defer server.Close()
	adapter := NewCommentAdapter(newCommentTestClient(t, server.URL), "42")
	if _, err := adapter.ListRecent(context.Background()); err != nil || subCalls.Load() != 1 {
		t.Fatalf("initial calls=%d err=%v", subCalls.Load(), err)
	}
	if _, err := adapter.ListRecent(context.Background()); err != nil || subCalls.Load() != 1 {
		t.Fatalf("same count calls=%d err=%v", subCalls.Load(), err)
	}
	replyCount.Store(2)
	if _, err := adapter.ListRecent(context.Background()); err != nil || subCalls.Load() != 2 {
		t.Fatalf("changed count calls=%d err=%v", subCalls.Load(), err)
	}
	replyCount.Store(201)
	if _, err := adapter.ListRecent(context.Background()); err != nil || subCalls.Load() != 12 {
		t.Fatalf("incomplete run calls=%d err=%v", subCalls.Load(), err)
	}
	if _, err := adapter.ListRecent(context.Background()); err != nil || subCalls.Load() != 22 {
		t.Fatalf("page-capped count was cached: calls=%d err=%v", subCalls.Load(), err)
	}
}

func TestCommentAdapterNullSubRepliesDoesNotAbortPollOrCacheCount(t *testing.T) {
	var subCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/x/v2/reply/reply" {
			subCalls.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"replies":null,"page":{"count":0}}}`))
			return
		}
		root := biliCommentFixture("root", 1)
		root["rcount"] = 1
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"list": []any{root}}})
	}))
	defer server.Close()
	adapter := NewCommentAdapter(newCommentTestClient(t, server.URL), "42")
	for range 2 {
		comments, err := adapter.ListRecent(context.Background())
		if err != nil || len(comments) != 1 || comments[0].ID != "root" {
			t.Fatalf("comments=%#v err=%v", comments, err)
		}
	}
	if subCalls.Load() != 2 {
		t.Fatalf("null child list was cached: calls=%d", subCalls.Load())
	}
}

func TestCommentAdapterSubReplyFailureDoesNotUpdateStateAndFiltersCreator(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/x/v2/reply/reply" {
			t.Fatal("complete direct replies must not fetch the sub-reply API")
		}
		root := biliCommentFixture("root", 1)
		root["rcount"] = 2
		root["member"] = map[string]any{"mid": "42", "uname": "creator"}
		creator := subReplyFixture("creator", "root", 2)
		creator["member"] = map[string]any{"mid": "42", "uname": "creator"}
		root["replies"] = []any{creator, subReplyFixture("visitor", "root", 3)}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"list": []any{root}}})
	}))
	defer server.Close()
	adapter := NewCommentAdapter(newCommentTestClient(t, server.URL), "42")
	comments, err := adapter.ListRecent(context.Background())
	if err != nil || len(comments) != 1 || comments[0].ID != "visitor" {
		t.Fatalf("creator filtering comments=%#v err=%v", comments, err)
	}
	var subCalls atomic.Int32
	riskServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/x/v2/reply/reply" {
			subCalls.Add(1)
			_, _ = w.Write([]byte(`{"code":-352}`))
			return
		}
		root := biliCommentFixture("new-root", 1)
		root["rcount"] = 1
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"list": []any{root}}})
	}))
	defer riskServer.Close()
	riskAdapter := NewCommentAdapter(newCommentTestClient(t, riskServer.URL), "42")
	_, err = riskAdapter.ListRecent(context.Background())
	if err == nil || subCalls.Load() != 1 || len(riskAdapter.seenIDs) != 0 || len(riskAdapter.replyCounts) != 0 {
		t.Fatalf("err=%v calls=%d seen=%v replyCounts=%v", err, subCalls.Load(), riskAdapter.seenIDs, riskAdapter.replyCounts)
	}
	_, err = riskAdapter.ListRecent(context.Background())
	if !errors.Is(err, ErrCommentRiskControl) || subCalls.Load() != 1 {
		t.Fatalf("fuse err=%v calls=%d", err, subCalls.Load())
	}
}

func subReplyFixture(id, root string, ctime int64) map[string]any {
	return map[string]any{"rpid_str": id, "root_str": root, "parent_str": root, "ctime": ctime, "member": map[string]any{"mid": "9", "uname": "reply"}, "content": map[string]any{"message": "reply"}}
}

func TestCommentAdapterReplyRequiresCredentialsWithoutNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	c, err := NewClient(Config{BaseURL: server.URL, Gate: NewRequestGate(GateConfig{Disabled: true}), RetryMax: 0})
	if err != nil {
		t.Fatal(err)
	}
	c.SetCredential(Credential{Cookies: map[string]string{"SESSDATA": "session"}})
	_, err = NewCommentAdapter(c, "42").Reply(context.Background(), commentbridge.Comment{Platform: commentbridge.PlatformBilibili, ID: "1", ResourceID: "2"}, "hello")
	if err == nil || calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestCommentAdapterReplyRejectsWrongPlatformOrAccountWithoutNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	adapter := NewCommentAdapter(newCommentTestClient(t, server.URL), "42")
	for _, target := range []commentbridge.Comment{
		{Platform: commentbridge.PlatformYouTube, AccountID: "42", ID: "1", ResourceID: "2"},
		{Platform: commentbridge.PlatformBilibili, AccountID: "99", ID: "1", ResourceID: "2"},
	} {
		if _, err := adapter.Reply(context.Background(), target, "hello"); err == nil {
			t.Fatal("accepted reply target from another platform or account")
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("unexpected network calls: %d", calls.Load())
	}
}

func TestCommentAdapterReplyRequestAndSuccess(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/x/v2/reply/add" || r.Header.Get("Origin") != "https://www.bilibili.com" || r.Header.Get("Referer") != "https://www.bilibili.com/" {
			t.Fatal("unexpected reply request method, path, or browser headers")
		}
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.URL.Query().Get("w_rid") == "" || r.URL.Query().Get("wts") == "" || r.URL.Query().Get("dm_img_list") != "[]" || r.URL.Query().Get("dm_img_str") == "" || r.URL.Query().Get("dm_cover_img_str") == "" || !strings.Contains(r.URL.Query().Get("dm_img_inter"), `"ds":[]`) {
			t.Fatal("missing signed reply query fields")
		}
		body, _ := ioReadAll(r.Body)
		form, err := url.ParseQuery(string(body))
		if err != nil || form.Get("oid") != "2" || form.Get("type") != "1" || form.Get("message") != "hello" || form.Get("plat") != "1" || form.Get("root") != "root" || form.Get("parent") != "1" || form.Get("statistics") != `{"appId":100,"platform":5}` || form.Get("gaia_source") != "main_web" || form.Get("csrf") == "" || form.Get("csrf_token") == "" {
			t.Fatal("unexpected reply form")
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"reply":{"rpid_str":"99"}}}`))
	}))
	defer server.Close()
	id, err := NewCommentAdapter(newCommentTestClient(t, server.URL), "42").Reply(context.Background(), commentbridge.Comment{Platform: commentbridge.PlatformBilibili, AccountID: "42", ID: "1", RootID: "root", ResourceID: "2"}, " hello ")
	if err != nil || id != "99" || calls.Load() != 1 {
		t.Fatalf("id=%q err=%v calls=%d", id, err, calls.Load())
	}
}

func TestCommentAdapterRiskFuseStopsLaterNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"code":-352,"message":"risk"}`))
	}))
	defer server.Close()
	adapter := NewCommentAdapter(newCommentTestClient(t, server.URL), "42")
	_, firstErr := adapter.Reply(context.Background(), commentbridge.Comment{Platform: commentbridge.PlatformBilibili, ID: "1", ResourceID: "2"}, "hello")
	_, secondErr := adapter.ListRecent(context.Background())
	if !errors.Is(firstErr, ErrCommentRiskControl) || !errors.Is(secondErr, ErrCommentRiskControl) || calls.Load() != 1 {
		t.Fatalf("first=%v second=%v calls=%d", firstErr, secondErr, calls.Load())
	}
}

func TestReadRiskCooldownRecoversWithoutRetryingWrites(t *testing.T) {
	now := time.Unix(1702204169, 0)
	var calls int
	risky := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if risky {
			w.Header().Set("Retry-After", "5400")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{"list":[{"rpid":123,"oid":456,"ctime":1702204169,"member":{"mid":9},"content":{"message":"new"}}]}}`)
	}))
	defer server.Close()
	client := newCommentTestClient(t, server.URL)
	client.now = func() time.Time { return now }
	client.retryBase = time.Millisecond
	a := NewCommentAdapter(client, "42")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := a.ListRecent(ctx); !errors.Is(err, ErrRateLimited) || calls != 1 {
		t.Fatalf("429 must stop immediately: calls=%d err=%v", calls, err)
	}
	now = now.Add(time.Hour)
	if _, err := a.ListRecent(context.Background()); !errors.Is(err, ErrCommentRiskControl) || calls != 1 {
		t.Fatalf("must respect Retry-After: calls=%d err=%v", calls, err)
	}
	now = now.Add(30 * time.Minute)
	if _, err := a.ListRecent(context.Background()); !errors.Is(err, ErrRateLimited) || calls != 2 {
		t.Fatalf("one recovery attempt: calls=%d err=%v", calls, err)
	}
	risky = false
	now = now.Add(time.Hour)
	a.ListRecent(context.Background())
	if calls != 2 {
		t.Fatal("second cooldown must be longer")
	}
	now = now.Add(2 * time.Hour)
	parent := commentbridge.Comment{Platform: commentbridge.PlatformBilibili, AccountID: "42", ID: "123", ResourceID: "456"}
	if _, err := a.Reply(context.Background(), parent, "hello"); !errors.Is(err, ErrCommentRiskControl) {
		t.Fatal("write reopened before successful read")
	}
	if _, err := a.Engage(context.Background(), parent, commentbridge.EngagementLike); !errors.Is(err, ErrCommentRiskControl) {
		t.Fatal("like reopened before successful read")
	}
	comments, err := a.ListRecent(context.Background())
	if err != nil || len(comments) != 1 || calls != 3 || a.isRiskFused() {
		t.Fatalf("read recovery: comments=%d calls=%d err=%v", len(comments), calls, err)
	}
	a.tripRiskFuse() // A write-side fuse must never be cleared by the read timer.
	now = now.Add(48 * time.Hour)
	if _, err := a.ListRecent(context.Background()); !errors.Is(err, ErrCommentRiskControl) || calls != 3 {
		t.Fatalf("write fuse must remain closed: calls=%d err=%v", calls, err)
	}
}

func TestReadRiskBackoffAndAuthenticationStayConservative(t *testing.T) {
	now := time.Unix(1702204169, 0)
	code, calls := -352, 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprintf(w, `{"code":%d}`, code)
	}))
	defer s.Close()
	c := newCommentTestClient(t, s.URL)
	c.now = func() time.Time { return now }
	a := NewCommentAdapter(c, "42")
	for i, hours := range []int{1, 2, 4, 8, 16, 24, 24} {
		if _, err := a.ListRecent(context.Background()); err == nil || calls != i+1 {
			t.Fatalf("probe %d: %v, calls=%d", i, err, calls)
		}
		want := time.Duration(hours) * time.Hour
		if a.readRetryAt.Sub(now) != want {
			t.Fatalf("delay=%s want=%s", a.readRetryAt.Sub(now), want)
		}
		now = now.Add(want - time.Second)
		a.ListRecent(context.Background())
		if calls != i+1 {
			t.Fatal("request during cooldown")
		}
		now = now.Add(time.Second)
	}
	code = -101
	if _, err := a.ListRecent(context.Background()); !commentbridge.IsCredentialInvalid(err) {
		t.Fatalf("login alert lost: %v", err)
	}
	before := calls
	now = now.Add(72 * time.Hour)
	a.ListRecent(context.Background())
	if calls != before {
		t.Fatal("expired login retried without human intervention")
	}
}

func TestDeletedChildThreadDoesNotBlockOtherComments(t *testing.T) {
	childCalls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/x/v2/reply/reply" {
			childCalls++
			fmt.Fprint(w, `{"code":12006,"message":"没有该评论"}`)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{"list":[{"rpid":123,"oid":456,"ctime":1702204169,"rcount":1,"member":{"mid":9},"content":{"message":"stale thread"}},{"rpid":124,"oid":456,"ctime":1702204170,"member":{"mid":9},"content":{"message":"new comment"}}]}}`)
	}))
	defer s.Close()
	a := NewCommentAdapter(newCommentTestClient(t, s.URL), "42")
	for i := 0; i < 2; i++ {
		comments, err := a.ListRecent(context.Background())
		if err != nil || len(comments) != 2 || a.isRiskFused() {
			t.Fatalf("other comments blocked: count=%d err=%v", len(comments), err)
		}
	}
	if childCalls != 1 {
		t.Fatalf("deleted thread repeatedly requested: %d", childCalls)
	}
}

func TestCommentAdapterAuthenticationFailureTripsFuse(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"code":-101,"message":"not logged in"}`))
	}))
	defer server.Close()
	adapter := NewCommentAdapter(newCommentTestClient(t, server.URL), "42")
	_, firstErr := adapter.Reply(context.Background(), commentbridge.Comment{Platform: commentbridge.PlatformBilibili, AccountID: "42", ID: "1", ResourceID: "2"}, "hello")
	_, secondErr := adapter.ListRecent(context.Background())
	if !errors.Is(firstErr, ErrAuthentication) || !commentbridge.IsCredentialInvalid(firstErr) || commentbridge.CredentialInvalidReasonOf(firstErr) != commentbridge.CredentialInvalidLogin || !errors.Is(secondErr, ErrCommentRiskControl) || calls.Load() != 1 {
		t.Fatalf("first=%v second=%v calls=%d", firstErr, secondErr, calls.Load())
	}
}

func TestCommentAdapterListRecentCredentialInvalidClassification(t *testing.T) {
	tests := []struct {
		name              string
		code              int
		child             bool
		credentialInvalid bool
		reason            commentbridge.CredentialInvalidReason
	}{
		{name: "top-level login", code: -101, credentialInvalid: true, reason: commentbridge.CredentialInvalidLogin},
		{name: "top-level csrf", code: -111, credentialInvalid: true, reason: commentbridge.CredentialInvalidCSRF},
		{name: "top-level account state", code: -102, reason: commentbridge.CredentialInvalidUnknown},
		{name: "top-level risk", code: -352, reason: commentbridge.CredentialInvalidUnknown},
		{name: "child login", code: -101, child: true, credentialInvalid: true, reason: commentbridge.CredentialInvalidLogin},
		{name: "child csrf", code: -111, child: true, credentialInvalid: true, reason: commentbridge.CredentialInvalidCSRF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/x/v2/reply/up/fulllist":
					if tt.child {
						root := biliCommentFixture("root", 1)
						root["rcount"] = 1
						_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"list": []any{root}}})
						return
					}
					_, _ = fmt.Fprintf(w, `{"code":%d,"message":"test"}`, tt.code)
				case "/x/v2/reply/reply":
					_, _ = fmt.Fprintf(w, `{"code":%d,"message":"test"}`, tt.code)
				default:
					t.Fatalf("unexpected path %s", r.URL.Path)
				}
			}))
			defer server.Close()
			adapter := NewCommentAdapter(newCommentTestClient(t, server.URL), "42")
			_, err := adapter.ListRecent(context.Background())
			if err == nil {
				t.Fatal("ListRecent unexpectedly succeeded")
			}
			if got := commentbridge.IsCredentialInvalid(err); got != tt.credentialInvalid {
				t.Fatalf("credential-invalid=%v want %v err=%v", got, tt.credentialInvalid, err)
			}
			if got := commentbridge.CredentialInvalidReasonOf(err); got != tt.reason {
				t.Fatalf("credential reason=%q want %q err=%v", got, tt.reason, err)
			}
			if tt.credentialInvalid && !errors.Is(err, ErrAuthentication) {
				t.Fatalf("credential error lost ErrAuthentication compatibility: %v", err)
			}
		})
	}
}

func TestCommentAdapterReplyCredentialInvalidClassification(t *testing.T) {
	tests := []struct {
		name              string
		code              int
		nav               bool
		credentialInvalid bool
		reason            commentbridge.CredentialInvalidReason
	}{
		{name: "nav login", code: -101, nav: true, credentialInvalid: true, reason: commentbridge.CredentialInvalidLogin},
		{name: "nav csrf", code: -111, nav: true, credentialInvalid: true, reason: commentbridge.CredentialInvalidCSRF},
		{name: "nav account state", code: -102, nav: true, reason: commentbridge.CredentialInvalidUnknown},
		{name: "nav risk", code: -352, nav: true, reason: commentbridge.CredentialInvalidUnknown},
		{name: "submit login", code: -101, credentialInvalid: true, reason: commentbridge.CredentialInvalidLogin},
		{name: "submit csrf", code: -111, credentialInvalid: true, reason: commentbridge.CredentialInvalidCSRF},
		{name: "submit account state", code: -102, reason: commentbridge.CredentialInvalidUnknown},
		{name: "submit risk", code: -352, reason: commentbridge.CredentialInvalidUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/x/web-interface/nav":
					if !tt.nav {
						t.Fatal("unexpected nav request")
					}
					_, _ = fmt.Fprintf(w, `{"code":%d,"message":"test"}`, tt.code)
				case "/x/v2/reply/add":
					if tt.nav {
						t.Fatal("nav failure must stop before reply submission")
					}
					_, _ = fmt.Fprintf(w, `{"code":%d,"message":"test"}`, tt.code)
				default:
					t.Fatalf("unexpected path %s", r.URL.Path)
				}
			}))
			defer server.Close()
			client := newCommentTestClient(t, server.URL)
			if tt.nav {
				client.invalidateWBIKeys()
			}
			_, err := NewCommentAdapter(client, "42").Reply(context.Background(), commentbridge.Comment{Platform: commentbridge.PlatformBilibili, AccountID: "42", ID: "1", ResourceID: "2"}, "hello")
			if err == nil {
				t.Fatal("Reply unexpectedly succeeded")
			}
			if got := commentbridge.IsCredentialInvalid(err); got != tt.credentialInvalid {
				t.Fatalf("credential-invalid=%v want %v err=%v", got, tt.credentialInvalid, err)
			}
			if got := commentbridge.CredentialInvalidReasonOf(err); got != tt.reason {
				t.Fatalf("credential reason=%q want %q err=%v", got, tt.reason, err)
			}
			if tt.credentialInvalid && !errors.Is(err, ErrAuthentication) {
				t.Fatalf("credential error lost ErrAuthentication compatibility: %v", err)
			}
		})
	}
}

func TestCommentAdapterRefreshesRejectedWBISignatureOnce(t *testing.T) {
	var posts, navCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/x/web-interface/nav":
			navCalls.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"wbi_img":{"img_url":"https://i0.hdslb.com/bfs/wbi/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.png","sub_url":"https://i0.hdslb.com/bfs/wbi/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.png"}}}`))
		case "/x/v2/reply/add":
			if posts.Add(1) == 1 {
				_, _ = w.Write([]byte(`{"code":-403}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"rpid_str":"99"}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	adapter := NewCommentAdapter(newCommentTestClient(t, server.URL), "42")
	id, err := adapter.Reply(context.Background(), commentbridge.Comment{Platform: commentbridge.PlatformBilibili, AccountID: "42", ID: "1", ResourceID: "2"}, "hello")
	if err != nil || id != "99" || posts.Load() != 2 || navCalls.Load() != 1 {
		t.Fatalf("id=%q err=%v posts=%d nav=%d", id, err, posts.Load(), navCalls.Load())
	}
}

func TestCommentAdapterReplyOutcomeClassification(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		unknown bool
	}{
		{"server error", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }, true},
		{"bad json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{")) }, true},
		{"http client error", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }, false},
		{"api error", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"code":-352}`)) }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()
			_, err := NewCommentAdapter(newCommentTestClient(t, server.URL), "42").Reply(context.Background(), commentbridge.Comment{Platform: commentbridge.PlatformBilibili, ID: "1", ResourceID: "2"}, "hello")
			if err == nil || commentbridge.IsUnknownOutcome(err) != tt.unknown {
				t.Fatalf("err=%v unknown=%v", err, commentbridge.IsUnknownOutcome(err))
			}
			if tt.name == "api error" && !errors.Is(err, ErrCommentRiskControl) {
				t.Fatalf("risk control error is not identifiable: %v", err)
			}
		})
	}

	t.Run("network error", func(t *testing.T) {
		c := newCommentTestClient(t, "http://127.0.0.1:1")
		_, err := NewCommentAdapter(c, "42").Reply(context.Background(), commentbridge.Comment{Platform: commentbridge.PlatformBilibili, ID: "1", ResourceID: "2"}, "hello")
		if err == nil || !commentbridge.IsUnknownOutcome(err) || strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestCommentAdapterReplyRejectsRedirectWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "/other", http.StatusFound)
	}))
	defer server.Close()
	_, err := NewCommentAdapter(newCommentTestClient(t, server.URL), "42").Reply(context.Background(), commentbridge.Comment{Platform: commentbridge.PlatformBilibili, ID: "1", ResourceID: "2"}, "hello")
	if err == nil || !commentbridge.IsUnknownOutcome(err) || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestCommentAdapterEngageRequestContractAndSuccess(t *testing.T) {
	var calls atomic.Int32
	expectedRPIDs := []string{"top-level", "child"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := int(calls.Add(1)) - 1
		if call >= len(expectedRPIDs) || r.Method != http.MethodPost || r.URL.Path != "/x/v2/reply/action" || r.URL.RawQuery != "" {
			t.Fatal("unexpected engagement endpoint")
		}
		if r.Header.Get("Origin") != "https://www.bilibili.com" || r.Header.Get("Referer") != "https://www.bilibili.com/" || r.Header.Get("User-Agent") == "" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Fatal("missing browser engagement headers")
		}
		if cookie, err := r.Cookie("SESSDATA"); err != nil || cookie.Value != "session" {
			t.Fatal("missing session cookie")
		}
		if cookie, err := r.Cookie("bili_jct"); err != nil || cookie.Value != "csrf" {
			t.Fatal("missing CSRF cookie")
		}
		body, err := ioReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		form, err := url.ParseQuery(string(body))
		if err != nil || len(form) != 6 || form.Get("oid") != "video" || form.Get("type") != "1" || form.Get("rpid") != expectedRPIDs[call] || form.Get("action") != "1" || form.Get("csrf") != "csrf" || form.Get("csrf_token") != "csrf" {
			t.Fatal("unexpected engagement form")
		}
		if form.Get("w_rid") != "" || form.Get("wts") != "" {
			t.Fatal("engagement form must not contain WBI fields")
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()
	adapter := NewCommentAdapter(newCommentTestClient(t, server.URL), "42")
	for _, id := range expectedRPIDs {
		comment := commentbridge.Comment{Platform: commentbridge.PlatformBilibili, AccountID: "42", ID: id, ResourceID: "video"}
		got, err := adapter.Engage(context.Background(), comment, commentbridge.EngagementLike)
		if err != nil || got != id {
			t.Fatalf("id=%q engagementID=%q err=%v", id, got, err)
		}
	}
	if calls.Load() != int32(len(expectedRPIDs)) {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestCommentAdapterEngageRejectsInvalidTargetsWithoutNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	adapter := NewCommentAdapter(newCommentTestClient(t, server.URL), "42")
	for _, tc := range []struct {
		name    string
		comment commentbridge.Comment
		kind    commentbridge.EngagementKind
	}{
		{"wrong platform", commentbridge.Comment{Platform: commentbridge.PlatformYouTube, AccountID: "42", ID: "1", ResourceID: "2"}, commentbridge.EngagementLike},
		{"wrong account", commentbridge.Comment{Platform: commentbridge.PlatformBilibili, AccountID: "99", ID: "1", ResourceID: "2"}, commentbridge.EngagementLike},
		{"missing account", commentbridge.Comment{Platform: commentbridge.PlatformBilibili, ID: "1", ResourceID: "2"}, commentbridge.EngagementLike},
		{"missing comment ID", commentbridge.Comment{Platform: commentbridge.PlatformBilibili, AccountID: "42", ResourceID: "2"}, commentbridge.EngagementLike},
		{"missing resource ID", commentbridge.Comment{Platform: commentbridge.PlatformBilibili, AccountID: "42", ID: "1"}, commentbridge.EngagementLike},
		{"unsupported kind", commentbridge.Comment{Platform: commentbridge.PlatformBilibili, AccountID: "42", ID: "1", ResourceID: "2"}, commentbridge.EngagementKind("other")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := adapter.Engage(context.Background(), tc.comment, tc.kind); err == nil {
				t.Fatal("invalid engagement was accepted")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid engagements made %d requests", calls.Load())
	}
}

func TestCommentAdapterEngageRequiresCredentialsWithoutNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, Gate: NewRequestGate(GateConfig{Disabled: true}), RetryMax: 0})
	if err != nil {
		t.Fatal(err)
	}
	client.SetCredential(Credential{Cookies: map[string]string{"SESSDATA": "session"}})
	_, err = NewCommentAdapter(client, "42").Engage(context.Background(), commentbridge.Comment{Platform: commentbridge.PlatformBilibili, AccountID: "42", ID: "1", ResourceID: "2"}, commentbridge.EngagementLike)
	if err == nil || calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestCommentAdapterEngageOutcomeClassification(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		unknown bool
	}{
		{"redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/other", http.StatusFound) }, true},
		{"server error", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }, true},
		{"truncated body", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "9")
			_, _ = w.Write([]byte("x"))
		}, true},
		{"bad JSON", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{")) }, true},
		{"missing code", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"data":{}}`)) }, true},
		{"HTTP client error", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }, false},
		{"business failure", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"code":12002}`)) }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				tc.handler(w, r)
			}))
			defer server.Close()
			_, err := NewCommentAdapter(newCommentTestClient(t, server.URL), "42").Engage(context.Background(), commentbridge.Comment{Platform: commentbridge.PlatformBilibili, AccountID: "42", ID: "1", ResourceID: "2"}, commentbridge.EngagementLike)
			if err == nil || commentbridge.IsUnknownOutcome(err) != tc.unknown || calls.Load() != 1 {
				t.Fatalf("err=%v unknown=%v calls=%d", err, commentbridge.IsUnknownOutcome(err), calls.Load())
			}
		})
	}

	for _, code := range []int{12002, 12004, 12006, 12009, 12011} {
		t.Run(fmt.Sprintf("deterministic API failure %d", code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"code":%d}`, code)
			}))
			defer server.Close()
			_, err := NewCommentAdapter(newCommentTestClient(t, server.URL), "42").Engage(context.Background(), commentbridge.Comment{Platform: commentbridge.PlatformBilibili, AccountID: "42", ID: "1", ResourceID: "2"}, commentbridge.EngagementLike)
			if err == nil || commentbridge.IsUnknownOutcome(err) {
				t.Fatalf("err=%v", err)
			}
		})
	}

	t.Run("network failure", func(t *testing.T) {
		adapter := NewCommentAdapter(newCommentTestClient(t, "http://127.0.0.1:1"), "42")
		_, err := adapter.Engage(context.Background(), commentbridge.Comment{Platform: commentbridge.PlatformBilibili, AccountID: "42", ID: "1", ResourceID: "2"}, commentbridge.EngagementLike)
		if err == nil || !commentbridge.IsUnknownOutcome(err) || strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("context cancelled after request", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			close(started)
			<-release
		}))
		defer server.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := NewCommentAdapter(newCommentTestClient(t, server.URL), "42").Engage(ctx, commentbridge.Comment{Platform: commentbridge.PlatformBilibili, AccountID: "42", ID: "1", ResourceID: "2"}, commentbridge.EngagementLike)
			result <- err
		}()
		<-started
		cancel()
		err := <-result
		close(release)
		if err == nil || !commentbridge.IsUnknownOutcome(err) || calls.Load() != 1 {
			t.Fatalf("err=%v calls=%d", err, calls.Load())
		}
	})
}

func TestCommentAdapterEngageRiskFailuresFuseWithoutASecondRequest(t *testing.T) {
	tests := []struct {
		name              string
		statusCode        int
		apiCode           int
		auth              bool
		credentialInvalid bool
		reason            commentbridge.CredentialInvalidReason
	}{
		{name: "HTTP forbidden", statusCode: http.StatusForbidden, reason: commentbridge.CredentialInvalidUnknown},
		{name: "HTTP precondition", statusCode: http.StatusPreconditionFailed, reason: commentbridge.CredentialInvalidUnknown},
		{name: "HTTP rate limited", statusCode: http.StatusTooManyRequests, reason: commentbridge.CredentialInvalidUnknown},
		{name: "not logged in", apiCode: -101, auth: true, credentialInvalid: true, reason: commentbridge.CredentialInvalidLogin},
		{name: "banned", apiCode: -102, auth: true, reason: commentbridge.CredentialInvalidUnknown},
		{name: "CSRF invalid", apiCode: -111, auth: true, credentialInvalid: true, reason: commentbridge.CredentialInvalidCSRF},
		{name: "risk control", apiCode: -352, reason: commentbridge.CredentialInvalidUnknown},
		{name: "API forbidden", apiCode: -403, reason: commentbridge.CredentialInvalidUnknown},
		{name: "API precondition", apiCode: -412, reason: commentbridge.CredentialInvalidUnknown},
		{name: "API busy", apiCode: -509, reason: commentbridge.CredentialInvalidUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				if tc.statusCode != 0 {
					w.WriteHeader(tc.statusCode)
					return
				}
				_, _ = fmt.Fprintf(w, `{"code":%d}`, tc.apiCode)
			}))
			defer server.Close()
			adapter := NewCommentAdapter(newCommentTestClient(t, server.URL), "42")
			comment := commentbridge.Comment{Platform: commentbridge.PlatformBilibili, AccountID: "42", ID: "1", ResourceID: "2"}
			_, firstErr := adapter.Engage(context.Background(), comment, commentbridge.EngagementLike)
			if firstErr == nil || (tc.auth && !errors.Is(firstErr, ErrAuthentication)) || (!tc.auth && !errors.Is(firstErr, ErrCommentRiskControl)) {
				t.Fatalf("first error=%v", firstErr)
			}
			if got := commentbridge.IsCredentialInvalid(firstErr); got != tc.credentialInvalid {
				t.Fatalf("credential-invalid=%v want %v err=%v", got, tc.credentialInvalid, firstErr)
			}
			if got := commentbridge.CredentialInvalidReasonOf(firstErr); got != tc.reason {
				t.Fatalf("credential reason=%q want %q err=%v", got, tc.reason, firstErr)
			}
			_, secondErr := adapter.Engage(context.Background(), comment, commentbridge.EngagementLike)
			if !errors.Is(secondErr, ErrCommentRiskControl) || calls.Load() != 1 {
				t.Fatalf("second error=%v calls=%d", secondErr, calls.Load())
			}
		})
	}
}

// ioReadAll isolates the test from a handler's body lifetime and keeps the
// assertions above focused on field presence, never their sensitive values.
func ioReadAll(r io.Reader) ([]byte, error) { return io.ReadAll(r) }
