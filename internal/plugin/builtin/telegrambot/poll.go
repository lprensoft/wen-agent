package telegrambot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"wen/internal/imbot"

	"wen/internal/i18n"
)

// getUpdates 长轮询主循环。
//
// 游标（offset）持久化到 StateDir/offset：它只活在内存里的话，重启就会把已处理
// 的消息再收一遍，或者在服务端 24 小时保留窗口内漏掉一批。这与心跳「记上一次
// 发生的时刻而不是还剩多久」是同一条道理——进程内的状态不是状态。
const (
	maxConsecutiveFails = 3
	failBackoff         = 30 * time.Second
	failRetry           = 2 * time.Second
	// conflictRetry 是 409（已设 webhook）这类配置性错误的重试间隔。它不会自己好，
	// 快速重试只是在刷屏，隔久一点给用户改配置的时间。
	conflictRetry = 60 * time.Second
)

func offsetPath(stateDir string) string { return filepath.Join(stateDir, "offset") }

func loadOffset(stateDir string) int64 {
	raw, err := os.ReadFile(offsetPath(stateDir))
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func saveOffset(lang, stateDir string, offset int64) {
	if err := os.WriteFile(offsetPath(stateDir), []byte(strconv.FormatInt(offset, 10)), 0o644); err != nil {
		i18n.Logf(lang, "log.telegram.save_cursor_failed", "telegram_bot: 保存收信游标失败: %v", err)
	}
}

func (p *Plugin) pollLoop(ctx context.Context) {
	defer p.wg.Done()

	p.mu.Lock()
	stateDir, client := p.stateDir, p.pollClient
	p.mu.Unlock()

	offset := loadOffset(stateDir)
	fails := 0
	i18n.Logf(p.langOf(), "log.telegram.polling", "telegram_bot: 开始收信（游标 %d）", offset)

	for ctx.Err() == nil {
		updates, err := p.getUpdates(ctx, client, offset)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var ae *apiError
			if errors.As(err, &ae) && ae.isConflict() {
				i18n.Logf(p.langOf(), "log.telegram.webhook_conflict", "telegram_bot: 该机器人已设置 webhook，与长轮询互斥，收不到任何消息。"+
					"请先调用 deleteWebhook 取消，%v 后重试", conflictRetry)
				sleepCtx(ctx, conflictRetry)
				continue
			}
			if errors.As(err, &ae) && ae.RetryAfter > 0 {
				sleepCtx(ctx, time.Duration(ae.RetryAfter)*time.Second)
				continue
			}
			if isTimeout(err) { // 长轮询窗口内无消息，正常继续
				continue
			}
			fails++
			i18n.Logf(p.langOf(), "log.telegram.poll_failed", "telegram_bot: 收信失败（%d/%d）: %v", fails, maxConsecutiveFails, err)
			if fails >= maxConsecutiveFails {
				fails = 0
				sleepCtx(ctx, failBackoff)
			} else {
				sleepCtx(ctx, failRetry)
			}
			continue
		}
		fails = 0
		for _, u := range updates {
			// 先推进游标再处理：处理里可能跑一整轮对话，中途重启不该让这条重来
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
				saveOffset(p.langOf(), stateDir, offset)
			}
			p.dispatch(ctx, u)
		}
	}
}

// getUpdates 发一次长轮询请求。
func (p *Plugin) getUpdates(ctx context.Context, client *http.Client, offset int64) ([]update, error) {
	params := map[string]any{
		"timeout": int(longPollTimeout / time.Second),
		"limit":   100,
		// 只要私聊消息。不订阅其余类型可以让服务端直接跳过，省一轮往返
		"allowed_updates": []string{"message"},
	}
	if offset > 0 {
		params["offset"] = offset
	}
	var out []update
	if err := p.apiCall(ctx, client, "getUpdates", params, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// dispatch 把一条更新转成 imbot.Message 交给公共骨架。
// 仅处理私聊文本；图片等媒体只带 caption 时用 caption，纯媒体回一句暂不支持。
func (p *Plugin) dispatch(ctx context.Context, u update) {
	if u.Message == nil {
		return
	}
	m := u.Message
	chatID := strconv.FormatInt(m.Chat.ID, 10)
	if m.Chat.Type != "private" {
		i18n.Logf(p.langOf(), "log.telegram.group_ignored", "telegram_bot: 忽略非私聊消息（chat_id=%s type=%s，仅支持私聊）", chatID, m.Chat.Type)
		return
	}

	text := strings.TrimSpace(m.Text)
	note := ""
	if text == "" {
		if c := strings.TrimSpace(m.Caption); c != "" {
			text = c
		} else {
			note = i18n.T(p.langOf(), "plugin.telegram_bot.msg.unsupported",
				"暂不支持该消息类型，请发文字。")
		}
	}

	p.mu.Lock()
	core := p.core
	p.mu.Unlock()
	if core == nil {
		return
	}
	core.Handle(ctx, imbot.Message{
		UserID:  chatID,
		DedupID: fmt.Sprintf("%d", u.UpdateID),
		ReplyTo: strconv.FormatInt(m.MessageID, 10),
		Text:    text,
		Note:    note,
	})
}

// isTimeout 判定「等待窗口内没有新消息」这类超时，它不算失败。
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var terr interface{ Timeout() bool }
	return errors.As(err, &terr) && terr.Timeout()
}

// sleepCtx 可被 ctx 打断的休眠。
func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
