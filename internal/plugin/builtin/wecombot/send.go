package wecombot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/mdtext"
)

const (
	// maxContentBytes 是企业微信单条消息内容的上限（官方：20480 字节，回复的
	// stream 与主动推送的 markdown 同限）。分段留出「(1/2) 」这类序号前缀的余量。
	maxContentBytes = 20480
	chunkByteLimit  = maxContentBytes - 480 // 留给「(1/2) 」这类序号前缀

	defaultAckTimeout = 5 * time.Second
)

// send 把文本发到企业微信。有回调 req_id 时用 aibot_respond_msg 回复，否则用
// aibot_send_msg 主动推送。多条（超长分段、像人一样发消息）都用同一个 req_id：
// 回复的每一段各自带新的 stream.id，平台按 stream.id 区分气泡；主动推送则各用
// 新的请求 id。
//
// 一旦某段回复超时，这个 req_id 就不能再用了：迟到的回执会被当成下一段的回执。
// 超时的那段可能已经送达，不重发；还没发出去的后续段落改走主动推送。
func (p *Plugin) send(ctx context.Context, userID, text, replyTo string) bool {
	if strings.TrimSpace(text) == "" {
		return true
	}
	p.mu.Lock()
	format, lang := p.format, p.lang
	p.mu.Unlock()

	// 纯文本转换要在分段之前做：链接展开等操作会改变长度
	if format == formatPlain {
		text = mdtext.ToPlainText(lang, text)
	}
	chunks := mdtext.ChunkMarkdownBytes(text, chunkByteLimit)

	total := len(chunks)
	usePush := replyTo == ""
	ok := true
	for i, chunk := range chunks {
		if total > 1 {
			chunk = fmt.Sprintf("(%d/%d) %s", i+1, total, chunk)
		}

		var err error
		kind := i18n.T(lang, "log.wecom.kind.reply", "被动回复")
		if usePush {
			err = p.pushMessage(ctx, userID, chunk)
			kind = i18n.T(lang, "log.wecom.kind.push", "主动推送")
		} else {
			err = p.reply(ctx, replyTo, chunk)
			switch {
			case errors.Is(err, errReqIDStale):
				// 这一段还没发出去，换主动推送；不能挂在回执没确认的 req_id 上
				usePush = true
				err = p.pushMessage(ctx, userID, chunk)
				kind = i18n.T(lang, "log.wecom.kind.push", "主动推送")
			case errors.Is(err, errReplyTimeout):
				// 这一段可能已经送达，不能重发；后面的段落改走主动推送
				usePush = true
			}
		}
		if err != nil {
			ok = false
			if ctx.Err() != nil {
				return false
			}
			i18n.Logf(lang, "log.wecom.send_failed", "wecom_bot: %s发送失败: %v", kind, err)
		}
	}
	return ok
}

// reply 用回调用过的 req_id 回复一段。官方与 SDK 的普通回复都用 stream 消息：
// 带一个新的 stream.id、直接 finish=true，一段就是一个气泡；同一次回调的多段
// 共用 req_id 但 stream.id 不同，不会互相覆盖。必须在收到回调后 24 小时内发。
func (p *Plugin) reply(ctx context.Context, reqID, content string) error {
	w := p.connSnapshot()
	if w == nil {
		return errNoConn
	}
	frame := outboundFrame{
		Cmd:     cmdRespond,
		Headers: frameHeaders{ReqID: reqID},
		Body: map[string]any{
			"msgtype": "stream",
			"stream": map[string]any{
				"id":      newReqID("stream"),
				"finish":  true,
				"content": content,
			},
		},
	}
	return w.call(ctx, reqID, frame, p.replyWait())
}

// pushMessage 主动推送一条 markdown 消息。单聊的 chatid 就是 userid，chat_type=1
// 明确按单聊解析；用户必须先给机器人发过消息，否则平台会拒绝。
func (p *Plugin) pushMessage(ctx context.Context, userID, content string) error {
	if userID == "" {
		return fmt.Errorf("缺少 userid")
	}
	w := p.connSnapshot()
	if w == nil {
		return errNoConn
	}
	reqID := newReqID(cmdSend)
	frame := outboundFrame{
		Cmd:     cmdSend,
		Headers: frameHeaders{ReqID: reqID},
		Body: map[string]any{
			"chatid":    userID,
			"chat_type": 1,
			"msgtype":   "markdown",
			"markdown":  map[string]any{"content": content},
		},
	}
	return w.call(ctx, reqID, frame, p.replyWait())
}

// push 主动推送：后台轮次没有可回带的 req_id，只能走 aibot_send_msg。返回值如实
// 报告有没有真的被平台收下，转投方据此判断这段话是否送达。
func (p *Plugin) push(ctx context.Context, userID, text string) bool {
	return p.send(ctx, userID, text, "")
}

// replyWait 是等待平台回执的时长；测试会把它调短。
func (p *Plugin) replyWait() time.Duration {
	p.mu.Lock()
	wait := p.ackWait
	p.mu.Unlock()
	if wait <= 0 {
		return defaultAckTimeout
	}
	return wait
}
