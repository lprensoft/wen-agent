package dingtalkbot

import (
	"context"
	"fmt"
	"strings"

	"wen/internal/i18n"
	"wen/internal/mdtext"
)

const (
	// batchSend 的官方文档没有给出统一字数上限，只会在超长时返回
	// invalidParameter.msgParam.tooLong。这里取一个保守值，宁可多分一条。
	chunkByteLimit = 3600
	chunkRuneLimit = 1800
)

// send 把文本发到钉钉。回调回复与后台推送都走同一个 OpenAPI：用户标识是
// senderStaffId，主动推送要求用户先给机器人发过消息。
func (p *Plugin) send(ctx context.Context, userID, text string) bool {
	if strings.TrimSpace(text) == "" {
		return true
	}
	if userID == "" {
		return false
	}
	p.mu.Lock()
	format, lang := p.format, p.lang
	p.mu.Unlock()

	// 纯文本转换要在分段之前做：链接展开等操作会改变长度
	if format == formatPlain {
		text = mdtext.ToPlainText(lang, text)
	}
	chunks := chunkText(text)
	total := len(chunks)
	ok := true
	for i, chunk := range chunks {
		title := titleFor(lang, chunk)
		if total > 1 {
			chunk = fmt.Sprintf("(%d/%d) %s", i+1, total, chunk)
		}
		if err := p.sendMarkdown(ctx, userID, title, chunk); err != nil {
			ok = false
			if ctx.Err() != nil {
				return false
			}
			i18n.Logf(lang, "log.dingtalk.send_failed", "dingtalk_bot: 发送失败: %v", err)
		}
	}
	return ok
}

// chunkText 同时限制字节数与 rune 数：接口只报「太长」，不告诉具体口径，
// 两个都卡住最稳。先按字节切能保住代码块围栏，再按 rune 收一遍。
func chunkText(text string) []string {
	var out []string
	for _, chunk := range mdtext.ChunkMarkdownBytes(text, chunkByteLimit) {
		out = append(out, mdtext.ChunkMarkdown(chunk, chunkRuneLimit)...)
	}
	return out
}

// titleFor 取首行做通知标题（钉钉 sampleMarkdown 的 title 是必填项）。
func titleFor(lang, chunk string) string {
	for _, line := range strings.Split(chunk, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#>*- "))
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > 12 {
			return string(r[:12]) + "…"
		}
		return line
	}
	return i18n.T(lang, "plugin.dingtalk_bot.msg.title", "消息")
}

// push 主动推送：后台轮次没有可回带的回调凭据，同样走 batchSend。返回值如实
// 报告有没有被平台收下。
func (p *Plugin) push(ctx context.Context, userID, text string) bool {
	return p.send(ctx, userID, text)
}
