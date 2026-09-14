package commentbridge

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestErrorAlertsPersistDedupAndRateLimits(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bridge.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	now := time.Unix(1800000000, 0)
	var messages []string
	send := func(_ context.Context, text, id string) (string, error) {
		if id == "" || strings.Contains(text, "SECRET") {
			t.Fatal("unsafe alert")
		}
		messages = append(messages, text)
		return "message", nil
	}
	newAlerts := func() *ErrorAlerts {
		a, e := NewErrorAlerts(s, send)
		if e != nil {
			t.Fatal(e)
		}
		a.now = func() time.Time { return now }
		return a
	}
	a := newAlerts()
	issue := errors.New("SECRET raw provider response")
	flush := func() {
		if e := a.Flush(ctx); e != nil {
			t.Fatal(e)
		}
	}
	a.Observe(ctx, "poll_bilibili", issue)
	flush()
	for i := 0; i < 100; i++ {
		a.Observe(ctx, "poll_bilibili", issue)
	}
	flush()
	if len(messages) != 1 {
		t.Fatal("repeated incident spammed")
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a = newAlerts()
	now = now.Add(24 * time.Hour)
	a.Observe(ctx, "poll_bilibili", issue)
	flush()
	if len(messages) != 1 {
		t.Fatal("restart or elapsed time repeated active incident")
	}
	a.Observe(ctx, "poll_bilibili", nil)
	a.Observe(ctx, "poll_bilibili", issue)
	flush()
	if len(messages) != 2 {
		t.Fatal("new incident lost")
	}
	a.Observe(ctx, "poll_youtube", issue)
	flush()
	if len(messages) != 2 {
		t.Fatal("global cooldown violated")
	}
	now = now.Add(10 * time.Minute)
	flush()
	if len(messages) != 3 || !strings.Contains(messages[2], "YouTube") {
		t.Fatal("pending category lost")
	}
	a.Observe(ctx, "poll_bilibili", nil)
	a.Observe(ctx, "poll_bilibili", issue)
	now = now.Add(10 * time.Minute)
	flush()
	if len(messages) != 3 {
		t.Fatal("flapping bypassed category cooldown")
	}
	now = now.Add(6 * time.Hour)
	flush()
	if len(messages) != 4 {
		t.Fatal("new incident not retained")
	}
}

func TestErrorAlertsCatchDurableFailuresAndDoNotRecurse(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Unix(1800000000, 0)
	calls := 0
	reject := true
	a, err := NewErrorAlerts(s, func(_ context.Context, text, id string) (string, error) {
		calls++
		if !strings.Contains(text, "登录失效提醒") {
			t.Fatal("failed task not reported")
		}
		if reject {
			return "", errors.New("send outcome unknown")
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a.now = func() time.Time { return now }
	_, err = s.db.Exec(`INSERT INTO credential_alert_jobs(action_id,platform,account_id,kind,incident_seq,status) VALUES('a','bilibili','account','credential_invalid',1,'failed')`)
	if err != nil {
		t.Fatal(err)
	}
	if a.Flush(ctx) == nil {
		t.Fatal("send failure lost")
	}
	for i := 0; i < 10; i++ {
		if err = a.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatal("alert recursively retried")
	}
	now = now.Add(6 * time.Hour)
	reject = false
	if err = a.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("notice did not retry after long cooldown")
	}
	now = now.Add(24 * time.Hour)
	a.Flush(ctx)
	if calls != 2 {
		t.Fatal("same persisted job alerted twice")
	}
}

func TestErrorAlertsStorageFailureHasBoundedFallback(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	a, err := NewErrorAlerts(s, func(_ context.Context, text, id string) (string, error) { calls++; return "", errors.New("offline") })
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	for i := 0; i < 10; i++ {
		a.Observe(context.Background(), "storage", errors.New("db failed"))
		a.Flush(context.Background())
	}
	if calls != 1 {
		t.Fatalf("fallback spammed: %d", calls)
	}
}
