package relationship

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

// Plugin 是 relationship 系统插件。
type Plugin struct {
	mu sync.RWMutex

	lang string // 进程语言，注入片段与工具文本据此取用
	base string // 基准快照库目录（= 共享域）

	// 按可见域分出的库，惰性创建。单独一把锁：这张表的生命周期与配置无关。
	storesMu sync.Mutex
	stores   map[string]*Store

	// 设置页操作的状态。StartAction 与 ActionState 会被 HTTP 并发调用，自带一把锁。
	actMu    sync.Mutex
	actState plugin.ActionState
}

func New() *Plugin { return &Plugin{} }

func (p *Plugin) Name() string { return "relationship" }

func (p *Plugin) Category() string { return plugin.CategoryPersona }

func (p *Plugin) Description() string {
	return "人际感知：角色与对方处在什么阶段、彼此怎么称呼、最近的变动、默契与禁区记成一份快照，每轮注入，态度不再忽远忽近"
}

// Requires 硬依赖 roleplay：关系是角色与对方之间的，没有角色就没有归属对象。
func (p *Plugin) Requires() []string { return []string{"roleplay"} }

// Init 需要一个可写目录；没有则拒绝启用，避免把快照散落到进程当前目录。
func (p *Plugin) Init(ictx plugin.InitContext, _ map[string]any) error {
	if ictx.StateDir == "" {
		return fmt.Errorf("%s", prompt.T(ictx.Lang, "prompt.relationship.no_state_dir",
			"没有可用的持久化目录，无法保存关系状态"))
	}

	p.storesMu.Lock()
	p.stores = map[string]*Store{}
	p.storesMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.lang = ictx.Lang
	p.base = filepath.Join(ictx.StateDir, "relationship")
	return nil
}

func (p *Plugin) Tools() []plugin.Tool {
	return []plugin.Tool{&updateTool{p: p}}
}

// SystemPrompt 注入维护判据，全部为静态内容。
// 快照本体改由 TurnPrompt 注入：它取决于本轮的可见域，且要访问磁盘。
func (p *Plugin) SystemPrompt() string { return guideFor(p.langOf()) }

// langOf 取进程语言。未初始化时是空串，prompt.T 据此回中文——Manager.List 会对
// 未启用的插件调 SystemPrompt，走的正是这一档。
func (p *Plugin) langOf() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lang
}

// TurnPrompt 注入本轮的关系快照。没有记录、或只剩一条已过期的「最近」时不注入。
func (p *Plugin) TurnPrompt(ctx context.Context, _ plugin.TurnEvent) (string, error) {
	lang := p.langOf()
	store := p.storeFor(plugin.ScopeFrom(ctx).Write)
	if store == nil {
		return "", nil
	}
	snap, ok, err := store.Load()
	if err != nil || !ok {
		return "", err
	}
	body := renderSnapshot(lang, snap, time.Now())
	if body == "" {
		return "", nil
	}
	return stateHeaderFor(lang) + "\n" + body, nil
}

// storeFor 返回某个可见域的快照库（惰性创建）。未初始化时返回 nil。
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

// 快照**只读写本轮的写入域**，不跨域合并——关系属于人格：表人格与对方是老友，
// 里人格与对方是恋人，两份各自成立，合并只会得到一个两边都不对的态度。
// 代价是切换人格时关系跟着换一套，而这正是两条线该有的样子。

// ---------- 注入渲染 ----------

// renderSnapshot 渲染快照。字段长度都有硬上限，整块体量有界（全字段填满不到三百字），
// 不需要预算与降级。样子：
//
//	阶段：热恋。你叫对方「阿宁」，对方叫你「小舟」。
//	最近：3 天前为对方忘了纪念日吵过一架，昨天和好了。
//	默契与禁区：对方不爱被催；晚安一定要说；别提前任。
//	对方近况：项目上线前在赶工。
func renderSnapshot(lang string, snap Snapshot, now time.Time) string {
	var lines []string
	if head := renderHead(lang, snap); head != "" {
		lines = append(lines, head)
	}
	if snap.Recent != "" {
		if prefix, ok := recentPrefix(lang, snap.RecentAt, now); ok {
			lines = append(lines, prompt.Tf(lang, "prompt.relationship.render.recent", "最近：%s%s",
				prefix, sentence(snap.Recent)))
		}
	}
	if len(snap.Bonds) > 0 {
		lines = append(lines, prompt.Tf(lang, "prompt.relationship.render.bonds", "默契与禁区：%s。",
			strings.Join(snap.Bonds, prompt.T(lang, "prompt.relationship.render.bond_sep", "；"))))
	}
	if snap.TheirNow != "" {
		lines = append(lines, prompt.Tf(lang, "prompt.relationship.render.their_now", "对方近况：%s",
			sentence(snap.TheirNow)))
	}
	return strings.Join(lines, "\n")
}

// renderHead 渲染第一行：阶段与称呼。三项都可缺，缺哪项省哪句。
func renderHead(lang string, snap Snapshot) string {
	var parts []string
	if snap.Stage != "" {
		parts = append(parts, prompt.Tf(lang, "prompt.relationship.render.stage", "阶段：%s。", snap.Stage))
	}
	switch {
	case snap.MyCall != "" && snap.TheirCall != "":
		parts = append(parts, prompt.Tf(lang, "prompt.relationship.render.calls_both",
			"你叫对方「%s」，对方叫你「%s」。", snap.MyCall, snap.TheirCall))
	case snap.MyCall != "":
		parts = append(parts, prompt.Tf(lang, "prompt.relationship.render.call_mine",
			"你叫对方「%s」。", snap.MyCall))
	case snap.TheirCall != "":
		parts = append(parts, prompt.Tf(lang, "prompt.relationship.render.call_theirs",
			"对方叫你「%s」。", snap.TheirCall))
	}
	return strings.Join(parts, "")
}

// recentPrefix 给「最近」配上时间前缀。过了有效期返回 false，整行不注入——过期的
// 「最近」不是最近。没有时刻（手改过的文件）或时钟倒流时不标前缀但仍注入。
func recentPrefix(lang string, at, now time.Time) (string, bool) {
	if at.IsZero() {
		return "", true
	}
	if now.Sub(at) > recentTTL {
		return "", false
	}
	return agoText(lang, at, now), true
}

// sentence 给一句话补上句号，已有终结标点的不重复加。
func sentence(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	if strings.ContainsRune("。！？…~～", r[len(r)-1]) {
		return s
	}
	return s + "。"
}

// agoText 把时刻化成「今天 / 昨天 / N 天前」，按日历日算而不是按 24 小时——昨晚的事
// 今天凌晨不该说「今天」。零值或倒流（改过系统时钟）时不标。口径与人物库一致。
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
		return prompt.T(lang, "prompt.relationship.ago.today", "今天")
	case days == 1:
		return prompt.T(lang, "prompt.relationship.ago.yesterday", "昨天")
	default:
		return prompt.Tf(lang, "prompt.relationship.ago.days", "%d 天前", days)
	}
}
