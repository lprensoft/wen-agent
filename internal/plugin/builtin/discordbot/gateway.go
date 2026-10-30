package discordbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"wen/internal/i18n"
	"wen/internal/imbot"
)

// Discord Gateway 操作码（官方文档 v10）。
const (
	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opResume         = 6
	opReconnect      = 7
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatACK   = 11
)

// 只订阅私信与基础服务器事件。私信正文不需要 MESSAGE_CONTENT 这个 privileged
// intent——官方把「发给 bot 的私信」列为例外。
const intents = (1 << 0) | (1 << 12) // GUILDS | DIRECT_MESSAGES

type gatewayFrame struct {
	Op int             `json:"op"`
	S  int64           `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
	D  json.RawMessage `json:"d,omitempty"`
}

var errBadFrame = errors.New("bad gateway frame")

// gatewayState 跨一次连接保留：RESUME 需要 session_id 与最后一个序号；
// resume_gateway_url 是 READY 下发的专用重连地址。
type gatewayState struct {
	sessionID string
	resumeURL string
	seq       atomic.Int64
}

// gatewayLoop 维护到 Discord 事件网关的长连接：断线按 [2,5,10,30,60] 秒退避重连，
// 有 session 的断线优先 Resume，session 失效则重新 Identify。
func (p *Plugin) gatewayLoop(ctx context.Context) {
	backoff := []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second}
	attempt := 0
	st := &gatewayState{}

	for ctx.Err() == nil {
		start := time.Now()
		err := p.connectOnce(ctx, st)
		if ctx.Err() != nil {
			return
		}
		var fatal *fatalGatewayError
		if errors.As(err, &fatal) {
			i18n.Logf(p.langOf(), "log.discord.gateway_fatal",
				"discord_bot: 网关以不可恢复的关闭码断开（%v），不再重连。请检查 Bot Token 与 Gateway Intents 配置", err)
			return
		}
		if time.Since(start) > time.Minute {
			attempt = 0 // 稳定连接过一段时间，重置退避
		}
		wait := backoff[min(attempt, len(backoff)-1)]
		attempt++
		i18n.Logf(p.langOf(), "log.discord.gateway_disconnected",
			"discord_bot: 网关连接断开（%v），%v 后重连", err, wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}
}

// connectOnce 完成一次完整的连接生命周期：Hello → Identify/Resume → 心跳循环 →
// 读事件直到出错。返回断开原因。
func (p *Plugin) connectOnce(ctx context.Context, st *gatewayState) error {
	p.mu.Lock()
	token, gwURL, proxy := p.token, p.gatewayURL, p.proxy
	p.mu.Unlock()
	if st.resumeURL != "" {
		gwURL = st.resumeURL
	} else if !strings.Contains(gwURL, "?") {
		gwURL += "?v=10&encoding=json"
	}

	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second, Proxy: http.ProxyFromEnvironment}
	if proxy != nil {
		dialer.Proxy = http.ProxyURL(proxy)
	}
	conn, _, err := dialer.DialContext(ctx, gwURL, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	// 服务端的心跳请求、定时心跳与 Identify/Resume 都可能在不同 goroutine 上写，
	// gorilla/websocket 只允许一个并发写者。
	var writeMu sync.Mutex
	writeJSON := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteJSON(v)
	}

	// ctx 取消时主动关连接，解除读阻塞
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-connCtx.Done()
		_ = conn.Close()
	}()

	frame, err := readFrame(conn)
	if err != nil {
		return err
	}
	if frame.Op != opHello {
		return fmt.Errorf("期望 Hello 帧，收到 op=%d", frame.Op)
	}
	var hello struct {
		HeartbeatInterval int64 `json:"heartbeat_interval"`
	}
	_ = json.Unmarshal(frame.D, &hello)
	if hello.HeartbeatInterval <= 0 {
		hello.HeartbeatInterval = 41250
	}

	if st.sessionID != "" && st.seq.Load() > 0 {
		err = writeJSON(map[string]any{"op": opResume, "d": map[string]any{
			"token": token, "session_id": st.sessionID, "seq": st.seq.Load(),
		}})
	} else {
		err = writeJSON(map[string]any{"op": opIdentify, "d": map[string]any{
			"token":   token,
			"intents": intents,
			"properties": map[string]string{
				"os": runtime.GOOS, "browser": "wen", "device": "wen",
			},
		}})
	}
	if err != nil {
		return err
	}

	var awaitingACK atomic.Bool
	sendHeartbeat := func() {
		var d any
		if v := st.seq.Load(); v > 0 {
			d = v
		}
		_ = writeJSON(map[string]any{"op": opHeartbeat, "d": d})
		awaitingACK.Store(true)
	}
	tickHeartbeat := func() {
		// 上一拍还没收到 ACK：TCP 可能已被 NAT 静默切断，主动关连接让读循环
		// 走退避重连，避免「连接看起来还在、消息却再也收不到」。
		if awaitingACK.Swap(true) {
			_ = conn.Close()
			return
		}
		sendHeartbeat()
	}

	// 心跳按官方下发的间隔发送。Discord 要求首拍带抖动；立即发一拍再按间隔走，
	// 同样落在服务端允许的窗口内。
	hbCtx, hbStop := context.WithCancel(connCtx)
	defer hbStop()
	go func() {
		interval := time.Duration(hello.HeartbeatInterval) * time.Millisecond
		tickHeartbeat()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				tickHeartbeat()
			}
		}
	}()

	for {
		frame, err := readFrame(conn)
		if err != nil {
			if errors.Is(err, errBadFrame) {
				i18n.Logf(p.langOf(), "log.discord.event_parse_failed",
					"discord_bot: 网关帧解析失败（忽略）: %v", err)
				continue
			}
			switch closeAction(err) {
			case closeReidentify:
				st.sessionID = ""
				st.resumeURL = ""
				st.seq.Store(0)
			case closeThrottled:
				select {
				case <-time.After(60 * time.Second):
				case <-ctx.Done():
				}
			case closeFatal:
				return &fatalGatewayError{err: err}
			}
			return err
		}
		switch frame.Op {
		case opDispatch:
			if frame.S > 0 {
				st.seq.Store(frame.S)
			}
			p.handleDispatch(ctx, frame, st)
		case opHeartbeat: // 服务端主动要求立刻回一拍，不参与定时心跳的 ACK 超时判定
			sendHeartbeat()
		case opHeartbeatACK:
			awaitingACK.Store(false)
		case opReconnect:
			return fmt.Errorf("服务端要求重连")
		case opInvalidSession:
			var resumable bool
			_ = json.Unmarshal(frame.D, &resumable)
			if !resumable {
				st.sessionID = ""
				st.resumeURL = ""
				st.seq.Store(0)
			}
			return fmt.Errorf("网关 session 失效（resumable=%v），将重连", resumable)
		}
	}
}

// 网关关闭码的处理方式（官方 WebSocket 文档）。
const (
	closeRetry      = iota // 照常退避后重连，有 session 就 Resume
	closeReidentify        // 丢掉 session，重新 Identify
	closeThrottled         // 频控，多歇一会再连
	closeFatal             // 不能再连：凭证或 intents 有误
)

func closeAction(err error) int {
	var ce *websocket.CloseError
	if !errors.As(err, &ce) {
		return closeRetry
	}
	switch c := ce.Code; {
	case c == 4008:
		return closeThrottled
	case c == 4007, c == 4009:
		return closeReidentify
	case c == 4004, c >= 4010 && c <= 4014:
		return closeFatal
	}
	return closeRetry
}

// fatalGatewayError 表示网关以「不能再连」的关闭码断开，网关循环据此停下。
type fatalGatewayError struct{ err error }

func (e *fatalGatewayError) Error() string { return e.err.Error() }
func (e *fatalGatewayError) Unwrap() error { return e.err }

func readFrame(conn *websocket.Conn) (gatewayFrame, error) {
	var f gatewayFrame
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, fmt.Errorf("%w: %v", errBadFrame, err)
	}
	return f, nil
}

func (p *Plugin) handleDispatch(ctx context.Context, frame gatewayFrame, st *gatewayState) {
	switch frame.T {
	case "READY":
		var d struct {
			SessionID     string `json:"session_id"`
			ResumeGateway string `json:"resume_gateway_url"`
			User          struct {
				Username string `json:"username"`
			} `json:"user"`
		}
		if err := json.Unmarshal(frame.D, &d); err != nil {
			i18n.Logf(p.langOf(), "log.discord.event_parse_failed",
				"discord_bot: READY 解析失败: %v", err)
			return
		}
		st.sessionID = d.SessionID
		st.resumeURL = d.ResumeGateway
		i18n.Logf(p.langOf(), "log.discord.gateway_ready",
			"discord_bot: 网关已就绪（%s）", d.User.Username)
	case "RESUMED":
		i18n.Logf(p.langOf(), "log.discord.gateway_restored", "discord_bot: 网关连接已恢复")
	case "MESSAGE_CREATE":
		p.handleMessageCreate(ctx, frame)
	}
}

// handleMessageCreate 只处理私信：带 guild_id 的服务器消息直接忽略。正文为空的
// 消息（表情、附件、贴纸等）交给骨架回一句「暂不支持」，不进对话轮次。
func (p *Plugin) handleMessageCreate(ctx context.Context, frame gatewayFrame) {
	var d struct {
		ID        string `json:"id"`
		ChannelID string `json:"channel_id"`
		GuildID   string `json:"guild_id"`
		Content   string `json:"content"`
		Author    struct {
			ID       string `json:"id"`
			Bot      bool   `json:"bot"`
			Username string `json:"username"`
		} `json:"author"`
	}
	if err := json.Unmarshal(frame.D, &d); err != nil {
		i18n.Logf(p.langOf(), "log.discord.event_parse_failed",
			"discord_bot: 消息事件解析失败: %v", err)
		return
	}
	if d.Author.Bot {
		return
	}
	if d.GuildID != "" {
		i18n.Logf(p.langOf(), "log.discord.guild_ignored",
			"discord_bot: 忽略服务器频道消息（channel=%s，仅支持私信）", d.ChannelID)
		return
	}
	if d.ID == "" || d.ChannelID == "" {
		return
	}

	text := strings.TrimSpace(d.Content)
	note := ""
	if text == "" {
		note = i18n.T(p.langOf(), "plugin.discord_bot.msg.unsupported",
			"暂不支持该消息类型，请发文字。")
	}

	p.mu.Lock()
	core := p.core
	p.mu.Unlock()
	if core == nil {
		return
	}
	core.Handle(ctx, imbot.Message{
		UserID:  d.ChannelID,
		DedupID: d.ID,
		ReplyTo: d.ID,
		Text:    text,
		Note:    note,
	})
}
