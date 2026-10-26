package youtube

import (
	"comment-bridge/internal/commentbridge"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestModerationExactCommentAndBanParameter(t *testing.T) {
	for _, kind := range []commentbridge.ModerationKind{commentbridge.ModerationDelete, commentbridge.ModerationBan} {
		for _, status := range []int{204, 200, 302, 401, 403, 500} {
			t.Run(fmt.Sprint(kind, status), func(t *testing.T) {
				calls := 0
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/token" {
						fmt.Fprint(w, `{"access_token":"a","expires_in":3600,"scope":"https://www.googleapis.com/auth/youtube.force-ssl"}`)
						return
					}
					calls++
					if r.URL.Path != "/youtube/v3/comments/setModerationStatus" || r.Method != "POST" || r.URL.Query().Get("id") != "child" || r.URL.Query().Get("moderationStatus") != "rejected" || r.Header.Get("Authorization") != "Bearer a" {
						t.Error("wrong request")
					}
					if (r.URL.Query().Get("banAuthor") == "true") != (kind == commentbridge.ModerationBan) {
						t.Error("bad banAuthor")
					}
					w.WriteHeader(status)
				}))
				defer s.Close()
				c, err := NewClient(testConfig(s.URL))
				if err != nil {
					t.Fatal(err)
				}
				target := commentbridge.Comment{Platform: c.Platform(), AccountID: c.AccountID(), ID: "child", RootID: "root", AuthorID: "author"}
				_, err = c.Moderate(t.Context(), target, kind)
				unknown := status == 200 || status == 302 || status == 500
				if (err != nil) != (status != 204) || commentbridge.IsUnknownOutcome(err) != unknown || calls != 1 {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
				target.ID = "one,two"
				if _, err = c.Moderate(t.Context(), target, kind); err == nil || calls != 1 {
					t.Fatal("multi-target accepted")
				}
			})
		}
	}
}
