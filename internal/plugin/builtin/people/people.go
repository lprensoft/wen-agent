package people

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
	defaultMaxPeople = 30
	defaultMaxInject = 2 * 1024
)

// Plugin 是 people 系统插件。
type Plugin struct {
	mu sync.RWMutex

	lang           string // 进程语言，注入片段与工具文本据此取用
	base           string // 基准人物库目录（= 共享域）
	maxPeople      int
	maxInjectBytes int

	// 按可见域分出的库，惰性创建。单独一把锁：这张表的生命周期与配置无关。
	storesMu sync.Mutex
	stores   map[string]*Store
}

func New() *Plugin {
	return &Plugin{
		maxPeople:      defaultMaxPeople,
		maxInjectBytes: defaultMaxInject,
	}
}

func (p *Plugin) Name() string { return "people" }

func (p *Plugin) Category() string { return plugin.CategoryPersona }

func (p *Plugin) Description() string {
	return "人物库：角色生活里的朋友、家人、同事各有名字、关系与亲近度，随相处更新，演绎里的熟人只从这里来"
}

// Requires 硬依赖 roleplay：人物是角色的社交圈，没有角色就没有归属对象。
func (p *Plugin) Requires() []string { return []string{"roleplay"} }

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key: "max_people", Label: "人数上限", Type: plugin.FieldInt,
			Description: "人物库最多记多少人。超出后拒绝登记新人并提示先移除不再往来的。",
			Default:     defaultMaxPeople,
			Min:         plugin.IntPtr(5),
			Max:         plugin.IntPtr(200),
		},
		{
			Key: "max_inject_bytes", Label: "清单注入字节上限", Type: plugin.FieldInt,
			Description: "清单随每轮对话重复发送，因此需要上限。" +
				"超出时先省略关系与近况只留名字与亲近度，仍超出则只注明人数。",
			Default: defaultMaxInject,
			Min:     plugin.IntPtr(512),
			Max:     plugin.IntPtr(16 * 1024),
		},
	}
}

// Init 需要一个可写目录；没有则拒绝启用，避免把人物库散落到进程当前目录。
func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	if ictx.StateDir == "" {
		return fmt.Errorf("%s", prompt.T(ictx.Lang, "prompt.people.no_state_dir",
			"没有可用的持久化目录，无法保存人物库"))
	}

	p.storesMu.Lock()
	p.stores = map[string]*Store{}
	p.storesMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.lang = ictx.Lang
	p.base = filepath.Join(ictx.StateDir, "people")
	p.maxPeople = plugin.CfgInt(cfg, "max_people", defaultMaxPeople)
	p.maxInjectBytes = plugin.CfgInt(cfg, "max_inject_bytes", defaultMaxInject)
	return nil
}

func (p *Plugin) Tools() []plugin.Tool {
	return []plugin.Tool{
		&upsertTool{p: p},
		&listTool{p: p},
		&removeTool{p: p},
	}
}

// settings 是一次调用期间使用的配置快照。
type settings struct {
	lang           string
	base           string
	maxPeople      int
	maxInjectBytes int
}

// snapshot 取一份配置快照：SetConfig 会在运行时重新 Init，而工具可能正在执行。
func (p *Plugin) snapshot() settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return settings{
		lang:           p.lang,
		base:           p.base,
		maxPeople:      p.maxPeople,
		maxInjectBytes: p.maxInjectBytes,
	}
}

// SystemPrompt 注入维护判据，全部为静态内容。
// 清单本体改由 TurnPrompt 注入：它取决于本轮的可见域，且要访问磁盘。
func (p *Plugin) SystemPrompt() string { return guideFor(p.langOf()) }

// langOf 取进程语言。SystemPrompt 与 storeFor 在快照之外单独取一次，
// 它们不需要整份配置；零值（未初始化的插件）表示回落中文。
func (p *Plugin) langOf() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lang
}

// TurnPrompt 注入本轮可读的清单。一个人都没有时不注入：判据已在 SystemPrompt 里。
func (p *Plugin) TurnPrompt(ctx context.Context, _ plugin.TurnEvent) (string, error) {
	s := p.snapshot()
	if s.base == "" {
		return "", nil
	}
	ps, err := p.visiblePeople(ctx)
	if err != nil || len(ps) == 0 {
		return "", err
	}
	return stateHeaderFor(s.lang) + "\n" +
		renderPeople(s.lang, ps, s.maxInjectBytes, time.Now()), nil
}

// ---------- 给其它插件的只读查询 ----------

// Lookup 是交给其它插件的只读查询：日程插件排「和谁」时据此校验名字。
//
// 这是插件之间直接的 Go 依赖（由 main.go 经构造函数注入），不是核心机制。允许的条件
// 有三条且要同时满足：只读、接口窄到只答一个问题、双方用 Requires 绑定启用状态。
// 替代方案——对方自己读这里的文件，或不校验只靠提示词——都更差。
type Lookup interface {
	// Known 判断本轮可读域里有没有这个人（大小写不敏感），有则返回库里的规范写法。
	Known(ctx context.Context, name string) (canonical string, ok bool)
	// Names 返回本轮可读域里全部人物的名字，按记录顺序，用于报错时列候选。
	Names(ctx context.Context) []string
}

var _ Lookup = (*Plugin)(nil)

func (p *Plugin) Known(ctx context.Context, name string) (string, bool) {
	ps, err := p.visiblePeople(ctx)
	if err != nil {
		return "", false
	}
	if i := personIndex(ps, name); i >= 0 {
		return ps[i].Name, true
	}
	return "", false
}

func (p *Plugin) Names(ctx context.Context) []string {
	ps, err := p.visiblePeople(ctx)
	if err != nil {
		return nil
	}
	return names(ps)
}

// ---------- 按可见域分库 ----------

// storeFor 返回某个可见域的人物库（惰性创建）。未初始化时返回 nil。
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

// visiblePeople 合并本轮所有可读域的人物，保持各库内的记录顺序，写入域在前。
// 同名只保留一个，取自靠前的域：模型看到的是一份没有重影的清单。
func (p *Plugin) visiblePeople(ctx context.Context) ([]Person, error) {
	p.mu.RLock()
	base := p.base
	p.mu.RUnlock()
	if base == "" {
		return nil, nil
	}

	var (
		out  []Person
		seen = map[string]bool{}
		errs []string
	)
	for _, tag := range plugin.ReadDomains(base, plugin.ScopeFrom(ctx)) {
		s := p.storeFor(tag)
		if s == nil {
			continue
		}
		ps, err := s.List()
		if err != nil {
			errs = append(errs, err.Error())
			continue // 单个域读不出来不该让其余域也用不了
		}
		for _, person := range ps {
			key := strings.ToLower(person.Name)
			if seen[key] {
				continue
			}
			seen[key] = true
			person.Domain = tag
			out = append(out, person)
		}
	}
	if out == nil && len(errs) > 0 {
		return nil, fmt.Errorf(prompt.T(p.langOf(), "prompt.people.store.read_failed_all",
			"读取人物库失败: %s"), strings.Join(errs, "; "))
	}
	return out, nil
}

// ---------- 注入渲染 ----------

// renderPeople 按预算渲染清单，分三级降级：
// 全列（关系 + 亲近度 + 上次来往与摘要）→ 只留名字与亲近度 → 只注明人数。
// 降级保住存在性：名字在，模型才知道这个人存在，不会另编一个。
// 按记录顺序不按时间：按时间排会让每次更新都重排整块文本。
func renderPeople(lang string, ps []Person, maxBytes int, now time.Time) string {
	if out, ok := renderLines(lang, ps, maxBytes, now, true); ok {
		return out
	}
	if out, ok := renderLines(lang, ps, maxBytes, now, false); ok {
		return out
	}
	return prompt.Tf(lang, "prompt.people.render.budget_short",
		"（共 %d 人，注入预算不足以列出，可用 list_people 查看）", len(ps))
}

func renderLines(lang string, ps []Person, maxBytes int, now time.Time, full bool) (string, bool) {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString(renderPerson(lang, p, now, full))
		b.WriteString("\n")
	}
	out := strings.TrimRight(b.String(), "\n")
	if maxBytes > 0 && len(out) > maxBytes {
		return "", false
	}
	return out, true
}

// renderPerson 渲染一行：`林舟（大学同学，熟）上次来往 3 天前：一起吃了晚饭`。
// 精简形态只留 `林舟（熟）`。
func renderPerson(lang string, p Person, now time.Time, full bool) string {
	closeness := closenessLevels.For(lang, p.Closeness)
	if !full {
		return prompt.Tf(lang, "prompt.people.render.brief", "%s（%s）", p.Name, closeness)
	}
	s := prompt.Tf(lang, "prompt.people.render.full", "%s（%s，%s）", p.Name, p.Relation, closeness)
	if a := agoText(lang, p.LastMet, now); a != "" {
		s += prompt.Tf(lang, "prompt.people.render.last_met", "上次来往 %s", a)
		if p.LastNote != "" {
			s += prompt.Tf(lang, "prompt.people.render.last_note", "：%s", p.LastNote)
		}
	}
	return s
}

// agoText 把时刻化成「今天 / 昨天 / N 天前 / N 个月前 / N 年前」，按日历日算而不是
// 按 24 小时——昨晚见的面今天凌晨不该说「今天」。零值或倒流（改过系统时钟）时不标。
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
		return prompt.T(lang, "prompt.people.ago.today", "今天")
	case days == 1:
		return prompt.T(lang, "prompt.people.ago.yesterday", "昨天")
	}
	// 天数以上走全程序共用的时长口径。按日历日算得的天数换回时长再交给它，
	// 是为了让「N 天前 / N 个月前 / N 年前」的分档与别处一致。
	return prompt.Tf(lang, "prompt.people.ago.before", "%s前",
		prompt.Duration(lang, time.Duration(days)*24*time.Hour))
}
