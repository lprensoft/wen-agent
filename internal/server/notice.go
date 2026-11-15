package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"wen/internal/session"
)

// 会话事件的实时通道。
//
// /api/chat 的事件流是按请求的，一轮返回就关；而后台工作（记忆提炼、清扫、心跳主动
// 开口）在那之后才跑完，结果无处可送。这里开一条常驻的流：界面订阅一次，之后任何
// 会话上的事件都即时推过去，由前端按事件类型与会话筛选。
//
// 两种事件：notice 是一条已落盘的注记，直接显示在当前会话里；turn 只说明这个会话
// 刚结束一轮，用来标未读、在页面不可见时弹通知，不带整轮内容。
//
// 一条流服务所有会话而不是按会话开一条：界面同一时刻只看一个会话，按会话开需要在
// 切换时反复重连，还得处理旧连接的收尾。

// noticeBuffer 是每个订阅者的待发缓冲。慢的浏览器不该拖住写事件的 goroutine，
// 缓冲满了就丢——注记同时已经落盘，刷新页面就能补齐；turn 只是提示，丢了也不影响
// 会话内容，下一次轮次或列表刷新会盖过它。
const noticeBuffer = 32

// hubEvent 是一条已经编码好的事件，name 同时是 SSE 的事件名。
type hubEvent struct {
	name string
	data []byte
}

// noticeHub 向所有订阅者广播会话事件。
type noticeHub struct {
	mu   sync.Mutex
	next int
	subs map[int]chan hubEvent
}

func newNoticeHub() *noticeHub {
	return &noticeHub{subs: map[int]chan hubEvent{}}
}

// subscribe 返回一个接收通道与退订函数。
func (h *noticeHub) subscribe() (<-chan hubEvent, func()) {
	ch := make(chan hubEvent, noticeBuffer)
	h.mu.Lock()
	id := h.next
	h.next++
	h.subs[id] = ch
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		if c, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(c)
		}
		h.mu.Unlock()
	}
}

// publish 把一条注记广播出去。绝不阻塞：它跑在插件的后台 goroutine 上。
func (h *noticeHub) publish(sessionID string, m session.StoredMessage) {
	data, err := json.Marshal(map[string]any{
		"type":       "notice",
		"session_id": sessionID,
		"content":    m.Content,
		"origin":     m.Origin,
		"tag":        m.Tag,
		"ts":         m.TS,
	})
	if err != nil {
		return
	}
	h.broadcast(hubEvent{name: "notice", data: data})
}

// publishTurn 广播「一轮结束」。
//
// 只带会话 id、时间、发起方与一小段预览：界面需要它判断是否有新内容，以及在页面
// 不可见时弹一条通知；整轮内容已经在会话文件里，刷新就能拿到。
func (h *noticeHub) publishTurn(sessionID string, ts time.Time, interactive bool, text string) {
	data, err := json.Marshal(map[string]any{
		"type":        "turn",
		"session_id":  sessionID,
		"ts":          ts,
		"interactive": interactive,
		"preview":     turnPreview(text),
	})
	if err != nil {
		return
	}
	h.broadcast(hubEvent{name: "turn", data: data})
}

// broadcast 把一条事件发给所有订阅者。慢订阅者丢事件，不阻塞调用方。
func (h *noticeHub) broadcast(ev hubEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// turnPreview 把这一轮的收尾文本压成一行短预览，供桌面通知的正文。
// 换行会撑破通知布局，空白先折叠；太长就截断，不把整段话塞进系统通知。
func turnPreview(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > 80 {
		return string(runes[:80]) + "…"
	}
	return text
}

// events 是会话事件的 SSE 端点。连接一直开着，直到客户端断开或进程退出。
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, unsubscribe := s.notices.subscribe()
	defer unsubscribe()

	// 心跳兼作探活：中间的代理常在闲置若干分钟后掐断连接，
	// 而事件本身可能几小时都没有一条。
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, ev.data)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
