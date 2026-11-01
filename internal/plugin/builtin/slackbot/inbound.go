package slackbot

import (
	"context"
	"encoding/json"
	"strings"

	"wen/internal/i18n"
	"wen/internal/imbot"
)

// eventPayload 是 Socket Mode 里 events_api 的 payload（即标准 Events API 回调体）。
type eventPayload struct {
	Type    string `json:"type"`
	EventID string `json:"event_id"`
	Event   struct {
		Type        string            `json:"type"`
		Channel     string            `json:"channel"`
		ChannelType string            `json:"channel_type"`
		User        string            `json:"user"`
		Text        string            `json:"text"`
		TS          string            `json:"ts"`
		Subtype     string            `json:"subtype"`
		BotID       string            `json:"bot_id"`
		Files       []json.RawMessage `json:"files"`
	} `json:"event"`
}

// handleEvent 把一条 message.im 事件归一化成 imbot.Message。用户标识用 DM
// channel ID（后续回复与推送都发给它），去重用 event_id。
func (p *Plugin) handleEvent(ctx context.Context, raw json.RawMessage) {
	var payload eventPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		i18n.Logf(p.langOf(), "log.slack.event_parse_failed", "slack_bot: 无法解析事件（已跳过）: %v", err)
		return
	}
	if payload.Type != "event_callback" || payload.Event.Type != "message" {
		return
	}
	if payload.Event.ChannelType != "im" {
		i18n.Logf(p.langOf(), "log.slack.non_im_ignored",
			"slack_bot: 忽略非私聊消息（channel_type=%s，仅支持私聊）", payload.Event.ChannelType)
		return
	}
	if payload.Event.BotID != "" || payload.Event.User == "" {
		return // 机器人自己的消息或没有发送者
	}
	channelID := strings.TrimSpace(payload.Event.Channel)
	if channelID == "" {
		return
	}

	// 编辑、删除、系统提示等不是一条新的用户发言；文件分享只取文字部分。
	unsupported := false
	switch payload.Event.Subtype {
	case "":
	case "file_share":
		unsupported = len(payload.Event.Files) > 0
	default:
		return
	}
	text := strings.TrimSpace(payload.Event.Text)
	if len(payload.Event.Files) > 0 && text == "" {
		unsupported = true
	}

	note := ""
	if unsupported {
		note = i18n.T(p.langOf(), "plugin.slack_bot.msg.unsupported", "暂不支持该消息类型，请发文字。")
	}

	dedup := payload.EventID
	if dedup == "" {
		dedup = channelID + ":" + payload.Event.TS
	}

	p.mu.Lock()
	core := p.core
	p.mu.Unlock()
	if core == nil {
		return
	}
	core.Handle(ctx, imbot.Message{
		UserID:  channelID,
		DedupID: dedup,
		Text:    text,
		Note:    note,
	})
}
