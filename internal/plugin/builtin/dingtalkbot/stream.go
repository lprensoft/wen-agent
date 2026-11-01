package dingtalkbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"wen/internal/i18n"
	"wen/internal/imbot"
)

const (
	streamEndpointPath = "/v1.0/gateway/connections/open"
	callbackTopic      = "/v1.0/im/bot/messages/get"

	streamTypeSystem   = "SYSTEM"
	streamTypeCallback = "CALLBACK"
	topicPing          = "ping"
	topicDisconnect    = "disconnect"

	streamDialTimeout  = 15 * time.Second
	streamWriteTimeout = 5 * time.Second
	streamKeepAlive    = 120 * time.Second
	maxStreamAuthFails = 5
)

var errServerDisconnect = errors.New("服务端要求断开 Stream 连接")

type streamSubscription struct {
	Type  string `json:"type"`
	Topic string `json:"topic"`
}

type streamEndpointRequest struct {
	ClientID      string               `json:"clientId"`
	ClientSecret  string               `json:"clientSecret"`
	Subscriptions []streamSubscription `json:"subscriptions"`
	UserAgent     string               `json:"ua"`
}

type streamEndpoint struct {
	Endpoint string `json:"endpoint"`
	Ticket   string `json:"ticket"`
}

// endpointError 是换取接入点时的 HTTP 错误。4xx 多半是凭证或订阅配置问题，
// 连续失败就不该无限重试。
type endpointError struct {
	status int
	body   string
}

func (e *endpointError) Error() string {
	return fmt.Sprintf("Stream 接入点返回 HTTP %d: %s", e.status, truncate(e.body, 300))
}

type streamFrame struct {
	SpecVersion string            `json:"specVersion"`
	Type        string            `json:"type"`
	Time        int64             `json:"time"`
	Headers     map[string]string `json:"headers"`
	Data        string            `json:"data"`
}

type streamAck struct {
	Code    int               `json:"code"`
	Headers map[string]string `json:"headers"`
	Message string            `json:"message"`
	Data    string            `json:"data"`
}

type callbackJob struct {
	raw   string // 回调 data 的原文
	dedup string // Stream 帧的 messageId；重推时不变
}

// streamLoop 维护 Stream 长连接：断线按 [2,5,10,30,60] 秒退避重连。接入点
// 连续 4xx 说明凭证或订阅有问题，试满 maxStreamAuthFails 次后放弃。服务端主动
// 发 disconnect 是正常的连接管理（连接到期、负载均衡等），照常重连。
func (p *Plugin) streamLoop(ctx context.Context) {
	backoff := []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second}
	attempt, authFails := 0, 0

	for ctx.Err() == nil {
		start := time.Now()
		err := p.streamOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errServerDisconnect) {
			i18n.Logf(p.langOf(), "log.dingtalk.stream_disconnected",
				"dingtalk_bot: 服务端要求断开连接（连接到期或负载均衡），稍后重连")
		}

		var ee *endpointError
		if errors.As(err, &ee) && ee.status >= 400 && ee.status < 500 && ee.status != http.StatusTooManyRequests {
			authFails++
			if authFails >= maxStreamAuthFails {
				i18n.Logf(p.langOf(), "log.dingtalk.auth_give_up",
					"dingtalk_bot: 换取 Stream 接入点连续失败 %d 次（%v），不再重连。请检查 Client ID / Secret 与订阅权限", maxStreamAuthFails, err)
				return
			}
			i18n.Logf(p.langOf(), "log.dingtalk.auth_failed",
				"dingtalk_bot: 换取 Stream 接入点失败（%v）", err)
		} else {
			authFails = 0
		}

		if time.Since(start) > time.Minute {
			attempt = 0 // 稳定连接过一段时间，重置退避
		}
		wait := backoff[min(attempt, len(backoff)-1)]
		attempt++
		i18n.Logf(p.langOf(), "log.dingtalk.stream_retry", "dingtalk_bot: Stream 连接断开（%v），%v 后重连", err, wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}
}

// streamOnce 完成一次连接生命周期：换取接入点 → WebSocket → 读帧/回 ack，直到断开。
func (p *Plugin) streamOnce(ctx context.Context) error {
	p.mu.Lock()
	appKey, appSecret, base := p.appKey, p.appSecret, p.apiBase
	client, queue := p.client, p.queue
	p.mu.Unlock()

	reqCtx, cancelReq := context.WithTimeout(ctx, streamDialTimeout)
	defer cancelReq()
	raw, status, err := postJSON(reqCtx, client, base+streamEndpointPath, "", streamEndpointRequest{
		ClientID:     appKey,
		ClientSecret: appSecret,
		UserAgent:    "wen",
		Subscriptions: []streamSubscription{
			{Type: streamTypeSystem, Topic: topicPing},
			{Type: streamTypeSystem, Topic: topicDisconnect},
			{Type: streamTypeCallback, Topic: callbackTopic},
		},
	})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return &endpointError{status: status, body: string(raw)}
	}
	var ep streamEndpoint
	if err := json.Unmarshal(raw, &ep); err != nil {
		return fmt.Errorf("解析 Stream 接入点失败: %w", err)
	}
	if ep.Endpoint == "" || ep.Ticket == "" {
		return errors.New("Stream 接入点缺少 endpoint 或 ticket")
	}

	wsURL, err := url.Parse(ep.Endpoint)
	if err != nil {
		return fmt.Errorf("Stream 接入点地址无效: %w", err)
	}
	q := wsURL.Query()
	q.Set("ticket", ep.Ticket)
	wsURL.RawQuery = q.Encode()

	dialer := websocket.Dialer{HandshakeTimeout: streamDialTimeout, Proxy: http.ProxyFromEnvironment}
	conn, resp, err := dialer.DialContext(ctx, wsURL.String(), nil)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			status := resp.StatusCode
			if status >= 400 && status < 500 && status != http.StatusTooManyRequests {
				return &endpointError{status: status, body: string(body)}
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

	// 长时间没流量时发控制帧 ping，并等 pong 或任意入站帧；等不到就断开重连。
	// 只看写成功不够：半开连接上写会成功，消息却已经收不到了。
	activity := make(chan struct{}, 1)
	signal := func() {
		select {
		case activity <- struct{}{}:
		default:
		}
	}
	conn.SetPongHandler(func(string) error {
		signal()
		return nil
	})
	keepDone := make(chan struct{})
	defer close(keepDone)
	go func() {
		t := time.NewTicker(streamKeepAlive)
		defer t.Stop()
		for {
			select {
			case <-keepDone:
				return
			case <-t.C:
				select { // 丢掉上一轮的残留信号
				case <-activity:
				default:
				}
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(streamWriteTimeout)); err != nil {
					_ = conn.Close()
					return
				}
				select {
				case <-activity: // pong 或任意入站帧，连接还活着
				case <-time.After(10 * time.Second):
					_ = conn.Close()
					return
				case <-keepDone:
					return
				}
			}
		}
	}()

	var writeMu sync.Mutex
	writeAck := func(messageID, data, message string) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
		return conn.WriteJSON(streamAck{
			Code:    200,
			Headers: map[string]string{"messageId": messageID, "contentType": "application/json"},
			Message: message,
			Data:    data,
		})
	}

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		signal()
		var f streamFrame
		if err := json.Unmarshal(data, &f); err != nil {
			i18n.Logf(p.langOf(), "log.dingtalk.frame_parse_failed", "dingtalk_bot: 无法解析的 Stream 帧（已跳过）: %v", err)
			continue
		}
		messageID := f.Headers["messageId"]
		topic := f.Headers["topic"]
		if messageID == "" {
			i18n.Logf(p.langOf(), "log.dingtalk.frame_no_message_id", "dingtalk_bot: Stream 帧缺少 messageId（已跳过，topic=%s）", topic)
			continue
		}

		// 先入队再 ack：ack 成功就代表这条消息已经落到本地队列，服务端不会重推。
		if f.Type == streamTypeCallback && topic == callbackTopic && queue != nil {
			queue.Push(callbackJob{raw: f.Data, dedup: messageID})
		}
		ackData, ackMessage := "", ""
		if topic == topicPing {
			// 与官方 SDK 的 pong 一致：回显 data，message 为 ok
			ackData, ackMessage = f.Data, "ok"
		}
		if err := writeAck(messageID, ackData, ackMessage); err != nil {
			return err
		}
		if topic == topicDisconnect {
			return errServerDisconnect
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
		p.handleCallback(ctx, job)
	}
}
