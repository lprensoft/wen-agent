// Package stylewatch 提供文风观察的系统插件：每轮对话结束后，用 stylecheck 的规则
// 检查助手的最终文本里有没有「助手腔」，并按天累计命中、字数与【】演绎占比。
//
// 本期只测量、只记录：不改写回复、不重试、不把结果注入模型上下文。检测结果落在
// 三个只给人看的地方——按天的统计文件、状态行、可选的会话注记。先有数字，才谈得上
// 之后要不要干预、怎么干预；在那之前任何干预都是盲调。
//
// 检测的是角色扮演里的助手腔——写代码的时候用列表与加粗是对的——所以硬依赖
// roleplay，与 mood、presence 同一组。
package stylewatch

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/stylecheck"
)

const (
	defaultNotify = true
	// stopWait 是 Stop 等待在途统计收尾的上限。统计只是几次正则加一次写盘，
	// 正常情况下瞬间完成；上限只防写盘卡在坏掉的磁盘上。
	stopWait = 3 * time.Second
	// noticeMaxRunes 是一条文风注记的长度上限：它是旁注，要一眼扫完。
	noticeMaxRunes = 200
)

// Plugin 是 style_watch 系统插件。
type Plugin struct {
	mu sync.RWMutex

	langCode string
	dir      string
	notify   bool
	ignore   map[string]bool
	notice   plugin.NoticeFunc
	ctx      context.Context // 后台 goroutine 用的 ctx：广播进来的那个在轮次结束时就被取消
	cancel   context.CancelFunc

	// 统计状态。stopped 让 wg.Add 与 wg.Wait 有明确的先后：两者都在 statsMu 内决定。
	statsMu sync.Mutex
	st      stats
	seq     uint64
	stopped bool
	wg      sync.WaitGroup

	// 写盘串行化：每轮都起 goroutine 写盘时完成顺序不保证，晚到的旧快照不能盖掉新进展。
	saveMu   sync.Mutex
	savedSeq uint64

	// 设置页操作的状态。StartAction 与 ActionState 会被 HTTP 并发调用。
	actMu     sync.Mutex
	actStates map[string]plugin.ActionState
}

func New() *Plugin {
	return &Plugin{notify: defaultNotify, ignore: map[string]bool{}, actStates: map[string]plugin.ActionState{}}
}

func (p *Plugin) Name() string { return "style_watch" }

func (p *Plugin) Category() string { return plugin.CategoryPersona }

func (p *Plugin) Description() string {
	return "文风观察：每轮检查回复里的助手腔（自称 AI、客套、套话、列表加粗、emoji 等）并按天统计，只测量不干预"
}

// Requires 硬依赖 roleplay：检测的是角色扮演里的助手腔，没有角色就没有「腔」可言——
// 写代码的时候用列表与加粗是对的。
func (p *Plugin) Requires() []string { return []string{"roleplay"} }

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key: "notify", Label: "命中时写会话注记", Type: plugin.FieldBool,
			Description: "命中助手腔时在当前会话里留一行「文风提醒」。只给人看，不进模型上下文，" +
				"不改变回复。一轮多处命中合并成一条。",
			Default: defaultNotify,
		},
		{
			Key: "ignore_rules", Label: "忽略的规则", Type: plugin.FieldText,
			Description: "一行一个规则 id，被忽略的规则既不计入统计也不提醒。可用的规则：" + ruleCatalog() + "。",
			Default:     "",
		},
	}
}

// lang 取进程语言。加锁是因为 Init 可重入，而统计与注记可能正在渲染。
func (p *Plugin) lang() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.langCode
}

// ruleCatalog 把规则表渲染成「id（标签）」的顿号列表，给配置项说明用。
//
// 它进的是 ConfigField 的说明，而那句说明按请求语言翻译（走 i18n），这里拿不到
// 请求——所以标签一律用中文原文，与说明的中文原文同语言；英文那份说明在语言包里
// 自带英文的标签列表。
func ruleCatalog() string {
	rs := stylecheck.Rules()
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, r.ID+"（"+r.Label.ZH()+"）")
	}
	return strings.Join(parts, "、")
}

// Init 需要一个可写目录；没有则拒绝启用。可重入：先停旧的后台活动再装新配置。
func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	if ictx.StateDir == "" {
		return fmt.Errorf("没有可用的持久化目录，无法保存统计")
	}
	ignore, err := parseIgnore(ictx.Lang, plugin.CfgString(cfg, "ignore_rules", ""))
	if err != nil {
		return err
	}

	p.stopWorkers()

	p.mu.Lock()
	p.langCode = ictx.Lang
	p.dir = ictx.StateDir
	p.notify = plugin.CfgBool(cfg, "notify", defaultNotify)
	p.ignore = ignore
	p.notice = ictx.Notice
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.mu.Unlock()

	p.statsMu.Lock()
	p.st = loadStats(ictx.StateDir)
	p.stopped = false
	p.statsMu.Unlock()
	return nil
}

// parseIgnore 解析一行一个的规则 id。未知的 id 当作错误报出：配置是在设置页保存时
// 校验的，一个拼错的 id 静默忽略掉，用户会以为它生效了。
func parseIgnore(lang, text string) (map[string]bool, error) {
	out := map[string]bool{}
	var unknown []string
	for _, line := range strings.Split(text, "\n") {
		id := strings.TrimSpace(line)
		if id == "" || strings.HasPrefix(id, "#") {
			continue
		}
		if !stylecheck.Known(id) {
			unknown = append(unknown, id)
			continue
		}
		out[id] = true
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf(i18n.T(lang, "plugin.style_watch.unknown_rule",
			"未知的规则 id：%s；可用的规则：%s"), strings.Join(unknown, "、"), ruleCatalog())
	}
	return out, nil
}

// SystemPrompt 不注入任何内容：本期只测量，不对模型说任何话。
func (p *Plugin) SystemPrompt() string { return "" }

func (p *Plugin) Tools() []plugin.Tool { return []plugin.Tool{&reportTool{p: p}} }

// settings 是一次调用期间使用的配置快照。
type settings struct {
	lang   string
	dir    string
	notify bool
	ignore map[string]bool
	notice plugin.NoticeFunc
	ctx    context.Context
}

func (p *Plugin) snapshot() settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return settings{lang: p.langCode, dir: p.dir, notify: p.notify, ignore: p.ignore, notice: p.notice, ctx: p.ctx}
}

// OnTurnEnd 把本轮的检测结果计入当天的统计。在轮次收尾的同步路径上被调用，
// 检测与写盘都放到 goroutine 里，这里只做登记。
func (p *Plugin) OnTurnEnd(ctx context.Context, ev plugin.TurnEndEvent) {
	text := strings.TrimSpace(ev.FinalText)
	if text == "" {
		return // 决定不开口的轮次没有文风可言
	}
	s := p.snapshot()
	if s.ctx == nil {
		return
	}
	// 可见域必须当场从 ctx 取：广播用的是本轮的 ctx，它在轮次返回后立即被取消。
	scope := plugin.ScopeFrom(ctx)

	p.statsMu.Lock()
	if p.stopped {
		p.statsMu.Unlock()
		return
	}
	p.wg.Add(1)
	p.statsMu.Unlock()

	go func() {
		defer p.wg.Done()
		p.record(s, scope, ev, text)
	}()
}

// record 跑检测、计入统计、落盘，命中时留注记。在后台 goroutine 中执行。
func (p *Plugin) record(s settings, scope plugin.Scope, ev plugin.TurnEndEvent, text string) {
	hits := filterHits(stylecheck.Check(s.lang, text), s.ignore)
	m := stylecheck.Measure(s.lang, text)

	p.statsMu.Lock()
	day := p.st.day(dateOf(ev.EndedAt))
	if ev.Interactive {
		day.Interactive.add(m, hits)
	} else {
		day.Background.add(m, hits)
	}
	p.seq++
	snap, seq := p.st.clone(), p.seq
	p.statsMu.Unlock()

	p.persist(s.dir, snap, seq)

	if s.notify && len(hits) > 0 && s.notice != nil && ev.SessionID != "" {
		if err := s.notice(plugin.WithScope(s.ctx, scope), ev.SessionID, renderNotice(s.lang, hits)); err != nil {
			i18n.Logf(s.lang, "log.stylewatch.notice_failed", "文风观察：注记写入失败：%v", err)
		}
	}
}

// persist 串行写盘并丢弃过期的快照。
func (p *Plugin) persist(dir string, snap stats, seq uint64) {
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	if seq <= p.savedSeq {
		return // 已经写过更新的快照了
	}
	p.savedSeq = seq
	if err := saveStats(dir, snap); err != nil {
		i18n.Logf(p.lang(), "log.stylewatch.stats_write_failed", "文风观察：统计写入失败：%v", err)
	}
}

// filterHits 去掉被忽略的规则。
func filterHits(hits []stylecheck.Hit, ignore map[string]bool) []stylecheck.Hit {
	if len(ignore) == 0 {
		return hits
	}
	out := hits[:0:0]
	for _, h := range hits {
		if !ignore[h.Rule] {
			out = append(out, h)
		}
	}
	return out
}

// renderNotice 把一轮的命中合并成一条注记：同一规则只举一处片段，多处带次数。
func renderNotice(lang string, hits []stylecheck.Hit) string {
	type agg struct {
		label, snippet string
		n              int
	}
	var order []string
	byRule := map[string]*agg{}
	for _, h := range hits {
		a, ok := byRule[h.Rule]
		if !ok {
			a = &agg{label: h.Label, snippet: h.Snippet}
			byRule[h.Rule] = a
			order = append(order, h.Rule)
		}
		a.n++
	}
	parts := make([]string, 0, len(order))
	for _, id := range order {
		a := byRule[id]
		s := a.label
		if a.n > 1 {
			s += fmt.Sprintf(" ×%d", a.n)
		}
		if a.snippet != "" {
			s += i18n.Tf(lang, "plugin.style_watch.snippet", "「%s」", a.snippet)
		}
		parts = append(parts, s)
	}
	return clipRunes(i18n.Tf(lang, "plugin.style_watch.notice", "✍️ 文风提醒：%s",
		strings.Join(parts, i18n.T(lang, "plugin.style_watch.sep", "、"))), noticeMaxRunes)
}

func clipRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// dateOf 取本地日期，统计按它分天。
func dateOf(t time.Time) string { return t.Local().Format("2006-01-02") }

// Stop 取消后台 ctx 并等待已起的 goroutine 收尾。
func (p *Plugin) Stop() { p.stopWorkers() }

func (p *Plugin) stopWorkers() {
	p.statsMu.Lock()
	p.stopped = true
	p.statsMu.Unlock()

	p.mu.Lock()
	cancel := p.cancel
	p.cancel, p.ctx = nil, nil
	p.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(stopWait):
	}
}

// recent 返回最近 n 天的统计（新的在前），只含有记录的日子。
func (p *Plugin) recent(n int) []dayStat {
	p.statsMu.Lock()
	snap := p.st.clone()
	p.statsMu.Unlock()
	days := snap.Days
	sort.Slice(days, func(i, j int) bool { return days[i].Date > days[j].Date })
	if len(days) > n {
		days = days[:n]
	}
	return days
}

var (
	_ plugin.TurnObserver   = (*Plugin)(nil)
	_ plugin.StatusReporter = (*Plugin)(nil)
	_ plugin.Actionable     = (*Plugin)(nil)
	_ plugin.Stoppable      = (*Plugin)(nil)
	_ plugin.Dependent      = (*Plugin)(nil)
	_ plugin.Configurable   = (*Plugin)(nil)
)
