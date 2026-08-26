package telegrambot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"wen/internal/mdtext"
)

// mdOffTTL 是「该用户的消息不能用 MarkdownV2 发」判定的有效期，过期后重新探测。
// 解析失败通常是某段特定内容触发的，不该让整个会话永久退回纯文本。
const mdOffTTL = 6 * time.Hour

// maxRetryAfter 是限流重试愿意等的上限。超过这个时长说明被限得很重，
// 等下去只会把整轮对话拖死，不如记日志放弃这一条。
const maxRetryAfter = 30 * time.Second

// send 把文本发到 Telegram：默认转成 MarkdownV2，解析失败时按用户降级为纯文本
// 重发，消息不丢；限流（429）按官方给的秒数等一次再重试。
func (p *Plugin) send(ctx context.Context, chatID, text, replyTo string) {
	p.mu.Lock()
	format, client := p.format, p.client
	p.mu.Unlock()

	useMD := format != formatPlain && p.markdownAllowed(chatID)

	// 分段在转换之前做：按原始 markdown 切能保住代码块围栏的完整性，
	// 而转义后的文本里围栏已经带上反斜杠，认不出来了
	chunks := mdtext.ChunkMarkdown(text, chunkLimit)
	total := len(chunks)
	for i, chunk := range chunks {
		if total > 1 {
			chunk = fmt.Sprintf("(%d/%d) %s", i+1, total, chunk)
		}

		var err error
		if useMD {
			err = p.sendMessage(ctx, client, chatID, toMarkdownV2(p.langOf(), chunk), "MarkdownV2", replyTo)
			var ae *apiError
			if errors.As(err, &ae) && ae.isParseError() {
				// 转换漏了某个字符：记住并当场降级纯文本重发，这一条不能丢
				p.disableMarkdown(chatID)
				useMD = false
				log.Printf("telegram_bot: MarkdownV2 解析失败（%s），对 %s 降级为纯文本（%v 后重新探测）",
					ae.Description, chatID, mdOffTTL)
				err = p.sendMessage(ctx, client, chatID, mdtext.ToPlainText(p.langOf(), chunk), "", replyTo)
			}
		} else {
			err = p.sendMessage(ctx, client, chatID, mdtext.ToPlainText(p.langOf(), chunk), "", replyTo)
		}

		// 限流：官方给了等待秒数，等一次再试一次。其余错误不重试——
		// 失败往往会连着失败，重试只是雪上加霜
		var ae *apiError
		if errors.As(err, &ae) && ae.RetryAfter > 0 {
			wait := time.Duration(ae.RetryAfter) * time.Second
			if wait <= maxRetryAfter {
				log.Printf("telegram_bot: 触发限流，%v 后重试", wait)
				sleepCtx(ctx, wait)
				if useMD {
					err = p.sendMessage(ctx, client, chatID, toMarkdownV2(p.langOf(), chunk), "MarkdownV2", replyTo)
				} else {
					err = p.sendMessage(ctx, client, chatID, mdtext.ToPlainText(p.langOf(), chunk), "", replyTo)
				}
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("telegram_bot: 发送失败: %v", err)
		}
		// 只有第一段挂在原消息上回复，后续段落各自独立，免得刷一屏引用
		replyTo = ""
	}
}

// sendMessage 发一条消息。parseMode 为空表示纯文本。
func (p *Plugin) sendMessage(ctx context.Context, client *http.Client, chatID, text, parseMode, replyTo string) error {
	params := map[string]any{
		"chat_id": chatID,
		"text":    text,
		// 回复里常带链接（如文档地址），预览图会把聊天界面撑得很长
		"link_preview_options": map[string]any{"is_disabled": true},
	}
	if parseMode != "" {
		params["parse_mode"] = parseMode
	}
	if replyTo != "" {
		if id, err := strconv.ParseInt(replyTo, 10, 64); err == nil {
			params["reply_parameters"] = map[string]any{
				"message_id": id,
				// 被回复的消息可能已被删除，那时不该整条发不出去
				"allow_sending_without_reply": true,
			}
		}
	}
	return p.apiCall(ctx, client, "sendMessage", params, nil)
}

// sendChatAction 发「正在输入」状态。它 5 秒自动消失，失败无所谓，只记调试信息。
func (p *Plugin) sendChatAction(ctx context.Context, chatID string) {
	p.mu.Lock()
	client := p.client
	p.mu.Unlock()
	params := map[string]any{"chat_id": chatID, "action": "typing"}
	if err := p.apiCall(ctx, client, "sendChatAction", params, nil); err != nil && ctx.Err() == nil {
		log.Printf("telegram_bot: 发送输入状态失败: %v", err)
	}
}

// markdownAllowed 查询该用户的 MarkdownV2 能力缓存（默认允许，失败后在 TTL 内关闭）。
func (p *Plugin) markdownAllowed(chatID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.mdOff[chatID]
	if !ok {
		return true
	}
	if time.Now().After(until) {
		delete(p.mdOff, chatID)
		return true
	}
	return false
}

func (p *Plugin) disableMarkdown(chatID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mdOff[chatID] = time.Now().Add(mdOffTTL)
}
