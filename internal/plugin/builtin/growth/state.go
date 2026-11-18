package growth

import (
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// stateMaxRows 是状态页上最多列几条成长。条数本身有配置上限，这里只是兜长度。
const stateMaxRows = 40

// State 把各可见域的成长交给设置页的「角色状态」。
//
// 待批准的那一条排在最前并标成需要注意：它是唯一需要主人动手的东西。
// 按域分块，其余人格域由界面折叠——成长属于人格，表里两面各长各的。
func (p *Plugin) State(q plugin.StateQuery) []plugin.StateSection {
	if q.PinnedOnly {
		return nil
	}
	s := p.snapshot()
	if s.base == "" {
		return nil
	}

	var out []plugin.StateSection
	failed := false
	for _, tag := range plugin.ReadDomains(s.base, plugin.Scope{}) {
		store := p.storeFor(tag)
		if store == nil {
			continue
		}
		st, err := store.Load()
		if err != nil {
			i18n.Logf(p.langOf(), "log.growth.state_failed", "读取成长状态失败（域 %q）: %v", tag, err)
			failed = true
			continue
		}
		if len(st.Details) == 0 && len(st.Core) == 0 && st.Pending == nil {
			continue
		}

		rows := make([]plugin.StateRow, 0, min(len(st.Details)+len(st.Core)+1, stateMaxRows))
		pendingLabel := i18n.T(q.Lang, "plugin.growth.state.pending", "待批准")
		if st.Pending != nil && len(rows) < stateMaxRows {
			rows = append(rows, plugin.StateRow{
				Label:  pendingLabel,
				Meta:   pendingSince(st.Pending),
				Value:  st.Pending.Text,
				Detail: entryDetail(st.Pending),
				Tone:   plugin.StateToneWarn,
			})
		}
		coreLabel := i18n.T(q.Lang, "plugin.growth.state.core", "核心")
		for _, e := range st.Core {
			if len(rows) >= stateMaxRows {
				break
			}
			rows = append(rows, plugin.StateRow{
				Label:  coreLabel,
				Meta:   e.ID,
				Value:  e.Text,
				Detail: entryDetail(&e),
			})
		}
		detailLabel := i18n.T(q.Lang, "plugin.growth.state.detail", "枝叶")
		for _, e := range st.Details {
			if len(rows) >= stateMaxRows {
				break
			}
			rows = append(rows, plugin.StateRow{
				Label:  detailLabel,
				Meta:   e.ID,
				Value:  e.Text,
				Detail: entryDetail(&e),
			})
		}

		summary := i18n.Tf(q.Lang, "plugin.growth.state.summary", "枝叶 %d · 核心 %d", len(st.Details), len(st.Core))
		if st.Pending != nil {
			summary = i18n.Tf(q.Lang, "plugin.growth.state.summary_pending",
				"枝叶 %d · 核心 %d · 待批准", len(st.Details), len(st.Core))
		}
		out = append(out, plugin.StateSection{
			Key:     "growth",
			Title:   i18n.T(q.Lang, "plugin.growth.state.title", "成长"),
			Summary: summary,
			Rows:    rows,
			Domain:  tag,
			More:    len(st.Details) + len(st.Core) + boolInt(st.Pending != nil) - len(rows),
		})
	}
	if len(out) == 0 {
		if failed {
			return nil
		}
		return []plugin.StateSection{{
			Key:   "growth",
			Title: i18n.T(q.Lang, "plugin.growth.state.title", "成长"),
			Empty: i18n.T(q.Lang, "plugin.growth.state.empty", "还没有长出新的东西。"),
		}}
	}
	return out
}

// pendingSince 是提议的提出日期；零值时留空，不编一个时刻。
func pendingSince(e *Entry) string {
	if e.Since.IsZero() {
		return ""
	}
	return e.Since.Format(time.DateOnly)
}

// entryDetail 拼依据与「为什么这还是自己」。两者都是模型写的自由文本，
// 截到一句能读完的长度。
func entryDetail(e *Entry) string {
	detail := clipText(e.Basis, 80)
	if e.Why != "" {
		if detail != "" {
			detail += " · "
		}
		detail += clipText(e.Why, 80)
	}
	return detail
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
