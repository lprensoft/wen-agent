package heartbeat

import (
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// State 把心跳的节奏交给设置页的「角色状态」，并给聊天页顶部贡献一行
// 「下次开口」摘要。心跳是全局的，没有可见域。
func (p *Plugin) State(q plugin.StateQuery) []plugin.StateSection {
	p.mu.Lock()
	cur, dynamic, lastBeat := p.cur, p.dynamic, p.lastBeat
	pausedUntil, curContext, asleep := p.pausedUntil, p.curContext, p.asleep
	p.mu.Unlock()
	if cur <= 0 {
		return nil
	}

	lang := q.Lang
	mode := i18n.T(lang, "plugin.heartbeat.state.mode.fixed", "固定")
	if dynamic {
		mode = i18n.T(lang, "plugin.heartbeat.state.mode.dynamic", "动态")
	}
	rows := []plugin.StateRow{{
		Label:  i18n.T(lang, "plugin.heartbeat.state.interval", "间隔"),
		Value:  humanDur(lang, cur),
		Detail: mode,
	}}
	if curContext != "" {
		rows = append(rows, plugin.StateRow{
			Label: i18n.T(lang, "plugin.heartbeat.state.context", "情境"),
			Value: curContext,
		})
	}

	var summary string
	if until := time.Until(pausedUntil); until > 0 {
		if asleep {
			summary = i18n.Tf(lang, "plugin.heartbeat.state.asleep", "睡着了，约 %s后醒", humanDur(lang, until))
		} else {
			summary = i18n.Tf(lang, "plugin.heartbeat.state.paused", "暂停中，约 %s后恢复", humanDur(lang, until))
		}
	} else {
		summary = nextBeatDesc(lang, lastBeat.Add(cur))
	}
	statusValue := i18n.T(lang, "plugin.heartbeat.state.running", "运行中")
	statusDetail := summary
	if time.Until(pausedUntil) > 0 {
		// 暂停/睡着时这一行本身就是那句「暂停中…」，不再重复「运行中」。
		statusValue, statusDetail = summary, ""
	}
	rows = append(rows, plugin.StateRow{
		Label:  i18n.T(lang, "plugin.heartbeat.state.status", "状态"),
		Value:  statusValue,
		Detail: statusDetail,
	})

	return []plugin.StateSection{{
		Key:         "heartbeat",
		Title:       i18n.T(lang, "plugin.heartbeat.state.title", "心跳"),
		Summary:     summary,
		Rows:        rows,
		Pinned:      true,
		PinnedOrder: 3,
	}}
}
