package commentbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

type testDanmakuQueue struct {
	id     string
	items  []Danmaku
	result DanmakuReview
	ready  bool
}

func (q *testDanmakuQueue) Submit(id string, items []Danmaku) error {
	q.id, q.items = id, items
	return nil
}
func (q *testDanmakuQueue) Result(string) (DanmakuReview, bool, error) { return q.result, q.ready, nil }
func (q *testDanmakuQueue) Remove(string) error                        { return nil }

func dm(id string) Danmaku {
	return Danmaku{ID: id, Text: "声音与画面不同步", VideoTitle: "测试视频", VideoURL: "https://www.bilibili.com/video/av1?t=3", PositionMS: 3000}
}

func TestDanmakuBaselineBoundaryAndRestart(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "bridge.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	now := time.Unix(1800000000, 0)
	items := []Danmaku{dm("3"), dm("2"), dm("1")}
	reads, sends := 0, 0
	q := &testDanmakuQueue{}
	read := func(context.Context) ([]Danmaku, error) { reads++; return items, nil }
	send := func(context.Context, []Danmaku, []DanmakuFinding, string) (string, error) {
		sends++
		return "message", nil
	}
	monitor := func() *DanmakuMonitor {
		m, e := NewDanmakuMonitor(s, "account", read, q, send)
		if e != nil {
			t.Fatal(e)
		}
		m.now = func() time.Time { return now }
		return m
	}
	m := monitor()
	if err := m.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.WorkOnce(ctx); err != nil || q.id != "" {
		t.Fatal("baseline was submitted", err)
	}
	if err := m.PollOnce(ctx); err != nil || reads != 1 {
		t.Fatal("poll interval not respected", err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	m = monitor()
	if err := m.PollOnce(ctx); err != nil || reads != 1 {
		t.Fatal("restart bypassed interval", err)
	}
	now = now.Add(DanmakuInterval)
	items = []Danmaku{dm("5"), dm("5"), dm("4"), dm("3"), dm("unseen-behind-boundary")}
	if err := m.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.WorkOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(q.items) != 2 || q.items[0].ID != "5" || q.items[1].ID != "4" {
		t.Fatalf("wrong fresh batch: %#v", q.items)
	}
	q.ready = true
	q.result = DanmakuReview{Findings: []DanmakuFinding{{ID: "4", Category: "video_problem", Reason: "反馈音画不同步"}}}
	if err := m.WorkOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if sends != 1 {
		t.Fatalf("send count %d", sends)
	}
	for range 2 {
		if err := m.WorkOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(DanmakuInterval)
	if err := m.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if sends != 1 {
		t.Fatal("duplicate alert")
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM danmaku_jobs`).Scan(&total); err != nil || total != 1 {
		t.Fatal("duplicate batch", total, err)
	}
}

func TestDanmakuNormalInvalidAndUnknownOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		review     DanmakuReview
		sendErr    error
		wantStatus string
		wantSends  int
		wantError  bool
	}{
		{"normal", DanmakuReview{}, nil, "succeeded", 0, false},
		{"hallucinated-id", DanmakuReview{Findings: []DanmakuFinding{{ID: "other", Category: "video_problem", Reason: "故障"}}}, nil, "failed", 0, true},
		{"worker-failure", DanmakuReview{Failed: true}, nil, "failed", 0, true},
		{"uncertain-delivery", DanmakuReview{Findings: []DanmakuFinding{{ID: "1", Category: "malicious", Reason: "疑似恶意信息"}}}, unknownDanmakuError{}, "unknown", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, e := Open(":memory:")
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			q := &testDanmakuQueue{ready: true, result: tc.review}
			sent := 0
			m, e := NewDanmakuMonitor(s, "account", func(context.Context) ([]Danmaku, error) { return nil, nil }, q, func(context.Context, []Danmaku, []DanmakuFinding, string) (string, error) {
				sent++
				return "msg", tc.sendErr
			})
			if e != nil {
				t.Fatal(e)
			}
			body, _ := json.Marshal([]Danmaku{dm("1")})
			if _, e := s.db.Exec(`INSERT INTO danmaku_jobs(action_id,account_id,items,status,created_at) VALUES('job','account',?,'pending',?)`, string(body), time.Now().Unix()); e != nil {
				t.Fatal(e)
			}
			if e := m.WorkOnce(t.Context()); (e != nil) != tc.wantError {
				t.Fatal(e)
			}
			if e := m.WorkOnce(t.Context()); e != nil {
				t.Fatal(e)
			}
			var status string
			s.db.QueryRow(`SELECT status FROM danmaku_jobs`).Scan(&status)
			if status != tc.wantStatus || sent != tc.wantSends {
				t.Fatalf("status=%s sends=%d", status, sent)
			}
		})
	}
}

type unknownDanmakuError struct{}

func (unknownDanmakuError) Error() string        { return "uncertain" }
func (unknownDanmakuError) OutcomeUnknown() bool { return true }

func TestDanmakuMergesMatchingContentAcrossBatchesAndRestart(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprint("uncertain=", uncertain), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bridge.db")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.Close() }()
			q := &testDanmakuQueue{ready: true}
			sends := 0
			send := func(_ context.Context, _ []Danmaku, findings []DanmakuFinding, _ string) (string, error) {
				sends++
				var ids []string
				for _, finding := range findings {
					ids = append(ids, finding.ID)
				}
				if !slices.Equal(ids, []string{"1", "3", "4", "5"}) {
					t.Fatalf("wrong merged findings: %v", ids)
				}
				if uncertain {
					return "", unknownDanmakuError{}
				}
				return "message", nil
			}
			monitor := func() *DanmakuMonitor {
				m, err := NewDanmakuMonitor(s, "account", func(context.Context) ([]Danmaku, error) { return nil, nil }, q, send)
				if err != nil {
					t.Fatal(err)
				}
				return m
			}
			m := monitor()
			items := []Danmaku{dm("1"), dm("2"), dm("3"), dm("4"), dm("5")}
			items[2].PositionMS++ // Same displayed second, different exact position.
			items[3].VideoURL = "https://www.bilibili.com/video/av2?t=3"
			items[4].Text = "字幕出现错别字"
			enqueue := func(id string, batch []Danmaku) {
				q.result.Findings = nil
				for _, item := range batch {
					q.result.Findings = append(q.result.Findings, DanmakuFinding{ID: item.ID, Category: "video_problem", Reason: "疑似视频问题"})
				}
				body, err := json.Marshal(batch)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.db.Exec(`INSERT INTO danmaku_jobs(action_id,account_id,items,status,created_at) VALUES(?,'account',?,'pending',?)`, id, string(body), time.Now().Unix()); err != nil {
					t.Fatal(err)
				}
			}
			enqueue("first", items)
			if err := m.WorkOnce(t.Context()); IsUnknownOutcome(err) != uncertain || err != nil && !uncertain {
				t.Fatal(err)
			}
			var stored int
			if err := s.db.QueryRow(`SELECT json_array_length(findings) FROM danmaku_jobs WHERE action_id='first'`).Scan(&stored); err != nil || stored != 4 {
				t.Fatal("stored findings were not merged", stored, err)
			}
			s.Close()
			s, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			m = monitor()
			enqueue("later", []Danmaku{dm("6")})
			if err := m.WorkOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			var status string
			if err := s.db.QueryRow(`SELECT status,json_array_length(findings) FROM danmaku_jobs WHERE action_id='later'`).Scan(&status, &stored); err != nil || status != "succeeded" || stored != 0 || sends != 1 {
				t.Fatal("duplicate alert after restart", status, stored, sends, err)
			}
		})
	}
}

func TestDanmakuEmptyBaselineFailedReadAndSendingRecovery(t *testing.T) {
	s, e := Open(":memory:")
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	now := time.Unix(1800000000, 0)
	fail := true
	reads := 0
	q := &testDanmakuQueue{}
	read := func(context.Context) ([]Danmaku, error) {
		reads++
		if fail {
			return nil, errors.New("unavailable")
		}
		return nil, nil
	}
	send := func(context.Context, []Danmaku, []DanmakuFinding, string) (string, error) {
		t.Fatal("unexpected send")
		return "", nil
	}
	m, e := NewDanmakuMonitor(s, "account", read, q, send)
	if e != nil {
		t.Fatal(e)
	}
	m.now = func() time.Time { return now }
	if e := m.PollOnce(t.Context()); e == nil {
		t.Fatal("failure hidden")
	}
	if e := m.PollOnce(t.Context()); e != nil || reads != 1 {
		t.Fatal("immediate retry", e)
	}
	now = now.Add(DanmakuInterval)
	fail = false
	if e := m.PollOnce(t.Context()); e != nil {
		t.Fatal(e)
	}
	var initialized bool
	s.db.QueryRow(`SELECT initialized FROM danmaku_sources`).Scan(&initialized)
	if !initialized {
		t.Fatal("empty baseline not initialized")
	}
	m.read = func(context.Context) ([]Danmaku, error) { return []Danmaku{dm("1")}, nil }
	now = now.Add(DanmakuInterval)
	if e := m.PollOnce(t.Context()); e != nil {
		t.Fatal(e)
	}
	if _, e := s.db.Exec(`UPDATE danmaku_jobs SET status='sending'`); e != nil {
		t.Fatal(e)
	}
	if _, e := NewDanmakuMonitor(s, "account", read, q, send); e != nil {
		t.Fatal(e)
	}
	var status string
	s.db.QueryRow(`SELECT status FROM danmaku_jobs`).Scan(&status)
	if status != "unknown" {
		t.Fatal(status)
	}
}
