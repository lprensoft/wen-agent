package presence

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

// staleAfter 是字段「注明多久前更新」的门槛：新鲜的字段不标时间，隔久了的才标——
// 给模型一个判断「这项是否已自然失效」的线索，又不让每行都挂着时间戳。
const staleAfter = 30 * time.Minute

// Plugin 是 presence 系统插件。
type Plugin struct {
	mu sync.RWMutex

	lang string
	base string // 基准快照库目录（= 共享域）

	// 按可见域分出的库，惰性创建。单独一把锁：这张表的生命周期与配置无关。
	storesMu sync.Mutex
	stores   map[string]*Store

	// 设置页操作的状态。StartAction 与 ActionState 会被 HTTP 并发调用，自带一把锁。
	actMu    sync.Mutex
	actState plugin.ActionState
}

func New() *Plugin { return &Plugin{} }

func (p *Plugin) Name() string { return "presence" }

func (p *Plugin) Category() string { return plugin.CategoryPersona }

func (p *Plugin) Description() string {
	return "现场状态：把所在、穿着、姿态、在做的事记成一份快照，每轮注入，跨会话与压缩不丢"
}

// Requires 硬依赖 roleplay：快照描述的是角色的现场，没有角色就没有作用对象。
func (p *Plugin) Requires() []string { return []string{"roleplay"} }

// Init 需要一个可写目录；没有则拒绝启用，避免把快照散落到进程当前目录。
func (p *Plugin) Init(ictx plugin.InitContext, _ map[string]any) error {
	if ictx.StateDir == "" {
		return fmt.Errorf("没有可用的持久化目录，无法保存现场状态")
	}

	p.storesMu.Lock()
	p.stores = map[string]*Store{}
	p.storesMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.lang = ictx.Lang
	p.base = filepath.Join(ictx.StateDir, "presence")
	return nil
}

func (p *Plugin) Tools() []plugin.Tool {
	return []plugin.Tool{&updateTool{p: p}}
}

// SystemPrompt 注入维护判据，全部为静态内容。
// 快照本体改由 TurnPrompt 注入：它取决于本轮的可见域，且要访问磁盘。
func (p *Plugin) SystemPrompt() string {
	return prompt.T(p.snapshotLang(), "prompt.presence.guide", guidePromptZH)
}

// snapshotLang 取进程语言。加锁是因为 Init 可重入，而提示词可能正在生成。
func (p *Plugin) snapshotLang() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lang
}

// TurnPrompt 注入本轮的现场快照。没有记录时不注入。
func (p *Plugin) TurnPrompt(ctx context.Context, _ plugin.TurnEvent) (string, error) {
	store := p.storeFor(plugin.ScopeFrom(ctx).Write)
	if store == nil {
		return "", nil
	}
	snap, ok, err := store.Load()
	if err != nil || !ok {
		return "", err
	}
	lang := p.snapshotLang()
	return prompt.T(lang, "prompt.presence.state_header", stateHeaderZH) + "\n" +
		renderSnapshot(lang, snap, time.Now()), nil
}

// storeFor 返回某个可见域的快照库（惰性创建）。未初始化时返回 nil。
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
	s := NewStore(p.snapshotLang(), plugin.DomainDir(base, tag))
	p.stores[tag] = s
	return s
}

// 快照**只读写本轮的写入域**，不跨域合并——「当下在哪、穿着什么」属于正在进行的
// 那条故事线：表人格的办公室场景与里人格的深夜场景各自成立，合并只会得到一个
// 两边都不对的现场。代价是切换人格时现场跟着换一套，而这正是两条线该有的样子。

// ---------- 注入渲染 ----------

// renderSnapshot 按 fieldDefs 的顺序渲染快照。字段长度都有硬上限，整块体量
// 有界（全字段填满约七百字），不需要预算与降级。
// 隔了超过 staleAfter 才更新过的字段附上「N 前」，给模型判断时效的线索。
func renderSnapshot(lang string, snap Snapshot, now time.Time) string {
	var b strings.Builder
	for _, d := range fieldDefs {
		f, ok := snap[d.key]
		if !ok || f.Text == "" {
			continue
		}
		line := d.label.For(lang) + prompt.T(lang, "prompt.presence.field_sep", "：") + f.Text
		if !f.Updated.IsZero() {
			if gap := now.Sub(f.Updated); gap >= staleAfter {
				line += prompt.Tf(lang, "prompt.presence.noted_ago", "（%s前记下）", prompt.Duration(lang, gap))
			}
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
