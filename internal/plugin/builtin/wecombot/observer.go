package wecombot

import (
	"context"
	"strings"

	"wen/internal/imbot"
	"wen/internal/plugin"
)

// OnTurnEnd 观察每轮对话的结束：后台轮次（心跳、定时任务等其他插件发起）落在
// 绑定到企业微信用户的会话上时，把助手的最终文本推送过去——否则结果只写进会话
// 文件，对面的人永远看不到。前台轮次与本插件自己发起的轮次不推：它们各有回复渠道。
//
// 推送走 aibot_send_msg（单聊 chatid 即 userid）。官方要求用户先给机器人发过消息，
// 没发过时平台会拒绝，push 返回 false，转投方据此知道没送达。
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
