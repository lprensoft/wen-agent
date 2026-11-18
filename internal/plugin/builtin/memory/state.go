package memory

import (
	"sort"
	"strings"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// stateIndexLimit 是状态页上最多列几条记忆。插件方向来会先截到可读的条数，
// 核心还有一层硬上限；这里取三十，够看清最近记住的事，又不会把页面撑长。
const stateIndexLimit = 30

// stateDiaryLimit 是状态页上最多列几天的日记。时间线本身留九十条，页面只要最近
// 一段；更早的靠会话检索。
const stateDiaryLimit = 30

// State 把记忆索引与日记交给设置页的「角色状态」。
//
// 按可见域分库，一块对应一条故事线；状态页是主人看的，所以枚举全部域，
// 每块挂 Domain 标签，由界面折叠其它人格。正文不发——索引是标题与一句钩子，
// 想看全文仍走 recall_memory 或编辑器。日记读的是已收束的时间线，不是当日缓冲：
// 当天还没结束，那一行还没写下来。
func (p *Plugin) State(q plugin.StateQuery) []plugin.StateSection {
	if q.PinnedOnly {
		return nil
	}
	s := p.snapshot()
	if s.libBase == "" {
		return nil
	}

	var out []plugin.StateSection
	failed := false
	for _, tag := range plugin.ReadDomains(s.libBase, plugin.Scope{}) {
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
				ID:     e.Name,
				Edits:  indexEdits(q.Lang, e),
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

	out = append(out, p.diarySections(q.Lang, s.libBase)...)
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

// diarySections 把各域的已收束时间线读成状态块。
//
// 只读 timeline.json，不读当日缓冲：当天的日记还没收束，那一行还不存在。
// 时间线在 appendTimeline 里按日期升序保存；这里复制一份按新到旧排给页面看。
// 没有记录的域不出块——日记为空时整块隐藏，不给每个用户多摆一块空版面。
func (p *Plugin) diarySections(lang, base string) []plugin.StateSection {
	var out []plugin.StateSection
	for _, tag := range plugin.ReadDomains(base, plugin.Scope{}) {
		dir := plugin.DomainDir(base, tag)
		p.timelineMu.Lock()
		f := loadTimeline(dir)
		p.timelineMu.Unlock()
		if len(f.Entries) == 0 {
			continue
		}
		entries := append([]timelineEntry(nil), f.Entries...)
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].Date > entries[j].Date })

		total := len(entries)
		rows := make([]plugin.StateRow, 0, min(total, stateDiaryLimit))
		for _, e := range entries {
			if len(rows) >= stateDiaryLimit {
				break
			}
			rows = append(rows, plugin.StateRow{
				Meta:   e.Date,
				Value:  e.Summary,
				Detail: strings.Join(e.Facts, " · "),
				ID:     e.Date,
				Edits:  diaryEdits(lang, e),
			})
		}
		out = append(out, plugin.StateSection{
			Key:     "diary",
			Title:   i18n.T(lang, "plugin.memory.state.diary.title", "日记"),
			Summary: i18n.Tf(lang, "plugin.memory.state.diary.summary", "最近 %d 天", total),
			Rows:    rows,
			Domain:  tag,
			More:    total - len(rows),
		})
	}
	return out
}

// 状态页上的操作标识。静态、不翻译。
const (
	stateSectionIndex = "index"
	stateSectionDiary = "diary"
	stateOpEdit       = "edit"
	stateOpDelete     = "delete"
)

// indexEdits 是一行记忆索引上的操作：改摘要/分类，或删除。
// 标题是落盘身份，本轮不改名；正文只在 recall 时读，也不在这里改。
func indexEdits(lang string, e Entry) []plugin.StateEdit {
	return []plugin.StateEdit{
		{
			Op:    stateOpEdit,
			Label: i18n.T(lang, "plugin.memory.state.edit", "修改"),
			Fields: []plugin.StateField{
				{
					Key: "description", Label: i18n.T(lang, "plugin.memory.state.field.description", "摘要"),
					Type: plugin.StateFieldText, Value: e.Description, MaxLen: descMaxRunes,
				},
				{
					Key: "type", Label: i18n.T(lang, "plugin.memory.state.field.type", "分类"),
					Type: plugin.StateFieldSelect, Value: e.Type, Options: memoryTypeOptions(lang),
				},
			},
		},
		{
			Op:      stateOpDelete,
			Label:   i18n.T(lang, "plugin.memory.state.delete", "删除"),
			Confirm: i18n.T(lang, "plugin.memory.state.delete_confirm", "删除这条记忆？删掉后角色就不再记得它。"),
			Danger:  true,
		},
	}
}

// diaryEdits 是一行日记上的操作：改摘要，或删掉那一天。
func diaryEdits(lang string, e timelineEntry) []plugin.StateEdit {
	return []plugin.StateEdit{
		{
			Op:    stateOpEdit,
			Label: i18n.T(lang, "plugin.memory.state.edit", "修改"),
			Fields: []plugin.StateField{{
				Key: "summary", Label: i18n.T(lang, "plugin.memory.state.field.summary", "摘要"),
				Type: plugin.StateFieldText, Value: e.Summary, MaxLen: maxSummaryRunes, Required: true,
			}},
		},
		{
			Op:      stateOpDelete,
			Label:   i18n.T(lang, "plugin.memory.state.delete", "删除"),
			Confirm: i18n.T(lang, "plugin.memory.state.diary_delete_confirm", "删除这一天的日记？删掉后不会重新生成。"),
			Danger:  true,
		},
	}
}

func memoryTypeOptions(lang string) []plugin.StateOption {
	values := memTypes.ZHValues()
	out := make([]plugin.StateOption, 0, len(values))
	for _, v := range values {
		out = append(out, plugin.StateOption{Value: v, Label: memoryTypeLabel(lang, v)})
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
