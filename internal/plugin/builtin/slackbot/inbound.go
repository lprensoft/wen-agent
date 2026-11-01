package slackbot

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
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

// handleEnvelope 按 Socket Mode 的信封类型分发：Events API 消息与斜杠命令
// 都归一化成 imbot.Message；其他类型（按钮、模态框）已在读循环里 ack 后忽略。
func (p *Plugin) handleEnvelope(ctx context.Context, job callbackJob) {
	switch job.typ {
	case envelopeTypeEventsAPI:
		p.handleEvent(ctx, job.raw, job.dedupID)
	case envelopeTypeSlash:
		p.handleSlashCommand(ctx, job)
	}
}

// slashPayload 是 Socket Mode 下斜杠命令的 payload。
type slashPayload struct {
	Command   string `json:"command"`
	Text      string `json:"text"`
	ChannelID string `json:"channel_id"`
	UserID    string `json:"user_id"`
	TriggerID string `json:"trigger_id"`
}

// Slack 内置了一批 /status、/new 之类的命令名，直接占用会冲突。只注册一个
// /wen，用子命令映射到公共骨架；/wen-status 这类别名也一并支持。
var slackSubcommands = map[string]string{
	"new": "/new", "status": "/status", "compact": "/compact",
	"help": "/help", "apply": "/apply", "deny": "/deny",
}

func (p *Plugin) handleSlashCommand(ctx context.Context, job callbackJob) {
	var payload slashPayload
	if err := json.Unmarshal(job.raw, &payload); err != nil {
		i18n.Logf(p.langOf(), "log.slack.event_parse_failed", "slack_bot: 无法解析事件（已跳过）: %v", err)
		return
	}
	command := strings.ToLower(strings.TrimSpace(payload.Command))
	channelID := strings.TrimSpace(payload.ChannelID)
	if command == "" || channelID == "" {
		return
	}

	var text string
	switch {
	case command == "/wen":
		fields := strings.Fields(payload.Text)
		if len(fields) == 0 {
			p.slashUsage(ctx, channelID, job)
			return
		}
		coreCommand, ok := slackSubcommands[strings.ToLower(fields[0])]
		if !ok {
			p.slashUsage(ctx, channelID, job)
			return
		}
		text = coreCommand
		if len(fields) > 1 {
			text += " " + strings.Join(fields[1:], " ")
		}
	case strings.HasPrefix(command, "/wen-"):
		coreCommand, ok := slackSubcommands[strings.TrimPrefix(command, "/wen-")]
		if !ok {
			i18n.Logf(p.langOf(), "log.slack.slash_ignored", "slack_bot: 忽略未接入的斜杠命令 %s", command)
			return
		}
		text = coreCommand
		if arg := strings.TrimSpace(payload.Text); arg != "" {
			text += " " + arg
		}
	default:
		i18n.Logf(p.langOf(), "log.slack.slash_ignored", "slack_bot: 忽略未接入的斜杠命令 %s", command)
		return
	}

	dedup := "slash:" + job.dedupID
	if job.dedupID == "" {
		dedup = "slash:" + payload.TriggerID
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
	})
}

// slashUsage 用骨架的 Note 通道回一句用法；Text 为空时骨架只发 Note，不进模型。
func (p *Plugin) slashUsage(ctx context.Context, channelID string, job callbackJob) {
	dedup := "slash:" + job.dedupID
	p.mu.Lock()
	core := p.core
	p.mu.Unlock()
	if core == nil {
		return
	}
	core.Handle(ctx, imbot.Message{
		UserID:  channelID,
		DedupID: dedup,
		Note:    i18n.T(p.langOf(), "plugin.slack_bot.msg.usage", "用法：/wen new|status|compact|help|apply|deny"),
	})
}

// handleEvent 把一条 message.im 事件归一化成 imbot.Message。用户标识用 DM
// channel ID（后续回复与推送都发给它），去重用 event_id。
func (p *Plugin) handleEvent(ctx context.Context, raw json.RawMessage, envelopeID string) {
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
	text := strings.TrimSpace(decodeSlackText(payload.Event.Text))
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
	if dedup == ":" || dedup == "" {
		dedup = envelopeID
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

var slackTokenRe = regexp.MustCompile(`<([^>]+)>`)

// decodeSlackText 还原 Slack 在 message 文本里的协议编码：&amp; / &lt; / &gt;
// 以及 <url|label>、<@U…>、<#C…|name>、<!here> 这类控制序列。不还原的话，
// 用户写的代码、命令和比较符会带着实体进模型，回显时还会被再转义一次。
func decodeSlackText(s string) string {
	var tokens []string
	s = slackTokenRe.ReplaceAllStringFunc(s, func(m string) string {
		tokens = append(tokens, decodeSlackToken(m[1:len(m)-1]))
		return fmt.Sprintf("\x00%d\x00", len(tokens)-1)
	})
	s = decodeSlackEntities(s)
	for i, t := range tokens {
		s = strings.ReplaceAll(s, fmt.Sprintf("\x00%d\x00", i), t)
	}
	return s
}

func decodeSlackToken(inner string) string {
	inner = decodeSlackEntities(inner)
	switch {
	case strings.HasPrefix(inner, "@"):
		parts := strings.SplitN(inner[1:], "|", 2)
		if len(parts) == 2 && parts[1] != "" {
			return "@" + parts[1]
		}
		return "@" + parts[0]
	case strings.HasPrefix(inner, "#"):
		parts := strings.SplitN(inner[1:], "|", 2)
		if len(parts) == 2 && parts[1] != "" {
			return "#" + parts[1]
		}
		return "#" + parts[0]
	case strings.HasPrefix(inner, "!"):
		body := inner[1:]
		if strings.HasPrefix(body, "date^") {
			if i := strings.Index(body, "|"); i >= 0 {
				return body[i+1:]
			}
			return ""
		}
		if i := strings.Index(body, "|"); i >= 0 {
			return body[i+1:]
		}
		return "@" + body
	default:
		if i := strings.Index(inner, "|"); i >= 0 {
			url, label := inner[:i], inner[i+1:]
			if label == "" {
				return url
			}
			return label + " (" + url + ")"
		}
		return inner
	}
}

func decodeSlackEntities(s string) string {
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&amp;", "&") // 最后解码 &，避免 &amp;lt; 被二次解成 <
	return s
}
