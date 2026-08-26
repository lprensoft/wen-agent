package imbot

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// confirmBroker 按用户登记等待中的确认请求（同一用户的消息由 worker 串行处理，
// 同一时刻至多一个确认在途）。/apply、/deny 在入站分发层直接投递——它们不能进
// worker 队列：worker 正阻塞在等确认上，排队会自锁。
type confirmBroker struct {
	mu      sync.Mutex
	pending map[string]chan bool
}

func newConfirmBroker() *confirmBroker {
	return &confirmBroker{pending: map[string]chan bool{}}
}

// register 登记一个等待中的确认，返回接收答复的 channel。
func (b *confirmBroker) register(userID string) chan bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan bool, 1)
	b.pending[userID] = ch
	return ch
}

func (b *confirmBroker) release(userID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pending, userID)
}

// take 取出该用户正在等的那次确认，答复由调用方投递；没有等待中的确认时返回 false。
//
// 取出与投递分成两步，是为了让回执能排在放行前面：答复一旦投出去，被解开的那一轮
// 立刻继续往下跑，它的最终回复可能抢在回执之前发到用户那里——用户会先看到执行
// 结果、再看到「已允许」。
func (b *confirmBroker) take(userID string) (chan<- bool, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch, ok := b.pending[userID]
	if !ok {
		return nil, false
	}
	delete(b.pending, userID)
	return ch, true
}

// confirmerFor 构造该用户本轮对话的确认通道：把确认请求文案发到远端，阻塞等
// /apply 或 /deny，超时或断开一律按拒绝。
func (c *Core) confirmerFor(msg Message) plugin.ConfirmFunc {
	return func(ctx context.Context, req plugin.ConfirmRequest) (bool, error) {
		timeout := c.cfg.ConfirmTimeout

		lang := c.cfg.Lang
		var b strings.Builder
		b.WriteString(i18n.T(lang, "imbot.confirm.head", "⚠️ 有操作需要你确认\n"))
		if req.Title != "" {
			b.WriteString(req.Title + "\n")
		}
		if req.Detail != "" {
			b.WriteString(req.Detail + "\n")
		}
		if req.Reason != "" {
			b.WriteString(i18n.Tf(lang, "imbot.confirm.reason", "原因：%s\n", req.Reason))
		}
		// /apply 与 /deny 是要照着打进去的字面量，不翻译
		b.WriteString(i18n.Tf(lang, "imbot.confirm.how",
			"回复 /apply 允许，/deny 拒绝（%s 内未回复按拒绝处理）", timeout))

		ch := c.broker.register(msg.UserID)
		defer c.broker.release(msg.UserID)
		c.send(ctx, msg, b.String())

		wctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		select {
		case approved := <-ch:
			return approved, nil
		case <-wctx.Done():
			return false, fmt.Errorf("%s", i18n.T(lang, "imbot.confirm.timeout",
				"等待确认超时或连接中断"))
		}
	}
}
