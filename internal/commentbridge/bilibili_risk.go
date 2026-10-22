package commentbridge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"time"
)

var ErrBilibiliRiskStopped = errors.New("bilibili requests stopped; manual review required")

// BilibiliRiskStop has no automatic reset. The transport serializes Begin/End.
type BilibiliRiskStop struct {
	store   *Store
	send    func(context.Context, string, string) (string, error)
	stopped atomic.Bool
}

func NewBilibiliRiskStop(s *Store, send func(context.Context, string, string) (string, error)) (*BilibiliRiskStop, error) {
	if s == nil || send == nil {
		return nil, errors.New("bilibili risk stop requires storage and notifier")
	}
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS bilibili_risk_stop (
	 id INTEGER PRIMARY KEY CHECK(id=1), state TEXT NOT NULL, http_status INTEGER NOT NULL DEFAULT 0,
	 api_code INTEGER NOT NULL DEFAULT 0, detected_at INTEGER NOT NULL DEFAULT 0,
	 alert_status TEXT NOT NULL DEFAULT '', message_id TEXT NOT NULL DEFAULT '');
	 INSERT OR IGNORE INTO bilibili_risk_stop(id,state) VALUES(1,'ready');
	 UPDATE bilibili_risk_stop SET state='stopped',detected_at=unixepoch(),alert_status='pending' WHERE state='inflight';
	 UPDATE bilibili_risk_stop SET alert_status='unknown' WHERE alert_status='sending';`)
	if err != nil {
		return nil, err
	}
	a := &BilibiliRiskStop{store: s, send: send}
	var state string
	if err = s.db.QueryRow(`SELECT state FROM bilibili_risk_stop WHERE id=1`).Scan(&state); err != nil {
		return nil, err
	}
	a.stopped.Store(state != "ready")
	if a.Stopped() {
		a.notify()
	}
	return a, nil
}

func (a *BilibiliRiskStop) Stopped() bool { return a != nil && a.stopped.Load() }

func (a *BilibiliRiskStop) BeginRequest(ctx context.Context) error {
	if a.Stopped() {
		return ErrBilibiliRiskStopped
	}
	// Persist before network I/O: a crash or failed state write cannot reopen requests on restart.
	res, err := a.store.db.ExecContext(ctx, `UPDATE bilibili_risk_stop SET state='inflight' WHERE id=1 AND state='ready'`)
	if err == nil {
		if n, e := res.RowsAffected(); e == nil && n == 1 {
			return nil
		}
	}
	a.stopped.Store(true)
	log.Print("bilibili requests stopped: risk state could not be reserved")
	return ErrBilibiliRiskStopped
}

func (a *BilibiliRiskStop) EndRequest(risk bool, httpStatus, apiCode int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if risk {
		a.stopped.Store(true)
	}
	var err error
	if risk {
		_, err = a.store.db.ExecContext(ctx, `UPDATE bilibili_risk_stop SET state='stopped',http_status=?,api_code=?,detected_at=unixepoch(),alert_status='pending' WHERE id=1`, httpStatus, apiCode)
	} else {
		_, err = a.store.db.ExecContext(ctx, `UPDATE bilibili_risk_stop SET state='ready' WHERE id=1 AND state='inflight'`)
	}
	if err != nil {
		a.stopped.Store(true)
		log.Print("bilibili requests stopped: risk state could not be saved")
		return ErrBilibiliRiskStopped
	}
	if risk {
		log.Printf("bilibili requests permanently stopped: http_status=%d api_code=%d", httpStatus, apiCode)
		a.notify()
		return ErrBilibiliRiskStopped
	}
	return nil
}

func (a *BilibiliRiskStop) notify() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := a.store.db.ExecContext(ctx, `UPDATE bilibili_risk_stop SET alert_status='sending' WHERE id=1 AND state='stopped' AND alert_status='pending'`)
	if err != nil {
		log.Print("bilibili risk alert reservation failed")
		return
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return
	}
	var status, code int
	var detected int64
	if err = a.store.db.QueryRowContext(ctx, `SELECT http_status,api_code,detected_at FROM bilibili_risk_stop WHERE id=1`).Scan(&status, &code, &detected); err != nil {
		log.Print("bilibili risk alert state read failed")
		return
	}
	reason := fmt.Sprintf("检测到风控/限流或账号限制（HTTP %d，API %d）", status, code)
	if status == 0 && code == 0 {
		reason = "上次 B 站请求中断，无法确认是否遇到风控"
	}
	messageID, sendErr := a.send(ctx, "🛑 B 站请求已全部停止\n"+reason+"。\n评论、弹幕、回复、点赞及二维码登录均已停用；到期、重启或更换凭据均不会自动恢复。请人工检查并明确解除停用后再恢复。YouTube 与飞书服务继续运行。", actionID("bilibili-risk-stop", fmt.Sprint(detected)))
	result := "sent"
	if sendErr != nil || messageID == "" {
		result = "failed"
		if IsUnknownOutcome(sendErr) || sendErr == nil {
			result = "unknown"
		}
		log.Print("bilibili risk alert delivery failed or unknown; requests remain stopped")
	}
	resultCtx, resultCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer resultCancel()
	if _, err = a.store.db.ExecContext(resultCtx, `UPDATE bilibili_risk_stop SET alert_status=?,message_id=? WHERE id=1`, result, messageID); err != nil {
		log.Print("bilibili risk alert result could not be saved")
	}
}

func (s *Store) BilibiliRiskStatus(ctx context.Context) (string, error) {
	exists, err := s.tableExists(ctx, "bilibili_risk_stop")
	if err != nil || !exists {
		return "", err
	}
	var state, alert string
	var status, code int
	err = s.db.QueryRowContext(ctx, `SELECT state,alert_status,http_status,api_code FROM bilibili_risk_stop WHERE id=1`).Scan(&state, &alert, &status, &code)
	return fmt.Sprintf("bilibili-risk state=%s alert=%s http=%d api=%d", state, alert, status, code), err
}
