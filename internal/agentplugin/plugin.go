package agentplugin

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

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

// nonWord 把标准允许的连字符与点换成下划线：标准的插件名允许 [a-z0-9.-]，
// 而本项目的插件名只允许 [a-z0-9_]（它要拼进目录名）。
var nonWord = regexp.MustCompile(`[.-]+`)

// PluginName 把标准的插件名转成本项目的插件名。
func PluginName(manifestName string) string {
	return NamePrefix + nonWord.ReplaceAllString(manifestName, "_")
}

// Plugin 把一个插件包接进插件系统：一个包一张卡片、一个开关。
//
// 一个包可能同时带多篇技能与多台 MCP server，但对用户来说他装的是「一个东西」，
// 开关也该按这个东西给——按 server 拆成多张卡的话，装一个包会凭空冒出三个开关，
// 而它们其实是同生共死的。
type Plugin struct {
	name string

	// mu 保护下面这组：Init 可能在运行时被重复调用（改配置即重新 Init），
	// 那时可能有别的 goroutine 正在读它们。
	mu     sync.RWMutex
	bundle *Bundle
}

// NewPlugin 由一个已加载的插件包生成插件。
func NewPlugin(b *Bundle) *Plugin {
	return &Plugin{name: PluginName(b.Manifest.Name), bundle: b}
}

func (p *Plugin) Name() string     { return p.name }
func (p *Plugin) Category() string { return CategoryExternal }

// Description 直接用清单里的说明。
//
// 它是外部数据，不翻译也不改写：语言包里不可能有这个包的键，而按本项目的约定，
// 展示文字查不到译文时回落原文——对外源插件来说原文往往就是英文，中文界面上因此
// 会看到英文。这是来源如此，不是漏翻。
func (p *Plugin) Description() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	desc := strings.TrimSpace(p.bundle.Manifest.Description)
	if desc == "" {
		desc = "外源插件包（清单里没有写说明）"
	}
	if v := p.bundle.Manifest.Version; v != "" {
		desc += "（" + v + "）"
	}
	return desc
}

// Init 把包里的技能登记进技能清单。
//
// 本版只处理技能组件。包里带 mcp.json 时不报错，只在状态行里说一句还没支持——
// 装了个纯 MCP 的包却什么都没发生，用户最需要知道的是「是本程序还没做」而不是
// 「我装错了」。
func (p *Plugin) Init(_ plugin.InitContext, _ map[string]any) error {
	p.mu.RLock()
	b := p.bundle
	p.mu.RUnlock()

	if b.SkillsDir == "" && !b.HasMCP {
		return fmt.Errorf("这个插件包里既没有技能也没有 MCP 声明，没有可用的内容")
	}
	if b.SkillsDir != "" {
		skillroots.Set(skillroots.Root{
			Owner: p.name,
			Dir:   b.SkillsDir,
			Label: b.Manifest.Name,
		})
	}
	return nil
}

// Stop 在插件被禁用、重配或进程退出时摘掉登记，包里的技能随即从清单里消失。
func (p *Plugin) Stop() { skillroots.Clear(p.name) }

// SystemPrompt 不注入任何东西：包里的技能由技能插件统一列进它那份清单，
// 每个包再各自注入一段的话，同样的说明会重复出现好几遍。
func (p *Plugin) SystemPrompt() string { return "" }

// Tools 本版为空。MCP 工具在下一步接入。
func (p *Plugin) Tools() []plugin.Tool { return nil }

// StatusLines 报一行这个包里有什么。
func (p *Plugin) StatusLines() []string {
	p.mu.RLock()
	b := p.bundle
	p.mu.RUnlock()

	var parts []string
	if b.SkillsDir != "" {
		parts = append(parts, "技能已并入技能清单")
	}
	if b.HasMCP {
		parts = append(parts, "带 MCP 声明（本版尚未支持，暂不生效）")
	}
	if len(b.Problems) > 0 {
		parts = append(parts, fmt.Sprintf("%d 处需要留意", len(b.Problems)))
	}
	if len(parts) == 0 {
		return nil
	}
	return []string{"🧩 " + b.Manifest.Name + "：" + strings.Join(parts, "，")}
}

// Problems 返回加载时记下的问题，供 CLI 展示。
func (p *Plugin) Problems() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.bundle.Problems
}
