package memory

import (
	"sort"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// stateIndexLimit 是状态页上最多列几条记忆。插件方向来会先截到可读的条数，
// 核心还有一层硬上限；这里取三十，够看清最近记住的事，又不会把页面撑长。
const stateIndexLimit = 30

// State 把记忆索引交给设置页的「角色状态」。
//
// 按可见域分库，一块对应一条故事线；状态页是主人看的，所以枚举全部域，
// 每块挂 Domain 标签，由界面折叠其它人格。正文不发——索引是标题与一句钩子，
// 想看全文仍走 recall_memory 或编辑器。
func (p *Plugin) State(q plugin.StateQuery) []plugin.StateSection {
	p.mu.RLock()
	base := p.libBase
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
		entries, err := store.List()
		if err != nil {
			i18n.Logf(p.langOf(), "log.memory.state_failed", "读取记忆索引失败（域 %q）: %v", tag, err)
			failed = true
			continue
		}
		if len(entries) == 0 {
			continue
		}
		// Store.List 返回的是它自己的缓存切片；状态页要按更新时间重排，
		// 必须先复制，否则会把索引与召回共用的缓存顺序改掉。
		entries = append([]Entry(nil), entries...)
		sort.SliceStable(entries, func(i, j int) bool {
			if !entries[i].Updated.Equal(entries[j].Updated) {
				return entries[i].Updated.After(entries[j].Updated)
			}
			return entries[i].Name < entries[j].Name
		})

		total := len(entries)
		rows := make([]plugin.StateRow, 0, min(total, stateIndexLimit))
		latest := entries[0].Updated
		if latest.IsZero() {
			latest = entries[0].Created
		}
		for _, e := range entries {
			if len(rows) >= stateIndexLimit {
				break
			}
			detail := e.Description
			if e.Blurred {
				note := i18n.T(q.Lang, "plugin.memory.state.index.blurred", "细节已淡忘")
				if detail != "" {
					detail += " · "
				}
				detail += note
			}
			tone := ""
			if e.Blurred {
				tone = plugin.StateToneMuted
			}
			rows = append(rows, plugin.StateRow{
				Meta:   memoryTypeLabel(q.Lang, e.Type),
				Value:  e.Name,
				Detail: detail,
				Tone:   tone,
			})
		}
		out = append(out, plugin.StateSection{
			Key:       "index",
			Title:     i18n.T(q.Lang, "plugin.memory.state.index.title", "记忆索引"),
			Summary:   i18n.Tf(q.Lang, "plugin.memory.state.index.count", "%d 条", total),
			Rows:      rows,
			Domain:    tag,
			UpdatedAt: latest,
			More:      total - len(rows),
		})
	}
	if len(out) == 0 {
		if failed {
			// 读失败与「确实没有记忆」是两件事。前者不该显示成空状态，
			// 否则界面会肯定地告诉用户「她什么都没记住」。
			return nil
		}
		return []plugin.StateSection{{
			Key:   "index",
			Title: i18n.T(q.Lang, "plugin.memory.state.index.title", "记忆索引"),
			Empty: i18n.T(q.Lang, "plugin.memory.state.index.empty", "还没有记住任何事。"),
		}}
	}
	return out
}

// memoryTypeLabel 把落盘的规范中文分类换成界面语言。键写成字面量：
// 语言包覆盖检查是静态扫 (键, 中文) 这一对的，拼出来的键它看不见。
func memoryTypeLabel(lang, t string) string {
	switch t {
	case "偏好":
		return i18n.T(lang, "plugin.memory.state.type.preference", "偏好")
	case "约定":
		return i18n.T(lang, "plugin.memory.state.type.agreement", "约定")
	case "事实":
		return i18n.T(lang, "plugin.memory.state.type.fact", "事实")
	case "踩坑":
		return i18n.T(lang, "plugin.memory.state.type.pitfall", "踩坑")
	case "经历":
		return i18n.T(lang, "plugin.memory.state.type.experience", "经历")
	case "边界":
		return i18n.T(lang, "plugin.memory.state.type.boundary", "边界")
	}
	return t
}
