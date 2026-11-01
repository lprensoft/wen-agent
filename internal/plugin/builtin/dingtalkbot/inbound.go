package dingtalkbot

import (
	"context"
	"encoding/json"
	"strings"

	"wen/internal/i18n"
	"wen/internal/imbot"
)

// botMessage 是机器人回调 data 里分发需要的字段。文本在 text.content，
// 语音转写文本在 content.recognition，富文本在 content.richText。
type botMessage struct {
	ConversationID   string `json:"conversationId"`
	ConversationType string `json:"conversationType"`
	MsgID            string `json:"msgId"`
	SenderStaffID    string `json:"senderStaffId"`
	MsgType          string `json:"msgtype"`
	Text             struct {
		Content string `json:"content"`
	} `json:"text"`
	Content json.RawMessage `json:"content"`
}

// handleCallback 把一条机器人消息归一化成 imbot.Message。用户标识用
// senderStaffId（就是批量发送接口要的 userId），去重用 Stream 帧的 messageId。
func (p *Plugin) handleCallback(ctx context.Context, job callbackJob) {
	var msg botMessage
	if err := json.Unmarshal([]byte(job.raw), &msg); err != nil {
		i18n.Logf(p.langOf(), "log.dingtalk.callback_parse_failed", "dingtalk_bot: 无法解析消息回调（已跳过）: %v", err)
		return
	}
	if msg.ConversationType != "1" {
		i18n.Logf(p.langOf(), "log.dingtalk.group_ignored",
			"dingtalk_bot: 忽略非单聊消息（conversationType=%s，仅支持单聊）", msg.ConversationType)
		return
	}
	userID := strings.TrimSpace(msg.SenderStaffID)
	if userID == "" {
		// 机器人发布上线后才会返回 senderStaffId；没有它就既不能匹配白名单也不能发回去
		i18n.Logf(p.langOf(), "log.dingtalk.no_staff_id",
			"dingtalk_bot: 回调缺少 senderStaffId（机器人可能尚未发布上线），已忽略")
		return
	}

	text, unsupported := extractText(p.langOf(), msg)
	note := ""
	if unsupported {
		note = i18n.T(p.langOf(), "plugin.dingtalk_bot.msg.unsupported", "暂不支持该消息类型，请发文字或语音。")
	}

	dedup := job.dedup
	if dedup == "" {
		dedup = msg.MsgID
	}

	p.mu.Lock()
	core := p.core
	p.mu.Unlock()
	if core == nil {
		return
	}
	core.Handle(ctx, imbot.Message{
		UserID:  userID,
		DedupID: dedup,
		Text:    text,
		Note:    note,
	})
}

// extractText 抽取可读文本。语音用钉钉自带的 recognition 转写；富文本把文字项
// 拼起来，图片等非文字项标记为不支持。
func extractText(lang string, msg botMessage) (string, bool) {
	switch msg.MsgType {
	case "text":
		return strings.TrimSpace(msg.Text.Content), false
	case "audio":
		var c struct {
			Recognition string `json:"recognition"`
		}
		if json.Unmarshal(msg.Content, &c) == nil {
			if s := strings.TrimSpace(c.Recognition); s != "" {
				return i18n.T(lang, "plugin.dingtalk_bot.voice_prefix", "【语音】") + s, false
			}
		}
		return "", true
	case "richText":
		var c struct {
			RichText []struct {
				Text string `json:"text"`
			} `json:"richText"`
		}
		if json.Unmarshal(msg.Content, &c) != nil {
			return "", true
		}
		var parts []string
		unsupported := false
		for _, item := range c.RichText {
			if s := strings.TrimSpace(item.Text); s != "" {
				parts = append(parts, s)
			} else {
				unsupported = true
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n")), unsupported
	default:
		return "", true
	}
}
