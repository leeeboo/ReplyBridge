package commentbridge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type ModerationKind string

const (
	ModerationDelete ModerationKind = "delete"
	ModerationBan    ModerationKind = "ban"
	moderationTTL                   = 5 * time.Minute
)

type CommentModerator interface {
	Platform() Platform
	Moderate(context.Context, Comment, ModerationKind) (string, error)
}

type HelpSink interface {
	AcceptHelp(context.Context, IncomingReply) (bool, error)
}

const HelpText = `回声桥 · 视频评论助手
• 回复评论：直接引用本机器人发出的评论通知并 @机器人，输入回复内容。
• 点赞：给原通知添加 [THUMBSUP] 表情。B站自动点赞；YouTube 提供手动操作提示，暂不支持自动 [HEART]。
• 删除评论：给原通知添加 [ThumbsDown] 表情。
• 拉黑作者：给原通知添加 [MinusOne] 表情。
• 删除/拉黑不会立即执行：发起人须在 5 分钟内直接引用机器人确认提示并 @机器人，发送“确认”；发送“取消”则放弃。其他人无法代确认。
• B站拉黑加入账号黑名单，不会自动删除原评论；YouTube 拉黑会同时移除该评论并拒绝该作者后续评论。删除主评论可能影响其回复。
• 仅支持本机器人有记录的原通知；历史 Webhook 通知不能操作。结果不确定时不会自动重试，请去平台核对。
• 本群可用；在另一个已配置群中同样可用。@本机器人 输入“帮助”或“help”查看本说明。`

func (s *Store) migrateModeration(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS moderation_requests (
 event_id TEXT PRIMARY KEY, message_id TEXT NOT NULL, chat_id TEXT NOT NULL, user_id TEXT NOT NULL,
 platform TEXT NOT NULL, account_id TEXT NOT NULL, comment_id TEXT NOT NULL, author_id TEXT NOT NULL,
 kind TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'waiting', expires_at INTEGER NOT NULL DEFAULT 0,
 FOREIGN KEY(message_id) REFERENCES feishu_messages(message_id));
CREATE TABLE IF NOT EXISTS bot_command_jobs (
 action_id TEXT PRIMARY KEY, parent_id TEXT NOT NULL, text TEXT NOT NULL,
 request_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'pending', external_id TEXT NOT NULL DEFAULT '',
 error_category TEXT NOT NULL DEFAULT '');
CREATE UNIQUE INDEX IF NOT EXISTS bot_command_message_id ON bot_command_jobs(external_id) WHERE external_id<>'';
CREATE TABLE IF NOT EXISTS moderation_jobs (
 action_id TEXT PRIMARY KEY, request_id TEXT NOT NULL REFERENCES moderation_requests(event_id),
 confirm_message_id TEXT NOT NULL UNIQUE, status TEXT NOT NULL DEFAULT 'pending', external_id TEXT NOT NULL DEFAULT '',
 error_category TEXT NOT NULL DEFAULT '');`)
	return err
}

func moderationKind(emoji string) ModerationKind {
	switch emoji {
	case "ThumbsDown":
		return ModerationDelete
	case "MinusOne":
		return ModerationBan
	}
	return ""
}

func (s *Store) AcceptModeration(ctx context.Context, in IncomingEngagement) (bool, error) {
	kind := moderationKind(in.ReactionType)
	if kind == "" || in.EventID == "" || in.MessageID == "" || in.ExpectedChatID == "" || in.UserID == "" || in.ReceivedAt.IsZero() || time.Since(in.ReceivedAt) > moderationTTL || time.Until(in.ReceivedAt) > time.Minute {
		return false, ErrInvalidEngagement
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	c, err := scanComment(tx.QueryRowContext(ctx, `SELECT c.platform,c.account_id,c.comment_id,c.root_id,c.parent_id,c.resource_id,c.resource_title,c.resource_url,c.author_id,c.author_name,c.text,c.published_at,c.is_charge_plus FROM feishu_messages f JOIN comments c USING(platform,account_id,comment_id) WHERE f.message_id=? AND f.chat_id=?`, in.MessageID, in.ExpectedChatID))
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrUnmappedEngagement
	}
	if err != nil {
		return false, err
	}
	if c.Platform != PlatformBilibili && c.Platform != PlatformYouTube {
		return false, ErrUnsupportedEngagement
	}
	if kind == ModerationBan && (c.AuthorID == "" || c.AuthorID == c.AccountID) {
		return false, ErrInvalidEngagement
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO moderation_requests(event_id,message_id,chat_id,user_id,platform,account_id,comment_id,author_id,kind) VALUES(?,?,?,?,?,?,?,?,?)`, in.EventID, in.MessageID, in.ExpectedChatID, in.UserID, c.Platform, c.AccountID, c.ID, c.AuthorID, kind)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return false, tx.Commit()
	}
	operation := "删除这条评论。删除主评论可能影响其回复。"
	if c.Platform == PlatformYouTube {
		operation = "从频道移除这条评论（拒绝展示）。主评论被移除可能影响其回复。"
	}
	if kind == ModerationBan {
		operation = "将这条评论的作者加入 B站账号黑名单；原评论保留。"
		if c.Platform == PlatformYouTube {
			operation = "拉黑这条评论的作者，同时移除该评论并拒绝该作者后续评论。"
		}
	}
	// Quote the original notification so names and comment text cannot impersonate instructions.
	text := fmt.Sprintf("⚠️ 二次确认：%s\n发起人：<at user_id=\"%s\">发起人</at>\n仅发起人本人可在本提示发出后 5 分钟内，直接引用本提示并 @机器人发送“确认”或“取消”。超时不执行。", operation, in.UserID)
	_, err = tx.ExecContext(ctx, `INSERT INTO bot_command_jobs(action_id,parent_id,text,request_id) VALUES(?,?,?,?)`, actionID("moderation-prompt", in.EventID), in.MessageID, text, in.EventID)
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (b *Bridge) AcceptHelp(ctx context.Context, in IncomingReply) (bool, error) {
	if in.MessageID == "" || in.UserID == "" || !b.acceptsCommandChat(in.ChatID) {
		return false, ErrInvalidReply
	}
	r, err := b.Store.db.ExecContext(ctx, `INSERT OR IGNORE INTO bot_command_jobs(action_id,parent_id,text) VALUES(?,?,?)`, actionID("help", in.MessageID), in.MessageID, HelpText)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}

func (b *Bridge) acceptsCommandChat(chat string) bool {
	return chat != "" && (chat == b.TargetChatID || chat == b.YouTubeTargetChatID)
}

// Every reply to a confirmation prompt is consumed here, even if it cannot authorize an action.
func (s *Store) acceptModerationConfirmation(ctx context.Context, in IncomingReply) (handled, accepted bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback()
	var event, chat, user, platform, account, comment, author, kind, state string
	var expires int64
	err = tx.QueryRowContext(ctx, `SELECT r.event_id,r.chat_id,r.user_id,r.platform,r.account_id,r.comment_id,r.author_id,r.kind,r.state,r.expires_at FROM moderation_requests r JOIN bot_command_jobs m ON m.request_id=r.event_id WHERE m.external_id=? AND m.status='sent'`, in.ParentMessageID).Scan(&event, &chat, &user, &platform, &account, &comment, &author, &kind, &state, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if in.MessageID == "" || in.ChatID != chat || in.UserID != user || state != "waiting" {
		return true, false, nil
	}
	reply := strings.TrimSpace(in.Text)
	if reply != "确认" && reply != "取消" {
		return true, false, nil
	}
	now := time.Now()
	newState, response := "cancelled", "已取消，未执行平台操作。"
	if expires == 0 || now.UnixNano() >= expires || in.ReceivedAt.IsZero() || in.ReceivedAt.After(now.Add(time.Minute)) {
		newState, response = "expired", "确认已过期，未执行。请重新给原通知添加表情发起。"
	} else if reply == "确认" {
		newState = "confirmed"
		target := comment
		if ModerationKind(kind) == ModerationBan {
			target = author
		}
		result, insertErr := tx.ExecContext(ctx, `INSERT OR IGNORE INTO moderation_jobs(action_id,request_id,confirm_message_id) VALUES(?,?,?)`, actionID("moderation", platform, account, target, kind), event, in.MessageID)
		if insertErr != nil {
			return true, false, insertErr
		}
		n, _ := result.RowsAffected()
		accepted = n == 1
		response = "已确认，正在处理；请等待本条消息下的结果。"
		if !accepted {
			response = "此操作已有执行记录，未重复执行；请核对先前结果和平台状态。"
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE moderation_requests SET state=? WHERE event_id=?`, newState, event); err != nil {
		return true, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO bot_command_jobs(action_id,parent_id,text) VALUES(?,?,?)`, actionID("moderation-confirm", event), in.MessageID, response); err != nil {
		return true, false, err
	}
	return true, accepted, tx.Commit()
}

func (b *Bridge) BotCommandOnce(ctx context.Context) (resultErr error) {
	var action, parent, text, request string
	err := b.Store.db.QueryRowContext(ctx, `UPDATE bot_command_jobs SET status='sending' WHERE action_id=(SELECT action_id FROM bot_command_jobs WHERE status='pending' ORDER BY rowid LIMIT 1) AND status='pending' RETURNING action_id,parent_id,text,request_id`).Scan(&action, &parent, &text, &request)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			b.markControlUnknown("bot_command_jobs", action)
		}
	}()
	id := ""
	sendErr := errors.New("missing message replier")
	if b.MessageReplier != nil {
		id, sendErr = b.MessageReplier.ReplyToMessage(ctx, parent, text, action)
	}
	status := JobSent
	if sendErr != nil {
		status = JobFailed
		if IsUnknownOutcome(sendErr) {
			status = JobUnknown
		}
	} else if id == "" {
		status = JobUnknown
	}
	tx, err := b.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE bot_command_jobs SET status=?,external_id=? WHERE action_id=? AND status='sending'`, status, id, action); err != nil {
		return err
	}
	if request != "" && status == JobSent {
		if _, err = tx.ExecContext(ctx, `UPDATE moderation_requests SET expires_at=? WHERE event_id=? AND state='waiting'`, time.Now().Add(moderationTTL).UnixNano(), request); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (b *Bridge) ModerationOnce(ctx context.Context) (resultErr error) {
	var action, event, confirm string
	err := b.Store.db.QueryRowContext(ctx, `UPDATE moderation_jobs SET status='sending' WHERE action_id=(SELECT action_id FROM moderation_jobs WHERE status='pending' ORDER BY rowid LIMIT 1) AND status='pending' RETURNING action_id,request_id,confirm_message_id`).Scan(&action, &event, &confirm)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			b.markControlUnknown("moderation_jobs", action)
		}
	}()
	var kind ModerationKind
	var author, chat string
	err = b.Store.db.QueryRowContext(ctx, `SELECT kind,author_id,chat_id FROM moderation_requests WHERE event_id=?`, event).Scan(&kind, &author, &chat)
	if err != nil {
		return err
	}
	c, err := scanComment(b.Store.db.QueryRowContext(ctx, `SELECT c.platform,c.account_id,c.comment_id,c.root_id,c.parent_id,c.resource_id,c.resource_title,c.resource_url,c.author_id,c.author_name,c.text,c.published_at,c.is_charge_plus FROM comments c JOIN moderation_requests r USING(platform,account_id,comment_id) WHERE r.event_id=?`, event))
	if err != nil {
		return err
	}
	c.AuthorID = author
	id := ""
	callErr := errors.New("missing moderation adapter or inactive chat")
	if moderator := b.Moderators[c.Platform]; moderator != nil && b.acceptsCommandChat(chat) {
		id, callErr = moderator.Moderate(ctx, c, kind)
	}
	status, response := JobSent, "✅ 平台已确认操作成功。"
	if callErr != nil {
		status, response = JobFailed, "❌ 平台操作未成功，未自动重试。请核对权限、登录状态或风控提示。"
	}
	if IsUnknownOutcome(callErr) || (callErr == nil && id == "") {
		status, response = JobUnknown, "⚠️ 平台操作结果不确定，不会自动重试。请前往平台核对。"
	}
	tx, err := b.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE moderation_jobs SET status=?,external_id=? WHERE action_id=? AND status='sending'`, status, id, action); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO bot_command_jobs(action_id,parent_id,text) VALUES(?,?,?)`, actionID("moderation-result", action), confirm, response); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return b.recordCredentialInvalid(ctx, c.Platform, c.AccountID, callErr)
}

func (b *Bridge) markControlUnknown(table, action string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = b.Store.db.ExecContext(ctx, "UPDATE "+table+" SET status='unknown',error_category='persistence' WHERE action_id=? AND status='sending'", action)
}
