package slackbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"wen/internal/i18n"
	"wen/internal/imbot"
)

const (
	openConnectionsPath = "/apps.connections.open"

	// 每个 envelope 都要回 ack，Slack 才会停止重推。我们收到就入队并立刻回，
	// 不把耗时留给模型。
	envelopeTypeEventsAPI = "events_api"
	frameTypeHello        = "hello"
	frameTypeDisconnect   = "disconnect"

	socketDialTimeout    = 15 * time.Second
	socketWriteTimeout   = 5 * time.Second
	maxSocketAuthFailure = 5
)

var errSocketDisconnect = errors.New("Slack 要求刷新 Socket Mode 连接")

// socketAuthError 是 app-level token 或权限层面的错误，连续出现就不该无限重试。
type socketAuthError struct {
	code string
}

func (e *socketAuthError) Error() string {
	return "Slack 拒绝建立 Socket Mode 连接: " + e.code
}

type openConnectionsResponse struct {
	OK    bool   `json:"ok"`
	URL   string `json:"url"`
	Error string `json:"error"`
}

type envelope struct {
	EnvelopeID             string          `json:"envelope_id"`
	Type                   string          `json:"type"`
	Payload                json.RawMessage `json:"payload"`
	AcceptsResponsePayload bool            `json:"accepts_response_payload"`
}

type callbackJob struct {
	raw json.RawMessage
}

// socketLoop 维护 Socket Mode 长连接：断线与 Slack 的定期刷新都按
// [2,5,10,30,60] 秒退避重连；app 凭证/权限错误连续出现则放弃。
func (p *Plugin) socketLoop(ctx context.Context) {
	backoff := []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second}
	attempt, authFails := 0, 0

	for ctx.Err() == nil {
		start := time.Now()
		err := p.socketOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errSocketDisconnect) {
			i18n.Logf(p.langOf(), "log.slack.connection_refresh",
				"slack_bot: Slack 要求刷新连接（定期轮换），正在重连")
		}

		var ae *socketAuthError
		if errors.As(err, &ae) {
			authFails++
			if authFails >= maxSocketAuthFailure {
				i18n.Logf(p.langOf(), "log.slack.auth_give_up",
					"slack_bot: Socket Mode 连接连续失败 %d 次（%v），不再重连。请检查 App-Level Token 的 connections:write 权限", maxSocketAuthFailure, err)
				return
			}
			i18n.Logf(p.langOf(), "log.slack.auth_failed", "slack_bot: Socket Mode 连接失败（%v）", err)
		} else {
			authFails = 0
		}

		if time.Since(start) > time.Minute {
			attempt = 0 // 稳定连接过一段时间，重置退避
		}
		wait := backoff[min(attempt, len(backoff)-1)]
		attempt++
		i18n.Logf(p.langOf(), "log.slack.socket_retry", "slack_bot: Socket Mode 连接断开（%v），%v 后重连", err, wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}
}

// socketOnce 完成一次连接生命周期：apps.connections.open → WebSocket → 读 envelope
// 并回 ack，直到断开或 Slack 发 disconnect。
func (p *Plugin) socketOnce(ctx context.Context) error {
	p.mu.Lock()
	appToken, base, proxy, client, queue := p.appToken, p.apiBase, p.proxy, p.client, p.queue
	p.mu.Unlock()

	reqCtx, cancelReq := context.WithTimeout(ctx, socketDialTimeout)
	defer cancelReq()
	raw, status, err := postForm(reqCtx, client, base+openConnectionsPath, appToken)
	if err != nil {
		return err
	}
	var open openConnectionsResponse
	if err := json.Unmarshal(raw, &open); err != nil {
		return fmt.Errorf("解析 Socket Mode 接入点失败: %w", err)
	}
	if status == http.StatusTooManyRequests {
		return fmt.Errorf("Slack 限流，稍后重试")
	}
	if status != http.StatusOK || !open.OK || open.URL == "" {
		msg := open.Error
		if msg == "" {
			msg = truncate(string(raw), 200)
		}
		if isSocketAuthCode(msg) {
			return &socketAuthError{code: msg}
		}
		return fmt.Errorf("Socket Mode 接入点返回 HTTP %d: %s", status, msg)
	}

	dialer := websocket.Dialer{HandshakeTimeout: socketDialTimeout, Proxy: http.ProxyFromEnvironment}
	if proxy != nil {
		dialer.Proxy = http.ProxyURL(proxy)
	}
	wsURL, err := url.Parse(open.URL)
	if err != nil {
		return fmt.Errorf("Socket Mode 地址无效: %w", err)
	}
	conn, resp, err := dialer.DialContext(ctx, wsURL.String(), nil)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				return &socketAuthError{code: truncate(string(body), 120)}
			}
		}
		return err
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	defer conn.Close()

	connCtx, cancelConn := context.WithCancel(ctx)
	defer cancelConn()
	go func() {
		<-connCtx.Done()
		_ = conn.Close()
	}()

	var writeMu sync.Mutex
	writeAck := func(envelopeID string) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(socketWriteTimeout))
		return conn.WriteJSON(map[string]string{"envelope_id": envelopeID})
	}

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			i18n.Logf(p.langOf(), "log.slack.frame_parse_failed", "slack_bot: 无法解析的 Socket 帧（已跳过）: %v", err)
			continue
		}

		switch env.Type {
		case frameTypeHello:
			i18n.Logf(p.langOf(), "log.slack.connected", "slack_bot: Socket Mode 已连接")
			continue
		case frameTypeDisconnect:
			return errSocketDisconnect
		}
		if env.EnvelopeID == "" {
			continue
		}

		// 先入队再 ack：ack 成功代表消息已落到本地队列，Slack 不会重推。
		if env.Type == envelopeTypeEventsAPI && queue != nil {
			queue.Push(callbackJob{raw: env.Payload})
		}
		if err := writeAck(env.EnvelopeID); err != nil {
			return err
		}
	}
}

// dispatchLoop 串行消化入站消息，保持读循环收到的顺序。
func (p *Plugin) dispatchLoop(ctx context.Context, q *imbot.OrderedQueue[callbackJob]) {
	for {
		job, ok := q.Pop(ctx)
		if !ok {
			return
		}
		p.handleEvent(ctx, job.raw)
	}
}

func isSocketAuthCode(code string) bool {
	switch code {
	case "invalid_auth", "not_authed", "token_revoked", "account_inactive",
		"missing_scope", "not_allowed_token_type", "invalid_token":
		return true
	}
	return false
}

// postForm 发一个空的表单请求，Slack 的 apps.connections.open 用 Authorization
// 头带 app-level token。
func postForm(ctx context.Context, client *http.Client, url, token string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(""))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return raw, resp.StatusCode, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
