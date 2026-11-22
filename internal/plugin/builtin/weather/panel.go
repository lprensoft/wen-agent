package weather

import (
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// State 把天气现况交给设置页的「角色状态」。
//
// 天气不按人格分：同一座城市的天气对谁都一样，整块标为 Global，切到哪一面都看得到。
// 只读内存里的观测缓存，不发网络请求——与 StatusLines 同契约，随时可调。
func (p *Plugin) State(q plugin.StateQuery) []plugin.StateSection {
	if q.PinnedOnly {
		return nil
	}
	s := p.snapshot()
	title := i18n.T(q.Lang, "plugin.weather.state.title", "天气")
	empty := i18n.T(q.Lang, "plugin.weather.state.empty", "没有设置城市，看不到天气。")
	if !s.configured() {
		return []plugin.StateSection{{
			Key:    "weather",
			Title:  title,
			Empty:  empty,
			Global: true,
		}}
	}

	now := q.Now()
	var (
		rows   []plugin.StateRow
		newest time.Time
	)
	add := func(label, loc string) {
		if loc == "" {
			return
		}
		rep, ok, lastErr := p.lastReport(loc)
		if !ok {
			row := plugin.StateRow{
				Label: label,
				Value: i18n.T(q.Lang, "plugin.weather.state.pending", "尚未取得"),
			}
			if lastErr != "" {
				row.Detail = lastErr
				row.Tone = plugin.StateToneMuted
			}
			rows = append(rows, row)
			return
		}
		detail := shortPlace(rep.Place)
		if d := rep.Today; d.known() && d.Date == now.Format("2006-01-02") {
			detail += " · " + i18n.Tf(q.Lang, "plugin.weather.state.today",
				"今天 %s %.0f~%.0f℃", conditions.For(q.Lang, d.Condition), d.MinC, d.MaxC)
		}
		cond := rep.Condition
		if cond == "" {
			cond = condUnknown // 磁盘上的旧缓存可能缺这一项：宁可说不知道，也不摆一个空值
		}
		row := plugin.StateRow{
			Label: label,
			Value: i18n.Tf(q.Lang, "plugin.weather.state.reading", "%s，%.0f℃",
				conditions.For(q.Lang, cond), rep.TempC),
			Detail: detail,
		}
		if s.stale > 0 && now.Sub(rep.Fetched) > s.stale {
			row.Detail = i18n.Tf(q.Lang, "plugin.weather.state.stale",
				"%s（已过期，不参与对话）", detail)
			row.Tone = plugin.StateToneWarn
		}
		if rep.Fetched.After(newest) {
			newest = rep.Fetched
		}
		rows = append(rows, row)
	}

	if s.sameCity {
		add(i18n.T(q.Lang, "plugin.weather.state.both", "同在"), s.personaLoc)
	} else {
		add(i18n.T(q.Lang, "plugin.weather.state.persona", "角色"), s.personaLoc)
		add(i18n.T(q.Lang, "plugin.weather.state.user", "我"), s.userLoc)
	}

	sec := plugin.StateSection{
		Key:       "weather",
		Title:     title,
		Rows:      rows,
		Global:    true,
		UpdatedAt: newest,
	}
	if len(rows) == 0 {
		sec.Empty = empty
	}
	return []plugin.StateSection{sec}
}

var _ plugin.StateReporter = (*Plugin)(nil)
