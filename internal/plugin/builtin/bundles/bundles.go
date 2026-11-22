// Package bundles 提供设置页上的外源插件包管理入口。
//
// 它不注入提示词、不提供工具，只用可配置项、操作入口与状态行三样现成契约。
// 更新检查也归这里：它是系统维护工作，不是模型创建的对话任务。
package bundles

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"wen/internal/agentplugin"
	"wen/internal/i18n"
	"wen/internal/plugin"
)

// Plugin 是外源插件包的管理入口。
type Plugin struct {
	lang    string
	store   *agentplugin.Store
	baseDir string

	mu         sync.Mutex
	actSeq     uint64
	actState   plugin.ActionState
	actRunning bool
	saved      map[string]any

	autoCheck bool
	interval  time.Duration
	started   time.Time
	lastTry   time.Time
	now       func() time.Time
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	wake      chan struct{}
}

// New 由 main.go 给出配置目录；可选 store 用来让中央管理卡与各 ext_* 共享操作锁。
func New(lang, baseDir string, stores ...*agentplugin.Store) *Plugin {
	store := agentplugin.NewStore(lang, baseDir)
	if len(stores) > 0 && stores[0] != nil {
		store = stores[0]
	}
	return &Plugin{lang: lang, store: store, baseDir: baseDir, now: time.Now, wake: make(chan struct{}, 1)}
}

func (p *Plugin) Name() string     { return "bundles" }
func (p *Plugin) Category() string { return plugin.CategoryExternal }

func (p *Plugin) Description() string {
	return "外源插件包：安装 Agent Plugins 包，绑定签名更新来源并检查新版本"
}

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key: "source", Label: "要安装的插件包", Type: plugin.FieldString,
			Description: "本机上的一个目录或 .zip 文件的路径。填好之后点下面的「安装」——" +
				"这里只认服务所在机器上的路径，浏览器里的文件传不过去。卸载用命令行：wen plugins uninstall <名称>。",
			Default: "",
		},
		{Key: "update_name", Label: "要绑定更新的包名", Type: plugin.FieldString,
			Description: "一个已经安装的插件包名称；换来源或公钥时先解除绑定。", Default: ""},
		{Key: "update_url", Label: "更新清单 URL", Type: plugin.FieldString,
			Description: "发布者提供的 HTTPS update.json；同路径的 update.json.sig 是签名。", Default: ""},
		{Key: "update_key", Label: "发布者 Ed25519 公钥", Type: plugin.FieldString,
			Description: "Base64 编码的 32 字节公钥，应从发布者的独立可信渠道取得。", Default: ""},
		{Key: "auto_check", Label: "自动检查更新", Type: plugin.FieldBool,
			Description: "只下载并验签更新清单，不下载 ZIP、不安装、不握手、不重启。", Default: true},
		{Key: "check_hours", Label: "检查间隔（小时）", Type: plugin.FieldInt,
			Description: "每个已绑定来源按各自上次成功检查的时间安排；失败后至少一小时再试。",
			Default:     24, Min: plugin.IntPtr(1), Max: plugin.IntPtr(720)},
	}
}

// Init 可重入：先停旧检查循环，再应用新配置。
func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	p.Stop()
	if p.store == nil || p.store.Dir() == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.bundles.no_base_dir", "没有可用的配置目录，无法管理插件包"))
	}
	hours := plugin.CfgInt(cfg, "check_hours", 24)
	if hours < 1 {
		hours = 1
	}
	if hours > 720 {
		hours = 720
	}
	saved := make(map[string]any, len(cfg))
	for k, v := range cfg {
		saved[k] = v
	}
	p.mu.Lock()
	p.lang = ictx.Lang
	p.saved = saved
	p.autoCheck = plugin.CfgBool(cfg, "auto_check", true)
	p.interval = time.Duration(hours) * time.Hour
	p.started = p.now()
	p.lastTry = time.Time{}
	auto := p.autoCheck
	p.mu.Unlock()

	if auto && !ictx.ManualTime {
		ctx, cancel := context.WithCancel(context.Background())
		p.mu.Lock()
		p.cancel = cancel
		p.wg.Add(1)
		p.mu.Unlock()
		go p.loop(ctx)
	}
	return nil
}

// Stop 停止后台检查；已在进行的 HTTP 请求经 context 取消，不等待安装或握手操作。
func (p *Plugin) Stop() {
	p.mu.Lock()
	cancel := p.cancel
	p.cancel = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	p.wg.Wait()
}

func (p *Plugin) SystemPrompt() string { return "" }
func (p *Plugin) Tools() []plugin.Tool { return nil }

func (p *Plugin) langOf() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lang
}

func (p *Plugin) savedString(key string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return plugin.CfgString(p.saved, key, "")
}

func (p *Plugin) StatusLines(lang string) []string {
	bundles, problems := p.store.List()
	if len(bundles) == 0 && len(problems) == 0 {
		return []string{i18n.T(lang, "plugin.bundles.status.none", "🧩 外源插件包：一个都没装")}
	}
	var tools int
	for _, b := range bundles {
		ts, _ := p.store.LoadTools(b.Manifest.Name, b.Manifest.Version)
		tools += len(ts)
	}
	line := i18n.Tf(lang, "plugin.bundles.status.line", "🧩 外源插件包：%d 个，%d 件工具", len(bundles), tools)
	if n := len(problems); n > 0 {
		line += i18n.Tf(lang, "plugin.bundles.status.broken", "，%d 个读不出来", n)
	}
	if sources, err := p.store.Sources(); err == nil && len(sources) > 0 {
		var candidates, pending int
		for name, src := range sources {
			if src.Candidate != nil {
				candidates++
			}
			if p, _ := p.store.Pending(name); p != nil {
				pending++
			}
		}
		line += i18n.Tf(lang, "plugin.bundles.status.tracked", "，%d 个签名来源", len(sources))
		if candidates > 0 {
			line += i18n.Tf(lang, "plugin.bundles.status.updates", "，%d 个已验签更新", candidates)
		}
		if pending > 0 {
			line += i18n.Tf(lang, "plugin.bundles.status.pending", "，%d 个等待重启", pending)
		}
	}
	return []string{line}
}

func (p *Plugin) names() []string {
	bundles, _ := p.store.List()
	out := make([]string, 0, len(bundles))
	for _, b := range bundles {
		out = append(out, b.Manifest.Name)
	}
	return out
}

func (p *Plugin) installedSummary() string {
	names := p.names()
	if len(names) == 0 {
		return ""
	}
	return "· " + strings.Join(names, " · ")
}

func (p *Plugin) trackedSummary() string {
	sources, err := p.store.Sources()
	if err != nil || len(sources) == 0 {
		return ""
	}
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	return "· " + strings.Join(names, " · ")
}
