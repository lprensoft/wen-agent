package wechatbot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"wen/internal/i18n"
)

// getupdates 长轮询主循环，对照官方 monitor.ts 移植：
//   - 游标 get_updates_buf 持久化到 StateDir/updates_buf，重启后续传不重复收消息；
//   - 服务端可通过 longpolling_timeout_ms 调整下次轮询的客户端超时；
//   - errcode/ret = -14（会话过期）暂停一切请求一段时间后重试（官方行为），并在日志里
//     提示可能需要重新扫码；
//   - 其它错误连败 3 次退避 30s，否则 2s 重试。

const (
	maxConsecutiveFails = 3
	failBackoff         = 30 * time.Second
	failRetry           = 2 * time.Second
)

func bufPath(stateDir string) string { return filepath.Join(stateDir, "updates_buf") }

func loadUpdatesBuf(stateDir string) string {
	raw, err := os.ReadFile(bufPath(stateDir))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func saveUpdatesBuf(lang, stateDir, buf string) {
	if err := os.WriteFile(bufPath(stateDir), []byte(buf), 0o644); err != nil {
		i18n.Logf(lang, "log.wechat.save_cursor_failed", "wechat_bot: 保存收信游标失败: %v", err)
	}
}

func (p *Plugin) pollLoop(ctx context.Context) {
	defer p.wg.Done()

	p.mu.Lock()
	creds, stateDir, pause := p.creds, p.stateDir, p.pauseOnExpired
	p.mu.Unlock()

	buf := loadUpdatesBuf(stateDir)
	timeout := longPollTimeout
	fails := 0
	i18n.Logf(p.langOf(), "log.wechat.connected", "wechat_bot: 已连接微信收信通道（bot_id=%s）", creds.BotID)
	// 上线与下线各告诉服务端一声（官方渠道包的生命周期行为）。下线那一声用自己的
	// 短超时：收信的 ctx 那时已经取消了，而 Stop 会等这个 goroutine 退出，不能久等
	p.notifyLifecycle(ctx, creds, "/ilink/bot/msg/notifystart")
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), lifecycleTimeout)
		defer cancel()
		p.notifyLifecycle(sctx, creds, "/ilink/bot/msg/notifystop")
	}()

	for ctx.Err() == nil {
		resp, err := p.getUpdates(ctx, creds, buf, timeout)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if isTimeout(err) { // 长轮询窗口内无消息，正常继续
				continue
			}
			fails++
			i18n.Logf(p.langOf(), "log.wechat.poll_failed", "wechat_bot: 收信失败（%d/%d）: %v", fails, maxConsecutiveFails, err)
			if fails >= maxConsecutiveFails {
				fails = 0
				sleepCtx(ctx, failBackoff)
			} else {
				sleepCtx(ctx, failRetry)
			}
			continue
		}
		if ms := resp.LongpollingTimeoutMs; ms > 0 {
			timeout = time.Duration(ms) * time.Millisecond
		}
		if resp.Ret != 0 || resp.Errcode != 0 {
			if resp.Ret == errcodeExpired || resp.Errcode == errcodeExpired {
				i18n.Logf(p.langOf(), "log.wechat.session_expired", "wechat_bot: 微信侧 token 已失效（errcode %d），收发都暂停 %v 后重试；若持续出现请到设置页重新扫码绑定",
					errcodeExpired, pause)
				fails = 0
				p.pauseUntil(time.Now().Add(pause))
				sleepCtx(ctx, pause)
				continue
			}
			fails++
			i18n.Logf(p.langOf(), "log.wechat.poll_api_error", "wechat_bot: 收信接口报错（%d/%d）: ret=%d errcode=%d %s",
				fails, maxConsecutiveFails, resp.Ret, resp.Errcode, resp.Errmsg)
			if fails >= maxConsecutiveFails {
				fails = 0
				sleepCtx(ctx, failBackoff)
			} else {
				sleepCtx(ctx, failRetry)
			}
			continue
		}
		fails = 0
		if resp.GetUpdatesBuf != "" {
			buf = resp.GetUpdatesBuf
			saveUpdatesBuf(p.langOf(), stateDir, buf)
		}
		for _, msg := range resp.Msgs {
			p.dispatch(ctx, msg)
		}
	}
}

// lifecycleTimeout 是上线、下线通知的超时：只是知会一声，失败不影响收发。
const lifecycleTimeout = 3 * time.Second

// notifyLifecycle 发一次上线或下线通知。失败与非零 ret 只记日志，不阻断启动或停止。
func (p *Plugin) notifyLifecycle(ctx context.Context, creds credentials, endpoint string) {
	raw, err := apiPost(ctx, creds.BaseURL, endpoint, map[string]any{"base_info": newBaseInfo()}, creds.BotToken, lifecycleTimeout)
	var resp struct {
		Ret    int    `json:"ret"`
		Errmsg string `json:"errmsg"`
	}
	if err == nil && json.Unmarshal(raw, &resp) == nil && resp.Ret == 0 {
		return
	}
	if err == nil {
		err = fmt.Errorf("ret=%d %s", resp.Ret, resp.Errmsg)
	}
	i18n.Logf(p.langOf(), "log.wechat.lifecycle_failed", "wechat_bot: %s 未成功（忽略）: %v", endpoint, err)
}

// getUpdates 发一次长轮询请求。
func (p *Plugin) getUpdates(ctx context.Context, creds credentials, buf string, timeout time.Duration) (getUpdatesResp, error) {
	var resp getUpdatesResp
	body := map[string]any{
		"get_updates_buf": buf,
		"base_info":       newBaseInfo(),
	}
	raw, err := apiPost(ctx, creds.BaseURL, "/ilink/bot/getupdates", body, creds.BotToken, timeout)
	if err != nil {
		return resp, err
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return resp, fmt.Errorf("解析收信响应失败: %w", err)
	}
	return resp, nil
}

// dispatch 把一条入站消息转成 inbound 交给分发层。仅处理用户发来的私聊；
// 语音用微信自带的转写文本，其余媒体类型回复暂不支持。
func (p *Plugin) dispatch(ctx context.Context, msg wxMessage) {
	if msg.MessageType != msgTypeUser {
		return // Bot 自己的消息回显等
	}
	if msg.GroupID != "" {
		i18n.Logf(p.langOf(), "log.wechat.group_ignored", "wechat_bot: 忽略群聊消息（group_id=%s，仅支持私聊）", msg.GroupID)
		return
	}
	if msg.FromUserID == "" || msg.ContextToken == "" {
		return
	}

	var parts []string
	unsupported := false
	for _, item := range msg.ItemList {
		switch item.Type {
		case 1:
			if item.TextItem != nil && strings.TrimSpace(item.TextItem.Text) != "" {
				parts = append(parts, item.TextItem.Text)
			}
		case 3:
			if item.VoiceItem != nil && strings.TrimSpace(item.VoiceItem.Text) != "" {
				parts = append(parts, "【语音】"+item.VoiceItem.Text)
			} else {
				unsupported = true
			}
		default:
			unsupported = true
		}
	}

	in := inbound{
		userID:       msg.FromUserID,
		contextToken: msg.ContextToken,
		text:         strings.TrimSpace(strings.Join(parts, "\n")),
	}
	dedupKey := msg.ClientID
	if msg.MessageID != 0 {
		dedupKey = strconv.FormatUint(msg.MessageID, 10)
	}
	p.handleInbound(ctx, in, dedupKey, unsupported)
}

// sleepCtx 可被 ctx 打断的休眠。
func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
