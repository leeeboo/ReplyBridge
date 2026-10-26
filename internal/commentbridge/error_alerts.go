package commentbridge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"
)

// ErrorAlerts persists incident deduplication separately from platform writes.
// Alert delivery is reserved before sending, including unknown HTTP outcomes.
type ErrorAlerts struct {
	store       *Store
	send        func(context.Context, string, string) (string, error)
	now         func() time.Time
	storageLast atomic.Int64
}

var errorLabels = map[string]string{
	"poll_bilibili": "🟦 B 站评论抓取", "poll_youtube": "📺 YouTube 评论抓取",
	"poll": "评论抓取或存储", "notification": "飞书评论通知", "reply": "平台回复",
	"reaction": "飞书状态表情", "engagement": "评论点赞", "engagement_feedback": "点赞结果表情",
	"manual_engagement_feedback": "手动点赞提示", "credential_alert": "登录失效提醒",
	"credential_refresh": "二维码登录", "connection": "飞书长连接", "event": "飞书消息处理",
	"startup": "服务启动或运行", "storage": "告警状态存储", "bilibili_thread": "B 站失效评论已跳过",
	"moderation": "删除评论或拉黑", "bot_command": "机器人帮助与确认提示",
	"danmaku_poll": "弹幕抓取", "danmaku": "弹幕 AI 分析或提醒",
}

func NewErrorAlerts(s *Store, send func(context.Context, string, string) (string, error)) (*ErrorAlerts, error) {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS error_alerts (
	 category TEXT PRIMARY KEY, active INTEGER NOT NULL DEFAULT 0,
	 pending INTEGER NOT NULL DEFAULT 0, last_attempt INTEGER NOT NULL DEFAULT 0,
	 last_message_id TEXT NOT NULL DEFAULT '');
	 CREATE TABLE IF NOT EXISTS error_alert_seen_jobs (
	 kind TEXT NOT NULL, action_id TEXT NOT NULL, status TEXT NOT NULL, PRIMARY KEY(kind,action_id,status));`)
	if err != nil {
		return nil, err
	}
	return &ErrorAlerts{store: s, send: send, now: time.Now}, nil
}

// Observe receives fixed categories only. Upstream error text never enters an alert.
func (a *ErrorAlerts) Observe(ctx context.Context, category string, issue error) {
	if a == nil || errorLabels[category] == "" || errors.Is(issue, context.Canceled) || errors.Is(issue, ErrBilibiliRiskStopped) {
		return
	}
	var err error
	if issue == nil {
		_, err = a.store.db.ExecContext(ctx, `UPDATE error_alerts SET active=0 WHERE category=? AND active=1`, category)
	} else {
		_, err = a.store.db.ExecContext(ctx, `INSERT INTO error_alerts(category,active,pending) VALUES(?,1,1)
		 ON CONFLICT(category) DO UPDATE SET active=1,pending=CASE WHEN active=0 THEN 1 ELSE pending END`, category)
	}
	if err != nil {
		log.Print("comment bridge error alert state failed")
		a.storageFailure(ctx)
	}
}

// If SQLite itself fails, durable deduplication is unavailable. Still attempt
// one fixed warning, bounded to once per six hours in this process.
func (a *ErrorAlerts) storageFailure(ctx context.Context) {
	now := a.now().Unix()
	last := a.storageLast.Load()
	if last != 0 && now-last < 21600 {
		return
	}
	if !a.storageLast.CompareAndSwap(last, now) {
		return
	}
	_, _ = a.send(ctx, "⚠️ 视频评论助手存储异常，评论处理或告警记录可能受影响，请检查服务器。本次存储故障提醒已限频。", actionID("error-alert-storage", fmt.Sprint(now)))
}

// Failed workers often return nil after persisting failed/unknown. Observe those
// durable outcomes too, once per action and status, without replaying the action.
func (a *ErrorAlerts) scanJobs(ctx context.Context) error {
	kinds := []string{"notification", "reply", "reaction", "engagement", "engagement_feedback", "manual_engagement_feedback", "credential_alert", "credential_refresh", "moderation", "bot_command"}
	if exists, err := a.store.tableExists(ctx, "danmaku_jobs"); err != nil {
		return err
	} else if exists {
		kinds = append(kinds, "danmaku")
	}
	for _, kind := range kinds {
		tx, err := a.store.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO error_alert_seen_jobs(kind,action_id,status)
		 SELECT ?,action_id,status FROM `+kind+`_jobs WHERE status IN ('failed','unknown')`, kind)
		if err == nil {
			n, _ := res.RowsAffected()
			if n > 0 {
				_, err = tx.ExecContext(ctx, `INSERT INTO error_alerts(category,active,pending) VALUES(?,1,1)
			 ON CONFLICT(category) DO UPDATE SET pending=1`, kind)
			}
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Flush sends a combined message: at most once per 10 minutes globally and once
// per 6 hours per category. A continuous incident is reported only once; later
// new incidents/failed jobs are retained until the cooldown ends.
func (a *ErrorAlerts) Flush(ctx context.Context) error {
	if err := a.scanJobs(ctx); err != nil {
		a.storageFailure(ctx)
		return err
	}
	now := a.now().Unix()
	tx, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var last int64
	err = tx.QueryRowContext(ctx, `SELECT last_attempt FROM error_alerts WHERE category='_delivery'`).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if last != 0 && now-last < 600 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT category FROM error_alerts WHERE pending=1 AND (last_attempt=0 OR last_attempt<=?) ORDER BY category`, now-21600)
	if err != nil {
		return err
	}
	var categories, lines []string
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			rows.Close()
			return err
		}
		if label := errorLabels[key]; label != "" {
			categories = append(categories, key)
			lines = append(lines, "• "+label)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(categories) == 0 {
		return err
	}
	for _, key := range categories {
		if _, err = tx.ExecContext(ctx, `UPDATE error_alerts SET pending=0,last_attempt=? WHERE category=?`, now, key); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO error_alerts(category,last_attempt) VALUES('_delivery',?)
	 ON CONFLICT(category) DO UPDATE SET last_attempt=excluded.last_attempt`, now); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	text := "⚠️ 视频评论助手异常提醒\n" + strings.Join(lines, "\n") + "\n发现运行异常或尚未报告的失败/结果不确定任务，请检查服务。\n同类持续错误不会重复提醒；恢复后再发生或出现新失败任务时，按限频合并通知。"
	messageID, sendErr := a.send(ctx, text, actionID("error-alert", fmt.Sprint(now), strings.Join(categories, ",")))
	err = sendErr
	if err == nil {
		_, err = a.store.db.ExecContext(ctx, `UPDATE error_alerts SET last_message_id=? WHERE category='_delivery'`, messageID)
	}
	if err != nil {
		// Alert failures never recursively create another alert. Retry the notice
		// after the six-hour limit, not a platform reply/like with unknown outcome.
		for _, key := range categories {
			_, _ = a.store.db.ExecContext(ctx, `UPDATE error_alerts SET pending=1 WHERE category=?`, key)
		}
	}
	return err
}
