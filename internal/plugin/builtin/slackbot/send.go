package slackbot

import (
	"context"
	"fmt"
	"strings"

	"wen/internal/i18n"
	"wen/internal/mdtext"
)

// chunkLimit 是单条消息的 rune 上限。Slack 官方建议 text 控制在 4000 字符内，
// 这里留出分段序号的余量。
const chunkLimit = 3500

// send 把文本发到 Slack。markdown 先降级成 Slack mrkdwn；纯文本先压成可读文本，
// 并让 chat.postMessage 关闭 mrkdwn，避免残留字符又被渲染。
func (p *Plugin) send(ctx context.Context, channelID, text string) bool {
	if strings.TrimSpace(text) == "" {
		return true
	}
	if channelID == "" {
		return false
	}
	p.mu.Lock()
	format, lang := p.format, p.lang
	p.mu.Unlock()

	plain := format == formatPlain
	if plain {
		text = mdtext.ToPlainText(lang, text)
	} else {
		text = toSlackMarkdown(text)
	}
	chunks := mdtext.ChunkMarkdown(text, chunkLimit)

	total := len(chunks)
	ok := true
	for i, chunk := range chunks {
		if total > 1 {
			chunk = fmt.Sprintf("(%d/%d) %s", i+1, total, chunk)
		}
		if err := p.postMessage(ctx, channelID, chunk, !plain); err != nil {
			ok = false
			if ctx.Err() != nil {
				return false
			}
			i18n.Logf(lang, "log.slack.send_failed", "slack_bot: 发送失败: %v", err)
		}
	}
	return ok
}

// push 主动推送：后台轮次没有可回带的入站凭据，直接发到绑定用户的 DM channel。
func (p *Plugin) push(ctx context.Context, channelID, text string) bool {
	return p.send(ctx, channelID, text)
}
