package qqbot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/imbot"

	"wen/internal/mdtext"
)

// chunkLimit 是单条消息的 rune 上限（QQ 上限约 5000，留足余量）。
const chunkLimit = 3500

// mdOffTTL 是「该用户不支持 markdown」判定的有效期，过期后重新探测。
const mdOffTTL = 6 * time.Hour

// send 把文本发到 QQ：默认按原生 markdown（msg_type=2，单聊已对所有机器人开放），
// 发送前过方言降级管线；收到「不允许发送原生 markdown」类错误时按用户记忆
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
	wakeupTried := false
	for i, chunk := range chunks {
		if total > 1 {
			chunk = fmt.Sprintf("(%d/%d) %s", i+1, total, chunk)
		}
		var m sendMode
		if ok, s := limiter.next(replyMsgID); ok {
			m = sendMode{msgID: replyMsgID, seq: s}
		}

		err := p.sendChunk(ctx, openid, chunk, m, &useMD)
		// 主动消息发不出去（对方关了「允许主动发送」、超了额度）时退一步用互动召回：
		// 对方最近 30 天里来过就有这个额度，每个时段一条。一次发送只试一次——同一时段
		// 第二条必然被拒
		if err != nil && m.msgID == "" && !wakeupTried && ctx.Err() == nil {
			wakeupTried = true
			if werr := p.sendChunk(ctx, openid, chunk, sendMode{wakeup: true}, &useMD); werr == nil {
				i18n.Logf(p.langOf(), "log.qq.wakeup_used", "qq_bot: 主动消息发不出去（%v），已改用互动召回送达", err)
				err = nil
			} else {
				err = fmt.Errorf("%w；%s", err, i18n.Tf(p.langOf(), "log.qq.wakeup_failed", "互动召回也失败: %v", werr))
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			kind := i18n.T(p.langOf(), "log.qq.kind.reply", "被动回复")
			if m.msgID == "" {
				kind = i18n.T(p.langOf(), "log.qq.kind.push", "主动消息（官方策略下可能受限）")
			}
			i18n.Logf(p.langOf(), "log.qq.send_failed", "qq_bot: %s发送失败: %v", kind, err)
		}
	}
}

// sendChunk 发一段：按 markdown 发，平台拒绝原生 markdown 时记住并当场降级纯文本重发。
func (p *Plugin) sendChunk(ctx context.Context, openid, chunk string, m sendMode, useMD *bool) error {
	if !*useMD {
		return p.sendC2C(ctx, openid, chunk, m)
	}
	err := p.sendC2CMarkdown(ctx, openid, chunk, m)
	if err != nil && isMarkdownForbidden(err) {
		// 平台按机器人/用户粒度拒绝原生 markdown：记住并当场降级纯文本重发
		p.disableMarkdown(openid)
		*useMD = false
		i18n.Logf(p.langOf(), "log.qq.markdown_downgraded", "qq_bot: 平台拒绝原生 markdown，对 %s 降级为纯文本（%v 后重新探测）", openid, mdOffTTL)
		err = p.sendC2C(ctx, openid, mdtext.ToPlainText(p.langOf(), chunk), m)
	}
	return err
}

// typing 在这一轮开始时发「正在输入」。它挂在入站消息上发（被动回复），官方没说清它
// 算不算被动回复的四条额度，这里按「算」处理、照常记账：要是不记而它其实算，第四段
// 正文会被平台拒掉而丢失；多记一条的代价只是长回复的最后一段改走主动消息。
// 主动推送没有入站消息可挂，不发。
func (p *Plugin) typing(ctx context.Context, msg imbot.Message, active bool) {
	if !active || msg.ReplyTo == "" {
		return
	}
	p.mu.Lock()
	limiter := p.limiter
	p.mu.Unlock()
	ok, seq := limiter.next(msg.ReplyTo)
	if !ok {
		return
	}
	if err := p.sendInputNotify(ctx, msg.UserID, sendMode{msgID: msg.ReplyTo, seq: seq}); err != nil && ctx.Err() == nil {
		i18n.Logf(p.langOf(), "log.qq.typing_failed", "qq_bot: 发送「正在输入」失败（忽略）: %v", err)
	}
}

// markdownErrCodes 是现行错误码表里与 markdown 相关的几个（40034008–40034011、
// 40034124、40034127）。早先认的 40034012 已不在表里，留着无害。错误串里含 markdown
// 的也算，作为错误码再变时的兜底。
var markdownErrCodes = []string{"40034008", "40034009", "40034010", "40034011", "40034012", "40034124", "40034127"}

// isMarkdownForbidden 判定「不允许发送原生 markdown」类错误。
func isMarkdownForbidden(err error) bool {
	s := err.Error()
	for _, c := range markdownErrCodes {
		if strings.Contains(s, c) {
			return true
		}
	}
	return strings.Contains(strings.ToLower(s), "markdown")
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
