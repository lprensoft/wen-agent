package discordbot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

	"wen/internal/i18n"
	"wen/internal/imbot"
	"wen/internal/mdtext"
)

// chunkLimit 是单条消息的 rune 上限。Discord 上限 2000，留出 (i/n) 前缀的余量。
const chunkLimit = 1900

// maxRetryAfter 是 429 重试愿意等的上限。再长就放弃这一条，免得把整轮拖死。
const maxRetryAfter = 30 * time.Second

// send 把文本发到 Discord：默认按原生 markdown 原样发送；纯文本模式先转成
// 可读文本。按 2000 字上限分段，只有第一段带回复引用。429 按平台给的秒数
// 等一次再重试，其余错误只记日志。
func (p *Plugin) send(ctx context.Context, channelID, text, replyTo string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	p.mu.Lock()
	format := p.format
	p.mu.Unlock()

	usePlain := format == formatPlain
	chunks := chunkText(text)
	total := len(chunks)
	for i, chunk := range chunks {
		if usePlain {
			chunk = mdtext.ToPlainText(p.langOf(), chunk)
		}
		if total > 1 {
			chunk = fmt.Sprintf("(%d/%d) %s", i+1, total, chunk)
		}

		err := p.sendMessage(ctx, channelID, chunk, replyTo)
		var ae *apiError
		if errors.As(err, &ae) && ae.RetryAfter > 0 && ae.RetryAfter <= maxRetryAfter {
			i18n.Logf(p.langOf(), "log.discord.rate_limited",
				"discord_bot: 触发限流，%v 后重试", ae.RetryAfter)
			sleepCtx(ctx, ae.RetryAfter)
			err = p.sendMessage(ctx, channelID, chunk, replyTo)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			i18n.Logf(p.langOf(), "log.discord.send_failed", "discord_bot: 发送失败: %v", err)
		}
		// 只有第一段挂在入站消息上回复，后续段落独立发，免得刷一屏引用
		replyTo = ""
	}
}

// sleepCtx 等待 d，或被 ctx 提前取消。
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// chunkText 先按 markdown 结构分段，再按 Discord 的 UTF-16 长度兜底切一刀：
// 2000 的上限按 UTF-16 code unit 计，一个 emoji 占两个单位；只按 rune 数切，
// emoji 密集的长回复会在平台侧超限被拒。前缀 (i/n) 的余量也在 1900 里。
func chunkText(text string) []string {
	var out []string
	for _, c := range mdtext.ChunkMarkdown(text, chunkLimit) {
		out = append(out, splitUTF16(c, chunkLimit)...)
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

func splitUTF16(s string, limit int) []string {
	var out []string
	var b strings.Builder
	n := 0
	for _, r := range s {
		w := utf16.RuneLen(r)
		if w < 0 {
			w = 1
		}
		if n+w > limit {
			out = append(out, b.String())
			b.Reset()
			n = 0
		}
		b.WriteRune(r)
		n += w
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

// typing 在这一轮开始时显示「正在输入」。Discord 的状态约 10 秒自动消失，
// 结束时不发关闭请求。
func (p *Plugin) typing(ctx context.Context, msg imbot.Message, active bool) {
	if !active {
		return
	}
	if err := p.sendTyping(ctx, msg.UserID); err != nil && ctx.Err() == nil {
		i18n.Logf(p.langOf(), "log.discord.typing_failed",
			"discord_bot: 发送「正在输入」失败（忽略）: %v", err)
	}
}
