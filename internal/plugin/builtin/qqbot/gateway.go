package qqbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"wen/internal/i18n"

	"github.com/gorilla/websocket"
)

// WebSocket 网关的操作码（与官方文档一致）。
const (
	opDispatch     = 0  // 事件推送，s 为序号
	opHeartbeat    = 1  // 客户端心跳，d 为最新收到的 s
	opIdentify     = 2  // 鉴权
	opResume       = 6  // 断线恢复
	opReconnect    = 7  // 服务端要求重连
	opInvalidSess  = 9  // session 失效，需重新 Identify
	opHello        = 10 // 连接建立后服务端下发，含心跳间隔
	opHeartbeatACK = 11
)

// intentC2CMessage 是单聊消息事件（C2C_MESSAGE_CREATE）所属的 intents 位。
const intentC2CMessage = 1 << 25

// wsFrame 是网关帧的通用信封。
type wsFrame struct {
	Op int             `json:"op"`
	S  int64           `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
	D  json.RawMessage `json:"d,omitempty"`
}

// gatewayLoop 维护到事件网关的长连接：断线按 [2,5,10,30,60] 秒退避重连，
// 有 session 的断线优先 Resume，session 失效则重新 Identify。
func (p *Plugin) gatewayLoop(ctx context.Context) {
	defer p.wg.Done()
	backoff := []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second}
	attempt := 0
	var sessionID string // 网关会话（Resume 用），与对话会话无关
	var seq int64

	for ctx.Err() == nil {
		start := time.Now()
		err := p.connectOnce(ctx, &sessionID, &seq)
		if ctx.Err() != nil {
			return
		}
		var fatal *fatalGatewayError
		if errors.As(err, &fatal) {
			i18n.Logf(p.langOf(), "log.qq.gateway_fatal",
				"qq_bot: 网关以不可恢复的关闭码断开（%v），不再重连。机器人可能已下架或被封禁，或配置有误；处理后在设置页重新保存一次即可", err)
			return
		}
		if time.Since(start) > time.Minute {
			attempt = 0 // 稳定连接过一段时间，重置退避
		}
		wait := backoff[min(attempt, len(backoff)-1)]
		attempt++
		i18n.Logf(p.langOf(), "log.qq.gateway_disconnected", "qq_bot: 网关连接断开（%v），%v 后重连", err, wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}
}

// connectOnce 完成一次完整的连接生命周期：取网关地址 → 握手（Hello/Identify 或
// Resume）→ 心跳循环 → 读事件直到出错。返回断开原因。
func (p *Plugin) connectOnce(ctx context.Context, sessionID *string, seq *int64) error {
	token, err := p.tokens.Get(ctx)
	if err != nil {
		return err
	}
	wsURL, err := p.getGatewayURL(ctx)
	if err != nil {
		return err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	// ctx 取消时主动关连接，解除读阻塞
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-connCtx.Done()
		_ = conn.Close()
	}()

	// 第一帧必须是 Hello
	frame, err := readFrame(conn)
	if err != nil {
		return err
	}
	if frame.Op != opHello {
		return fmt.Errorf("期望 Hello 帧，收到 op=%d", frame.Op)
	}
	var hello struct {
		HeartbeatInterval int64 `json:"heartbeat_interval"` // 毫秒
	}
	_ = json.Unmarshal(frame.D, &hello)
	if hello.HeartbeatInterval <= 0 {
		hello.HeartbeatInterval = 30000
	}

	// 有旧 session 先尝试 Resume，否则 Identify
	if *sessionID != "" {
		err = conn.WriteJSON(map[string]any{"op": opResume, "d": map[string]any{
			"token": "QQBot " + token, "session_id": *sessionID, "seq": *seq,
		}})
	} else {
		err = conn.WriteJSON(map[string]any{"op": opIdentify, "d": map[string]any{
			"token":      "QQBot " + token,
			"intents":    intentC2CMessage,
			"shard":      []int{0, 1},
			"properties": map[string]string{},
		}})
	}
	if err != nil {
		return err
	}

	// 心跳按官方下发间隔的 80% 发送，带最新序号
	hbCtx, hbStop := context.WithCancel(connCtx)
	defer hbStop()
	go func() {
		t := time.NewTicker(time.Duration(hello.HeartbeatInterval) * time.Millisecond * 4 / 5)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				// 还没收到过事件时官方要求 d 发 null 而不是 0
				var d any
				if *seq > 0 {
					d = *seq
				}
				_ = conn.WriteJSON(map[string]any{"op": opHeartbeat, "d": d})
			}
		}
	}()

	for {
		frame, err := readFrame(conn)
		if err != nil {
			switch closeAction(err) {
			case closeReidentify:
				*sessionID = "" // session 无法恢复，下次直接重新 Identify
			case closeFatal:
				return &fatalGatewayError{err: err}
			case closeThrottled: // 触发频控，多歇一会
				select {
				case <-time.After(60 * time.Second):
				case <-ctx.Done():
				}
			}
			return err
		}
		switch frame.Op {
		case opDispatch:
			*seq = frame.S
			p.handleDispatch(ctx, frame, sessionID)
		case opHeartbeatACK: // 忽略
		case opReconnect:
			return fmt.Errorf("服务端要求重连")
		case opInvalidSess:
			*sessionID = ""
			return fmt.Errorf("网关 session 失效，将重新鉴权")
		}
	}
}

// 网关关闭码的处理方式（官方 WebSocket 文档）。
const (
	closeRetry      = iota // 照常退避后重连，有 session 就 Resume
	closeReidentify        // 丢掉 session，重新 Identify
	closeThrottled         // 频控，多歇一会再连
	closeFatal             // 不能再连：重连只会一直失败，还会耗掉每天 1000 次的会话启动配额
)

// closeAction 按关闭码决定怎么办。4009（连接过期）可以 Resume，不丢 session；
// 4006/4007 与 4900–4913 要重新 Identify；4914（机器人已下架）、4915（已封禁）以及
// 4001/4002/4010–4014（参数或 intent 有误）重连也没用，停下来等人处理。
func closeAction(err error) int {
	var ce *websocket.CloseError
	if !errors.As(err, &ce) {
		return closeRetry
	}
	switch c := ce.Code; {
	case c == 4008:
		return closeThrottled
	case c == 4006, c == 4007, c >= 4900 && c <= 4913:
		return closeReidentify
	case c == 4914, c == 4915, c == 4001, c == 4002, c >= 4010 && c <= 4014:
		return closeFatal
	}
	return closeRetry
}

// fatalGatewayError 表示网关以「不能再连」的关闭码断开，网关循环据此停下。
type fatalGatewayError struct{ err error }

func (e *fatalGatewayError) Error() string { return e.err.Error() }
func (e *fatalGatewayError) Unwrap() error { return e.err }

// handleDispatch 处理一条事件帧。
func (p *Plugin) handleDispatch(ctx context.Context, frame wsFrame, sessionID *string) {
	switch frame.T {
	case "READY":
		var d struct {
			SessionID string `json:"session_id"`
			User      struct {
				Username string `json:"username"`
			} `json:"user"`
		}
		_ = json.Unmarshal(frame.D, &d)
		*sessionID = d.SessionID
		i18n.Logf(p.langOf(), "log.qq.gateway_ready", "qq_bot: 网关已就绪（%s）", d.User.Username)
	case "RESUMED":
		i18n.Logf(p.langOf(), "log.qq.gateway_restored", "qq_bot: 网关连接已恢复")
	case "C2C_MESSAGE_CREATE":
		var d struct {
			ID      string `json:"id"`
			Content string `json:"content"`
			Author  struct {
				UserOpenID string `json:"user_openid"`
			} `json:"author"`
		}
		if err := json.Unmarshal(frame.D, &d); err != nil {
			i18n.Logf(p.langOf(), "log.qq.event_parse_failed", "qq_bot: 消息事件解析失败: %v", err)
			return
		}
		p.handleInbound(ctx, inbound{openid: d.Author.UserOpenID, msgID: d.ID, content: d.Content})
	}
}

func readFrame(conn *websocket.Conn) (wsFrame, error) {
	var f wsFrame
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return f, err
	}
	err = json.Unmarshal(raw, &f)
	return f, err
}
