package commentbridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const DanmakuInterval = 10 * time.Minute
const DanmakuLimit = 100

type Danmaku struct {
	ID         string `json:"id"`
	VideoTitle string `json:"video_title"`
	VideoURL   string `json:"video_url"`
	Text       string `json:"text"`
	PositionMS int64  `json:"position_ms"`
	CreatedAt  int64  `json:"created_at"`
}

type DanmakuFinding struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Reason   string `json:"reason"`
}

type DanmakuReview struct {
	Findings []DanmakuFinding `json:"findings"`
	Failed   bool             `json:"failed,omitzero"`
}

// The queue crosses the Docker/host boundary without sharing Codex credentials.
type DanmakuReviewQueue interface {
	Submit(string, []Danmaku) error
	Result(string) (DanmakuReview, bool, error)
	Remove(string) error
}

type DanmakuMonitor struct {
	store   *Store
	account string
	read    func(context.Context) ([]Danmaku, error)
	queue   DanmakuReviewQueue
	send    func(context.Context, []Danmaku, []DanmakuFinding, string) (string, error)
	now     func() time.Time
}

func NewDanmakuMonitor(s *Store, account string, read func(context.Context) ([]Danmaku, error), queue DanmakuReviewQueue, send func(context.Context, []Danmaku, []DanmakuFinding, string) (string, error)) (*DanmakuMonitor, error) {
	if s == nil || strings.TrimSpace(account) == "" || read == nil || queue == nil || send == nil {
		return nil, errors.New("danmaku monitor configuration is incomplete")
	}
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS danmaku_sources (
	 account_id TEXT PRIMARY KEY, initialized INTEGER NOT NULL DEFAULT 0,
	 next_poll_at INTEGER NOT NULL DEFAULT 0);
	 CREATE TABLE IF NOT EXISTS danmaku_seen (
	 account_id TEXT NOT NULL, danmaku_id TEXT NOT NULL, PRIMARY KEY(account_id,danmaku_id));
	 CREATE TABLE IF NOT EXISTS danmaku_jobs (
	 action_id TEXT PRIMARY KEY, account_id TEXT NOT NULL, items TEXT NOT NULL,
	 status TEXT NOT NULL, created_at INTEGER NOT NULL, findings TEXT NOT NULL DEFAULT '[]',
	 external_id TEXT NOT NULL DEFAULT '');
	 CREATE TABLE IF NOT EXISTS danmaku_alert_keys (
	 account_id TEXT NOT NULL, video_url TEXT NOT NULL, position_ms INTEGER NOT NULL,
	 text TEXT NOT NULL, action_id TEXT NOT NULL,
	 PRIMARY KEY(account_id,video_url,position_ms,text));
	 UPDATE danmaku_jobs SET status='unknown' WHERE status='sending';`)
	if err != nil {
		return nil, err
	}
	return &DanmakuMonitor{store: s, account: account, read: read, queue: queue, send: send, now: time.Now}, nil
}

// PollOnce reserves the next slot before the request so restarts cannot burst.
// The first successful read is only a baseline, including an empty inbox.
func (m *DanmakuMonitor) PollOnce(ctx context.Context) error {
	now := m.now()
	if _, err := m.store.db.ExecContext(ctx, `INSERT OR IGNORE INTO danmaku_sources(account_id) VALUES(?)`, m.account); err != nil {
		return err
	}
	res, err := m.store.db.ExecContext(ctx, `UPDATE danmaku_sources SET next_poll_at=? WHERE account_id=? AND next_poll_at<=?`, now.Add(DanmakuInterval).Unix(), m.account, now.Unix())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	items, err := m.read(ctx)
	if err != nil {
		if IsCredentialInvalid(err) {
			_, recordErr := m.store.RecordCredentialInvalid(ctx, PlatformBilibili, m.account, CredentialInvalidReasonOf(err))
			return errors.Join(err, recordErr)
		}
		return err
	}
	if len(items) > DanmakuLimit {
		return errors.New("danmaku source exceeded item limit")
	}
	tx, err := m.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var initialized bool
	if err := tx.QueryRowContext(ctx, `SELECT initialized FROM danmaku_sources WHERE account_id=?`, m.account).Scan(&initialized); err != nil {
		return err
	}
	var fresh []Danmaku
	batchIDs := make(map[string]bool)
	for _, item := range items {
		if item.ID == "" || item.Text == "" || utf8.RuneCountInString(item.Text) > 2000 {
			return errors.New("invalid danmaku item")
		}
		if batchIDs[item.ID] {
			continue
		}
		batchIDs[item.ID] = true
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO danmaku_seen(account_id,danmaku_id) VALUES(?,?)`, m.account, item.ID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
		if initialized {
			fresh = append(fresh, item)
		}
	}
	if len(fresh) != 0 {
		body, err := json.Marshal(fresh)
		if err != nil {
			return err
		}
		id := actionID("danmaku", m.account, fresh[0].ID)
		if _, err := tx.ExecContext(ctx, `INSERT INTO danmaku_jobs(action_id,account_id,items,status,created_at) VALUES(?,?,?,'pending',?)`, id, m.account, string(body), now.Unix()); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE danmaku_sources SET initialized=1 WHERE account_id=?`, m.account); err != nil {
		return err
	}
	return tx.Commit()
}

// WorkOnce never waits for inference. Only the host worker invokes Codex.
func (m *DanmakuMonitor) WorkOnce(ctx context.Context) error {
	var id, body string
	var created int64
	err := m.store.db.QueryRowContext(ctx, `SELECT action_id,items,created_at FROM danmaku_jobs WHERE account_id=? AND status='pending' ORDER BY created_at,action_id LIMIT 1`, m.account).Scan(&id, &body, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var items []Danmaku
	if json.Unmarshal([]byte(body), &items) != nil {
		return errors.New("invalid stored danmaku batch")
	}
	if err := m.queue.Submit(id, items); err != nil {
		return err
	}
	review, ready, err := m.queue.Result(id)
	if err != nil {
		return err
	}
	if !ready {
		if m.now().Unix()-created > int64((30*time.Minute)/time.Second) {
			return errors.New("danmaku review worker is not responding")
		}
		return nil
	}
	if err := ValidateDanmakuReview(items, review); err != nil {
		_, markErr := m.store.db.ExecContext(ctx, `UPDATE danmaku_jobs SET status='failed' WHERE action_id=? AND status='pending'`, id)
		if markErr != nil {
			return errors.Join(err, markErr)
		}
		return errors.Join(err, m.queue.Remove(id))
	}
	if len(review.Findings) == 0 {
		if _, err := m.store.db.ExecContext(ctx, `UPDATE danmaku_jobs SET status='succeeded' WHERE action_id=? AND status='pending'`, id); err != nil {
			return err
		}
		return m.queue.Remove(id)
	}
	tx, err := m.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	byID := make(map[string]Danmaku, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	merged := make([]DanmakuFinding, 0, len(review.Findings))
	for _, finding := range review.Findings {
		item := byID[finding.ID]
		// Claim with the sending transition so restarts and later batches cannot repeat an alert.
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO danmaku_alert_keys(account_id,video_url,position_ms,text,action_id) VALUES(?,?,?,?,?)`, m.account, item.VideoURL, item.PositionMS, item.Text, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 0 {
			merged = append(merged, finding)
		}
	}
	review.Findings = merged
	status := "sending"
	if len(merged) == 0 {
		status = "succeeded"
	}
	findings, _ := json.Marshal(merged)
	res, err := tx.ExecContext(ctx, `UPDATE danmaku_jobs SET status=?,findings=? WHERE action_id=? AND status='pending'`, status, string(findings), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(merged) == 0 {
		return m.queue.Remove(id)
	}
	externalID, sendErr := m.send(ctx, items, review.Findings, id)
	status = "succeeded"
	if sendErr != nil {
		status = "failed"
		if IsUnknownOutcome(sendErr) {
			status = "unknown"
		}
	} else if externalID == "" {
		status = "unknown"
		sendErr = errors.New("danmaku alert has no delivery receipt")
	}
	_, err = m.store.db.ExecContext(ctx, `UPDATE danmaku_jobs SET status=?,external_id=? WHERE action_id=? AND status='sending'`, status, externalID, id)
	if err != nil {
		return errors.Join(sendErr, err)
	}
	return errors.Join(sendErr, m.queue.Remove(id))
}

func ValidateDanmakuReview(items []Danmaku, review DanmakuReview) error {
	if review.Failed {
		return errors.New("danmaku model review failed")
	}
	ids := make(map[string]bool, len(items))
	for _, item := range items {
		ids[item.ID] = true
	}
	for _, f := range review.Findings {
		if !ids[f.ID] || strings.TrimSpace(f.Reason) == "" || utf8.RuneCountInString(f.Reason) > 300 {
			return errors.New("invalid danmaku finding")
		}
		delete(ids, f.ID)
		if f.Category != "video_problem" && f.Category != "provocation" && f.Category != "malicious" {
			return errors.New("invalid danmaku finding category")
		}
	}
	return nil
}

func DanmakuCategoryLabel(category string) string {
	switch category {
	case "video_problem":
		return "视频问题反馈"
	case "provocation":
		return "疑似引战"
	case "malicious":
		return "疑似恶意信息"
	default:
		return fmt.Sprint(category)
	}
}
