package presence

import (
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// State 把当前域的现场快照交给设置页的「角色状态」。
//
// 快照只读写本轮写入域，但状态页是主人看的，所以这里枚举磁盘上存在的全部域，
// 每块挂一个 Domain 标签——界面按它把其它人格折叠起来。单人格用户没有标签。
func (p *Plugin) State(q plugin.StateQuery) []plugin.StateSection {
	p.mu.RLock()
	base := p.base
	p.mu.RUnlock()
	if base == "" {
		return nil
	}

	var out []plugin.StateSection
	failed := false
	for _, tag := range plugin.ReadDomains(base, plugin.Scope{}) {
		store := p.storeFor(tag)
		if store == nil {
			continue
		}
		snap, ok, err := store.Load()
		if err != nil {
			i18n.Logf(p.snapshotLang(), "log.presence.state_failed", "读取现场状态失败（域 %q）: %v", tag, err)
			failed = true
			continue
		}
		if !ok {
			continue
		}
		rows, latest := presenceRows(q.Lang, snap)
		if len(rows) == 0 {
			continue
		}
		out = append(out, plugin.StateSection{
			Key:         "presence",
			Title:       i18n.T(q.Lang, "plugin.presence.state.title", "现场"),
			Summary:     presenceSummary(rows),
			Rows:        rows,
			Domain:      tag,
			Pinned:      true,
			PinnedOrder: 1,
			UpdatedAt:   latest,
			Edits:       presenceEdits(q.Lang, snap),
		})
	}
	if len(out) == 0 {
		if failed {
			return nil
		}
		return []plugin.StateSection{{
			Key:   "presence",
			Title: i18n.T(q.Lang, "plugin.presence.state.title", "现场"),
			Empty: i18n.T(q.Lang, "plugin.presence.state.empty", "还没有记录现场。"),
			Edits: presenceEdits(q.Lang, Snapshot{}),
		}}
	}
	return out
}

// 状态页上的操作标识。静态、不翻译。
const (
	stateSection = "presence"
	stateOpEdit  = "edit"
)

// presenceEdits 是这一域现场的快照修正：六个字段都能改，清空即删除该字段。
func presenceEdits(lang string, snap Snapshot) []plugin.StateEdit {
	fields := make([]plugin.StateField, 0, len(fieldDefs))
	for _, d := range fieldDefs {
		value := ""
		if f, ok := snap[d.key]; ok {
			value = f.Text
		}
		fields = append(fields, plugin.StateField{
			Key: d.key, Label: presenceFieldLabel(lang, d.key),
			Type: plugin.StateFieldText, Value: value, MaxLen: d.maxRunes,
		})
	}
	return []plugin.StateEdit{{
		Op:     stateOpEdit,
		Label:  i18n.T(lang, "plugin.presence.state.edit", "修改现场"),
		Fields: fields,
	}}
}

// presenceRows 按 fieldDefs 的固定顺序生成行；字段的顺序是给读的人看的，
// 与快照里 map 的遍历顺序无关。
func presenceRows(lang string, snap Snapshot) ([]plugin.StateRow, time.Time) {
	var rows []plugin.StateRow
	var latest time.Time
	for _, d := range fieldDefs {
		f, ok := snap[d.key]
		if !ok || strings.TrimSpace(f.Text) == "" {
			continue
		}
		rows = append(rows, plugin.StateRow{
			Label: presenceFieldLabel(lang, d.key),
			Value: f.Text,
		})
		if f.Updated.After(latest) {
			latest = f.Updated
		}
	}
	return rows, latest
}

// presenceSummary 用前三个字段拼一行折叠摘要。值本身是运行时事实，连接符
// 用中性的「·」，不翻译——它不构成一句话。
func presenceSummary(rows []plugin.StateRow) string {
	var parts []string
	for _, r := range rows {
		if len(parts) == 3 {
			break
		}
		parts = append(parts, r.Value)
	}
	return strings.Join(parts, " · ")
}

// presenceFieldLabel 的键必须写成字面量：语言包覆盖检查是静态扫 (键, 中文)
// 这一对的，拼出来的键它看不见。
func presenceFieldLabel(lang, key string) string {
	switch key {
	case "location":
		return i18n.T(lang, "plugin.presence.state.field.location", "所在")
	case "attire":
		return i18n.T(lang, "plugin.presence.state.field.attire", "穿着")
	case "posture":
		return i18n.T(lang, "plugin.presence.state.field.posture", "姿态")
	case "activity":
		return i18n.T(lang, "plugin.presence.state.field.activity", "在做")
	case "effects":
		return i18n.T(lang, "plugin.presence.state.field.effects", "持续状态")
	case "focus":
		return i18n.T(lang, "plugin.presence.state.field.focus", "感官焦点")
	}
	return key
}
