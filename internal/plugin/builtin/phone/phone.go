// Package phone 提供小手机的系统插件：角色有自己的手机，闲下来时刷一刷订阅的消息
// （RSS / Atom），刷到的几条每轮注入，碰上她感兴趣的就投一条开口理由。
//
// 它补的是「各自有生活」的另一半：一个人的话题不全来自对面，她有自己的信息流，偶尔
// 分享给你。插件只负责「她刷到了什么」，提不提、怎么提归模型。
//
// **只在闲着的时候刷。** 不是定时器到点就刷：睡着、重忙、正和对方聊着天的时候，人不会
// 去刷手机。到点时先看忙碌公告板（internal/availability，心跳写睡眠、日程写忙碌）与
// 最近一次真人交互，不闲就往后挪到闲下来——有结束时刻的挪到结束之后，没有的等公告板
// 变化再重新判断。所以一觉醒来的第一次刷新会一下子攒着一夜的消息，按「每个源只留最新
// 几条」收成一批，发布时间照实标出来，夜里的就是夜里的。
package phone

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"wen/internal/availability"
	"wen/internal/cue"
	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

const (
	defRefreshHours = 3
	maxFeeds        = 20

	chatQuiet  = 15 * time.Minute // 对方最近这么久内来过，就当她正在聊天
	afterBusy  = 5 * time.Minute  // 忙完之后再过一会儿才刷：刚收尾那一刻还在收拾
	recheck    = 30 * time.Minute // 忙碌没有结束时刻时多久再看一眼（公告板一变也会叫醒）
	startGrace = time.Minute      // 刚启动时不立刻刷：服务刚起来那一秒不该先去联网

	cueTTL      = 3 * time.Hour  // 「刚看到」的新闻过了这么久还没说出口就不算数了
	injectFresh = 24 * time.Hour // 只注入一天内刷到的：再旧就不是「刚看到」的了
	maxInjected = 3
)

// Plugin 是 phone 系统插件。
type Plugin struct {
	mu        sync.RWMutex
	lang      string
	dir       string
	feeds     []string
	interests []string
	interval  time.Duration
	sessions  plugin.SessionQuery
	client    *http.Client
	started   time.Time

	dataMu sync.RWMutex
	st     State

	cancel context.CancelFunc
	wg     sync.WaitGroup
	unsub  func()

	actMu    sync.Mutex
	actSeq   uint64
	actState plugin.ActionState

	now func() time.Time
}

func New() *Plugin { return &Plugin{now: time.Now, interval: defRefreshHours * time.Hour} }

func (p *Plugin) Name() string { return "phone" }

func (p *Plugin) Category() string { return plugin.CategoryPersona }

func (p *Plugin) Description() string {
	return "小手机：角色闲下来时刷一刷你配置的订阅源，刷到的几条记在心上，碰上感兴趣的会主动跟你提"
}

// Requires 硬依赖 roleplay：刷手机的是角色。
func (p *Plugin) Requires() []string { return []string{"roleplay"} }

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key: "feeds", Label: "订阅源", Type: plugin.FieldText, Default: "",
			Description: "一行一个 RSS 或 Atom 地址，以 # 开头的行忽略。最多 20 个",
		},
		{
			Key: "interests", Label: "兴趣关键词", Type: plugin.FieldString, Default: "",
			Description: "用逗号或空格隔开。标题碰上的新闻会成为她主动开口的理由；不填就不为新闻开口，刷到的照样记在心上",
		},
		{
			Key: "refresh_hours", Label: "至少隔几小时刷一次", Type: plugin.FieldInt, Default: defRefreshHours,
			Min: plugin.IntPtr(1), Max: plugin.IntPtr(24),
			Description: "到点时她在睡觉、在忙或正和你聊天就往后挪，闲下来再刷",
		},
	}
}

// Init 应用配置并（重）启动刷新循环。可重入：先停旧的。不发网络请求。
func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	if ictx.StateDir == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.phone.no_state_dir",
			"没有可用的持久化目录，无法保存刷到的消息"))
	}
	feeds, err := parseFeeds(ictx.Lang, plugin.CfgString(cfg, "feeds", ""))
	if err != nil {
		return err
	}
	p.Stop()

	st, err := loadState(ictx.StateDir)
	if err != nil {
		st = State{} // 读不出来就当第一次用：最坏是把刷过的再当新的刷一遍
	}
	p.dataMu.Lock()
	p.st = st
	p.dataMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.lang = ictx.Lang
	p.dir = ictx.StateDir
	p.feeds = feeds
	p.interests = parseInterests(plugin.CfgString(cfg, "interests", ""))
	p.interval = time.Duration(plugin.CfgInt(cfg, "refresh_hours", defRefreshHours)) * time.Hour
	p.sessions = ictx.Sessions
	p.client = &http.Client{Timeout: fetchTimeout}
	p.started = p.now()
	if len(feeds) == 0 {
		return nil // 一个源都没有就不起循环
	}

	wake := make(chan struct{}, 1)
	// 回调可能在写入方持锁时被调用（心跳在持锁时宣布睡着），只投一下，不取任何锁
	p.unsub = availability.Subscribe(func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	})
	var ctx context.Context
	ctx, p.cancel = context.WithCancel(context.Background())
	p.wg.Add(1)
	go p.loop(ctx, wake)
	return nil
}

// Stop 停止刷新循环并退订公告板。
func (p *Plugin) Stop() {
	p.mu.Lock()
	cancel, unsub := p.cancel, p.unsub
	p.cancel, p.unsub = nil, nil
	p.mu.Unlock()
	if unsub != nil {
		unsub()
	}
	if cancel != nil {
		cancel()
		p.wg.Wait()
	}
}

func (p *Plugin) Tools() []plugin.Tool { return []plugin.Tool{&readTool{p: p}} }

func (p *Plugin) SystemPrompt() string { return guideFor(p.langOf()) }

func (p *Plugin) langOf() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lang
}

type settings struct {
	lang      string
	dir       string
	feeds     []string
	interests []string
	interval  time.Duration
	sessions  plugin.SessionQuery
	client    *http.Client
	started   time.Time
}

func (p *Plugin) snapshot() settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return settings{p.lang, p.dir, p.feeds, p.interests, p.interval, p.sessions, p.client, p.started}
}

// parseFeeds 一行一个地址，# 开头的是注释。地址不对就报错拒绝保存，而不是悄悄跳过——
// 跳过的话那个源永远没有动静，没人知道为什么。
func parseFeeds(lang, raw string) ([]string, error) {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf(i18n.T(lang, "plugin.phone.err.bad_feed",
				"订阅源「%s」不是 http 或 https 地址"), line)
		}
		out = append(out, line)
	}
	if len(out) > maxFeeds {
		return nil, fmt.Errorf(i18n.T(lang, "plugin.phone.err.too_many_feeds",
			"订阅源最多 %d 个，现在填了 %d 个"), maxFeeds, len(out))
	}
	return out, nil
}

func parseInterests(raw string) []string {
	f := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '，' || r == '、' || r == ';' || r == '；' || r == ' ' || r == '\n' || r == '\t'
	})
	var out []string
	for _, w := range f {
		if w = strings.ToLower(strings.TrimSpace(w)); w != "" {
			out = append(out, w)
		}
	}
	return out
}

func matches(title string, interests []string) bool {
	low := strings.ToLower(title)
	for _, w := range interests {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

// ---------- 刷新时机 ----------

func (p *Plugin) loop(ctx context.Context, wake <-chan struct{}) {
	defer p.wg.Done()
	for ctx.Err() == nil { // 刷到一半被取消时 refresh 不记时刻，不先看 ctx 的话 plan 会一直说「该刷了」
		wait, run := p.plan(p.now())
		if run {
			p.refresh(ctx)
			continue
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-wake:
			t.Stop() // 忙碌状态变了：重新判断，可能已经闲下来了
		}
	}
}

// plan 判断现在该不该刷；不该刷时给出多久之后再看。
//
// 先看时间到没到（距上次至少隔 interval），到了再看她闲不闲。不闲时挪到闲下来的时刻，
// 而不是再等一个 interval：三点到点时在开会、四点开完，四点零五就该刷，不是六点。
func (p *Plugin) plan(now time.Time) (time.Duration, bool) {
	s := p.snapshot()
	p.dataMu.RLock()
	last := p.st.LastRefresh
	p.dataMu.RUnlock()

	due := last.Add(s.interval)
	if earliest := s.started.Add(startGrace); due.Before(earliest) {
		due = earliest
	}
	if now.Before(due) {
		return due.Sub(now), false
	}
	if until, busy := busyUntil(now, s.sessions); busy {
		return max(until.Sub(now), time.Minute), false
	}
	return 0, true
}

// busyUntil 判断她此刻有没有空刷手机：睡着、重忙或不回、正和对方聊天都不刷。
// 轻忙不拦——人在轻忙时也会瞄两眼手机。返回最早能刷的时刻。
func busyUntil(now time.Time, sessions plugin.SessionQuery) (time.Time, bool) {
	var until time.Time
	busy := false
	if st, ok := availability.Current(now); ok && (st.Sleeping || st.Level >= availability.Heavy) {
		until, busy = now.Add(recheck), true
		if st.Until.After(now) {
			until = st.Until.Add(afterBusy)
		}
	}
	if sessions != nil {
		if last, err := sessions.LastInteraction(); err == nil && !last.IsZero() && now.Sub(last) < chatQuiet {
			if u := last.Add(chatQuiet); u.After(until) {
				until = u
			}
			busy = true
		}
	}
	return until, busy
}

// ---------- 刷新 ----------

type fetched struct {
	url     string
	name    string
	entries []entry
	err     error
}

// refresh 刷一遍全部订阅源。联网在锁外，取回来再一起并进状态。
// 不论成败都记下这次的时刻：源坏了不该让循环每分钟去敲一次门。
func (p *Plugin) refresh(ctx context.Context) {
	s := p.snapshot()
	var results []fetched
	for _, u := range s.feeds {
		if ctx.Err() != nil {
			return
		}
		name, entries, err := fetchFeed(ctx, s.client, u)
		results = append(results, fetched{u, name, entries, err})
	}
	if ctx.Err() != nil {
		return
	}
	now := p.now()

	p.dataMu.Lock()
	var added []Item
	p.st.Errors = nil
	for _, r := range results {
		if r.err != nil {
			if p.st.Errors == nil {
				p.st.Errors = map[string]string{}
			}
			p.st.Errors[r.url] = r.err.Error()
			continue
		}
		name := r.name
		if name == "" {
			name = hostOf(r.url)
		}
		added = append(added, merge(&p.st, name, r.entries, s.interests, now)...)
	}
	p.st.LastRefresh = now
	prune(&p.st, now)
	snap := p.st
	p.dataMu.Unlock()

	_ = saveState(s.dir, snap)
	postCue(s.lang, added, now)
}

// postCue 刷到碰上兴趣的新闻时投一条开口理由，一次刷新至多一条：一下子冒出好几条，
// 心跳也只该开一次口，挑最新的那条说，其余的在注入里看得到。
func postCue(lang string, added []Item, now time.Time) {
	var hits []Item
	for _, it := range added {
		if it.Hit {
			hits = append(hits, it)
		}
	}
	if len(hits) == 0 {
		return
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Published.After(hits[j].Published) })
	h := hits[0]
	text := prompt.Tf(lang, "prompt.phone.cue.news", "你刚刷手机看到一条：「%s」（%s）。", h.Title, h.Source)
	// 同一个键：新刷到的覆盖还没说出口的旧的，「刚看到」只有最新那条算数
	cue.Post(cue.Cue{Source: "phone", Key: "news", Text: text, Expire: now.Add(cueTTL)})
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// ---------- 注入 ----------

// TurnPrompt 注入一天内刷到的几条，碰上兴趣的排前面。手机是同一部，不分人格。
//
//	[手机上看到的]
//	· 「某某发布新款相机」（数码频道，3 小时前）
//	· 「周末气温骤降」（本地新闻，8 小时前）
func (p *Plugin) TurnPrompt(_ context.Context, _ plugin.TurnEvent) (string, error) {
	lang := p.langOf()
	now := p.now()
	p.dataMu.RLock()
	items := append([]Item(nil), p.st.Items...)
	p.dataMu.RUnlock()

	var recent []Item
	for _, it := range items {
		if now.Sub(it.Seen) <= injectFresh {
			recent = append(recent, it)
		}
	}
	if len(recent) == 0 {
		return "", nil
	}
	sort.SliceStable(recent, func(i, j int) bool {
		if recent[i].Hit != recent[j].Hit {
			return recent[i].Hit
		}
		return recent[i].Seen.After(recent[j].Seen)
	})
	if len(recent) > maxInjected {
		recent = recent[:maxInjected]
	}
	lines := []string{stateHeaderFor(lang)}
	for _, it := range recent {
		lines = append(lines, prompt.Tf(lang, "prompt.phone.render.item", "· 「%s」（%s，%s）",
			it.Title, it.Source, agoText(lang, it.when(), now)))
	}
	return strings.Join(lines, "\n"), nil
}

// when 是这条消息「多久前」的依据：有发布时间用发布时间——一夜攒下的消息早上刷到，
// 说的仍是「8 小时前」，夜里的就是夜里的——没有才用刷到的时刻。
func (it Item) when() time.Time {
	if !it.Published.IsZero() {
		return it.Published
	}
	return it.Seen
}

func agoText(lang string, t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Hour:
		return prompt.T(lang, "prompt.phone.ago.just_now", "刚刚")
	case d < 24*time.Hour:
		return prompt.Tf(lang, "prompt.phone.ago.hours", "%d 小时前", int(d.Hours()))
	default:
		return prompt.Tf(lang, "prompt.phone.ago.days", "%d 天前", int(d.Hours()/24))
	}
}
