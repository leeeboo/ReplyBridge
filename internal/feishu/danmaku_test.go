package feishu

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"comment-bridge/internal/commentbridge"
)

func TestDanmakuAlertHasNoMentionsAndKeepsUncertainDelivery(t *testing.T) {
	fake := &fakeSender{messageID: "dm-alert"}
	c := &Client{config: Config{ChatID: "target"}, sender: fake}
	items := []commentbridge.Danmaku{{ID: "1", Text: `<at user_id="all">所有人</at>`, VideoTitle: "视频", VideoURL: "https://www.bilibili.com/video/av1?t=1", PositionMS: 1000}}
	findings := []commentbridge.DanmakuFinding{{ID: "1", Category: "provocation", Reason: "疑似引战"}}
	id, e := c.NotifyDanmaku(t.Context(), items, findings, "action")
	if e != nil || id != "dm-alert" {
		t.Fatal(id, e)
	}
	if fake.message.ChatID != "target" || fake.message.MsgType != "post" || fake.message.UUID != feishuUUID("action") {
		t.Fatal(fake.message)
	}
	var content struct {
		ZH struct {
			Content [][]map[string]string `json:"content"`
		} `json:"zh_cn"`
	}
	if e := json.Unmarshal([]byte(fake.message.Content), &content); e != nil {
		t.Fatal(e)
	}
	for _, row := range content.ZH.Content {
		for _, element := range row {
			if element["tag"] == "at" {
				t.Fatal("unwanted mention")
			}
		}
	}
	if !strings.Contains(fake.message.Content, "疑似引战") {
		t.Fatal("missing explanation")
	}
	fake.err = errors.New("timeout")
	if _, e := c.NotifyDanmaku(t.Context(), items, findings, "action"); !commentbridge.IsUnknownOutcome(e) {
		t.Fatal("uncertain notification was retryable")
	}
}
