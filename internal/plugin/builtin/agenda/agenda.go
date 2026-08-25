package agenda

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"wen/internal/availability"
	"wen/internal/plugin"
	"wen/internal/plugin/builtin/people"
)

// 配置默认值。
const (
	defDayStartHour     = 5
	defMaxItems         = 4
	defStartGraceMin    = 15
	defRemindBeforeMin  = 60
	defMaxCommitInject  = 8
	defMaxInjectBytes   = 2 * 1024
	defMaxPlanAttempts  = 2  // 一天最多尝试规划几次，都没排成就到明天再说
	defBusyRetryMax     = 10 // 会话忙时每分钟重试的上限
	defaultPlanSettle   = 10 * time.Second
	defaultBusyRetry    = time.Minute
	defaultStartupGrace = 2 * time.Minute
	runTimeout          = 10 * time.Minute
	pollFallback        = 30 * time.Second
)

// Plugin 是 agenda 系统插件。有状态：Init 会被反复调用（SetConfig），配置字段由 mu
// 保护，重入时先停掉上一轮时钟再起新的。
type Plugin struct {
	mu sync.RWMutex

	lookup people.Lookup // 人物库的只读查询，经构造函数注入，不走任何核心机制

	// 配置（Init 写入）
	base            string
	autoPlan        bool
	runActivities   bool
	dayStartHour    int
	maxItems        int
	startGrace      time.Duration
	remindBefore    time.Duration
	maxCommitInject int
	maxInjectBytes  int

	// 能力（Init 写入）
	runTurn  plugin.RunTurnFunc
	notice   plugin.NoticeFunc
	sessions plugin.SessionQuery

	// 运行状态
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	wake      chan struct{}
	startedAt time.Time
	planning  map[string]bool // 域 → 规划轮次进行中
	// planSubmitted 域 → 本次规划轮次里已经提交过表。
	//
	// 它挡的是一种真实的绕圈：规划提示词是一次性输入，却在工具循环的每一次迭代里
	// 都被重新读到，于是「去排一张表」这条祈使句永远不会变成「已经做完」；模型每收到
	// 一次成功回执就微调一版再提交，实测连提十三次。有了这个标记，第二次提交当场被
	// 挡回去并告诉它已经排好了。只在规划轮次内有效——对话里说「重排一下今天」是正当的。
	planSubmitted map[string]bool

	// 可注入的时钟与节奏，测试用；生产取默认值。
	now          func() time.Time
	planSettle   time.Duration
	busyRetry    time.Duration
	startupGrace time.Duration

	// 按可见域分出的库，惰性创建。单独一把锁：这张表的生命周期与配置无关。
	storesMu sync.Mutex
	stores   map[string]*Store
}

// New 构造插件。lookup 来自 people 插件：排「和谁」时据此校验名字；为 nil 时
// 不校验（只在测试里这么用，生产由 Requires 保证 people 同时启用）。
func New(lookup people.Lookup) *Plugin {
	return &Plugin{
		lookup:          lookup,
		dayStartHour:    defDayStartHour,
		maxItems:        defMaxItems,
		startGrace:      defStartGraceMin * time.Minute,
		remindBefore:    defRemindBeforeMin * time.Minute,
		maxCommitInject: defMaxCommitInject,
		maxInjectBytes:  defMaxInjectBytes,
		now:             time.Now,
		planSettle:      defaultPlanSettle,
		busyRetry:       defaultBusyRetry,
		startupGrace:    defaultStartupGrace,
		planning:        map[string]bool{},
		planSubmitted:   map[string]bool{},
	}
}

func (p *Plugin) Name() string { return "agenda" }

func (p *Plugin) Category() string { return plugin.CategoryPersona }

func (p *Plugin) Description() string {
	return "日程感知：角色每天给自己排一张两到四件事的表，到点去做、做完带回一句经历，未来的约定单独登记并到日排入"
}

// Requires 硬依赖 roleplay 与 people：没有角色就没有谁的一天，「和谁」的名字只认人物库。
func (p *Plugin) Requires() []string { return []string{"roleplay", "people"} }

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key: "auto_plan", Label: "每天自动规划", Type: plugin.FieldBool, Default: true,
			Description: "新的一天第一轮对话（或心跳）结束后，让角色给自己排当天的表。关掉则只维护约定与手工改的表。",
		},
		{
			Key: "run_activities", Label: "到点自动跑开始 / 结束轮次", Type: plugin.FieldBool, Default: true,
			Description: "到了表上的时间，在最近活跃的会话上跑一轮让角色决定去不去、回来写一句经历。关掉则只把表注入上下文。",
		},
		{
			Key: "day_start_hour", Label: "几点算新的一天", Type: plugin.FieldInt,
			Default: defDayStartHour, Min: plugin.IntPtr(0), Max: plugin.IntPtr(12),
			Description: "在此之前的时段归前一天：凌晨一点还在聊天不算新的一天。",
		},
		{
			Key: "max_items", Label: "一天最多几项", Type: plugin.FieldInt,
			Default: defMaxItems, Min: plugin.IntPtr(1), Max: plugin.IntPtr(hardMaxItems),
			Description: "排表时的项数上限。提示词要求两到四件事、留大段空白，这里是硬上限。",
		},
		{
			Key: "start_grace_minutes", Label: "到点宽限（分钟）", Type: plugin.FieldInt,
			Default: defStartGraceMin, Min: plugin.IntPtr(1), Max: plugin.IntPtr(120),
			Description: "到点后多久内派发的开始轮次算准时；超过则在轮次里注明已经晚了多久。",
		},
		{
			Key: "remind_before_minutes", Label: "约定提前提醒（分钟）", Type: plugin.FieldInt,
			Default: defRemindBeforeMin, Min: plugin.IntPtr(0), Max: plugin.IntPtr(240),
			Description: "与对方的约定开始前多久投一条开口理由给心跳，0 表示不投。",
		},
		{
			Key: "max_commitments_inject", Label: "每轮注入的约定条数", Type: plugin.FieldInt,
			Default: defMaxCommitInject, Min: plugin.IntPtr(1), Max: plugin.IntPtr(30),
			Description: "未来约定按日期最近优先注入这么多条，其余只注明条数，可用 list_commitments 查看。",
		},
		{
			Key: "max_inject_bytes", Label: "注入字节上限", Type: plugin.FieldInt,
			Default: defMaxInjectBytes, Min: plugin.IntPtr(512), Max: plugin.IntPtr(16 * 1024),
			Description: "今日安排与未来约定两块合计的注入上限。超出时先去掉已做项的经历，再把已做项压成条数，最后约定只剩条数；进行中与下一项永远保留。",
		},
	}
}

// Init 应用配置，重建忙碌状态并（重）启动时钟。可重入：先停掉上一轮。
func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	if ictx.StateDir == "" {
		return fmt.Errorf("没有可用的持久化目录，无法保存日程")
	}
	if ictx.Sessions == nil {
		return fmt.Errorf("当前环境不支持会话查询")
	}

	p.Stop() // 重入：先停上一轮时钟，避免两套并行

	p.storesMu.Lock()
	p.stores = map[string]*Store{}
	p.storesMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.base = filepath.Join(ictx.StateDir, "agenda")
	p.autoPlan = plugin.CfgBool(cfg, "auto_plan", true)
	p.runActivities = plugin.CfgBool(cfg, "run_activities", true)
	p.dayStartHour = plugin.CfgInt(cfg, "day_start_hour", defDayStartHour)
	p.maxItems = plugin.CfgInt(cfg, "max_items", defMaxItems)
	if p.maxItems > hardMaxItems {
		p.maxItems = hardMaxItems
	}
	p.startGrace = time.Duration(plugin.CfgInt(cfg, "start_grace_minutes", defStartGraceMin)) * time.Minute
	p.remindBefore = time.Duration(plugin.CfgInt(cfg, "remind_before_minutes", defRemindBeforeMin)) * time.Minute
	p.maxCommitInject = plugin.CfgInt(cfg, "max_commitments_inject", defMaxCommitInject)
	p.maxInjectBytes = plugin.CfgInt(cfg, "max_inject_bytes", defMaxInjectBytes)
	p.runTurn = ictx.RunTurn
	p.notice = ictx.Notice
	p.sessions = ictx.Sessions
	p.planning = map[string]bool{}
	p.planSubmitted = map[string]bool{}
	p.startedAt = p.now()

	// 忙碌状态不落盘，权威在表里：重启后从「进行中且还没到点」的项重建
	p.rebuildAvailabilityLocked()

	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.wake = make(chan struct{}, 1)
	p.wg.Add(1)
	go p.loop(p.ctx, p.wake)
	return nil
}

// Stop 停掉时钟与所有后台轮次。只做取消与有界等待：轮次的阻塞点都挂在 ctx 上。
func (p *Plugin) Stop() {
	p.mu.Lock()
	cancel := p.cancel
	p.cancel = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		p.wg.Wait()
	}
}

// wakeup 通知时钟表变了（排表、改项、加约定），重算下一次到期时刻。
func (p *Plugin) wakeup() {
	p.mu.RLock()
	wake := p.wake
	p.mu.RUnlock()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (p *Plugin) Tools() []plugin.Tool {
	return []plugin.Tool{
		&setPlanTool{p: p},
		&updatePlanTool{p: p},
		&addCommitmentTool{p: p},
		&cancelCommitmentTool{p: p},
		&listCommitmentsTool{p: p},
		&addPromiseTool{p: p},
		&settlePromiseTool{p: p},
		&listPromisesTool{p: p},
	}
}

// SystemPrompt 注入维护判据，全部为静态内容；表与约定本体走 TurnPrompt。
func (p *Plugin) SystemPrompt() string { return guidePrompt }

// settings 是一次调用期间使用的配置快照：SetConfig 会在运行时重新 Init，而工具可能正在执行。
type settings struct {
	base            string
	autoPlan        bool
	runActivities   bool
	dayStartHour    int
	maxItems        int
	startGrace      time.Duration
	remindBefore    time.Duration
	maxCommitInject int
	maxInjectBytes  int
	runTurn         plugin.RunTurnFunc
	notice          plugin.NoticeFunc
	sessions        plugin.SessionQuery
}

func (p *Plugin) snapshot() settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return settings{
		base:            p.base,
		autoPlan:        p.autoPlan,
		runActivities:   p.runActivities,
		dayStartHour:    p.dayStartHour,
		maxItems:        p.maxItems,
		startGrace:      p.startGrace,
		remindBefore:    p.remindBefore,
		maxCommitInject: p.maxCommitInject,
		maxInjectBytes:  p.maxInjectBytes,
		runTurn:         p.runTurn,
		notice:          p.notice,
		sessions:        p.sessions,
	}
}

// today 返回此刻的逻辑日期。
func (s settings) today(now time.Time) time.Time { return logicalDay(now, s.dayStartHour) }

// ---------- 按可见域分库 ----------

// 表与约定**只读写本轮写入域**，不跨域合并——两条故事线各有各的一天（与 presence
// 同一条规则）。后台轮次的域由会话当前人格裁决，因此开始 / 结束轮次里的工具按 id
// 在可读域里找这一项、写回它所在的域；找不到就报「没有这一项」，派发记录不回滚。

// storeFor 返回某个可见域的库（惰性创建）。未初始化时返回 nil。
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
	s := NewStore(plugin.DomainDir(base, tag))
	p.stores[tag] = s
	return s
}

// writeStore 返回本轮该写入的库。
func (p *Plugin) writeStore(ctx context.Context) *Store {
	return p.storeFor(plugin.ScopeFrom(ctx).Write)
}

// readDomains 返回本轮可读的域标签，写入域在前。
func (p *Plugin) readDomains(ctx context.Context) []string {
	p.mu.RLock()
	base := p.base
	p.mu.RUnlock()
	return plugin.ReadDomains(base, plugin.ScopeFrom(ctx))
}

// allDomains 返回目前存在的全部域（共享域在前），时钟按它扫表。
func (p *Plugin) allDomains() []string {
	p.mu.RLock()
	base := p.base
	p.mu.RUnlock()
	return plugin.ReadDomains(base, plugin.Scope{})
}

// located 是在可读域里找到的一项及其所在的域。
type located struct {
	tag  string
	plan Plan
	item *Item
}

// findItem 按 id 在可读域里找一项（写入域优先）。
func (p *Plugin) findItem(ctx context.Context, id string) (located, bool) {
	for _, tag := range p.readDomains(ctx) {
		st := p.storeFor(tag)
		if st == nil {
			continue
		}
		pl, err := st.LoadPlan()
		if err != nil {
			continue
		}
		if it := pl.item(id); it != nil {
			return located{tag: tag, plan: pl, item: it}, true
		}
	}
	return located{}, false
}

// ---------- 忙碌状态 ----------

const availabilitySource = "agenda"

// setAvailability 把一项进行中的活动写上公告板。与对方一起的项不写：正和对方在一起，
// 不存在「不回对方消息」这回事。
func setAvailability(it *Item, day time.Time, dayStartHour int, now time.Time) {
	if it.WithUser || it.Status != statusOngoing {
		return
	}
	lvl, ok := availability.ParseLevel(it.Busy)
	if !ok {
		lvl = availability.Light
	}
	start, end := it.span(day, dayStartHour)
	if !end.After(now) {
		return
	}
	availability.Set(availability.State{
		Source: availabilitySource, Activity: it.Title, Level: lvl, Since: start, Until: end,
	})
}

func clearAvailability() { availability.Clear(availabilitySource) }

// rebuildAvailabilityLocked 在启动时从各域的表重建忙碌状态：进行中、还没到点的项。
// 多个域都有时取档位最高的（公告板每个来源只有一条）。调用方需持有 p.mu。
func (p *Plugin) rebuildAvailabilityLocked() {
	clearAvailability()
	now := p.now()
	day := logicalDay(now, p.dayStartHour)
	var best *Item
	var bestLvl availability.Level
	for _, tag := range plugin.ReadDomains(p.base, plugin.Scope{}) {
		pl, err := NewStore(plugin.DomainDir(p.base, tag)).LoadPlan()
		if err != nil || pl.Date != day.Format(dateLayout) {
			continue
		}
		for i := range pl.Items {
			it := &pl.Items[i]
			if it.Status != statusOngoing || it.WithUser {
				continue
			}
			if _, end := it.span(day, p.dayStartHour); !end.After(now) {
				continue
			}
			lvl, _ := availability.ParseLevel(it.Busy)
			if best == nil || lvl > bestLvl {
				cp := *it
				best, bestLvl = &cp, lvl
			}
		}
	}
	if best != nil {
		setAvailability(best, day, p.dayStartHour, now)
	}
}

// ---------- 状态行 ----------

// StatusLines 报一行今天的进度。共享域的表带活动名；只有按人格分开的表时只报条数——
// 状态输出会被转发到各条消息通道，哪一面的活动名都不该出现在另一面的窗口里。
func (p *Plugin) StatusLines(lang string) []string {
	s := p.snapshot()
	if s.base == "" {
		return nil
	}
	now := p.now()
	day := s.today(now)
	today := day.Format(dateLayout)

	if pl, err := p.storeFor("").LoadPlan(); err == nil && pl.Date == today {
		return []string{"📅 日程：" + statusText(pl, day, now, s.dayStartHour)}
	}
	total, done := 0, 0
	for _, tag := range p.allDomains() {
		if tag == "" {
			continue
		}
		pl, err := p.storeFor(tag).LoadPlan()
		if err != nil || pl.Date != today {
			continue
		}
		for _, it := range pl.Items {
			if it.Status == statusCancelled {
				continue
			}
			total++
			if it.Status == statusDone {
				done++
			}
		}
	}
	if total > 0 {
		return []string{fmt.Sprintf("📅 日程：今天 %d 项，已完成 %d（按人格分开的表）", total, done)}
	}
	return []string{"📅 日程：今天尚未排表"}
}

// statusText 写一张表的进度：`此刻 和林舟在图书馆查资料（到 16:30）；今天 3 项，已完成 1`。
func statusText(pl Plan, day, now time.Time, dayStartHour int) string {
	total, done := 0, 0
	var cur, next *Item
	for i := range pl.Items {
		it := &pl.Items[i]
		if it.Status == statusCancelled {
			continue
		}
		total++
		if it.Status == statusDone {
			done++
		}
		start, end := it.span(day, dayStartHour)
		switch {
		case it.Status == statusOngoing && end.After(now):
			cur = it
		case it.Status == statusPlanned && start.After(now) && next == nil:
			next = it
		}
	}
	if total == 0 {
		return "今天没有安排"
	}
	head := "空闲"
	switch {
	case cur != nil:
		head = fmt.Sprintf("此刻 %s（到 %s）", cur.Title, cur.End)
	case next != nil:
		head = fmt.Sprintf("空闲，下一项 %s", next.Start)
	}
	return fmt.Sprintf("%s；今天 %d 项，已完成 %d", head, total, done)
}
