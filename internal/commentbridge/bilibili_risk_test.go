package commentbridge

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestBilibiliRiskStopRecoversInterruptedRequestAndAlert(t *testing.T) {
	for _, delivery := range []string{"pending", "sent", "unknown", "sending", "failed"} {
		t.Run(delivery, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bridge.db")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.Close() }()
			calls := 0
			send := func(context.Context, string, string) (string, error) { calls++; return "message", nil }
			stop, err := NewBilibiliRiskStop(s, send)
			if err != nil {
				t.Fatal(err)
			}
			if err = stop.BeginRequest(t.Context()); err != nil {
				t.Fatal(err)
			}
			if delivery != "pending" {
				_, err = s.db.Exec(`UPDATE bilibili_risk_stop SET state='stopped',alert_status=? WHERE id=1`, delivery)
				if err != nil {
					t.Fatal(err)
				}
			}
			s.Close()
			s, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			stop, err = NewBilibiliRiskStop(s, send)
			if err != nil {
				t.Fatal(err)
			}
			if !errors.Is(stop.BeginRequest(t.Context()), ErrBilibiliRiskStopped) {
				t.Fatal("restart reopened interrupted request")
			}
			wantCalls := 0
			if delivery == "pending" {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("calls=%d want=%d", calls, wantCalls)
			}
			status, err := s.BilibiliRiskStatus(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if delivery == "sending" && !strings.Contains(status, "alert=unknown") {
				t.Fatalf("status=%s", status)
			}
		})
	}
}

func TestBilibiliRiskStopNeverSendsWithoutDurableReservation(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	stop, err := NewBilibiliRiskStop(s, func(context.Context, string, string) (string, error) {
		t.Error("unexpected alert without reservation")
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if !errors.Is(stop.BeginRequest(t.Context()), ErrBilibiliRiskStopped) || !stop.Stopped() {
		t.Fatal("storage failure allowed requests")
	}
}

func TestBilibiliRiskNoticeBypassesGenericCooldownAndDoesNotRepeat(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	calls := 0
	send := func(context.Context, string, string) (string, error) { calls++; return "message", nil }
	alerts, err := NewErrorAlerts(s, send)
	if err != nil {
		t.Fatal(err)
	}
	alerts.Observe(t.Context(), "poll_youtube", errors.New("failure"))
	if err = alerts.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	stop, err := NewBilibiliRiskStop(s, send)
	if err != nil {
		t.Fatal(err)
	}
	if err = stop.BeginRequest(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(stop.EndRequest(true, 200, -352), ErrBilibiliRiskStopped) || calls != 2 {
		t.Fatalf("risk notice delayed: calls=%d", calls)
	}
	alerts.Observe(t.Context(), "poll_bilibili", ErrBilibiliRiskStopped)
	var n int
	if err = s.db.QueryRow(`SELECT count(*) FROM error_alerts WHERE category='poll_bilibili'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("stop created recurring generic alert")
	}
	_, err = NewBilibiliRiskStop(s, send)
	if err != nil || calls != 2 {
		t.Fatal("restart repeated alert")
	}
}

func TestBilibiliRiskAlertUnknownIsNotReplayed(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	calls := 0
	send := func(context.Context, string, string) (string, error) {
		calls++
		return "", unknownErr{errors.New("response lost")}
	}
	stop, err := NewBilibiliRiskStop(s, send)
	if err != nil {
		t.Fatal(err)
	}
	if err = stop.BeginRequest(t.Context()); err != nil {
		t.Fatal(err)
	}
	_ = stop.EndRequest(true, 429, 0)
	status, err := s.BilibiliRiskStatus(t.Context())
	if err != nil || !strings.Contains(status, "alert=unknown") {
		t.Fatalf("status=%s err=%v", status, err)
	}
	_, err = NewBilibiliRiskStop(s, send)
	if err != nil || calls != 1 || !stop.Stopped() {
		t.Fatal("unknown alert replayed or stop cleared")
	}
}
