package unspoken

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

const (
	defaultMaxEntries = 8
	defaultMaxInject  = 1024
)

// Plugin 是 unspoken 系统插件。
type Plugin struct {
	mu sync.RWMutex

	lang           string // 进程语言，注入片段与工具文本据此取用
	base           string // 基准清单库目录（= 共享域）
	maxEntries     int
	maxInjectBytes int

	// 按可见域分出的库，惰性创建。单独一把锁：这张表的生命周期与配置无关。
	storesMu sync.Mutex
	stores   map[string]*Store

	// 设置页操作的状态。StartAction 与 ActionState 会被 HTTP 并发调用，自带一把锁。
	actMu    sync.Mutex
	actState plugin.ActionState
}

func New() *Plugin {
	return &Plugin{maxEntries: defaultMaxEntries, maxInjectBytes: defaultMaxInject}
}

func (p *Plugin) Name() string { return "unspoken" }

func (p *Plugin) Category() string { return plugin.CategoryPersona }

func (p *Plugin) Description() string {
	return "心里话：角色没说出口的事——真实看法、憋着的话、在等的事——记成一份有上限的清单，每轮注入，决定态度与潜台词而不被念出来"
}

// Requires 硬依赖 roleplay：心里话是角色的，没有角色就没有归属对象。
func (p *Plugin) Requires() []string { return []string{"roleplay"} }

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key: "max_entries", Label: "条数上限", Type: plugin.FieldInt,
			Description: "心里最多同时记着几件事。满了自动放下最早的一条，并在回执里告诉模型。",
			Default:     defaultMaxEntries,
			Min:         plugin.IntPtr(1),
			Max:         plugin.IntPtr(20),
		},
		{
			Key: "max_inject_bytes", Label: "清单注入字节上限", Type: plugin.FieldInt,
			Description: "清单随每轮对话重复发送，因此需要上限。超出时只保留最近的几条并注明另有几条。",
			Default:     defaultMaxInject,
			Min:         plugin.IntPtr(256),
			Max:         plugin.IntPtr(8 * 1024),
		},
	}
}

// Init 需要一个可写目录；没有则拒绝启用，避免把清单散落到进程当前目录。
func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	if ictx.StateDir == "" {
		return fmt.Errorf("%s", prompt.T(ictx.Lang, "prompt.unspoken.no_state_dir",
			"没有可用的持久化目录，无法保存心里话"))
	}

	p.storesMu.Lock()
	p.stores = map[string]*Store{}
	p.storesMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.lang = ictx.Lang
	p.base = filepath.Join(ictx.StateDir, "unspoken")
	p.maxEntries = plugin.CfgInt(cfg, "max_entries", defaultMaxEntries)
	p.maxInjectBytes = plugin.CfgInt(cfg, "max_inject_bytes", defaultMaxInject)
	return nil
}

func (p *Plugin) Tools() []plugin.Tool {
	return []plugin.Tool{&keepTool{p: p}, &letGoTool{p: p}}
}

// settings 是一次调用期间使用的配置快照。
type settings struct {
	lang           string
	base           string
	maxEntries     int
	maxInjectBytes int
}

// snapshot 取一份配置快照：SetConfig 会在运行时重新 Init，而工具可能正在执行。
func (p *Plugin) snapshot() settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return settings{lang: p.lang, base: p.base, maxEntries: p.maxEntries, maxInjectBytes: p.maxInjectBytes}
}

// SystemPrompt 注入维护判据，全部为静态内容。
// 清单本体改由 TurnPrompt 注入：它取决于本轮的可见域，且要访问磁盘。
func (p *Plugin) SystemPrompt() string { return guideFor(p.langOf()) }

// langOf 取进程语言。未初始化时是空串，prompt.T 据此回中文——Manager.List 会对
// 未启用的插件调 SystemPrompt，走的正是这一档。
func (p *Plugin) langOf() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lang
}

// TurnPrompt 注入本轮的清单。一条都没有时不注入：判据已在 SystemPrompt 里。
func (p *Plugin) TurnPrompt(ctx context.Context, _ plugin.TurnEvent) (string, error) {
	s := p.snapshot()
	store := p.storeFor(plugin.ScopeFrom(ctx).Write)
	if store == nil {
		return "", nil
	}
	es, err := store.List()
	if err != nil || len(es) == 0 {
		return "", err
	}
	return stateHeaderFor(s.lang) + "\n" + renderEntries(s.lang, es, s.maxInjectBytes, time.Now()), nil
}

// storeFor 返回某个可见域的清单库（惰性创建）。未初始化时返回 nil。
func (p *Plugin) storeFor(tag string) *Store {
	p.mu.RLock()
	base, lang := p.base, p.lang
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
	s := NewStore(lang, plugin.DomainDir(base, tag))
	p.stores[tag] = s
	return s
}

// 清单**只读写本轮的写入域**，不跨域合并——心里话属于人格：里人格憋着的话不该成为
// 表人格的潜台词。各域独立还顺带让泄漏问题不存在：读的就是自己那一份。

// ---------- 注入渲染 ----------

// renderEntries 按预算渲染清单，按记下的先后一行一条：
//
//   - 3 天前：他忘了纪念日，说不在意其实还在意。
//   - 今天：想约他周末去看展，还没开口。
//
// 超出预算时从最早的开始省，末尾注明「另有 N 条更早的」；一条都放不下时只报条数。
// 降级保住存在性：模型知道心里还压着事，不会演成什么都没发生。
func renderEntries(lang string, es []Entry, maxBytes int, now time.Time) string {
	for skip := 0; skip < len(es); skip++ {
		out := renderLines(lang, es[skip:], now)
		if skip > 0 {
			out += prompt.Tf(lang, "prompt.unspoken.render.more", "\n（另有 %d 条更早的未列出）", skip)
		}
		if maxBytes <= 0 || len(out) <= maxBytes {
			return out
		}
	}
	return prompt.Tf(lang, "prompt.unspoken.render.over_budget", "（共 %d 条，注入预算不足以列出）", len(es))
}

func renderLines(lang string, es []Entry, now time.Time) string {
	var b strings.Builder
	for _, e := range es {
		b.WriteString("- ")
		if a := agoText(lang, e.Created, now); a != "" {
			b.WriteString(prompt.Tf(lang, "prompt.unspoken.render.when", "%s：", a))
		}
		b.WriteString(e.Text + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// agoText 把时刻化成「今天 / 昨天 / N 天前 / N 个月前 / N 年前」，按日历日算而不是
// 按 24 小时——昨晚记下的事今天凌晨不该说「今天」。零值或倒流（改过系统时钟）时不标。
// 口径与人物库一致。
func agoText(lang string, t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	day := func(x time.Time) time.Time {
		y, m, d := x.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, x.Location())
	}
	days := int(day(now).Sub(day(t.In(now.Location()))).Hours() / 24)
	switch {
	case days < 0:
		return ""
	case days == 0:
		return prompt.T(lang, "prompt.unspoken.ago.today", "今天")
	case days == 1:
		return prompt.T(lang, "prompt.unspoken.ago.yesterday", "昨天")
	}
	// 天数以上走全程序共用的时长口径，分档与别处一致。
	return prompt.Tf(lang, "prompt.unspoken.ago.before", "%s前",
		prompt.Duration(lang, time.Duration(days)*24*time.Hour))
}
