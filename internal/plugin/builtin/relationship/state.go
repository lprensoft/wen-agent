package relationship

import (
	"strings"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// State 把各可见域的关系快照交给设置页的「角色状态」。
//
// 关系属于人格：表人格与里人格各有一份，状态页枚举全部域，每块挂 Domain 标签，
// 由界面把其它人格折叠起来。没有数据的域不出现，避免一页全是空块。
func (p *Plugin) State(q plugin.StateQuery) []plugin.StateSection {
	if q.PinnedOnly {
		return nil
	}
	p.mu.RLock()
	base := p.base
	p.mu.RUnlock()
	if base == "" {
		return nil
	}

	var out []plugin.StateSection
	for _, tag := range plugin.ReadDomains(base, plugin.Scope{}) {
		store := p.storeFor(tag)
		if store == nil {
			continue
		}
		snap, ok, err := store.Load()
		if err != nil {
			i18n.Logf(p.langOf(), "log.relationship.state_failed", "读取关系状态失败（域 %q）: %v", tag, err)
			continue
		}
		if !ok || snap.Empty() {
			continue
		}
		out = append(out, plugin.StateSection{
			Key:       "relationship",
			Title:     i18n.T(q.Lang, "plugin.relationship.state.title", "关系"),
			Summary:   relationshipSummary(snap),
			Rows:      relationshipRows(q.Lang, snap),
			Domain:    tag,
			UpdatedAt: snap.Updated,
			Edits:     relationshipEdits(q.Lang, snap),
		})
	}
	return out
}

// 状态页上的操作标识。静态、不翻译。
const (
	stateSection = "relationship"
	stateOpEdit  = "edit"
)

// relationshipEdits 是这一段关系的字段修正。日子不在这里改：那些是由代码按
// 阶段与纪念日维护的，单个删改走模型侧的 remove_date / add_date。
func relationshipEdits(lang string, snap Snapshot) []plugin.StateEdit {
	return []plugin.StateEdit{{
		Op:    stateOpEdit,
		Label: i18n.T(lang, "plugin.relationship.state.edit", "修改关系"),
		Fields: []plugin.StateField{
			{
				Key: "stage", Label: i18n.T(lang, "plugin.relationship.state.field.stage", "阶段"),
				Type: plugin.StateFieldText, Value: snap.Stage, MaxLen: maxStageRunes,
			},
			{
				Key: "my_call", Label: i18n.T(lang, "plugin.relationship.state.field.my_call", "你对对方的称呼"),
				Type: plugin.StateFieldText, Value: snap.MyCall, MaxLen: maxCallRunes,
			},
			{
				Key: "their_call", Label: i18n.T(lang, "plugin.relationship.state.field.their_call", "对方对你的称呼"),
				Type: plugin.StateFieldText, Value: snap.TheirCall, MaxLen: maxCallRunes,
			},
			{
				Key: "recent", Label: i18n.T(lang, "plugin.relationship.state.field.recent", "最近变动"),
				Type: plugin.StateFieldText, Value: snap.Recent, MaxLen: maxRecentRunes,
			},
			{
				Key: "their_now", Label: i18n.T(lang, "plugin.relationship.state.field.their_now", "对方近况"),
				Type: plugin.StateFieldText, Value: snap.TheirNow, MaxLen: maxTheirNowRunes,
			},
			{
				Key: "bonds", Label: i18n.T(lang, "plugin.relationship.state.field.bonds", "默契与禁区（一行一条）"),
				Type: plugin.StateFieldTextarea, Value: strings.Join(snap.Bonds, "\n"),
				MaxLen: maxBonds*maxBondRunes + maxBonds - 1,
			},
		},
	}}
}

// relationshipSummary 用阶段（没有就用最近变动）拼一行折叠摘要。
func relationshipSummary(snap Snapshot) string {
	if snap.Stage != "" {
		return snap.Stage
	}
	if snap.Recent != "" {
		return snap.Recent
	}
	if len(snap.Bonds) > 0 {
		return snap.Bonds[0]
	}
	return ""
}

func relationshipRows(lang string, snap Snapshot) []plugin.StateRow {
	var rows []plugin.StateRow
	add := func(label, value string) {
		if value != "" {
			rows = append(rows, plugin.StateRow{Label: label, Value: value})
		}
	}
	add(relationshipLabel(lang, "stage"), snap.Stage)
	if snap.MyCall != "" || snap.TheirCall != "" {
		calls := snap.MyCall
		if snap.TheirCall != "" {
			if calls != "" {
				calls += " / "
			}
			calls += snap.TheirCall
		}
		add(relationshipLabel(lang, "calls"), calls)
	}
	if snap.Recent != "" {
		row := plugin.StateRow{Label: relationshipLabel(lang, "recent"), Value: snap.Recent}
		if !snap.RecentAt.IsZero() {
			row.Detail = snap.RecentAt.Format("2006-01-02")
		}
		rows = append(rows, row)
	}
	if len(snap.Bonds) > 0 {
		sep := ", "
		if lang == "zh" {
			sep = "、"
		}
		add(relationshipLabel(lang, "bonds"), strings.Join(snap.Bonds, sep))
	}
	add(relationshipLabel(lang, "their_now"), snap.TheirNow)
	for _, d := range snap.Dates {
		rows = append(rows, plugin.StateRow{
			Label: relationshipLabel(lang, "dates"),
			Meta:  d.Date,
			Value: d.What,
		})
	}
	return rows
}

// relationshipLabel 的键写成字面量，语言包覆盖检查靠静态扫 (键, 中文) 这一对。
func relationshipLabel(lang, key string) string {
	switch key {
	case "stage":
		return i18n.T(lang, "plugin.relationship.state.stage", "阶段")
	case "calls":
		return i18n.T(lang, "plugin.relationship.state.calls", "称呼")
	case "recent":
		return i18n.T(lang, "plugin.relationship.state.recent", "最近")
	case "bonds":
		return i18n.T(lang, "plugin.relationship.state.bonds", "默契")
	case "their_now":
		return i18n.T(lang, "plugin.relationship.state.their_now", "对方近况")
	case "dates":
		return i18n.T(lang, "plugin.relationship.state.dates", "纪念日")
	}
	return key
}
