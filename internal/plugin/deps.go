package plugin

import (
	"fmt"
	"slices"
	"strings"

	"wen/internal/i18n"
)

// Dependent 是插件的可选能力：声明必须同时启用的其他插件。
// 依赖是硬性的——依赖未满足时插件无法启用，被依赖的插件也无法在依赖方仍启用时关闭。
type Dependent interface {
	Requires() []string
}

// Conflicting 是插件的可选能力：声明与之能力相抵的其他插件。
// 冲突只告警不阻止：声明冲突的一方往往是「隔离」类插件，而被冲突的一方是通用能力，
// 用户可能确实两者都要，代价由用户自己权衡。
type Conflicting interface {
	Conflicts() []string
}

// RequiresOf 返回插件声明的依赖；未实现 Dependent 时返回 nil。
func RequiresOf(p Plugin) []string {
	if d, ok := p.(Dependent); ok {
		return d.Requires()
	}
	return nil
}

// ConflictsOf 返回插件声明的冲突项；未实现 Conflicting 时返回 nil。
func ConflictsOf(p Plugin) []string {
	if c, ok := p.(Conflicting); ok {
		return c.Conflicts()
	}
	return nil
}

// unmetLocked 返回该插件当前未满足的依赖（未注册或未启用的），按声明顺序。
// 只看直接依赖：传递性由 Resolve 的迭代与「禁用需自上而下」的约束共同保证。
func (m *Manager) unmetLocked(name string) []string {
	e, ok := m.entries[name]
	if !ok {
		return nil
	}
	var unmet []string
	for _, dep := range RequiresOf(e.plugin) {
		if d, ok := m.entries[dep]; !ok || !d.enabled {
			unmet = append(unmet, dep)
		}
	}
	if slices.Contains(m.cyclesLocked(), name) {
		unmet = append(unmet, "（依赖成环，无法启用）")
	}
	return unmet
}

// dependentsLocked 返回直接依赖该插件的已启用插件（按注册顺序）。
func (m *Manager) dependentsLocked(name string) []string {
	var out []string
	for _, other := range m.order {
		e := m.entries[other]
		if !e.enabled || other == name {
			continue
		}
		if slices.Contains(RequiresOf(e.plugin), name) {
			out = append(out, other)
		}
	}
	return out
}

// conflictingLocked 返回与该插件相冲突且已启用的插件。冲突关系视为对称：
// 只要任一方声明了对方，双方的卡片上都应该看到告警。
func (m *Manager) conflictingLocked(name string) []string {
	e, ok := m.entries[name]
	if !ok {
		return nil
	}
	declared := ConflictsOf(e.plugin)
	var out []string
	for _, other := range m.order {
		o := m.entries[other]
		if other == name || !o.enabled {
			continue
		}
		if slices.Contains(declared, other) || slices.Contains(ConflictsOf(o.plugin), name) {
			out = append(out, other)
		}
	}
	return out
}

// cyclesLocked 返回处在依赖环上的插件名。
// 成环是插件作者的错误，但不能让它把校验拖进无限递归，所以显式检出并当作依赖不满足处理。
func (m *Manager) cyclesLocked() []string {
	const (
		white = 0 // 未访问
		gray  = 1 // 在当前递归路径上
		black = 2 // 已完成
	)
	color := make(map[string]int, len(m.entries))
	onCycle := map[string]bool{}

	var visit func(name string, path []string)
	visit = func(name string, path []string) {
		if _, ok := m.entries[name]; !ok {
			return // 未注册的依赖不构成环，由 unmetLocked 报告
		}
		switch color[name] {
		case gray:
			// 从 path 中回溯到 name，路径上的都在环里
			if i := slices.Index(path, name); i >= 0 {
				for _, n := range path[i:] {
					onCycle[n] = true
				}
			}
			return
		case black:
			return
		}
		color[name] = gray
		for _, dep := range RequiresOf(m.entries[name].plugin) {
			visit(dep, append(path, name))
		}
		color[name] = black
	}
	for _, name := range m.order {
		visit(name, nil)
	}

	var out []string
	for _, name := range m.order {
		if onCycle[name] {
			out = append(out, name)
		}
	}
	return out
}

// Resolve 在全部插件注册完成后统一校验依赖，必须在 Register 全部调用完之后调用一次：
// register 是逐个进行的，依赖方完全可能先于被依赖方注册，因此那时无法校验。
//
// 依赖未满足却处于启用状态的插件会被强制关闭，但记在 forcedOff 而不是直接改 enabled——
// 状态文件是全量重写的，直接改会在下一次任何开关/配置变更时把强制关闭固化成用户意图，
// 之后即使依赖恢复也不会自动回来。
func (m *Manager) Resolve() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if cycles := m.cyclesLocked(); len(cycles) > 0 {
		i18n.Logf(m.lang(), "log.plugin.dep_cycle", "警告: 插件依赖成环，涉及 %s，这些插件将保持禁用", joinNames(m.lang(), cycles))
	}
	// 迭代到稳定：关掉一个插件可能让依赖它的插件也变成依赖未满足
	for {
		changed := false
		for _, name := range m.order {
			e := m.entries[name]
			if !e.enabled {
				continue
			}
			if unmet := m.unmetLocked(name); len(unmet) > 0 {
				e.enabled = false
				e.forcedOff = true
				i18n.Logf(m.lang(), "log.plugin.dep_unmet", "插件 %q 的依赖未满足（%s），保持禁用", name, joinNames(m.lang(), unmet))
				changed = true
			}
		}
		if !changed {
			return
		}
	}
}

// checkEnableLocked 校验能否启用该插件。
func (m *Manager) checkEnableLocked(name string) error {
	if unmet := m.unmetLocked(name); len(unmet) > 0 {
		return fmt.Errorf("插件 %q 需要先启用: %s", name, strings.Join(unmet, "、"))
	}
	return nil
}

// checkDisableLocked 校验能否禁用该插件。
// 拒绝而不是级联关闭：界面没有确认或提示通道，级联只会表现为「另一个开关自己变灰了」，
// 用户无从得知发生了什么；而开关的失败分支本来就会把错误文案显示出来。
func (m *Manager) checkDisableLocked(name string) error {
	if deps := m.dependentsLocked(name); len(deps) > 0 {
		return fmt.Errorf("插件 %q 被 %s 依赖，请先禁用 %s",
			name, quoteJoin(deps), quoteJoin(deps))
	}
	return nil
}

func quoteJoin(names []string) string {
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%q", n))
	}
	return strings.Join(parts, "、")
}

// joinNames 按语言的习惯连接一串插件名：中文用顿号，西文用逗号加空格。
// 这种标点差异逐处硬写会漏。
func joinNames(lang string, names []string) string {
	return strings.Join(names, i18n.T(lang, "log.plugin.name_sep", "、"))
}
