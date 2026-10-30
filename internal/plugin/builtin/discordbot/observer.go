package discordbot

import (
	"context"
	"strings"

	"wen/internal/imbot"
	"wen/internal/plugin"
)

// OnTurnEnd 观察每轮对话的结束：后台轮次（心跳、定时任务等其他插件发起）落在
// 绑定到 Discord 私信频道的会话上时，把助手的最终文本推送过去——否则结果只写进
// 会话文件，对面的人永远看不到。前台轮次与本插件自己发起的轮次不推：它们各有回复渠道。
func (p *Plugin) OnTurnEnd(_ context.Context, ev plugin.TurnEndEvent) {
	if ev.Origin == "" || ev.Origin == p.Name() {
		return
	}
	// 另一条通道发起的轮次：投递责任归它那一侧的路由，观察者一律不插手。
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

	for _, channelID := range users {
		go func(channelID string) {
			defer p.wg.Done()
			core.PushReply(pctx, channelID, ev.FinalText)
		}(channelID)
	}
}

// push 主动推送：Discord 的 DM channel ID 就是发送地址，不需要回复凭据。
// 返回值要如实报告有没有全部交给平台——跨通道转投据此知道这段话是否送达。
func (p *Plugin) push(ctx context.Context, channelID, text string) bool {
	if channelID == "" {
		return false
	}
	return p.send(ctx, channelID, text, "")
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
