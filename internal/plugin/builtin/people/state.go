package people

import (
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// stateMaxRows 是状态页上最多列几个人。人数上限本身就有配置，这里只是让
// 「一个人都没有」与「多到读不完」都不至于把页面撑长。
const stateMaxRows = 30

// 状态页上的操作标识。静态、不翻译。
const (
	stateSection  = "people"
	stateOpEdit   = "edit"
	stateOpDelete = "delete"
	stateOpAdd    = "add"
)

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
				ID:     person.Name,
				Edits:  stateRowEdits(q.Lang, person),
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
			Edits:     stateAddEdits(q.Lang),
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
			Edits: stateAddEdits(q.Lang),
		}}
	}
	return out
}

// stateRowEdits 是一行人物上的操作：修改各字段，或从名册里移除。
func stateRowEdits(lang string, person Person) []plugin.StateEdit {
	return []plugin.StateEdit{
		{
			Op:     stateOpEdit,
			Label:  i18n.T(lang, "plugin.people.state.edit", "修改"),
			Fields: stateEditFields(lang, person),
		},
		{
			Op:      stateOpDelete,
			Label:   i18n.T(lang, "plugin.people.state.delete", "移除"),
			Confirm: i18n.T(lang, "plugin.people.state.delete_confirm", "从人物库移除这个人？之后演绎里不会再提到他/她。"),
			Danger:  true,
		},
	}
}

// stateEditFields 是修改表单。名字是落盘身份，本轮只读，不在表单里出现。
func stateEditFields(lang string, person Person) []plugin.StateField {
	return []plugin.StateField{
		{
			Key: "relation", Label: i18n.T(lang, "plugin.people.state.field.relation", "关系"),
			Type: plugin.StateFieldText, Value: person.Relation, MaxLen: maxRelationRunes, Required: true,
		},
		{
			Key: "closeness", Label: i18n.T(lang, "plugin.people.state.field.closeness", "亲近度"),
			Type: plugin.StateFieldSelect, Value: person.Closeness, Options: stateClosenessOptions(lang),
		},
		{
			Key: "profile", Label: i18n.T(lang, "plugin.people.state.field.profile", "设定"),
			Type: plugin.StateFieldTextarea, Value: person.Profile, MaxLen: maxProfileRunes,
		},
		{
			Key: "lately", Label: i18n.T(lang, "plugin.people.state.field.lately", "近况"),
			Type: plugin.StateFieldText, Value: person.Lately, MaxLen: maxLatelyRunes,
		},
	}
}

// stateAddFields 是登记新人物。关系必填，亲近度缺省为「认识」。
func stateAddFields(lang string) []plugin.StateField {
	return []plugin.StateField{
		{
			Key: "name", Label: i18n.T(lang, "plugin.people.state.field.name", "名字"),
			Type: plugin.StateFieldText, MaxLen: maxNameRunes, Required: true,
		},
		{
			Key: "relation", Label: i18n.T(lang, "plugin.people.state.field.relation", "关系"),
			Type: plugin.StateFieldText, MaxLen: maxRelationRunes, Required: true,
		},
		{
			Key: "closeness", Label: i18n.T(lang, "plugin.people.state.field.closeness", "亲近度"),
			Type: plugin.StateFieldSelect, Value: defaultCloseness, Options: stateClosenessOptions(lang),
		},
		{
			Key: "profile", Label: i18n.T(lang, "plugin.people.state.field.profile", "设定"),
			Type: plugin.StateFieldTextarea, MaxLen: maxProfileRunes,
		},
	}
}

func stateAddEdits(lang string) []plugin.StateEdit {
	return []plugin.StateEdit{{
		Op:     stateOpAdd,
		Label:  i18n.T(lang, "plugin.people.state.add", "登记新人物"),
		Fields: stateAddFields(lang),
	}}
}

func stateClosenessOptions(lang string) []plugin.StateOption {
	values := closenessLevels.ZHValues()
	out := make([]plugin.StateOption, 0, len(values))
	for _, v := range values {
		out = append(out, plugin.StateOption{Value: v, Label: stateClosenessLabel(lang, v)})
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
