package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// 插件来源。
const (
	SourceBuiltin  = "builtin"  // 随程序内置
	SourceExternal = "external" // 来自外部来源
)

// SourceLabel 返回来源的中文显示名。
func SourceLabel(source string) string {
	switch source {
	case SourceBuiltin:
		return "内置"
	case SourceExternal:
		return "外源"
	}
	return source
}

// Status 是插件在 /api/plugins 中展示的状态。
type Status struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Source      string   `json:"source"`
	Category    string   `json:"category"`     // 功能分组的展示名（会随界面语言变）
	CategoryKey string   `json:"category_key"` // 功能分组的稳定标识（界面按它分组）
	Enabled     bool     `json:"enabled"`
	ToolNames   []string `json:"tool_names"`
	HasPrompt   bool     `json:"has_prompt"`
	// ConfigFields 为空表示该插件没有可配置项（界面不显示配置入口）。
	ConfigFields []ConfigField  `json:"config_fields,omitempty"`
	Config       map[string]any `json:"config,omitempty"` // 当前生效值（含默认值）
	// 依赖与冲突。Unmet / Conflicting 由 Manager 算好，界面直接用，不必自己推。
	Requires    []string `json:"requires,omitempty"`    // 声明的依赖
	Unmet       []string `json:"unmet,omitempty"`       // 其中未满足的（未注册或未启用）
	Conflicts   []string `json:"conflicts,omitempty"`   // 声明的冲突项
	Conflicting []string `json:"conflicting,omitempty"` // 其中当前已启用的
	// Actions 是插件声明的操作入口，仅在启用且初始化成功时暴露。
	Actions []ActionDef `json:"actions,omitempty"`
}

type entry struct {
	plugin  Plugin
	source  string
	baseCfg map[string]any // 配置文件 plugins.<name>.config
	userCfg map[string]any // 界面上保存过的配置（持久化到状态文件，覆盖 baseCfg）
	cfg     map[string]any // 生效配置 = baseCfg 叠加 userCfg
	enabled bool
	inited  bool
	// forcedOff 表示「用户想开，但依赖没满足所以被强制关掉」。持久化时按想开来写，
	// 使依赖恢复后插件能自动回来，见 Resolve 的说明。
	forcedOff bool
}

// applyCfg 重算生效配置。
func (e *entry) applyCfg() {
	if len(e.userCfg) == 0 {
		e.cfg = e.baseCfg
		return
	}
	merged := make(map[string]any, len(e.baseCfg)+len(e.userCfg))
	maps.Copy(merged, e.baseCfg)
	maps.Copy(merged, e.userCfg)
	e.cfg = merged
}

// Manager 管理系统插件：注册、开关、状态持久化、工具与提示词聚合。
type Manager struct {
	mu        sync.RWMutex
	ictx      InitContext
	statePath string // 运行时开关状态文件（覆盖配置的初始值）
	entries   map[string]*entry
	order     []string // 注册顺序（提示词按此序拼接）

	// noInit 表示只登记插件、不初始化它们，见 WithoutInit。
	noInit bool
	// stateDir 非空时改写插件持久化目录的根，见 WithStateDir。
	stateDir string
	// suppressed 非 nil 时，命中的插件只登记、不启用，见 WithSuppressed。
	suppressed func(Plugin) bool
}

// Option 调整 Manager 的行为。
type Option func(*Manager)

// WithoutInit 让 Manager 只登记插件而绝不调用 Init。
//
// 给离线的配置工具用：读 ConfigFields、改开关与参数、写状态文件全都不需要插件
// 真的跑起来，而照常 Init 会把 QQ 的长连接、心跳与定时任务一并启动——用户只是想
// 改个参数。校验（NormalizeConfig、依赖与环检测）不依赖 Init，照常生效。
//
// 这种 Manager 不可用于服务运行：它的插件从未初始化，工具与提示词都不会正常工作。
func WithoutInit() Option {
	return func(m *Manager) { m.noInit = true }
}

// WithStateDir 把插件的持久化目录根改到 dir（默认是状态文件所在目录下的 plugins/）。
//
// 给临时运行用：回放评测要按真实的开关与配置把插件建起来，但插件的一切读写都该
// 落到临时目录——记忆库、心情、统计文件都不能被一次评测污染。开关状态仍从
// statePath 读，只是 StateDir 换了地方。
func WithStateDir(dir string) Option {
	return func(m *Manager) { m.stateDir = dir }
}

// WithSuppressed 让命中谓词的插件只登记、不启用，即便状态文件里是开着的。
//
// 同样给临时运行用：一次回放评测不该把消息通道连到真实平台上去、也不该让心跳在
// 评测会话上插话。记作 forcedOff 而不是直接改 enabled：万一状态文件被重写，
// 写回去的仍是用户「想开」的意图。
func WithSuppressed(pred func(Plugin) bool) Option {
	return func(m *Manager) { m.suppressed = pred }
}

// PluginConfig 来自配置文件 plugins.<name> 段。
type PluginConfig struct {
	Enabled bool           `yaml:"enabled"`
	Config  map[string]any `yaml:"config"`
}

func NewManager(ictx InitContext, statePath string, opts ...Option) *Manager {
	m := &Manager{ictx: ictx, statePath: statePath, entries: map[string]*entry{}}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// validName 限定插件名的取值，因为它会被用来拼持久化目录。
var validName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// fieldsFor 返回该插件的配置项声明，其中文本类的默认值已按**进程语言**解析。
//
// 「没存过值时实际生效的是哪一份」必须在这里定，不能留给 NormalizeConfig 去取
// 插件代码里的中文常量：那样英文用户不点一次保存，界面上显示的与模型收到的就是
// 两份东西。设置页的预填走的是同一个函数（见 LocalizeDefaults），两处因此不可能分叉。
//
// 用进程语言而不是请求语言：生效值只有一份，它属于这个服务而不属于某个浏览器。
func (m *Manager) fieldsFor(p Plugin, source string) []ConfigField {
	fields := ConfigFieldsOf(p)
	kn := p.Name()
	if source == SourceExternal {
		kn = ExternalKeyName
	}
	return LocalizeDefaults(m.ictx.Lang, kn, fields)
}

// initCtxFor 返回该插件专属的运行环境：在公共 InitContext 上补一个按插件名隔离的持久化目录。
// 未配置状态文件时 StateDir 留空，由插件自行决定是否拒绝启用。
// RunTurn 与 Notice 被包一层自动注入发起方标记——发起方由 Manager 裁定，
// 插件无法伪装成前台。
func (m *Manager) initCtxFor(name string) InitContext {
	ictx := m.ictx
	switch {
	case m.stateDir != "":
		ictx.StateDir = filepath.Join(m.stateDir, name)
	case m.statePath != "":
		ictx.StateDir = filepath.Join(filepath.Dir(m.statePath), "plugins", name)
	}
	if base := m.ictx.RunTurn; base != nil {
		ictx.RunTurn = func(ctx context.Context, sessionID, input string) (string, error) {
			return base(WithTurnOrigin(ctx, name), sessionID, input)
		}
	}
	if base := m.ictx.Notice; base != nil {
		ictx.Notice = func(ctx context.Context, sessionID, text string) error {
			return base(WithTurnOrigin(ctx, name), sessionID, text)
		}
	}
	return ictx
}

// Register 注册内置插件，其余行为见 register。
func (m *Manager) Register(p Plugin, cfg PluginConfig) error {
	return m.register(p, cfg, SourceBuiltin)
}

// RegisterExternal 注册来自外部来源的插件（界面上标记为「外源」）。
func (m *Manager) RegisterExternal(p Plugin, cfg PluginConfig) error {
	return m.register(p, cfg, SourceExternal)
}

// register 注册插件并按配置与持久化状态决定是否启用。
// 工具名与已注册插件冲突时拒绝注册。
func (m *Manager) register(p Plugin, cfg PluginConfig, source string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	name := p.Name()
	if !validName.MatchString(name) {
		return fmt.Errorf("插件名 %q 非法（只允许小写字母开头的小写字母、数字与下划线）", name)
	}
	if _, exists := m.entries[name]; exists {
		return fmt.Errorf("插件 %q 重复注册", name)
	}
	for _, t := range p.Tools() {
		for otherName, other := range m.entries {
			for _, ot := range other.plugin.Tools() {
				if ot.Name() == t.Name() {
					return fmt.Errorf("插件 %q 的工具 %q 与插件 %q 冲突", name, t.Name(), otherName)
				}
			}
		}
	}

	e := &entry{plugin: p, source: source, baseCfg: cfg.Config, cfg: cfg.Config, enabled: cfg.Enabled}
	m.entries[name] = e
	m.order = append(m.order, name)

	// 持久化状态覆盖配置文件的初始值（界面上的改动优先）
	if saved, ok := m.loadState()[name]; ok {
		e.enabled = saved.Enabled
		e.userCfg = saved.Config
		// JSON 会把整数读成 float64，按字段声明还原类型；非法值退回配置文件
		if fields := m.fieldsFor(p, source); len(fields) > 0 && len(e.userCfg) > 0 {
			values, err := NormalizeConfig(fields, e.userCfg)
			if err != nil {
				log.Printf("插件 %q 的持久化配置无效，改用配置文件的值: %v", name, err)
				values = nil
			}
			e.userCfg = values
		}
		e.applyCfg()
	}
	if e.enabled && m.suppressed != nil && m.suppressed(p) {
		e.enabled, e.forcedOff = false, true
	}
	if e.enabled && !m.noInit {
		if err := p.Init(m.initCtxFor(name), e.cfg); err != nil {
			e.enabled = false
			log.Printf("插件 %q 初始化失败，保持禁用: %v", name, err)
		} else {
			e.inited = true
		}
	}
	return nil
}

// SetEnabled 运行时启用/禁用插件并持久化状态。启用时执行 Init，失败保持禁用。
// 依赖校验在 Init 之前完成：依赖不满足时不该产生任何副作用。
// 禁用有后台活动的插件时调用其 Stop——必须在锁外：插件的后台 goroutine 可能正阻塞在
// Manager 的读锁上（如 SystemPrompts），锁内等待会死锁。
func (m *Manager) SetEnabled(name string, on bool) error {
	m.mu.Lock()
	var toStop Stoppable

	e, ok := m.entries[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("插件 %q 不存在", name)
	}
	if on {
		if err := m.checkEnableLocked(name); err != nil {
			m.mu.Unlock()
			return err
		}
		if !e.inited && !m.noInit {
			if err := e.plugin.Init(m.initCtxFor(name), e.cfg); err != nil {
				m.mu.Unlock()
				return fmt.Errorf("插件 %q 初始化失败: %w", name, err)
			}
			e.inited = true
		}
	} else {
		if err := m.checkDisableLocked(name); err != nil {
			m.mu.Unlock()
			return err
		}
		if s, ok := e.plugin.(Stoppable); ok && e.inited {
			// 停止后插件的内部状态已拆除，重新启用时必须重新 Init
			toStop = s
			e.inited = false
		}
	}
	e.enabled = on
	e.forcedOff = false // 用户显式操作过，此后以他的意图为准
	m.saveStateLocked()
	m.mu.Unlock()

	if toStop != nil {
		toStop.Stop()
	}
	return nil
}

// SetConfig 保存插件配置并持久化。已初始化的插件会以新配置重新 Init，
// 失败时回滚到旧配置。配置中未声明的键被忽略，缺失的键取默认值。
//
// 有后台活动的插件在重新 Init 前先在锁外 Stop（锁内调用可能与插件 goroutine 持有的
// 读锁死锁）。解锁窗口内其它请求可能穿插进来，代价是短暂读到「已停止但未重配」的状态，
// 可容忍；有状态插件本就要求 Init 可重入、自行加锁。
func (m *Manager) SetConfig(name string, cfg map[string]any) error {
	m.mu.Lock()

	e, ok := m.entries[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("插件 %q 不存在", name)
	}
	fields := m.fieldsFor(e.plugin, e.source)
	if len(fields) == 0 {
		m.mu.Unlock()
		return fmt.Errorf("插件 %q 没有可配置项", name)
	}
	next, err := NormalizeConfig(fields, cfg)
	if err != nil {
		m.mu.Unlock()
		return err
	}

	if s, ok := e.plugin.(Stoppable); ok && e.inited {
		m.mu.Unlock()
		s.Stop()
		m.mu.Lock()
	}

	oldUser, oldCfg := e.userCfg, e.cfg
	e.userCfg = next
	e.applyCfg()
	if e.inited {
		if err := e.plugin.Init(m.initCtxFor(name), e.cfg); err != nil {
			e.userCfg, e.cfg = oldUser, oldCfg
			_ = e.plugin.Init(m.initCtxFor(name), oldCfg) // 尽力恢复旧配置下的状态
			m.mu.Unlock()
			return fmt.Errorf("插件 %q 应用配置失败: %w", name, err)
		}
	}
	m.saveStateLocked()
	m.mu.Unlock()
	return nil
}

// actionableFor 解析出可执行操作的插件：必须存在、启用、已初始化且实现 Actionable。
// 返回后在锁外调用其方法——操作可能有网络请求，不能占着 Manager 的锁。
func (m *Manager) actionableFor(name string) (Actionable, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.entries[name]
	if !ok {
		return nil, fmt.Errorf("插件 %q 不存在", name)
	}
	if !e.enabled || !e.inited {
		return nil, fmt.Errorf("插件 %q 未启用", name)
	}
	a, ok := e.plugin.(Actionable)
	if !ok {
		return nil, fmt.Errorf("插件 %q 没有操作入口", name)
	}
	return a, nil
}

// StartAction 触发插件的一个操作。
func (m *Manager) StartAction(ctx context.Context, name, key string) error {
	a, err := m.actionableFor(name)
	if err != nil {
		return err
	}
	return a.StartAction(ctx, key)
}

// ActionState 查询插件操作的当前状态。
func (m *Manager) ActionState(name, key string) (ActionState, error) {
	a, err := m.actionableFor(name)
	if err != nil {
		return ActionState{}, err
	}
	return a.ActionState(key)
}

// StopAll 停止所有有后台活动的已初始化插件，供进程退出时调用。
// 与 SetEnabled 相同的理由，Stop 在锁外进行。
func (m *Manager) StopAll() {
	m.mu.RLock()
	var stops []Stoppable
	for _, name := range m.order {
		e := m.entries[name]
		if !e.inited {
			continue
		}
		if s, ok := e.plugin.(Stoppable); ok {
			stops = append(stops, s)
		}
	}
	m.mu.RUnlock()
	for _, s := range stops {
		s.Stop()
	}
}

// ---------- 对插件的回调（一律在锁外进行） ----------

// named 是一次快照里的一个插件：名字，加它实现的某个可选能力。
type named[T any] struct {
	name string
	impl T
}

// enabledAs 在锁内快照实现了 T 的启用插件（按注册顺序），供锁外逐个调用。
//
// 所有对插件的回调都要经这条路进出。读锁一旦跨进插件代码，插件里任何一次反向
// 调用（哪怕只是问一句状态）都可能撞上排队中的写锁而死锁——Go 的 RWMutex 在有
// 写者等待时会挡住后续所有 RLock。而 OnCompact 里还藏着一次真实的模型往返，锁
// 会一直握到它返回为止：那期间在设置页拨一下开关，整个服务就停到提炼结束。
//
// 代价是快照与调用之间有个窗口，期间被禁用的插件仍会被回调一次。这与 StopAll、
// actionableFor 早就采用的做法一致，可接受：插件本就要求自行加锁、Init 可重入。
func enabledAs[T any](m *Manager) []named[T] {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var out []named[T]
	for _, name := range m.order {
		e := m.entries[name]
		if !e.enabled {
			continue
		}
		if impl, ok := any(e.plugin).(T); ok {
			out = append(out, named[T]{name: name, impl: impl})
		}
	}
	return out
}

// reversed 返回切片的逆序副本，供按逆注册序征询的回调使用（见 TranslateFailure）。
func reversed[T any](s []T) []T {
	out := make([]T, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}

// safely 执行一次插件回调，panic 只记日志：单个插件不该连累整轮对话。
// 从前只有轮次结束的广播做了这层保护，其余回调点各自裸调——同样是插件代码，
// 没有理由区别对待。
func safely(name, what string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("插件 %q 在%s时 panic: %v", name, what, r)
		}
	}()
	fn()
}

// entrySnapshot 是列表接口在锁内取到的插件状态，插件自己的方法留到锁外调用。
// 依赖与冲突的推算留在锁内：它们要看整张注册表，必须在一致的快照上进行，
// 而 Requires / Conflicts 按契约就是静态声明，不会反向调用 Manager。
type entrySnapshot struct {
	name        string
	plugin      Plugin
	source      string
	enabled     bool
	inited      bool
	cfg         map[string]any
	requires    []string
	unmet       []string
	conflicts   []string
	conflicting []string
}

// List 返回全部插件状态（按注册顺序）。
func (m *Manager) List() []Status {
	m.mu.RLock()
	snaps := make([]entrySnapshot, 0, len(m.order))
	for _, name := range m.order {
		e := m.entries[name]
		snaps = append(snaps, entrySnapshot{
			name: name, plugin: e.plugin, source: e.source,
			enabled: e.enabled, inited: e.inited, cfg: e.cfg,
			requires:    RequiresOf(e.plugin),
			unmet:       m.unmetLocked(name),
			conflicts:   ConflictsOf(e.plugin),
			conflicting: m.conflictingLocked(name),
		})
	}
	m.mu.RUnlock()

	out := make([]Status, 0, len(snaps))
	for _, sn := range snaps {
		st := Status{
			Name:      sn.name,
			Source:    sn.source,
			Enabled:   sn.enabled,
			ToolNames: []string{},
			Requires:  sn.requires,
			Unmet:     sn.unmet,
			Conflicts: sn.conflicts, Conflicting: sn.conflicting,
		}
		safely(sn.name, "汇报插件状态", func() {
			st.Description = sn.plugin.Description()
			st.Category = CategoryOf(sn.plugin)
			st.CategoryKey = CategoryKeyOf(st.Category)
			for _, t := range sn.plugin.Tools() {
				st.ToolNames = append(st.ToolNames, t.Name())
			}
			st.HasPrompt = sn.plugin.SystemPrompt() != ""
			if a, ok := sn.plugin.(Actionable); ok && sn.enabled && sn.inited {
				st.Actions = a.Actions()
			}
			if fields := m.fieldsFor(sn.plugin, sn.source); len(fields) > 0 {
				st.ConfigFields = fields
				// 配置非法时（如手改配置文件）退回默认值展示，不影响列表可用
				values, err := NormalizeConfig(fields, sn.cfg)
				if err != nil {
					values, _ = NormalizeConfig(fields, nil)
				}
				st.Config = values
			}
		})
		out = append(out, st)
	}
	return out
}

// EnabledTools 返回所有启用插件的工具（按注册顺序）。
func (m *Manager) EnabledTools() []Tool {
	var out []Tool
	for _, e := range enabledAs[Plugin](m) {
		var tools []Tool
		safely(e.name, "列举工具", func() { tools = e.impl.Tools() })
		out = append(out, tools...)
	}
	return out
}

// FindTool 在启用插件中按名查找工具。
func (m *Manager) FindTool(name string) (Tool, bool) {
	for _, e := range enabledAs[Plugin](m) {
		var tools []Tool
		safely(e.name, "列举工具", func() { tools = e.impl.Tools() })
		for _, t := range tools {
			if t.Name() == name {
				return t, true
			}
		}
	}
	return nil, false
}

// SystemPrompts 返回所有启用插件的非空提示词片段（按注册顺序）。
func (m *Manager) SystemPrompts() []string {
	var out []string
	for _, e := range enabledAs[Plugin](m) {
		var frag string
		safely(e.name, "生成系统提示词", func() { frag = e.impl.SystemPrompt() })
		if frag != "" {
			out = append(out, frag)
		}
	}
	return out
}

// StatusLines 返回所有启用插件贡献的状态行（按注册顺序）。
// 只问启用的插件：被禁用的插件没有运行状况可言，报一行「已停」只是噪声。
func (m *Manager) StatusLines() []string {
	var out []string
	for _, e := range enabledAs[StatusReporter](m) {
		var lines []string
		safely(e.name, "汇报状态行", func() { lines = e.impl.StatusLines() })
		for _, line := range lines {
			if line != "" {
				out = append(out, line)
			}
		}
	}
	return out
}

// DecideScope 裁决本轮对话的可见域。
//
// 单所有者：按注册顺序第一个返回非零 Scope 的插件胜出，其余被忽略并记日志。
// Write 会被插件用来拼持久化目录，因此按插件名的字符集校验，非法则整条裁决作废
// 降级为零值——降级成「不限制」比让一个 "../x" 之类的标签流进文件路径安全得多。
func (m *Manager) DecideScope(ctx context.Context, ev TurnEvent) Scope {
	var out Scope
	owner := ""
	for _, e := range enabledAs[ScopeDecider](m) {
		var (
			sc  Scope
			err error
		)
		safely(e.name, "裁决可见域", func() { sc, err = e.impl.DecideScope(ctx, ev) })
		if err != nil {
			log.Printf("插件 %q 裁决可见域失败，按不限制处理: %v", e.name, err)
			continue
		}
		if sc.IsZero() {
			continue
		}
		if sc.Write != "" && !validName.MatchString(sc.Write) {
			log.Printf("插件 %q 返回了非法的可见域标签 %q，已忽略", e.name, sc.Write)
			continue
		}
		if owner != "" {
			log.Printf("插件 %q 的可见域裁决被忽略：本轮已由插件 %q 决定", e.name, owner)
			continue
		}
		out, owner = sc, e.name
	}
	return out
}

// failureTextMaxRunes 是转译文本的长度上限。它会以助手消息落盘并直接展示，
// 一句台词用不到更多；超限截断而不是作废——插件已经接手，缺一截好过整条丢弃。
const failureTextMaxRunes = 500

// TranslateFailure 在一轮对话失败后，给插件一个把失败转成一句面向用户回复的机会。
//
// 单所有者：第一个返回非空文本的插件胜出，其余被忽略并记日志——两个插件各给
// 一句「台词」是无法合并的组合，与 DecideScope 同理。但征询按**逆注册序**进行，
// 与 DecideScope 相反：提示词注入的约定是靠后注册的片段覆盖靠前的（里人格设定
// 「优先于上文的角色设定」正是这个语义），一句台词该由最上层的那个声音说出，
// 所以最后注册的转译者最先拿到机会。
func (m *Manager) TranslateFailure(ctx context.Context, ev TurnFailure) (string, bool) {
	var out string
	owner := ""
	for _, e := range reversed(enabledAs[FailureTranslator](m)) {
		var (
			text string
			ok   bool
		)
		safely(e.name, "转译轮次失败", func() { text, ok = e.impl.TranslateFailure(ctx, ev) })
		text = strings.TrimSpace(text)
		if !ok || text == "" {
			continue
		}
		if owner != "" {
			log.Printf("插件 %q 的失败转译被忽略：本轮已由插件 %q 接手", e.name, owner)
			continue
		}
		if r := []rune(text); len(r) > failureTextMaxRunes {
			text = string(r[:failureTextMaxRunes])
		}
		out, owner = text, e.name
	}
	return out, owner != ""
}

// TurnPrompts 在可见域裁决完成后收集各插件的一次性提示词片段（按注册顺序，已滤空）。
// 插件返回的错误只记日志：少一段提示词应当降级，不该让整轮对话失败。
func (m *Manager) TurnPrompts(ctx context.Context, ev TurnEvent) []string {
	var out []string
	for _, e := range enabledAs[TurnPrompter](m) {
		var (
			frag string
			err  error
		)
		safely(e.name, "生成本轮提示词", func() { frag, err = e.impl.TurnPrompt(ctx, ev) })
		if err != nil {
			log.Printf("插件 %q 生成本轮提示词失败: %v", e.name, err)
			continue
		}
		if frag != "" {
			out = append(out, frag)
		}
	}
	return out
}

// CompactPrompts 收集启用插件为压缩摘要提示词追加的要求（按注册顺序，已滤掉空串）。
// 与 SystemPrompts 同一族的收集：在锁外逐个调用，单个插件的 panic 不影响其余。
func (m *Manager) CompactPrompts(ctx context.Context) []string {
	var out []string
	for _, e := range enabledAs[CompactPrompter](m) {
		var frag string
		safely(e.name, "生成压缩提示词", func() { frag = e.impl.CompactPrompt(ctx) })
		if frag = strings.TrimSpace(frag); frag != "" {
			out = append(out, frag)
		}
	}
	return out
}

// NotifyCompact 在会话历史被替换前广播压缩事件，返回各插件的注记（按注册顺序，已滤掉空串）。
// 插件返回的错误只记录日志，不阻断压缩——压缩是上下文溢出时的保底手段，不能被插件卡住。
//
// 这里是回调必须走出锁外最硬的理由：memory 的 OnCompact 内含一次真实的模型往返。
func (m *Manager) NotifyCompact(ctx context.Context, ev CompactEvent) []string {
	var notes []string
	for _, e := range enabledAs[CompactObserver](m) {
		var (
			note string
			err  error
		)
		safely(e.name, "处理压缩事件", func() { note, err = e.impl.OnCompact(ctx, ev) })
		if err != nil {
			log.Printf("插件 %q 处理压缩事件失败: %v", e.name, err)
			continue
		}
		if note != "" {
			notes = append(notes, note)
		}
	}
	return notes
}

// NotifyTurnEnd 在一轮对话成功结束后广播给所有实现 TurnObserver 的启用插件。
// 广播发生在轮次收尾的同步路径上，实现须快速返回。
func (m *Manager) NotifyTurnEnd(ctx context.Context, ev TurnEndEvent) {
	for _, e := range enabledAs[TurnObserver](m) {
		safely(e.name, "处理轮次结束事件", func() { e.impl.OnTurnEnd(ctx, ev) })
	}
}

// NotifyNotice 在一条会话注记落盘后广播给所有实现 NoticeObserver 的启用插件。
// 在注记写入方的路径上调用，实现须快速返回。
func (m *Manager) NotifyNotice(ctx context.Context, ev NoticeEvent) {
	for _, e := range enabledAs[NoticeObserver](m) {
		safely(e.name, "处理会话注记事件", func() { e.impl.OnNotice(ctx, ev) })
	}
}

// ---------- 状态持久化（plugins.state.json） ----------

// persistedEntry 是单个插件的持久化状态（开关 + 界面上改过的配置）。
type persistedEntry struct {
	Enabled bool           `json:"enabled"`
	Config  map[string]any `json:"config,omitempty"`
}

// loadState 读取状态文件，兼容旧格式（name -> bool）。
func (m *Manager) loadState() map[string]persistedEntry {
	state := map[string]persistedEntry{}
	if m.statePath == "" {
		return state
	}
	raw, err := os.ReadFile(m.statePath)
	if err != nil {
		return state
	}
	var items map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return state
	}
	for name, item := range items {
		var enabled bool
		if err := json.Unmarshal(item, &enabled); err == nil {
			state[name] = persistedEntry{Enabled: enabled}
			continue
		}
		var pe persistedEntry
		if err := json.Unmarshal(item, &pe); err == nil {
			state[name] = pe
		}
	}
	return state
}

func (m *Manager) saveStateLocked() {
	if m.statePath == "" {
		return
	}
	state := map[string]persistedEntry{}
	for name, e := range m.entries {
		// 只持久化界面上改过的配置，未改动的插件仍跟随配置文件；
		// 被依赖强制关掉的插件按「用户想开」持久化，依赖恢复后能自动回来
		state[name] = persistedEntry{Enabled: e.enabled || e.forcedOff, Config: e.userCfg}
	}
	raw, _ := json.MarshalIndent(state, "", "  ")
	_ = os.MkdirAll(filepath.Dir(m.statePath), 0o755)
	// 0600：插件配置里可能有用户不愿外泄的内容（提示词设定、触发词等），与 models.json 一致
	if err := os.WriteFile(m.statePath, raw, 0o600); err != nil {
		log.Printf("保存插件状态失败: %v", err)
	}
}
