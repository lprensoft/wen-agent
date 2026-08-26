package agentplugin

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/skillroots"
)

// NamePrefix 是外源插件在插件系统里的名字前缀。
//
// 加前缀有两个理由，都很实际：插件名是全局唯一的，一个叫 memory 的插件包会与内置的
// memory 撞名而注册失败；插件名还会被拼成持久化目录，前缀让「哪些目录是外面装进来的」
// 一眼看得出来。代价是设置页上的名字长一点，而卡片上本来就有「外源」标签，不冲突。
const NamePrefix = "ext_"

// CategoryExternal 是外源插件包在设置页上的分组。
const CategoryExternal = plugin.CategoryExternal

// DataDirName 是交给 MCP server 的 PLUGIN_DATA 目录名，落在插件自己的持久化目录下。
//
// 标准要求它可写、且**跨更新保留**——所以它不能放在插件包目录里：那里装的是包的内容，
// 卸载重装就没了，而 server 攒在里面的索引、缓存与凭据不该跟着一起消失。
const DataDirName = "data"

// nonWord 把标准允许的连字符与点换成下划线：标准的插件名允许 [a-z0-9.-]，
// 而本项目的插件名只允许 [a-z0-9_]（它要拼进目录名）。
var nonWord = regexp.MustCompile(`[.-]+`)

// PluginName 把标准的插件名转成本项目的插件名。
func PluginName(manifestName string) string {
	return NamePrefix + nonWord.ReplaceAllString(manifestName, "_")
}

// 返回体量的默认上限与可取范围。
//
// 这是整套接入里最需要一个旋钮的地方：远端返回多少字节完全由对方决定，一个抓页面的
// server 返回整棵 a11y 树就是几万 token，而它会原样进上下文、并计入压缩判据。协议层
// 拦不住这件事，只能在适配层封顶。
const (
	defaultMaxResultBytes = 16 * 1024
	minResultBytes        = 1024
	maxResultBytes        = 256 * 1024
)

// Plugin 把一个插件包接进插件系统：一个包一张卡片、一个开关。
//
// 一个包可能同时带多篇技能与多台 MCP server，但对用户来说他装的是「一个东西」，
// 开关也该按这个东西给——按 server 拆成多张卡的话，装一个包会凭空冒出三个开关，
// 而它们其实是同生共死的。
type Plugin struct {
	name   string
	bundle *Bundle
	// tools 来自安装时落盘的缓存，在 Init 之前就要能拿出来：插件系统注册插件时
	// 会调 Tools() 查全局重名，而那一步握着写锁。
	tools []ToolInfo

	// mu 保护下面这组：Init 可能在运行时被重复调用（改配置即重新 Init），
	// 那时可能有 in-flight 的 Execute 正在读它们。
	mu       sync.RWMutex
	lang     string             // 进程语言，工具描述与回执据此取用
	clients  map[string]*client // server 名 → 连接，懒启动
	stateDir string             // 插件自己的持久化目录，PLUGIN_DATA 在它下面
	maxBytes int
	confirm  bool
	ready    bool

	store *Store // 读写工具清单缓存，可为 nil
	act   actionState
}

// NewPlugin 由一个已加载的插件包生成插件。store 用来读工具清单缓存，可为 nil
// （那样这个包就只有技能组件可用，工具清单为空）。
//
// lang 在这里就要给：插件是在 Init 之前构造的，而这一步已经会产生要给人看的话
// （工具清单读不出来那一条），构造完再补语言就晚了。
func NewPlugin(lang string, b *Bundle, store *Store) *Plugin {
	p := &Plugin{name: PluginName(b.Manifest.Name), lang: lang, bundle: b, maxBytes: defaultMaxResultBytes, store: store}
	if store != nil {
		tools, err := store.LoadTools(b.Manifest.Name)
		if err != nil {
			b.Problems = append(b.Problems, i18n.Tf(lang, "plugin.bundle.problem.rehandshake", "%s，请重新握手一次", err.Error()))
		}
		p.tools = tools
	}
	return p
}

func (p *Plugin) Name() string { return p.name }

// langOf 取进程语言。
func (p *Plugin) langOf() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lang
}
func (p *Plugin) Category() string { return CategoryExternal }

// Description 直接用清单里的说明。
//
// 它是外部数据，不翻译也不改写：语言包里不可能有这个包的键，而按本项目的约定，
// 展示文字查不到译文时回落原文——对外源插件来说原文往往就是英文，中文界面上因此
// 会看到英文。这是来源如此，不是漏翻。
func (p *Plugin) Description() string {
	lang := p.langOf()
	desc := strings.TrimSpace(p.bundle.Manifest.Description)
	if desc == "" {
		desc = i18n.T(lang, "plugin.bundle.desc.none", "外源插件包（清单里没有写说明）")
	}
	if v := p.bundle.Manifest.Version; v != "" {
		desc += i18n.Tf(lang, "plugin.bundle.desc.version", "（%s）", v)
	}
	return desc
}

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	if len(p.bundle.Servers) == 0 {
		return nil // 没有 MCP 组件就没什么可调的，别摆一张空表单
	}
	return []plugin.ConfigField{
		{
			Key:         "confirm",
			Label:       "调用前确认",
			Type:        plugin.FieldBool,
			Description: "每次调用这个插件包里的工具之前先问一句。外源工具能做什么由对方决定，拿不准的时候先开着。无人值守的轮次没有可确认的人，按拒绝处理。",
			Default:     false,
		},
		{
			Key:         "max_result_bytes",
			Label:       "单次结果最多保留",
			Type:        plugin.FieldInt,
			Description: "远端返回的内容超过这个字节数就截断。返回多少由对方决定，而它会原样进入上下文并计入压缩判据。",
			Default:     defaultMaxResultBytes,
			Min:         plugin.IntPtr(minResultBytes),
			Max:         plugin.IntPtr(maxResultBytes),
		},
	}
}

// Init 登记技能、准备 MCP 连接的参数。
//
// 这里**不连接**任何 server：Init 是在插件系统的写锁里调用的，在这儿拉起子进程做
// 握手，装了几台就让服务在启动时停上几秒，期间任何一次插件列表查询都会被挡住。
// 连接留到第一次真的要调用时。
func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	b := p.bundle
	if b.SkillsDir == "" && !b.HasMCP {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.bundle.err.empty", "这个插件包里既没有技能也没有 MCP 声明，没有可用的内容"))
	}

	var clients map[string]*client
	if len(b.Servers) > 0 {
		if ictx.StateDir == "" {
			return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.bundle.err.no_state_dir", "没有可用的持久化目录，无法为这个包里的 MCP server 准备数据目录"))
		}
		dataDir := filepath.Join(ictx.StateDir, DataDirName)
		clients = make(map[string]*client, len(b.Servers))
		for _, s := range b.Servers {
			resolved, err := s.Resolve(b.Root, dataDir)
			if err != nil {
				// 单台不可用不该拖垮整个包，其余的照常
				log := i18n.Tf(ictx.Lang, "plugin.bundle.err.bad_server", "mcp.json 里的 %s 用不了：%v", s.Name, err)
				if !contains(b.Problems, log) {
					b.Problems = append(b.Problems, log)
				}
				continue
			}
			clients[s.Name] = newClient(ictx.Lang, resolved)
		}
	}

	// 先停旧的再换新的：Init 可重入，上一轮的子进程要收干净
	p.mu.Lock()
	old := p.clients
	p.clients = clients
	p.lang = ictx.Lang
	p.stateDir = ictx.StateDir
	p.maxBytes = plugin.CfgInt(cfg, "max_result_bytes", defaultMaxResultBytes)
	p.confirm = plugin.CfgBool(cfg, "confirm", false)
	p.ready = true
	p.mu.Unlock()
	closeAll(old)

	if b.SkillsDir != "" {
		skillroots.Set(skillroots.Root{Owner: p.name, Dir: b.SkillsDir, Label: b.Manifest.Name})
	}
	return nil
}

// Stop 在插件被禁用、重配或进程退出时收干净：摘掉技能登记，结束全部子进程。
func (p *Plugin) Stop() {
	skillroots.Clear(p.name)
	p.mu.Lock()
	old := p.clients
	p.clients, p.ready, p.stateDir = nil, false, ""
	p.mu.Unlock()
	closeAll(old)
}

func closeAll(m map[string]*client) {
	for _, c := range m {
		c.Close()
	}
}

func contains(ss []string, s string) bool {
	for _, it := range ss {
		if it == s {
			return true
		}
	}
	return false
}

// SystemPrompt 不注入任何东西：包里的技能由技能插件统一列进它那份清单，工具则由
// 工具描述自己说明用途。每个包再各自注入一段的话，同样的话会重复出现好几遍。
func (p *Plugin) SystemPrompt() string { return "" }

// Tools 返回这个包暴露的工具。
//
// 它必须在 Init 之前就能回答（插件系统注册时要查全局重名），所以来自安装时落盘的
// 缓存而不是现问。缓存是空的（M1 装的包、手工拷进来的包）时返回空——设置页上的
// 「重新握手」按钮补得回来。
func (p *Plugin) Tools() []plugin.Tool {
	out := make([]plugin.Tool, 0, len(p.tools))
	for _, t := range p.tools {
		out = append(out, &remoteTool{p: p, info: t})
	}
	return out
}

// StatusLines 报一行这个包里有什么。
func (p *Plugin) StatusLines(lang string) []string {
	p.mu.RLock()
	live := len(p.clients)
	p.mu.RUnlock()

	b := p.bundle
	var parts []string
	if b.SkillsDir != "" {
		parts = append(parts, i18n.T(lang, "plugin.bundle.status.skills", "技能已并入技能清单"))
	}
	switch {
	case live > 0 && len(p.tools) > 0:
		parts = append(parts, i18n.Tf(lang, "plugin.bundle.status.tools", "%d 台 server、%d 件工具", live, len(p.tools)))
	case live > 0:
		parts = append(parts, i18n.Tf(lang, "plugin.bundle.status.pending", "%d 台 server 尚未握手（工具清单为空）", live))
	case b.HasMCP:
		parts = append(parts, i18n.T(lang, "plugin.bundle.status.no_server", "带 MCP 声明但没有可用的 server"))
	}
	if n := len(b.Problems); n > 0 {
		parts = append(parts, i18n.Tf(lang, "plugin.bundle.status.problems", "%d 处需要留意", n))
	}
	if len(parts) == 0 {
		return nil
	}
	return []string{i18n.Tf(lang, "plugin.bundle.status.line", "🧩 %s：%s", b.Manifest.Name,
		strings.Join(parts, i18n.T(lang, "plugin.bundle.status.sep", "，")))}
}

// Problems 返回加载时记下的问题，供 CLI 展示。
func (p *Plugin) Problems() []string { return p.bundle.Problems }

// Bundle 返回被包起来的插件包。
func (p *Plugin) Bundle() *Bundle { return p.bundle }
