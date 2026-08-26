package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"wen/internal/plugin"

	"wen/internal/i18n"
)

// confirmBroker 把「工具想问用户」与「用户在界面上点了按钮」这两件事对上。
//
// 请求走当前对话的 SSE 流发出去（工具执行是在同一个 goroutine 里同步进行的，
// 因此这里会阻塞住那一轮对话，这正是想要的：命令还没执行）；答复则由浏览器发到
// 另一个 HTTP 请求上，再经这里的 channel 交回等待中的那一方。
type confirmBroker struct {
	mu      sync.Mutex
	pending map[string]chan bool
}

func newConfirmBroker() *confirmBroker {
	return &confirmBroker{pending: map[string]chan bool{}}
}

// register 登记一次等待，返回 id 与接收答复的 channel。
func (b *confirmBroker) register() (string, chan bool) {
	id := randomID()
	ch := make(chan bool, 1)
	b.mu.Lock()
	b.pending[id] = ch
	b.mu.Unlock()
	return id, ch
}

// release 撤销登记。等待方无论因何结束都必须调用，否则 pending 会一直涨。
func (b *confirmBroker) release(id string) {
	b.mu.Lock()
	delete(b.pending, id)
	b.mu.Unlock()
}

// resolve 交付一次答复；id 不存在（已超时、已取消、或重复提交）时返回 false。
func (b *confirmBroker) resolve(id string, approved bool) bool {
	b.mu.Lock()
	ch, ok := b.pending[id]
	if ok {
		delete(b.pending, id)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	ch <- approved
	return true
}

// confirmerFor 造一个绑定到本次 SSE 流的确认通道。
//
// emitRaw 必须与 Agent 的事件写在同一个 goroutine 上——工具执行是 run 循环里的同步调用，
// 所以这个条件天然满足，不需要给 SSE 写入加锁。
func (s *Server) confirmerFor(emitRaw func(string, any)) plugin.ConfirmFunc {
	return func(ctx context.Context, req plugin.ConfirmRequest) (bool, error) {
		id, ch := s.confirms.register()
		defer s.confirms.release(id)

		emitRaw("confirm_request", map[string]any{
			"type":   "confirm_request",
			"id":     id,
			"source": req.Source,
			"title":  req.Title,
			"detail": req.Detail,
			"reason": req.Reason,
		})

		select {
		case approved := <-ch:
			// 让界面把卡片从「等待中」定稿，即使用户是在别的标签页点的
			emitRaw("confirm_done", map[string]any{
				"type": "confirm_done", "id": id, "approved": approved,
			})
			return approved, nil
		case <-ctx.Done():
			// 用户关掉页面、或等待超时。两种情况都不能当作同意
			emitRaw("confirm_done", map[string]any{
				"type": "confirm_done", "id": id, "approved": false, "expired": true,
			})
			return false, ctx.Err()
		}
	}
}

// confirmResolve 处理界面回执：POST /api/confirmations/{id}  {"approved": bool}
func (s *Server) confirmResolve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Approved bool `json:"approved"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !s.confirms.resolve(id, req.Approved) {
		// 已超时或已回答过。不是服务端错误，但要让界面知道这次点击没生效
		writeError(w, http.StatusConflict, i18n.T(reqLang(r), "server.err.confirm_gone", "该确认请求已失效（超时或已处理）"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func randomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 退化到一个仍然唯一的值：id 只用于在本进程内配对，不承担安全职责
		return fmt.Sprintf("fallback-%p", &b)
	}
	return hex.EncodeToString(b[:])
}
