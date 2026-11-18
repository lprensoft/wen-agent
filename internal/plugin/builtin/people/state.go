package people

import (
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// stateMaxRows 是状态页上最多列几个人。人数上限本身就有配置，这里只是让
// 「一个人都没有」与「多到读不完」都不至于把页面撑长。
const stateMaxRows = 30

// State 把各可见域的人物交给设置页的「角色状态」。
//
// 状态页是主人看的，所以枚举磁盘上存在的全部域，每块挂一个 Domain 标签，
// 由界面折叠其它人格。清单正文不发——这里只给名册，具体来往仍走记忆。
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
		ps, err := store.List()
		if err != nil {
			i18n.Logf(p.langOf(), "log.people.state_failed", "读取人物状态失败（域 %q）: %v", tag, err)
			failed = true
			continue
		}
		if len(ps) == 0 {
			continue
		}

		rows := make([]plugin.StateRow, 0, min(len(ps), stateMaxRows))
		var latest time.Time
		for _, person := range ps {
			if person.Updated.After(latest) {
				latest = person.Updated
			}
			if len(rows) >= stateMaxRows {
				continue
			}
			rows = append(rows, plugin.StateRow{
				Meta:   stateClosenessLabel(q.Lang, person.Closeness),
				Value:  person.Name,
				Detail: personDetail(q.Lang, person),
			})
		}
		out = append(out, plugin.StateSection{
			Key:       "people",
			Title:     i18n.T(q.Lang, "plugin.people.state.title", "人物"),
			Summary:   i18n.Tf(q.Lang, "plugin.people.state.count", "%d 人", len(ps)),
			Rows:      rows,
			Domain:    tag,
			UpdatedAt: latest,
			More:      len(ps) - len(rows),
		})
	}
	if len(out) == 0 {
		if failed {
			return nil
		}
		return []plugin.StateSection{{
			Key:   "people",
			Title: i18n.T(q.Lang, "plugin.people.state.title", "人物"),
			Empty: i18n.T(q.Lang, "plugin.people.state.empty", "还没有认识的人。"),
		}}
	}
	return out
}

// personDetail 拼一行人的人设与上次来往。设定是模型写的自由文本，截到一句
// 能读完的长度；日期用中性的 ISO 写法，不另造相对时间的说法。
func personDetail(lang string, person Person) string {
	detail := person.Relation
	if person.Profile != "" {
		if detail != "" {
			detail += " · "
		}
		detail += clipText(person.Profile, 60)
	}
	if !person.LastMet.IsZero() {
		if detail != "" {
			detail += " · "
		}
		detail += i18n.Tf(lang, "plugin.people.state.last_met", "上次 %s", person.LastMet.Format("2006-01-02"))
	}
	return detail
}

func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// stateClosenessLabel 把落盘的规范中文亲近度换成界面语言。键写成字面量：
// 语言包覆盖检查是静态扫 (键, 中文) 这一对的，拼出来的键它看不见。
func stateClosenessLabel(lang, v string) string {
	switch v {
	case "点头之交":
		return i18n.T(lang, "plugin.people.state.closeness.nodding", "点头之交")
	case "认识":
		return i18n.T(lang, "plugin.people.state.closeness.known", "认识")
	case "熟":
		return i18n.T(lang, "plugin.people.state.closeness.close", "熟")
	case "亲近":
		return i18n.T(lang, "plugin.people.state.closeness.intimate", "亲近")
	}
	return v
}
