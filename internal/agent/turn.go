package agent

import (
	"context"
	"strings"

	"wen/internal/plugin"
)

// TurnSink 接收一轮结束的通知，供界面判断会话有没有新内容。
//
// 它只带会话 id 与这一轮的收尾信息，不带上下文：服务端拿它广播一条事件，
// 页面据此标未读、在后台时弹通知。传 nil 表示不需要（没有人接的时候不产生开销）。
type TurnSink func(sessionID string, ev plugin.TurnEndEvent)

// SetTurnSink 安装轮次结束的实时出口；传 nil 表示不需要。
func (a *Agent) SetTurnSink(fn TurnSink) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.turnSink = fn
}

// endTurn 收尾一轮：记下最近消息时间、推给界面，再广播给插件。
//
// 顺序是有意的：界面那条流只用来提示「有新内容」，插件处理（记忆提炼、风格观察）
// 可能很慢，不该让会话列表等它们。写 meta 失败不影响对话本身，只是未读少一次刷新。
func (a *Agent) endTurn(ctx context.Context, ev plugin.TurnEndEvent) {
	// 心跳这类轮次可以「保持安静」：模型给出空回复就等于什么都没说。
	// 它仍要广播给插件（心跳靠 OnTurnEnd 记账），但不算新消息——否则界面上会
	// 亮起一个点、弹一条通知，点进去却什么都没有。发了图片的轮次例外：
	// 文字是空的，但对方确实收到了东西。
	if strings.TrimSpace(ev.FinalText) == "" && !ev.HasMedia {
		a.plugins.NotifyTurnEnd(ctx, ev)
		return
	}
	_ = a.store.SetLastMessage(ev.SessionID, ev.EndedAt)
	a.mu.RLock()
	sink := a.turnSink
	a.mu.RUnlock()
	if sink != nil {
		sink(ev.SessionID, ev)
	}
	a.plugins.NotifyTurnEnd(ctx, ev)
}
