package bili

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDanmakuCreatorInboxPrecisionAndSharedCooldown(t *testing.T) {
	calls := 0
	code := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/x/v2/dm/recent" || r.URL.Query().Get("ps") != "100" || r.URL.Query().Get("pn") != "1" || r.URL.Query().Get("oid") != "" {
			t.Error("wrong creator inbox request")
		}
		if c, e := r.Cookie("SESSDATA"); e != nil || c.Value != "session" {
			t.Error("missing authenticated cookie")
		}
		fmt.Fprintf(w, `{"code":%d,"data":{"result":[{"id":999999999999999999,"id_str":"999999999999999999","aid":123,"msg":"音画不同步","title":"视频","progress":12345,"ctime":1700000000}]}}`, code)
	}))
	defer server.Close()
	a := NewCommentAdapter(newCommentTestClient(t, server.URL), "1")
	items, e := a.ListRecentDanmaku(t.Context())
	if e != nil || len(items) != 1 {
		t.Fatal(items, e)
	}
	if items[0].ID != "999999999999999999" || items[0].VideoURL != "https://www.bilibili.com/video/av123?t=12" {
		t.Fatal(items)
	}
	code = -352
	if _, e := a.ListRecentDanmaku(t.Context()); e == nil {
		t.Fatal("risk response accepted")
	}
	if calls != 2 {
		t.Fatal("risk response retried", calls)
	}
	if _, e := a.ListRecent(t.Context()); e == nil || calls != 2 {
		t.Fatal("comment read bypassed danmaku cooldown")
	}
	if _, e := a.ListRecentDanmaku(t.Context()); e == nil || calls != 2 {
		t.Fatal("danmaku read bypassed cooldown")
	}
	a.readRetryAt = time.Time{}
	a.riskFused = true
	if _, e := a.ListRecentDanmaku(t.Context()); e == nil || calls != 2 {
		t.Fatal("danmaku read bypassed fuse")
	}
}

func TestDanmakuRejectsMalformedInboxWithoutExtraRequests(t *testing.T) {
	for _, body := range []string{`{"code":0,"data":{}}`, `{"code":0,"data":{"result":[{"id":1,"aid":"bad","msg":"text"}]}}`, `{"code":0,"data":{"result":[{"id":1,"aid":2,"msg":""}]}}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		a := NewCommentAdapter(newCommentTestClient(t, server.URL), "1")
		if _, e := a.ListRecentDanmaku(t.Context()); e == nil {
			t.Error("malformed inbox accepted")
		}
		server.Close()
	}
}
