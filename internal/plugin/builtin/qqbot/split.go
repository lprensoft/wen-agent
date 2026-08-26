package qqbot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"wen/internal/i18n"

	"wen/internal/mdtext"
)

// chunkLimit 是单条消息的 rune 上限（QQ 上限约 5000，留足余量）。
const chunkLimit = 3500

// mdOffTTL 是「该用户不支持 markdown」判定的有效期，过期后重新探测。
const mdOffTTL = 6 * time.Hour

// send 把文本发到 QQ：默认按原生 markdown（msg_type=2，单聊已对所有机器人开放），
// 发送前过方言降级管线；收到「不允许发送原生 markdown」(40034012) 时按用户记忆
// 降级为纯文本重发，消息不丢。每段独立判定被动回复额度，超限降级为主动消息。
func (p *Plugin) send(ctx context.Context, openid, text, replyMsgID string) {
	p.mu.Lock()
	limiter, format := p.limiter, p.format
	p.mu.Unlock()

	useMD := format != formatPlain && p.markdownAllowed(openid)
	text = sanitizeForQQ(p.langOf(), text)

	var chunks []string
	if useMD {
		chunks = mdtext.ChunkMarkdown(text, chunkLimit)
	} else {
		chunks = mdtext.ChunkMarkdown(mdtext.ToPlainText(p.langOf(), text), chunkLimit)
	}

	total := len(chunks)
	for i, chunk := range chunks {
		if total > 1 {
			chunk = fmt.Sprintf("(%d/%d) %s", i+1, total, chunk)
		}
		msgID, seq := "", 0
		if ok, s := limiter.next(replyMsgID); ok {
			msgID, seq = replyMsgID, s
		}

		var err error
		if useMD {
			err = p.sendC2CMarkdown(ctx, openid, chunk, msgID, seq)
			if err != nil && isMarkdownForbidden(err) {
				// 平台按机器人/用户粒度拒绝原生 markdown：记住并当场降级纯文本重发
				p.disableMarkdown(openid)
				useMD = false
				i18n.Logf(p.langOf(), "log.qq.markdown_downgraded", "qq_bot: 平台拒绝原生 markdown，对 %s 降级为纯文本（%v 后重新探测）", openid, mdOffTTL)
				err = p.sendC2C(ctx, openid, mdtext.ToPlainText(p.langOf(), chunk), msgID, seq)
			}
		} else {
			err = p.sendC2C(ctx, openid, chunk, msgID, seq)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			kind := i18n.T(p.langOf(), "log.qq.kind.reply", "被动回复")
			if msgID == "" {
				kind = i18n.T(p.langOf(), "log.qq.kind.push", "主动消息（官方策略下可能受限）")
			}
			i18n.Logf(p.langOf(), "log.qq.send_failed", "qq_bot: %s发送失败: %v", kind, err)
		}
	}
}

// isMarkdownForbidden 判定「不允许发送原生 markdown」类错误（错误码 40034012）。
func isMarkdownForbidden(err error) bool {
	s := err.Error()
	return strings.Contains(s, "40034012") || strings.Contains(s, "markdown")
}

// markdownAllowed 查询该用户的 markdown 能力缓存（默认允许，被拒后在 TTL 内关闭）。
func (p *Plugin) markdownAllowed(openid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.mdOff[openid]
	if !ok {
		return true
	}
	if time.Now().After(until) {
		delete(p.mdOff, openid)
		return true
	}
	return false
}

func (p *Plugin) disableMarkdown(openid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mdOff[openid] = time.Now().Add(mdOffTTL)
}

// 分段与纯文本转换的实现在 wen/internal/mdtext（与微信等其它 IM 通道共用）。
