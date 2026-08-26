package belongings

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

const (
	defaultMaxContainers = 10
	defaultMaxItems      = 50
	defaultMaxInject     = 4 * 1024
)

// Plugin 是 belongings 系统插件。
type Plugin struct {
	mu sync.RWMutex

	lang           string // 进程语言，注入片段与工具文本据此取用
	base           string // 基准清单库目录（= 共享域）
	maxContainers  int
	maxItems       int
	maxInjectBytes int

	// 按可见域分出的库，惰性创建。单独一把锁：这张表的生命周期与配置无关。
	storesMu sync.Mutex
	stores   map[string]*Store
}

func New() *Plugin {
	return &Plugin{
		maxContainers:  defaultMaxContainers,
		maxItems:       defaultMaxItems,
		maxInjectBytes: defaultMaxInject,
	}
}

func (p *Plugin) Name() string { return "belongings" }

func (p *Plugin) Category() string { return plugin.CategoryPersona }

func (p *Plugin) Description() string {
	return "持有物清单：把冰箱、衣柜这类容器里的物品记成清单，随演绎增减，跨会话与压缩不丢"
}

// Requires 硬依赖 roleplay：清单描述的是角色的持有物，没有角色就没有归属对象。
func (p *Plugin) Requires() []string { return []string{"roleplay"} }

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key: "max_containers", Label: "容器数量上限", Type: plugin.FieldInt,
			Description: "冰箱、衣柜这类容器最多建多少个。",
			Default:     defaultMaxContainers,
			Min:         plugin.IntPtr(1),
			Max:         plugin.IntPtr(50),
		},
		{
			Key: "max_items", Label: "单个容器的物品上限", Type: plugin.FieldInt,
			Description: "超出后拒绝放入并提示先清理，清单不该长成仓库账。",
			Default:     defaultMaxItems,
			Min:         plugin.IntPtr(1),
			Max:         plugin.IntPtr(500),
		},
		{
			Key: "max_inject_bytes", Label: "清单注入字节上限", Type: plugin.FieldInt,
			Description: "清单随每轮对话重复发送，因此需要上限。" +
				"超出时先省略物品只报件数，仍超出则只注明容器数。",
			Default: defaultMaxInject,
			Min:     plugin.IntPtr(512),
			Max:     plugin.IntPtr(64 * 1024),
		},
	}
}

// Init 需要一个可写目录；没有则拒绝启用，避免把清单散落到进程当前目录。
func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	if ictx.StateDir == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.belongings.no_state_dir",
			"没有可用的持久化目录，无法保存持有物清单"))
	}

	p.storesMu.Lock()
	p.stores = map[string]*Store{}
	p.storesMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.lang = ictx.Lang
	p.base = filepath.Join(ictx.StateDir, "belongings")
	p.maxContainers = plugin.CfgInt(cfg, "max_containers", defaultMaxContainers)
	p.maxItems = plugin.CfgInt(cfg, "max_items", defaultMaxItems)
	p.maxInjectBytes = plugin.CfgInt(cfg, "max_inject_bytes", defaultMaxInject)
	return nil
}

func (p *Plugin) Tools() []plugin.Tool {
	return []plugin.Tool{
		&updateTool{p: p},
		&listTool{p: p},
	}
}

// langOf 取进程语言。SystemPrompt 与 storeFor 在快照之外单独取一次，
// 它们不需要整份配置；零值（未初始化的插件）表示回落中文。
func (p *Plugin) langOf() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lang
}

// settings 是一次调用期间使用的配置快照。
type settings struct {
	lang           string
	base           string
	maxContainers  int
	maxItems       int
	maxInjectBytes int
}

// snapshot 取一份配置快照：SetConfig 会在运行时重新 Init，而工具可能正在执行。
func (p *Plugin) snapshot() settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return settings{
		lang:           p.lang,
		base:           p.base,
		maxContainers:  p.maxContainers,
		maxItems:       p.maxItems,
		maxInjectBytes: p.maxInjectBytes,
	}
}

// SystemPrompt 注入维护判据，全部为静态内容。
// 清单本体改由 TurnPrompt 注入：它取决于本轮的可见域，且要访问磁盘。
func (p *Plugin) SystemPrompt() string { return guideFor(p.langOf()) }

// TurnPrompt 注入本轮可读的清单。一个容器都没有时不注入：判据已在 SystemPrompt 里。
func (p *Plugin) TurnPrompt(ctx context.Context, _ plugin.TurnEvent) (string, error) {
	s := p.snapshot()
	if s.base == "" {
		return "", nil
	}
	cs, err := p.visibleContainers(ctx)
	if err != nil || len(cs) == 0 {
		return "", err
	}
	return stateHeaderFor(s.lang) + "\n" +
		renderContainers(s.lang, cs, s.maxInjectBytes, time.Now()), nil
}

// ---------- 按可见域分库 ----------

// storeFor 返回某个可见域的清单库（惰性创建）。未初始化时返回 nil。
func (p *Plugin) storeFor(tag string) *Store {
	p.mu.RLock()
	base := p.base
	p.mu.RUnlock()
	if base == "" {
		return nil
	}

	p.storesMu.Lock()
	defer p.storesMu.Unlock()
	if p.stores == nil {
		p.stores = map[string]*Store{}
	}
	if s, ok := p.stores[tag]; ok {
		return s
	}
	s := NewStore(p.langOf(), plugin.DomainDir(base, tag))
	p.stores[tag] = s
	return s
}

// writeStore 返回本轮该写入的库。
func (p *Plugin) writeStore(ctx context.Context) *Store {
	return p.storeFor(plugin.ScopeFrom(ctx).Write)
}

// visibleContainers 合并本轮所有可读域的容器，保持各库内的记录顺序，写入域在前。
// 同名容器只保留一个，取自靠前的域：模型看到的是一份没有重影的清单。
func (p *Plugin) visibleContainers(ctx context.Context) ([]Container, error) {
	p.mu.RLock()
	base := p.base
	p.mu.RUnlock()

	var (
		out  []Container
		seen = map[string]bool{}
		errs []string
	)
	for _, tag := range plugin.ReadDomains(base, plugin.ScopeFrom(ctx)) {
		s := p.storeFor(tag)
		if s == nil {
			continue
		}
		cs, err := s.List()
		if err != nil {
			errs = append(errs, err.Error())
			continue // 单个域读不出来不该让其余域也用不了
		}
		for _, c := range cs {
			key := strings.ToLower(c.Name)
			if seen[key] {
				continue
			}
			seen[key] = true
			c.Domain = tag
			out = append(out, c)
		}
	}
	if out == nil && len(errs) > 0 {
		return nil, fmt.Errorf(prompt.T(p.langOf(), "prompt.belongings.store.read_failed",
			"读取持有物清单失败: %s"), strings.Join(errs, "; "))
	}
	return out, nil
}

// ---------- 注入渲染 ----------

// renderContainers 按预算渲染清单，分三级降级：
// 全列（物品 + 备注 + 入库时间）→ 每容器只报件数 → 只注明容器数。
// 降级保住存在性：容器名在，模型才知道那处清单存在，不会凭空另起一份。
func renderContainers(lang string, cs []Container, maxBytes int, now time.Time) string {
	if out, ok := renderLines(lang, cs, maxBytes, now, true); ok {
		return out
	}
	if out, ok := renderLines(lang, cs, maxBytes, now, false); ok {
		return out
	}
	return prompt.Tf(lang, "prompt.belongings.render.budget_short",
		"（共 %d 个容器，注入预算不足以列出，可用 list_items 查看）", len(cs))
}

func renderLines(lang string, cs []Container, maxBytes int, now time.Time, withItems bool) (string, bool) {
	var b strings.Builder
	for _, c := range cs {
		b.WriteString(c.Name)
		b.WriteString(prompt.T(lang, "prompt.belongings.render.colon", "："))
		switch {
		case len(c.Items) == 0:
			b.WriteString(prompt.T(lang, "prompt.belongings.render.empty", "（空）"))
		case withItems:
			for i, it := range c.Items {
				if i > 0 {
					b.WriteString(prompt.T(lang, "prompt.belongings.render.sep", "、"))
				}
				b.WriteString(renderItem(lang, it, now))
			}
		default:
			b.WriteString(prompt.Tf(lang, "prompt.belongings.render.count_only",
				"（%d 项，可用 list_items 查看）", len(c.Items)))
		}
		b.WriteString("\n")
	}
	out := strings.TrimRight(b.String(), "\n")
	if maxBytes > 0 && len(out) > maxBytes {
		return "", false
	}
	return out, true
}

// renderItem 渲染单件物品：名称、数量、备注与入库时间。
// 不满一天的不标时间——今天放进去的默认就是新的，标了只是噪声。
func renderItem(lang string, it Item, now time.Time) string {
	s := it.Name
	if it.Qty > 0 {
		s += fmt.Sprintf("×%d", it.Qty)
	}
	var extras []string
	if it.Note != "" {
		extras = append(extras, it.Note)
	}
	if a := ageNote(lang, it.Added, now); a != "" {
		extras = append(extras, a)
	}
	if len(extras) > 0 {
		s += prompt.T(lang, "prompt.belongings.render.extras_open", "（") +
			strings.Join(extras, prompt.T(lang, "prompt.belongings.render.extras_sep", "；")) +
			prompt.T(lang, "prompt.belongings.render.extras_close", "）")
	}
	return s
}

// ageNote 把入库时刻化成「N 天前放入」。时长的说法走全程序共用的那一份
// （prompt.Duration），因此这里只判「够不够一天」——不满一天的不标，今天放进去的
// 默认就是新的，标了只是噪声。时刻缺失或倒流（改过系统时钟）时不标，不猜。
func ageNote(lang string, added, now time.Time) string {
	if added.IsZero() {
		return ""
	}
	gap := now.Sub(added)
	if gap < 24*time.Hour {
		return ""
	}
	return prompt.Tf(lang, "prompt.belongings.item.age", "%s前放入", prompt.Duration(lang, gap))
}
