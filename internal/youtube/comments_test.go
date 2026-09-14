package youtube

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"comment-bridge/internal/commentbridge"
)

func TestListPaginatesAndNormalizes(t *testing.T) {
	var pages atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"access_token":"a","expires_in":3600,"scope":"https://www.googleapis.com/auth/youtube.force-ssl"}`))
			return
		}
		if r.URL.Path == "/youtube/v3/videos" {
			_, _ = w.Write([]byte(`{"items":[{"id":"v1","snippet":{"title":"Video 1"}},{"id":"v2","snippet":{"title":"Video 2"}}]}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer a" || r.URL.Query().Get("maxResults") != "100" || r.URL.Query().Get("part") != "snippet,replies" || r.URL.Query().Get("textFormat") != "plainText" {
			t.Error("bad request")
		}
		if pages.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"nextPageToken":"n","items":[{"id":"thread2","snippet":{"topLevelComment":{"id":"new","snippet":{"videoId":"v2","authorChannelId":{"value":"u2"},"authorDisplayName":"N","textDisplay":"display","publishedAt":"2025-01-02T00:00:00Z"}}}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"id":"thread1","snippet":{"topLevelComment":{"id":"old","snippet":{"videoId":"v1","authorChannelId":{"value":"u1"},"authorDisplayName":"O","textOriginal":"original","publishedAt":"2025-01-01T00:00:00Z"}}}}]}`))
	}))
	defer s.Close()
	cfg := testConfig(s.URL)
	cfg.MaxPages = 2
	c, _ := NewClient(cfg)
	got, err := c.ListRecent(context.Background())
	if err != nil || len(got) != 2 || got[0].ID != "old" || got[0].RootID != "old" || got[0].ParentID != "" || got[0].Text != "original" || got[1].Text != "display" || got[0].ResourceURL != "https://www.youtube.com/watch?lc=old&v=v1" {
		t.Fatalf("%#v %v", got, err)
	}
}

func TestListRecentResolvesVideoTitles(t *testing.T) {
	var videoCalls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_, _ = w.Write([]byte(`{"access_token":"a","expires_in":3600,"scope":"https://www.googleapis.com/auth/youtube.force-ssl"}`))
		case "/youtube/v3/commentThreads":
			_, _ = w.Write([]byte(`{"items":[{"snippet":{"videoId":"video-1","topLevelComment":{"id":"comment-1","snippet":{"authorChannelId":{"value":"author-1"},"publishedAt":"2025-01-01T00:00:00Z"}}}},{"snippet":{"videoId":"video-2","topLevelComment":{"id":"comment-2","snippet":{"authorChannelId":{"value":"author-2"},"publishedAt":"2025-01-02T00:00:00Z"}}}}]}`))
		case "/youtube/v3/videos":
			videoCalls.Add(1)
			if r.URL.Query().Get("part") != "snippet" || r.URL.Query().Get("id") != "video-1,video-2" || r.URL.Query().Get("maxResults") != "" || r.Header.Get("Authorization") != "Bearer a" {
				t.Fatalf("unexpected videos request: %s?%s", r.URL.Path, r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"items":[{"id":"video-1","snippet":{"title":"First video"}},{"id":"video-2","snippet":{"title":"第二个视频"}}]}`))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer s.Close()

	c, err := NewClient(testConfig(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	comments, err := c.ListRecent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 2 || comments[0].ResourceTitle != "First video" || comments[1].ResourceTitle != "第二个视频" {
		t.Fatalf("video titles = %q, %q", comments[0].ResourceTitle, comments[1].ResourceTitle)
	}
	if _, err := c.ListRecent(context.Background()); err != nil || videoCalls.Load() != 1 {
		t.Fatalf("video title cache calls = %d, err = %v", videoCalls.Load(), err)
	}
}

func TestListNormalizesEmbeddedReplies(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"access_token":"a","expires_in":3600,"scope":"https://www.googleapis.com/auth/youtube.force-ssl"}`))
			return
		}
		if r.URL.Path == "/youtube/v3/comments" {
			t.Fatal("complete embedded replies must not be expanded")
		}
		if r.URL.Path == "/youtube/v3/videos" {
			_, _ = w.Write([]byte(`{"items":[{"id":"thread-video","snippet":{"title":"Thread video"}}]}`))
			return
		}
		if r.URL.Query().Get("part") != "snippet,replies" || r.URL.Query().Get("textFormat") != "plainText" {
			t.Error("comment thread reply fields were not requested")
		}
		_, _ = w.Write([]byte(`{"items":[{"snippet":{"videoId":"thread-video","totalReplyCount":1,"topLevelComment":{"id":"top","snippet":{"videoId":"top-video","authorChannelId":{"value":"top-author"},"authorDisplayName":"Top","textOriginal":"top text","publishedAt":"2025-01-02T00:00:00Z"}}},"replies":{"comments":[{"id":"reply","snippet":{"videoId":"wrong-video","authorChannelId":{"value":"reply-author"},"authorDisplayName":"Reply","textDisplay":"reply text","publishedAt":"2025-01-01T00:00:00Z"}}]}}]}`))
	}))
	defer s.Close()
	c, _ := NewClient(testConfig(s.URL))
	got, err := c.ListRecent(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("%#v %v", got, err)
	}
	if got[0].ID != "reply" || got[0].RootID != "top" || got[0].ParentID != "top" || got[0].ResourceID != "thread-video" || got[0].Text != "reply text" || got[0].AuthorID != "reply-author" {
		t.Fatalf("embedded reply = %#v", got[0])
	}
	if got[1].ID != "top" || got[1].RootID != "top" || got[1].ParentID != "" {
		t.Fatalf("top-level comment = %#v", got[1])
	}
	assertCommentWatchURL(t, got[0].ResourceURL, "thread-video", "reply")
	assertCommentWatchURL(t, got[1].ResourceURL, "thread-video", "top")
}

func TestWatchURLSafelyEncodesTopLevelAndReplyCommentIDs(t *testing.T) {
	topURL := watchURL("video & id", "top/ comment?x=1")
	replyURL := watchURL("video & id", "reply/ comment?x=2")
	assertCommentWatchURL(t, topURL, "video & id", "top/ comment?x=1")
	assertCommentWatchURL(t, replyURL, "video & id", "reply/ comment?x=2")
	if strings.Contains(topURL, "top/ comment?x=1") || strings.Contains(replyURL, "reply/ comment?x=2") {
		t.Fatalf("comment IDs were not query encoded: top=%q reply=%q", topURL, replyURL)
	}
}

func TestWatchURLWithoutCommentIDKeepsVideoURL(t *testing.T) {
	if got, want := watchURL("video", ""), "https://www.youtube.com/watch?v=video"; got != want {
		t.Fatalf("watch URL = %q, want %q", got, want)
	}
}

func assertCommentWatchURL(t *testing.T, rawURL, videoID, commentID string) {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("invalid watch URL %q: %v", rawURL, err)
	}
	if parsed.Scheme != "https" || parsed.Host != "www.youtube.com" || parsed.Path != "/watch" || parsed.Query().Get("v") != videoID || parsed.Query().Get("lc") != commentID {
		t.Fatalf("watch URL = %q", rawURL)
	}
}

func TestListSuppressesCreatorsOwnCommentsAndReplies(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"access_token":"a","expires_in":3600,"scope":"https://www.googleapis.com/auth/youtube.force-ssl"}`))
			return
		}
		if r.URL.Path == "/youtube/v3/videos" {
			_, _ = w.Write([]byte(`{"items":[{"id":"v","snippet":{"title":"Video"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"snippet":{"videoId":"v","totalReplyCount":2,"topLevelComment":{"id":"viewer-top","snippet":{"authorChannelId":{"value":"viewer"},"textOriginal":"question","publishedAt":"2025-01-01T00:00:00Z"}}},"replies":{"comments":[{"id":"creator-reply","snippet":{"authorChannelId":{"value":"channel"},"textOriginal":"answer","publishedAt":"2025-01-02T00:00:00Z"}},{"id":"viewer-reply","snippet":{"authorChannelId":{"value":"viewer2"},"textOriginal":"follow-up","publishedAt":"2025-01-03T00:00:00Z"}}]}},{"snippet":{"videoId":"v","topLevelComment":{"id":"creator-top","snippet":{"authorChannelId":{"value":"channel"},"textOriginal":"owner","publishedAt":"2025-01-04T00:00:00Z"}}}}]}`))
	}))
	defer s.Close()
	c, _ := NewClient(testConfig(s.URL))
	got, err := c.ListRecent(context.Background())
	if err != nil || len(got) != 2 || got[0].ID != "viewer-top" || got[1].ID != "viewer-reply" {
		t.Fatalf("comments=%#v err=%v", got, err)
	}
}

func TestListExpandsIncompleteRepliesOnlyWhenCountChanges(t *testing.T) {
	var polls, replyPages atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"access_token":"a","expires_in":3600,"scope":"https://www.googleapis.com/auth/youtube.force-ssl"}`))
			return
		}
		switch r.URL.Path {
		case "/youtube/v3/commentThreads":
			poll := polls.Add(1)
			count := 3
			if poll >= 3 {
				count = 4
			}
			_, _ = w.Write([]byte(`{"items":[{"snippet":{"videoId":"v","totalReplyCount":` + string(rune('0'+count)) + `,"topLevelComment":{"id":"top","snippet":{"authorChannelId":{"value":"u"},"authorDisplayName":"Top","textOriginal":"top","publishedAt":"2025-01-01T00:00:00Z"}}},"replies":{"comments":[{"id":"r1","snippet":{"authorChannelId":{"value":"u1"},"authorDisplayName":"One","textOriginal":"one","publishedAt":"2025-01-02T00:00:00Z"}}]}}]}`))
		case "/youtube/v3/comments":
			replyPages.Add(1)
			if r.Header.Get("Authorization") != "Bearer a" || r.URL.Query().Get("part") != "snippet" || r.URL.Query().Get("parentId") != "top" || r.URL.Query().Get("maxResults") != "100" || r.URL.Query().Get("textFormat") != "plainText" {
				t.Error("bad comments.list request")
			}
			if r.URL.Query().Get("pageToken") == "" {
				_, _ = w.Write([]byte(`{"nextPageToken":"next","items":[{"id":"r1","snippet":{"publishedAt":"2025-01-02T00:00:00Z"}},{"id":"r2","snippet":{"publishedAt":"2025-01-03T00:00:00Z"}}]}`))
				return
			}
			if polls.Load() >= 3 {
				_, _ = w.Write([]byte(`{"items":[{"id":"r3","snippet":{"publishedAt":"2025-01-04T00:00:00Z"}},{"id":"r4","snippet":{"publishedAt":"2025-01-05T00:00:00Z"}}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"items":[{"id":"r3","snippet":{"publishedAt":"2025-01-04T00:00:00Z"}}]}`))
		case "/youtube/v3/videos":
			_, _ = w.Write([]byte(`{"items":[{"id":"v","snippet":{"title":"Video"}}]}`))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer s.Close()
	c, _ := NewClient(testConfig(s.URL))

	got, err := c.ListRecent(context.Background())
	if err != nil || len(got) != 4 || replyPages.Load() != 2 {
		t.Fatalf("first poll comments=%#v reply pages=%d err=%v", got, replyPages.Load(), err)
	}
	got, err = c.ListRecent(context.Background())
	if err != nil || len(got) != 2 || replyPages.Load() != 2 {
		t.Fatalf("unchanged poll comments=%#v reply pages=%d err=%v", got, replyPages.Load(), err)
	}
	got, err = c.ListRecent(context.Background())
	if err != nil || len(got) != 5 || replyPages.Load() != 4 {
		t.Fatalf("changed poll comments=%#v reply pages=%d err=%v", got, replyPages.Load(), err)
	}
}

func TestListReplyFailureIsSafeAndNotCached(t *testing.T) {
	var replyRequests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"access_token":"a","expires_in":3600,"scope":"https://www.googleapis.com/auth/youtube.force-ssl"}`))
			return
		}
		if r.URL.Path == "/youtube/v3/commentThreads" {
			_, _ = w.Write([]byte(`{"items":[{"snippet":{"videoId":"v","totalReplyCount":1,"topLevelComment":{"id":"top","snippet":{"publishedAt":"2025-01-01T00:00:00Z"}}}}]}`))
			return
		}
		if r.URL.Path == "/youtube/v3/videos" {
			_, _ = w.Write([]byte(`{"items":[{"id":"v","snippet":{"title":"Video"}}]}`))
			return
		}
		if r.URL.Path != "/youtube/v3/comments" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if replyRequests.Add(1) == 1 {
			http.Error(w, "refresh-secret", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"id":"r1","snippet":{"publishedAt":"2025-01-02T00:00:00Z"}}]}`))
	}))
	defer s.Close()
	c, _ := NewClient(testConfig(s.URL))
	if _, err := c.ListRecent(context.Background()); err == nil || strings.Contains(err.Error(), "refresh-secret") {
		t.Fatalf("unsafe reply error: %v", err)
	}
	got, err := c.ListRecent(context.Background())
	if err != nil || len(got) != 2 || replyRequests.Load() != 2 {
		t.Fatalf("comments=%#v calls=%d err=%v", got, replyRequests.Load(), err)
	}
}

func TestReplyContractAndOutcomes(t *testing.T) {
	statuses := []int{500, 400, 200}
	var n atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"access_token":"a","expires_in":3600,"scope":"https://www.googleapis.com/auth/youtube.force-ssl"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.URL.Query().Get("part") != "snippet" || !strings.Contains(string(body), `"parentId":"p"`) || !strings.Contains(string(body), `"textOriginal":"hi"`) {
			t.Error("bad post")
		}
		status := statuses[n.Add(1)-1]
		w.WriteHeader(status)
		if status == 400 {
			_, _ = w.Write([]byte(`{"error":{"errors":[{"reason":"forbidden"}]}}`))
		}
		if status == 200 {
			_, _ = w.Write([]byte(`{"id":"reply"}`))
		}
	}))
	defer s.Close()
	for i := range statuses {
		c, _ := NewClient(testConfig(s.URL))
		id, err := c.Reply(context.Background(), commentbridge.Comment{Platform: commentbridge.PlatformYouTube, ID: "p", RootID: "p"}, " hi ")
		if i == 0 && !commentbridge.IsUnknownOutcome(err) {
			t.Fatal(err)
		}
		if i == 1 && (err == nil || commentbridge.IsUnknownOutcome(err) || !strings.Contains(err.Error(), "forbidden")) {
			t.Fatal(err)
		}
		if i == 2 && (err != nil || id != "reply") {
			t.Fatal(id, err)
		}
	}
}

func TestReplyUsesRootIDForChildComment(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"access_token":"a","expires_in":3600,"scope":"https://www.googleapis.com/auth/youtube.force-ssl"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"parentId":"top"`) || strings.Contains(string(body), `"parentId":"child"`) {
			t.Errorf("reply target = %s", body)
		}
		_, _ = w.Write([]byte(`{"id":"reply"}`))
	}))
	defer s.Close()
	c, _ := NewClient(testConfig(s.URL))
	if _, err := c.Reply(context.Background(), commentbridge.Comment{Platform: commentbridge.PlatformYouTube, ID: "child", RootID: "top", ParentID: "top"}, "hi"); err != nil {
		t.Fatal(err)
	}
}

func TestReplyRejectsAnotherAccountWithoutNetwork(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer s.Close()
	c, err := NewClient(testConfig(s.URL))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Reply(context.Background(), commentbridge.Comment{Platform: commentbridge.PlatformYouTube, AccountID: "other", ID: "p"}, "hi")
	if err == nil || calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func Test401NotRetriedAndRedirectUnknown(t *testing.T) {
	var lists atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"access_token":"a","expires_in":3600,"scope":"https://www.googleapis.com/auth/youtube.force-ssl"}`))
			return
		}
		if r.URL.Path == "/youtube/v3/commentThreads" {
			lists.Add(1)
			w.WriteHeader(401)
			return
		}
		http.Redirect(w, r, "/else", 302)
	}))
	defer s.Close()
	c, _ := NewClient(testConfig(s.URL))
	if _, err := c.ListRecent(context.Background()); err == nil || lists.Load() != 1 {
		t.Fatalf("401 retry: %v %d", err, lists.Load())
	}
	c, _ = NewClient(testConfig(s.URL))
	_, err := c.Reply(context.Background(), commentbridge.Comment{Platform: commentbridge.PlatformYouTube, ID: "p"}, "x")
	if !commentbridge.IsUnknownOutcome(err) {
		t.Fatal(err)
	}
}

func TestThreadListRejectsRepeatedPageToken(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"access_token":"a","expires_in":3600,"scope":"https://www.googleapis.com/auth/youtube.force-ssl"}`))
			return
		}
		_, _ = w.Write([]byte(`{"nextPageToken":"same","items":[]}`))
	}))
	defer s.Close()
	cfg := testConfig(s.URL)
	cfg.MaxPages = 3
	c, _ := NewClient(cfg)
	if _, err := c.ListRecent(context.Background()); err == nil {
		t.Fatal("accepted repeated thread page token")
	}
}
