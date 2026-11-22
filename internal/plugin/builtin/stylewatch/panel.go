package stylewatch

import (
	"wen/internal/i18n"
	"wen/internal/plugin"
)

// State 把今天的文风统计交给设置页的「角色状态」。
//
// 统计按进程累计、不分人格（量与后台轮次分开计），整块标为 Global。只读内存里的
// 统计，不写盘、不调模型——与 StatusLines 同契约，随时可调。
func (p *Plugin) State(q plugin.StateQuery) []plugin.StateSection {
	if q.PinnedOnly {
		return nil
	}
	title := i18n.T(q.Lang, "plugin.style_watch.state.title", "文风")

	p.statsMu.Lock()
	day, ok := p.st.find(dateOf(q.Now()))
	p.statsMu.Unlock()

	if !ok || (day.Interactive.Turns == 0 && day.Background.Turns == 0) {
		return []plugin.StateSection{{
			Key:    "style_watch",
			Title:  title,
			Empty:  i18n.T(q.Lang, "plugin.style_watch.state.empty", "今日尚无数据。"),
			Global: true,
		}}
	}

	rows := make([]plugin.StateRow, 0, 2)
	if day.Interactive.Turns > 0 {
		rows = append(rows, plugin.StateRow{
			Label: i18n.T(q.Lang, "plugin.style_watch.state.interactive", "真人对话"),
			Value: day.Interactive.summary(q.Lang),
		})
	}
	if day.Background.Turns > 0 {
		rows = append(rows, plugin.StateRow{
			Label: i18n.T(q.Lang, "plugin.style_watch.state.background", "后台轮次"),
			Value: day.Background.summary(q.Lang),
			Tone:  plugin.StateToneMuted,
		})
	}
	return []plugin.StateSection{{
		Key:    "style_watch",
		Title:  title,
		Rows:   rows,
		Global: true,
	}}
}

var _ plugin.StateReporter = (*Plugin)(nil)
