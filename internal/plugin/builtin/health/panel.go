package health

import (
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// State 把各可见域的身体状况交给设置页的「角色状态」。
// 身体状况属于人格：表里两面各有各的处境，状态页枚举全部域，由界面折叠其它人格。
func (p *Plugin) State(q plugin.StateQuery) []plugin.StateSection {
	if q.PinnedOnly {
		return nil
	}
	s := p.snapshot()
	if s.base == "" {
		return nil
	}
	now := time.Now()

	var out []plugin.StateSection
	failed := false
	for _, tag := range plugin.ReadDomains(s.base, plugin.Scope{}) {
		store := p.storeFor(tag)
		if store == nil {
			continue
		}
		snap, err := store.Current(now)
		if err != nil {
			i18n.Logf(p.langOf(), "log.health.state_failed", "读取身体状况失败（域 %q）: %v", tag, err)
			failed = true
			continue
		}
		var rows []plugin.StateRow
		for _, v := range snap.Active {
			if v.Phase == phaseRecovered {
				continue
			}
			tone := ""
			switch v.Phase {
			case phasePending:
				tone = plugin.StateToneMuted
			case phaseRising:
				tone = plugin.StateToneWarn
			}
			rows = append(rows, plugin.StateRow{
				Value:  v.Cond.Name,
				Detail: statusText(q.Lang, v, now),
				Tone:   tone,
			})
		}
		if len(rows) == 0 {
			continue
		}
		out = append(out, plugin.StateSection{
			Key:     "health",
			Title:   i18n.T(q.Lang, "plugin.health.state.title", "身体"),
			Summary: i18n.Tf(q.Lang, "plugin.health.state.summary", "共 %d 个状况", len(rows)),
			Rows:    rows,
			Domain:  tag,
		})
	}
	if len(out) == 0 {
		if failed {
			return nil
		}
		return []plugin.StateSection{{
			Key:   "health",
			Title: i18n.T(q.Lang, "plugin.health.state.title", "身体"),
			Empty: i18n.T(q.Lang, "plugin.health.state.empty", "现在没有状况。"),
		}}
	}
	return out
}
