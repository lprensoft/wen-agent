package wecombot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"wen/internal/i18n"
)

// 长连接上的命令名（官方文档 path/101463）。
const (
	cmdSubscribe = "aibot_subscribe"
	cmdPing      = "ping"
	cmdRespond   = "aibot_respond_msg"
	cmdSend      = "aibot_send_msg"
	cmdCallback  = "aibot_msg_callback"
	cmdEvent     = "aibot_event_callback"

	eventDisconnected = "disconnected_event"
)

const (
	heartbeatInterval = 30 * time.Second
	heartbeatTimeout  = 10 * time.Second
	authTimeout       = 15 * time.Second
	maxAuthFailures   = 5
)

var (
	errAuthRejected = errors.New("长连接订阅被拒绝")
	errServerKicked = errors.New("连接被新的订阅顶替")
	errNoConn       = errors.New("企业微信长连接尚未建立")
)

// wsFrame 是长连接上的通用信封。消息/事件回调带 cmd 与 body；订阅、心跳、回复
// 的回执只有 headers.req_id 与 errcode/errmsg。
type wsFrame struct {
	Cmd     string          `json:"cmd,omitempty"`
	Headers frameHeaders    `json:"headers"`
	Errcode int             `json:"errcode"`
	Errmsg  string          `json:"errmsg"`
	Body    json.RawMessage `json:"body,omitempty"`
}

type frameHeaders struct {
	ReqID string `json:"req_id"`
}

type outboundFrame struct {
	Cmd     string       `json:"cmd"`
	Headers frameHeaders `json:"headers"`
	Body    any          `json:"body,omitempty"`
}

type wsResponse struct {
	errcode int
	errmsg  string
}

type wsResult struct {
	resp wsResponse
	err  error
}

// wsConn 是一条已建立的连接。回执按 req_id 关联：我们在请求里预先登记一个
// channel，读循环收到同 req_id 的回执后投进去。连接断开时唤醒所有等待者。
type wsConn struct {
	conn *websocket.Conn

	writeMu sync.Mutex // gorilla/websocket 只允许一个并发写者

	mu       sync.Mutex
	pending  map[string]chan wsResult
	closed   bool
	closeErr error
}

func newWSConn(conn *websocket.Conn) *wsConn {
	return &wsConn{conn: conn, pending: map[string]chan wsResult{}}
}

// request 发一帧并等同一 req_id 的回执。超时后返回错误；迟到的回执会被丢弃，
// 不会阻塞读循环。
func (w *wsConn) request(ctx context.Context, reqID string, frame any, timeout time.Duration) (wsResponse, error) {
	ch := make(chan wsResult, 1)
	w.mu.Lock()
	if w.closed {
		err := w.closeErr
		w.mu.Unlock()
		return wsResponse{}, err
	}
	w.pending[reqID] = ch
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.pending, reqID)
		w.mu.Unlock()
	}()

	w.writeMu.Lock()
	err := w.conn.WriteJSON(frame)
	w.writeMu.Unlock()
	if err != nil {
		return wsResponse{}, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.resp, r.err
	case <-timer.C:
		return wsResponse{}, fmt.Errorf("等待 %s 回执超时", reqID)
	case <-ctx.Done():
		return wsResponse{}, ctx.Err()
	}
}

// call 发一帧并检查回执：errcode 非 0 也算失败。
func (w *wsConn) call(ctx context.Context, reqID string, frame any, timeout time.Duration) error {
	resp, err := w.request(ctx, reqID, frame, timeout)
	if err != nil {
		return err
	}
	if resp.errcode != 0 {
		return fmt.Errorf("errcode=%d %s", resp.errcode, resp.errmsg)
	}
	return nil
}

// respond 投递一条回执。写入用非阻塞方式：请求方可能已经超时走了。
func (w *wsConn) respond(reqID string, res wsResult) {
	w.mu.Lock()
	ch := w.pending[reqID]
	delete(w.pending, reqID)
	w.mu.Unlock()
	if ch != nil {
		select {
		case ch <- res:
		default:
		}
	}
}

// close 关连接并唤醒所有等待者，可重复调用。
func (w *wsConn) close(err error) {
	if err == nil {
		err = errNoConn
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	w.closeErr = err
	pending := w.pending
	w.pending = map[string]chan wsResult{}
	w.mu.Unlock()

	_ = w.conn.Close()
	for _, ch := range pending {
		select {
		case ch <- wsResult{err: err}:
		default:
		}
	}
}

// gatewayLoop 维护长连接：断线按 [2,5,10,30,60] 秒退避重连。订阅被拒连续
// 出现 maxAuthFailures 次就放弃——凭据错了，重试只会一直撞墙；被别的连接顶替
// 也不重连，否则两条连接会互相踢。
func (p *Plugin) gatewayLoop(ctx context.Context) {
	backoff := []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second}
	attempt, authFails := 0, 0

	for ctx.Err() == nil {
		start := time.Now()
		authed, err := p.connectOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if authed {
			authFails = 0
		}

		switch {
		case errors.Is(err, errServerKicked):
			i18n.Logf(p.langOf(), "log.wecom.kicked",
				"wecom_bot: 另一个进程用同一 BotID 建立了长连接，本连接被顶掉，不再重连（同一机器人只能保持一个长连接）")
			return
		case errors.Is(err, errAuthRejected):
			authFails++
			if authFails >= maxAuthFailures {
				i18n.Logf(p.langOf(), "log.wecom.auth_give_up",
					"wecom_bot: 订阅连续失败 %d 次（%v），不再重连。请检查 BotID / Secret，并在设置页重新保存配置", maxAuthFailures, err)
				return
			}
			i18n.Logf(p.langOf(), "log.wecom.auth_failed",
				"wecom_bot: 订阅失败（%v），请检查 BotID 与 Secret", err)
		}

		if time.Since(start) > time.Minute {
			attempt = 0 // 稳定连接过一段时间，重置退避
		}
		wait := backoff[min(attempt, len(backoff)-1)]
		attempt++
		i18n.Logf(p.langOf(), "log.wecom.disconnected", "wecom_bot: 长连接断开（%v），%v 后重连", err, wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}
}

// connectOnce 完成一次连接生命周期：WebSocket 握手 → aibot_subscribe → 心跳协程
// → 读帧直到断开。返回是否认证成功，以及断开原因。
func (p *Plugin) connectOnce(ctx context.Context) (bool, error) {
	p.mu.Lock()
	botID, secret, gwURL := p.botID, p.secret, p.gatewayURL
	every, wait := p.heartbeatEvery, p.heartbeatWait
	p.mu.Unlock()

	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, _, err := dialer.DialContext(ctx, gwURL, nil)
	if err != nil {
		return false, err
	}
	w := newWSConn(conn)
	authed := false
	defer func() {
		p.clearConn(w)
		w.close(errors.New("连接结束"))
	}()

	// ctx 取消时主动关连接，解除读阻塞
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			w.close(ctx.Err())
		case <-stop:
		}
	}()

	subID := newReqID(cmdSubscribe)
	sub := outboundFrame{
		Cmd:     cmdSubscribe,
		Headers: frameHeaders{ReqID: subID},
		Body: map[string]any{
			"bot_id": botID,
			"secret": secret,
		},
	}
	// 读循环必须先跑起来：订阅回执和后续消息都从这条连接读，等认证完再读会互等。
	// 缓冲一下，认证失败提前返回时协程也能退出。
	readErr := make(chan error, 1)
	go func() { readErr <- p.readLoop(ctx, w) }()

	resp, err := w.request(ctx, subID, sub, authTimeout)
	if err != nil {
		return false, err
	}
	if resp.errcode != 0 {
		return false, fmt.Errorf("%w: errcode=%d %s", errAuthRejected, resp.errcode, resp.errmsg)
	}

	authed = true
	p.setConn(w)
	i18n.Logf(p.langOf(), "log.wecom.authenticated", "wecom_bot: 长连接已认证，开始接收消息")

	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go p.heartbeat(hbCtx, w, every, wait)

	return authed, <-readErr
}

// readLoop 持续读帧：消息交给分发层，事件里只关心「被新连接顶替」，其余按
// req_id 投给等待回执的请求方。
func (p *Plugin) readLoop(ctx context.Context, w *wsConn) error {
	for {
		_, data, err := w.conn.ReadMessage()
		if err != nil {
			return err
		}
		var f wsFrame
		if err := json.Unmarshal(data, &f); err != nil {
			// 一帧坏了不该把整条连接丢掉：后面的消息还是要收
			i18n.Logf(p.langOf(), "log.wecom.frame_parse_failed", "wecom_bot: 无法解析的帧（已跳过）: %v", err)
			continue
		}

		switch f.Cmd {
		case cmdCallback:
			go p.handleCallback(ctx, f)
		case cmdEvent:
			if isDisconnectedEvent(f.Body) {
				return errServerKicked
			}
		default:
			if f.Headers.ReqID != "" {
				w.respond(f.Headers.ReqID, wsResult{resp: wsResponse{errcode: f.Errcode, errmsg: f.Errmsg}})
			}
		}
	}
}

// heartbeat 定时发 ping 并等回执。等不到就把连接关掉，交给 gatewayLoop 重连；
// 光发不查的「僵尸连接」会让消息停收而进程毫无察觉。
func (p *Plugin) heartbeat(ctx context.Context, w *wsConn, every, wait time.Duration) {
	if every <= 0 {
		every = heartbeatInterval
	}
	if wait <= 0 {
		wait = heartbeatTimeout
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reqID := newReqID(cmdPing)
			frame := outboundFrame{Cmd: cmdPing, Headers: frameHeaders{ReqID: reqID}}
			resp, err := w.request(ctx, reqID, frame, wait)
			if err != nil {
				if ctx.Err() == nil {
					i18n.Logf(p.langOf(), "log.wecom.heartbeat_failed", "wecom_bot: 心跳失败（%v），断开重连", err)
					w.close(err)
				}
				return
			}
			if resp.errcode != 0 {
				i18n.Logf(p.langOf(), "log.wecom.heartbeat_ack_error",
					"wecom_bot: 心跳回执异常: errcode=%d %s", resp.errcode, resp.errmsg)
			}
		}
	}
}

func (p *Plugin) setConn(w *wsConn) {
	p.mu.Lock()
	p.conn = w
	p.mu.Unlock()
}

// clearConn 只清掉自己这一条：重连时新连接可能已经先写进来了。
func (p *Plugin) clearConn(w *wsConn) {
	p.mu.Lock()
	if p.conn == w {
		p.conn = nil
	}
	p.mu.Unlock()
}

func (p *Plugin) connSnapshot() *wsConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn
}

func isDisconnectedEvent(body json.RawMessage) bool {
	var b struct {
		Event struct {
			EventType string `json:"eventtype"`
		} `json:"event"`
	}
	return json.Unmarshal(body, &b) == nil && b.Event.EventType == eventDisconnected
}

var reqSeq atomic.Uint64

func newReqID(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixMilli(), reqSeq.Add(1))
}
