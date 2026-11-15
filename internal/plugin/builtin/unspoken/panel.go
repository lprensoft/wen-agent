package unspoken

import (
	"wen/internal/i18n"
	"wen/internal/plugin"
)

const stateMaxRows = 10

// State 把各可见域的心里话交给设置页的「角色状态」。
// 它默认折叠：这些话是角色没说出口的，摊在首屏会改变相处的手感。
func (p *Plugin) State(q plugin.StateQuery) []plugin.StateSection {
	if q.PinnedOnly {
		return nil
	}
	s := p.snapshot()
	if s.base == "" {
		return nil
	}

	var out []plugin.StateSection
	for _, tag := range plugin.ReadDomains(s.base, plugin.Scope{}) {
		store := p.storeFor(tag)
		if store == nil {
			continue
		}
		entries, err := store.List()
		if err != nil {
			i18n.Logf(p.langOf(), "log.unspoken.state_failed", "读取心里话失败（域 %q）: %v", tag, err)
			continue
		}
		if len(entries) == 0 {
			continue
		}
		// 最新的放在最前：状态页要回答「此刻心里压着什么」。
		rows := make([]plugin.StateRow, 0, min(len(entries), stateMaxRows))
		for i := len(entries) - 1; i >= 0 && len(rows) < stateMaxRows; i-- {
			e := entries[i]
			rows = append(rows, plugin.StateRow{
				Meta:  e.Created.Format("2006-01-02"),
				Value: e.Text,
			})
		}
		latest := entries[len(entries)-1].Created
		out = append(out, plugin.StateSection{
			Key:       "unspoken",
			Title:     i18n.T(q.Lang, "plugin.unspoken.state.title", "心里话"),
			Summary:   i18n.Tf(q.Lang, "plugin.unspoken.state.summary", "%d 条", len(entries)),
			Rows:      rows,
			Domain:    tag,
			Collapsed: true,
			UpdatedAt: latest,
			More:      len(entries) - len(rows),
		})
	}
	return out
}
