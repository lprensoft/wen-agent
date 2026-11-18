package plugin

import (
	"fmt"
	"time"

	"wen/internal/i18n"
)

// StateReporter 由需要把自身状态展示给用户的插件实现（可选）。
//
// 它与 StatusReporter 同族，但去处不同：StatusLines 给终端与 IM 一行运行状况，
// State 给 Web UI 的「角色状态」页一块可展开的快照。核心只按注册顺序收集、
// 按功能分组、执行硬上限，不解释任何字段——它不知道「记忆」「日程」这些概念。
//
// 契约在 SystemPrompt / Actions / StatusLines 那几条之外，另有四条：
//   - 只读：不写盘、不调模型、不发网络请求；页面打开时才调用，不是每轮。
//   - 只给人看：绝不进入模型上下文、不落进会话、不经 IM 转发。
//   - 自己加锁：它可能与一轮对话的写入并发，和 StatusLines 一样。
//   - 不返回 error：读不出来的那一块返回 nil 并自行记日志，别让一个插件
//     拖垮整页；空内容用 Empty 表达，不要编。
//
// 静态文字用 i18n.T(q.Lang, "plugin.<name>.state.<key>...", 中文) 取，
// Value / Detail 是运行时事实，原样返回、不翻译——与 ActionDef.Detail 同理。
//
// 需要让主人直接修正条目的插件另实现 StateEditor（见 stateedit.go）：它在行/块上挂
// Edits，写入口与这里分开，本接口的只读契约不变。
type StateReporter interface {
	State(q StateQuery) []StateSection
}

// StateQuery 是这次查询的上下文。
//
// 刻意用结构体而不是参数表：以后要加搜索词、只看某个可见域、只要摘要，
// 都是加字段，三十多个实现不用改签名。
type StateQuery struct {
	Lang string // 按请求判定，不要插件自己存语言
	// PinnedOnly 表示这次只要会进聊天页顶部那一行的块。插件可以据此跳过
	// 明显不会被用到的磁盘读取；核心仍会再过滤一次，防止实现忽略它。
	PinnedOnly bool
	// queriedAt 是这次查询的时刻：同一页上的「N 小时前」都该按同一个时刻算。
	// 用 Now() 取，不要直接读字段。
	queriedAt time.Time
}

// Now 返回这次查询的时刻。同一页上的相对时间都按它算，于是天气、心情、现场
// 不会各按各的钟差上几毫秒；由核心填写。
//
// 零值时退回系统时间：直接构造 StateQuery 的测试与将来别的调用方不必关心这个字段，
// 而把它当公元元年的钟去算衰减会让每一块状态都显示成「很久以前」。
func (q StateQuery) Now() time.Time {
	if q.queriedAt.IsZero() {
		return time.Now()
	}
	return q.queriedAt
}

// NewStateQuery 组装一次状态查询。核心用它把查询时刻统一下来。
func NewStateQuery(lang string, pinnedOnly bool, now time.Time) StateQuery {
	return StateQuery{Lang: lang, PinnedOnly: pinnedOnly, queriedAt: now}
}

// StateSection 是状态页上的一块，通常对应插件里的一个概念。
type StateSection struct {
	Key     string     `json:"key"`               // 插件内唯一；用于调试与将来的折叠状态持久化
	Title   string     `json:"title"`             // 静态标题，已按 Lang 译好
	Summary string     `json:"summary,omitempty"` // 折叠时显示的一行，运行时事实
	Rows    []StateRow `json:"rows,omitempty"`
	Empty   string     `json:"empty,omitempty"` // 没有内容时显示的一句；留空则整块隐藏
	// Domain 是可见域标签，核心不解释。单人格用户永远看不到它；
	// 有多个人格时前端在块上挂一个 chip，并把其它域折叠起来。
	Domain string `json:"domain,omitempty"`
	// Pinned 表示这一块的 Summary 值得进聊天页顶部那一行「此刻」。
	// 由插件自己判断，核心与前端只按注册顺序取前几条。
	Pinned bool `json:"pinned,omitempty"`
	// PinnedOrder 决定聊天页顶部那一行里的先后，小的在前；0 按注册顺序排。
	PinnedOrder int `json:"pinned_order,omitempty"`
	// Collapsed 表示这一块在状态页里默认收起（如心里话）。
	// Global 表示这一块不属于任何一个可见域（如心跳），任何人格下都该显示。
	Global    bool `json:"global,omitempty"`
	Collapsed bool `json:"collapsed,omitempty"`
	// UpdatedAt 是这份状态最后一次变化的时刻，零值表示不知道（前端不显示）。
	// 不是查询时刻：天气、现场、心情都可能停在几小时前，读的人要知道。
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	// More 是被截掉的条数。先由插件按自己的语义截（最近 30 条），
	// 核心再兜一层硬上限；前端渲染成「还有 12 条」。
	More int `json:"more,omitempty"`
	// Edits 是块级操作（新增一条、整块重置）。行级操作挂在 StateRow.Edits 上；
	// 两者用同一种描述，请求里以 Row 是否为空区分。pinned 查询会裁掉它们。
	Edits []StateEdit `json:"edits,omitempty"`
}

// StateRow 是状态页上的一行，只有标签与值，不做富文本。
//
// 两种用法，按 Label 是否为空区分：
//   - 属性行：Label 是静态属性名（所在、阶段、称呼），Value 是运行时事实；
//   - 列表行：Label 为空，Meta 放时间或分类，Value 放条目名字。
//
// Value / Detail 可能来自记忆、人物、心里话，渲染一律用 textContent，
// 绝不能用 innerHTML——那是把用户与模型写的文本当 HTML 执行。
type StateRow struct {
	Label  string `json:"label,omitempty"`  // 静态属性名，已译
	Meta   string `json:"meta,omitempty"`   // 运行时前缀：时间、分类、序号；不译
	Value  string `json:"value"`            // 运行时主值
	Detail string `json:"detail,omitempty"` // 次要说明，纯文本，不译
	// Tone 只影响颜色，不承载语义："" 正常、"muted" 次要、"warn" 需要注意。
	Tone string `json:"tone,omitempty"`
	// ID 是这一行的不透明身份，编辑操作靠它指回来；空表示不可编辑。
	// 插件实现要在 EditState 里按它核对，不能当路径或索引直接使用。
	ID string `json:"id,omitempty"`
	// Edits 是这一行可用的修改操作。pinned 查询会裁掉它们。
	Edits []StateEdit `json:"edits,omitempty"`
}

const (
	StateToneMuted = "muted"
	StateToneWarn  = "warn"
)

// PluginState 是一个插件贡献的状态块。
type PluginState struct {
	Name        string         `json:"name"`
	Category    string         `json:"category"`     // 译名
	CategoryKey string         `json:"category_key"` // 稳定标识，界面按它分组
	Sections    []StateSection `json:"sections"`
	// Truncated 表示有整块被硬上限丢掉；行被压短只报在那一块的 More 上。
	Truncated bool `json:"truncated,omitempty"`
}

// StateSnapshot 是 /api/state 的响应。
type StateSnapshot struct {
	Plugins   []PluginState `json:"plugins"`
	Truncated bool          `json:"truncated,omitempty"` // 有内容被整块丢掉，界面提示「已截断」
}

const (
	// maxStateSections 是单个插件最多贡献几块；超过的丢掉。
	maxStateSections = 12
	// maxStateRows 是单块最多几行；超过的行累加进 More。
	maxStateRows = 60
	// maxStatePerPlugin 是单个插件的字节上限。记忆、人物这些列表要先由插件
	// 自己截到可读的条数，这里是兜底，防止某个实现把整库倒出来。
	maxStatePerPlugin = 48 << 10
	// maxStateTotal 是所有插件合计的字节上限。
	maxStateTotal = 512 << 10
)

// State 汇总启用插件的状态块（按注册顺序）。
//
// 与 List / StatusLines 一样：锁内只取插件实例，锁外再调插件代码；单个插件
// panic 只少一块，不影响整页。分类的译名在管理侧完成，与 /api/plugins 同源。
func (m *Manager) State(lang string, pinnedOnly bool) StateSnapshot {
	out := StateSnapshot{Plugins: []PluginState{}}
	total := 0
	for _, e := range enabledAs[StateReporter](m) {
		var secs []StateSection
		q := NewStateQuery(lang, pinnedOnly, time.Now())
		safely(m.lang(), e.name, "State", func() { secs = e.impl.State(q) })
		if pinnedOnly {
			secs = pinnedSections(secs)
		}
		secs, truncated := clampState(secs)
		if len(secs) == 0 {
			// 整块都被上限丢掉了：没有地方挂「已截断」，报到总标记上，
			// 否则界面看起来就像这个插件什么都没报。
			if truncated {
				out.Truncated = true
			}
			continue
		}
		cat := CategoryOther
		if pl, ok := any(e.impl).(Plugin); ok {
			safely(m.lang(), e.name, "Category", func() { cat = CategoryOf(pl) })
		}
		catKey := CategoryKeyOf(cat)
		n := stateSize(secs)
		if total+n > maxStateTotal {
			out.Truncated = true
			break
		}
		total += n
		out.Plugins = append(out.Plugins, PluginState{
			Name:        e.name,
			Category:    i18n.T(lang, CategoryTextKey(catKey), cat),
			CategoryKey: catKey,
			Sections:    secs,
			Truncated:   truncated,
		})
	}
	return out
}

// pinnedSections 只保留会进聊天页顶部那一行的块。
//
// 顶部只要 Summary：编辑描述与行身份一并裁掉，既让这条每轮都发的请求保持小，
// 也不把可写入口暴露在一个只用来瞄一眼的地方。不改插件返回的切片。
func pinnedSections(in []StateSection) []StateSection {
	out := make([]StateSection, 0, len(in))
	for _, s := range in {
		if !s.Pinned {
			continue
		}
		s.Edits = nil
		if len(s.Rows) > 0 {
			rows := make([]StateRow, len(s.Rows))
			for i, r := range s.Rows {
				r.ID, r.Edits = "", nil
				rows[i] = r
			}
			s.Rows = rows
		}
		out = append(out, s)
	}
	return out
}

// clampState 执行硬上限。不改插件返回的切片：只在需要裁剪行时复制一份行，
// 整块丢弃只影响这一次输出，下一次列表仍拿到插件原本的内容。
func clampState(in []StateSection) ([]StateSection, bool) {
	truncated := false
	if len(in) > maxStateSections {
		in = in[:maxStateSections]
		truncated = true
	}
	out := make([]StateSection, 0, len(in))
	total := 0
	for _, s := range in {
		s.Edits = clampEdits(s.Edits)
		needCopy := len(s.Rows) > maxStateRows
		if !needCopy {
			for _, r := range s.Rows {
				if len(r.Edits) > 0 {
					needCopy = true
					break
				}
			}
		}
		if needCopy {
			if len(s.Rows) > maxStateRows {
				// 行数超出只是这一块被压短，前端会显示「还有 N 条」；
				// Truncated 留给整块被丢的情况，两者不能混成同一个信号。
				s.More += len(s.Rows) - maxStateRows
				s.Rows = append([]StateRow(nil), s.Rows[:maxStateRows]...)
			} else {
				s.Rows = append([]StateRow(nil), s.Rows...)
			}
			for i := range s.Rows {
				s.Rows[i].Edits = clampEdits(s.Rows[i].Edits)
			}
		}
		n := stateSectionSize(s)
		if total+n > maxStatePerPlugin {
			truncated = true
			continue
		}
		total += n
		out = append(out, s)
	}
	return out, truncated
}

// stateSize 粗略估算一组状态块的 UTF-8 字节数。只用于限幅，不做精确计量。
func stateSize(secs []StateSection) int {
	n := 0
	for _, s := range secs {
		n += stateSectionSize(s)
	}
	return n
}

func stateSectionSize(s StateSection) int {
	n := len(s.Key) + len(s.Title) + len(s.Summary) + len(s.Empty) + len(s.Domain)
	for _, e := range s.Edits {
		n += stateEditSize(e)
	}
	for _, r := range s.Rows {
		n += len(r.Label) + len(r.Meta) + len(r.Value) + len(r.Detail) + len(r.Tone) + len(r.ID)
		for _, e := range r.Edits {
			n += stateEditSize(e)
		}
	}
	return n
}

// EditState 把一次状态修正请求交给插件。
//
// 与 StartAction 一样只在锁内取实例、锁外调用；错误文案面向界面，按请求语言取。
// 插件不存在、未启用、未实现 StateEditor 都拒绝——不能因为界面上显示过一行，
// 就假设那个插件此刻仍然可以写。
func (m *Manager) EditState(name string, req StateEditRequest) error {
	m.mu.RLock()
	e, ok := m.entries[name]
	var (
		enabled = ok && e.enabled && e.inited
		editor  StateEditor
	)
	if enabled {
		editor, _ = any(e.plugin).(StateEditor)
	}
	m.mu.RUnlock()

	switch {
	case !ok:
		return fmt.Errorf(i18n.T(req.Lang, "server.err.plugin_not_found", "插件 %q 不存在"), name)
	case !enabled:
		return fmt.Errorf(i18n.T(req.Lang, "server.err.plugin_disabled", "插件 %q 未启用"), name)
	case editor == nil:
		return fmt.Errorf(i18n.T(req.Lang, "server.err.state_edit_unsupported", "插件 %q 的状态不可修改"), name)
	}
	return safelyErr(req.Lang, name, "EditState", func() error { return editor.EditState(req) })
}
