package bili

import (
	"comment-bridge/internal/commentbridge"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestModerationExactTargetAndSingleAttempt(t *testing.T) {
	for _, kind := range []commentbridge.ModerationKind{commentbridge.ModerationDelete, commentbridge.ModerationBan} {
		for _, tc := range []struct {
			status        int
			body          string
			unknown, fail bool
		}{{200, `{"code":0}`, false, false}, {200, `{}`, true, true}, {500, ``, true, true}, {302, ``, true, true}, {200, `{"code":-101}`, false, true}, {200, `{"code":-352}`, false, true}} {
			t.Run(fmt.Sprint(kind, tc.status, tc.body), func(t *testing.T) {
				calls := 0
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if err := r.ParseForm(); err != nil {
						t.Fatal(err)
					}
					if r.Method != "POST" || r.Form.Get("csrf") != "csrf" || r.Form.Get("w_rid") != "" {
						t.Error("bad form")
					}
					if kind == commentbridge.ModerationDelete {
						if r.URL.Path != "/x/v2/reply/del" || r.Form.Get("rpid") != "3" || r.Form.Get("oid") != "2" {
							t.Error("wrong delete target")
						}
					} else {
						if r.URL.Path != "/x/relation/modify" || r.Form.Get("fid") != "4" || r.Form.Get("act") != "5" {
							t.Error("wrong ban target")
						}
					}
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
				}))
				defer s.Close()
				a := NewCommentAdapter(newCommentTestClient(t, s.URL), "1")
				c := commentbridge.Comment{Platform: commentbridge.PlatformBilibili, AccountID: "1", ID: "3", RootID: "999", ResourceID: "2", AuthorID: "4"}
				_, err := a.Moderate(t.Context(), c, kind)
				if (err != nil) != tc.fail || commentbridge.IsUnknownOutcome(err) != tc.unknown || calls != 1 {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
				c.AccountID = "other"
				if _, err = a.Moderate(t.Context(), c, kind); err == nil || calls != 1 {
					t.Fatal("account validation bypassed")
				}
			})
		}
	}
}
