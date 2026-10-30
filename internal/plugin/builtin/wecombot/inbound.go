package wecombot

import (
	"context"
	"encoding/json"
	"strings"

	"wen/internal/i18n"
	"wen/internal/imbot"
)

// callbackBody 只取分发需要的字段。各消息类型的正文结构见官方「接收消息」文档：
// 文本在 text.content，语音转写文本在 voice.content，图文混排在 mixed.msg_item。
type callbackBody struct {
	MsgID    string `json:"msgid"`
	ChatType string `json:"chattype"`
	MsgType  string `json:"msgtype"`
	From     struct {
		UserID string `json:"userid"`
	} `json:"from"`
	Text struct {
		Content string `json:"content"`
	} `json:"text"`
	Voice struct {
		Content string `json:"content"`
	} `json:"voice"`
	Mixed struct {
		MsgItem []struct {
			MsgType string `json:"msgtype"`
			Text    struct {
				Content string `json:"content"`
			} `json:"text"`
		} `json:"msg_item"`
	} `json:"mixed"`
	CompatInfo *struct {
		Source string `json:"source"`
	} `json:"compat_info"`
}

// handleCallback 把一条 aibot_msg_callback 归一化成 imbot.Message：userid 作为
// 用户标识，msgid 去重，回调的 req_id 作为回复凭据原样透传。
func (p *Plugin) handleCallback(ctx context.Context, f wsFrame) {
	var body callbackBody
	if err := json.Unmarshal(f.Body, &body); err != nil {
		i18n.Logf(p.langOf(), "log.wecom.callback_parse_failed", "wecom_bot: 无法解析消息回调（已跳过）: %v", err)
		return
	}
	if body.CompatInfo != nil {
		// 智能助理调用、频道等不是真人在单聊里发来的消息
		i18n.Logf(p.langOf(), "log.wecom.compat_ignored", "wecom_bot: 忽略非普通用户消息（来源 %s）", body.CompatInfo.Source)
		return
	}
	if body.ChatType != "single" {
		i18n.Logf(p.langOf(), "log.wecom.group_ignored", "wecom_bot: 忽略群聊消息（chattype=%s，仅支持单聊）", body.ChatType)
		return
	}
	userID := strings.TrimSpace(body.From.UserID)
	if userID == "" {
		return
	}

	lang := p.langOf()
	text, unsupported := extractText(lang, body)
	note := ""
	if unsupported {
		note = i18n.T(lang, "plugin.wecom_bot.msg.unsupported", "暂不支持该消息类型，请发文字或语音。")
	}

	p.mu.Lock()
	core := p.core
	p.mu.Unlock()
	if core == nil {
		return
	}
	core.Handle(ctx, imbot.Message{
		UserID:  userID,
		DedupID: body.MsgID,
		ReplyTo: f.Headers.ReqID,
		Text:    text,
		Note:    note,
	})
}

// extractText 抽取可读文本。语音用企业微信自带的转写结果；图文混排把文字项拼起来，
// 其余媒体类型标记为不支持（有文字时只处理文字，没文字时骨架会回一句提示）。
func extractText(lang string, body callbackBody) (string, bool) {
	switch body.MsgType {
	case "text":
		return strings.TrimSpace(body.Text.Content), false
	case "voice":
		if s := strings.TrimSpace(body.Voice.Content); s != "" {
			return i18n.T(lang, "plugin.wecom_bot.voice_prefix", "【语音】") + s, false
		}
		return "", true
	case "mixed":
		var parts []string
		unsupported := false
		for _, item := range body.Mixed.MsgItem {
			if item.MsgType == "text" {
				if s := strings.TrimSpace(item.Text.Content); s != "" {
					parts = append(parts, s)
				}
			} else {
				unsupported = true
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n")), unsupported
	default:
		return "", true
	}
}
