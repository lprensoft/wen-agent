// Package bundles 提供设置页上的外源插件包管理入口。
//
// 它是「不给模型任何东西的插件」的第二例（第一例是 self_update）：不注入提示词、
// 不提供工具，只用可配置项、操作入口与状态行三样现成的契约。这么做的理由是那条界线
// ——加进核心的必须是通用机制而非具体功能。给服务端开一个 /api/bundles/install，
// 等于让核心知道「外源插件包」这回事；做成插件则核心什么都不必知道，用户还能把它
// 整个关掉。
//
// 附带的好处是终端里的 wen config 也一起有了：插件声明的操作在那边同样会渲染出来，
// 远程机器上不必为装一个插件包打开浏览器。
package bundles

import (
	"fmt"
	"strings"
	"sync"

	"wen/internal/agentplugin"
	"wen/internal/i18n"
	"wen/internal/plugin"
)

// Plugin 是外源插件包的管理入口。
type Plugin struct {
	lang  string // 进程语言。本插件不给模型任何东西，文案一律走界面语言包
	store *agentplugin.Store
	// baseDir 用来推算每个包的 PLUGIN_DATA，握手要用与真正运行时同一份参数。
	baseDir string

	// mu 保护操作状态。Init 可重入，而 StartAction 与 ActionState 由 HTTP 并发调用。
	mu       sync.Mutex
	actSeq   uint64
	actState plugin.ActionState
}

// New 由 main.go 在建插件时给出配置目录——这与 self_update 拿 Restart 回调是同一种
// 形状：插件需要一样核心不该替它准备的东西，经构造函数交给它。
func New(baseDir string) *Plugin {
	return &Plugin{store: agentplugin.NewStore(baseDir), baseDir: baseDir}
}

func (p *Plugin) Name() string     { return "bundles" }
func (p *Plugin) Category() string { return plugin.CategoryExternal }

func (p *Plugin) Description() string {
	return "外源插件包：装 Agent Plugins 格式的插件包，把它带的技能与 MCP 工具接进来"
}

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{{
		Key:   "source",
		Label: "要安装的插件包",
		Type:  plugin.FieldString,
		Description: "本机上的一个目录或 .zip 文件的路径。填好之后点下面的「安装」——" +
			"这里只认服务所在机器上的路径，浏览器里的文件传不过去。卸载用命令行：wen plugins uninstall <名称>。",
		Default: "",
	}}
}

// Init 只校验有没有可用的存放位置。这个插件不起任何后台活动。
func (p *Plugin) Init(ictx plugin.InitContext, _ map[string]any) error {
	p.mu.Lock()
	p.lang = ictx.Lang
	p.mu.Unlock()
	if p.store == nil || p.store.Dir() == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.bundles.no_base_dir",
			"没有可用的配置目录，无法管理插件包"))
	}
	return nil
}

// SystemPrompt 与 Tools 都为空：它是给人用的，不给模型任何东西。
func (p *Plugin) SystemPrompt() string { return "" }
func (p *Plugin) Tools() []plugin.Tool { return nil }

// langOf 取进程语言。
func (p *Plugin) langOf() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lang
}

func (p *Plugin) StatusLines(lang string) []string {
	bundles, problems := p.store.List()
	if len(bundles) == 0 && len(problems) == 0 {
		return []string{i18n.T(lang, "plugin.bundles.status.none", "🧩 外源插件包：一个都没装")}
	}
	var tools int
	for _, b := range bundles {
		ts, _ := p.store.LoadTools(b.Manifest.Name)
		tools += len(ts)
	}
	line := i18n.Tf(lang, "plugin.bundles.status.line", "🧩 外源插件包：%d 个，%d 件工具",
		len(bundles), tools)
	if n := len(problems); n > 0 {
		line += i18n.Tf(lang, "plugin.bundles.status.broken", "，%d 个读不出来", n)
	}
	return []string{line}
}

// names 返回已装插件包的名字，供操作说明里列出来。
func (p *Plugin) names() []string {
	bundles, _ := p.store.List()
	out := make([]string, 0, len(bundles))
	for _, b := range bundles {
		out = append(out, b.Manifest.Name)
	}
	return out
}

// installedSummary 是给操作说明用的运行时事实：已经装了哪几个。
//
// 只列名字，不成句：它接在译好的说明后面而本身不翻译（见 plugin.ActionDef.Detail），
// 掺进中文的句子，英文界面上就会冒出一行中文。
func (p *Plugin) installedSummary() string {
	names := p.names()
	if len(names) == 0 {
		return ""
	}
	return "· " + strings.Join(names, " · ")
}
