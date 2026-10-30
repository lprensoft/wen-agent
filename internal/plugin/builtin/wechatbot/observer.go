package wechatbot

import (
	"context"
	"strings"

	"wen/internal/i18n"

	"wen/internal/imbot"
	"wen/internal/plugin"
)

// iLink 发消息必须回带 context_token，主动推送没有「本轮入站消息」可回带——
// 只能用该用户最近一次入站消息的 token。token 由微信侧签发，有效期未知，
// 失效按发送失败容忍；持久化交给 imbot.PushTargetStore（StateDir/tokens.json）。

// OnTurnEnd 观察每轮对话的结束：后台轮次（心跳、定时任务等其他插件发起）落在
// 绑定到微信用户的会话上时，把助手的最终文本推送过去——否则结果只写进会话文件，
// 对面的人永远看不到。前台轮次与本插件自己发起的轮次不推：它们各有回复渠道。
//
// 广播发生在轮次收尾的同步路径上，发送工作放 goroutine；不能用广播的 ctx——
// 发起方（如心跳）在轮次结束后立即取消它。
func (p *Plugin) OnTurnEnd(_ context.Context, ev plugin.TurnEndEvent) {
	if ev.Origin == "" || ev.Origin == p.Name() {
		return
	}
	// 另一条通道发起的轮次：投递责任归它那一侧的路由，观察者一律不插手。
	// 没有这一句，两条通道绑在同一会话上时，一边的前台回复会被另一边再推一遍
	// ——分通道功能正是要让它们绑在同一会话上。
	if imbot.IsChannel(ev.Origin) {
		return
	}
	// 装了分通道路由时，后台轮次也跟着人格走：不归我服务的会话不推
	if !imbot.ServedBy(p.Name(), ev.SessionID) {
		return
	}
	if strings.TrimSpace(ev.FinalText) == "" {
		return
	}

	p.mu.Lock()
	if p.cancel == nil { // 已停止
		p.mu.Unlock()
		return
	}
	core, pctx := p.core, p.ctx
	users := core.UsersFor(ev.SessionID)
	if len(users) == 0 {
		p.mu.Unlock()
		return
	}
	p.wg.Add(len(users))
	p.mu.Unlock()

	for _, userID := range users {
		go func(userID string) {
			defer p.wg.Done()
			core.PushReply(pctx, userID, ev.FinalText)
		}(userID)
	}
}

// OnNotice 旁听会话注记：开着「推送后台通知」时，把后台工作留下的说明（提炼的
// 记录等）推给绑定用户。过滤规则（开关、共享域、非 IM 来源、归属通道）都在骨架的
// PushNotice 里，通道只负责转交。
func (p *Plugin) OnNotice(_ context.Context, ev plugin.NoticeEvent) {
	p.mu.Lock()
	core := p.core
	p.mu.Unlock()
	if core != nil {
		core.PushNotice(ev)
	}
}

// push 主动推送：iLink 发消息必须回带 context_token，主动推送没有「本轮入站消息」
// 可回带，只能用该用户最近一次入站消息的 token。对方从没说过话时推不出去，如实
// 报 false——调用方（分通道转投）据此知道这段话没有送达。
func (p *Plugin) push(ctx context.Context, userID, text string) bool {
	p.mu.Lock()
	tokens := p.tokens
	p.mu.Unlock()
	return pushWith(ctx, p, tokens, userID, text)
}

func pushWith(ctx context.Context, p *Plugin, tokens *imbot.PushTargetStore, userID, text string) bool {
	if tokens == nil {
		return false
	}
	token := tokens.Get(userID)
	if token == "" {
		// 该用户还没发过消息（或票据文件丢失），没有可用的 context_token
		i18n.Logf(p.langOf(), "log.wechat.no_context_token", "wechat_bot: 无法把消息推给 %s：缺少 context_token（对方先发一条消息后即可）", userID)
		return false
	}
	p.send(ctx, userID, text, token)
	return true
}
