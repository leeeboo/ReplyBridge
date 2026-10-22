package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"comment-bridge/internal/commentbridge"
)

func (c *Client) NotifyDanmaku(ctx context.Context, items []commentbridge.Danmaku, findings []commentbridge.DanmakuFinding, actionID string) (string, error) {
	if actionID == "" || len(findings) == 0 {
		return "", errors.New("danmaku alert is empty")
	}
	if err := commentbridge.ValidateDanmakuReview(items, commentbridge.DanmakuReview{Findings: findings}); err != nil {
		return "", err
	}
	byID := make(map[string]commentbridge.Danmaku, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	// Rich-text text elements cannot turn an untrusted <at> into a mention.
	rows := [][]map[string]string{{{"tag": "text", "text": fmt.Sprintf("本批检查 %d 条弹幕，发现 %d 条需人工关注。AI 判断仅供复核。", len(items), len(findings))}}}
	for _, f := range findings[:min(len(findings), 10)] {
		item := byID[f.ID]
		u, err := url.Parse(item.VideoURL)
		if err != nil || u.Scheme != "https" || u.Host != "www.bilibili.com" || u.User != nil {
			return "", errors.New("invalid danmaku video URL")
		}
		rows = append(rows,
			[]map[string]string{{"tag": "text", "text": "\n" + commentbridge.DanmakuCategoryLabel(f.Category) + " · " + clipDanmakuText(item.VideoTitle, 120)}},
			[]map[string]string{{"tag": "text", "text": "弹幕：" + clipDanmakuText(item.Text, 240)}},
			[]map[string]string{{"tag": "text", "text": "原因：" + clipDanmakuText(f.Reason, 300)}},
			[]map[string]string{{"tag": "a", "text": fmt.Sprintf("打开视频 · %d:%02d · 弹幕 %s", item.PositionMS/60000, item.PositionMS/1000%60, item.ID), "href": item.VideoURL}},
		)
	}
	if len(findings) > 10 {
		rows = append(rows, []map[string]string{{"tag": "text", "text": fmt.Sprintf("另有 %d 条可疑弹幕；完整结果保存在本机，请到创作后台复核。", len(findings)-10)}})
	}
	content, err := json.Marshal(map[string]any{"zh_cn": map[string]any{"title": "⚠️ 弹幕异常提醒", "content": rows}})
	if err != nil {
		return "", errors.New("encode danmaku alert")
	}
	id, definite, err := c.sender.Send(ctx, outgoingMessage{ChatID: c.config.ChatID, MsgType: "post", Content: string(content), UUID: feishuUUID(actionID)})
	if err != nil && !definite {
		return "", unknownOutcome{cause: errors.New("danmaku alert delivery outcome is unknown")}
	}
	if err != nil {
		return "", errors.New("danmaku alert rejected")
	}
	if strings.TrimSpace(id) == "" {
		return "", unknownOutcome{cause: errors.New("danmaku alert has no message ID")}
	}
	return id, nil
}

func clipDanmakuText(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) > maxRunes {
		return string(r[:maxRunes]) + "…"
	}
	return s
}
