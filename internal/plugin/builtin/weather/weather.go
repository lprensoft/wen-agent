// Package weather 提供天气感知的系统插件：按设定的城市定时取现实天气，把此刻的
// 天气每轮注入上下文，作为角色所处环境的一部分。
//
// 它注入的是环境状态而不是一个可调用的工具——这是它依赖 roleplay 的原因。通用对话
// 里每轮塞一行天气只是噪声，真要问天气，模型自己抓网页就够了。
//
// **天气首先得是角色自己的**。只给「对方所在地的天气」时，那是一条关于第三方的信息：
// 角色对它既没有体感也没有可做的反应，于是它在对话里几乎不起作用。所以这里分成角色
// 与用户两处地点，同城时合成一处——角色自己那边的天气才会渗进环境描写与状态。
//
// 天气在后台按固定间隔刷新，注入路径只读内存里的缓存：TurnPrompt 在每轮对话的同步
// 路径上，在那里发网络请求就是让每一轮都替一次超时买单。
//
// 除了每轮注入现况，天气出现值得开口的转变（下起来/停了）时还会向 internal/cue
// 投递一条开口理由，由心跳带进主动开口的轮次（见 cue.go）。
package weather

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

const (
	defaultRefreshMinutes = 30
	// defaultTomorrowFrom 是明天的预报从几点起出现在角色眼前。白天的人想的是今天，
	// 临睡前、计划明天的事时才去看明天；0 表示全天都看。
	defaultTomorrowFrom = 18
	defaultStaleMinutes = 60
	defaultSameCity     = true
	// minRefreshMinutes 是刷新间隔的下限。数据源是免费的公共接口，取得再勤也没有
	// 更新的数据，只是白白打对方。
	minRefreshMinutes = 10

	// requestTimeout 是单次取数（含地址解析）的总时限。
	requestTimeout = 20 * time.Second
)

// observation 是一处地点的观测缓存。
type observation struct {
	place   Place
	placeOK bool
	cur     Report
	curOK   bool
	lastErr string
	// days 是按日留档的天气概要（日期 → 概要），供别处回头问某一天（见 dayfacts.go）。
	days map[string]DayInfo
}

// Plugin 是 weather 系统插件。
type Plugin struct {
	mu         sync.RWMutex
	lang       string // 进程语言，注入片段与天气说法据此取用
	personaLoc string // 角色所在的城市
	userLoc    string // 用户所在的城市；同城时归一化为空
	sameCity   bool
	refresh    time.Duration
	stale      time.Duration
	client     *http.Client
	stateDir   string // 观测缓存的落盘位置
	// tomorrowFrom 是明天的预报从几点起注入（0 = 全天）。
	tomorrowFrom int
	// now 可在测试里替换：注入里「几点了」决定明天的预报出不出现。
	now func() time.Time

	// 后台刷新循环。Init 可重入，每次先停旧的再起新的。
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// 观测缓存，按城市名原文分条。单独一把锁：它由后台循环写、由注入路径读，
	// 与配置的生命周期无关。两处地点填了同一个城市时天然共用一条，不会重复取。
	dataMu sync.RWMutex
	obs    map[string]*observation

	// 设置页操作的状态。StartAction 与 ActionState 会被 HTTP 并发调用，自带一把锁。
	actMu    sync.Mutex
	actSeq   uint64
	actState plugin.ActionState
}

func New() *Plugin {
	return &Plugin{
		sameCity:     defaultSameCity,
		refresh:      defaultRefreshMinutes * time.Minute,
		stale:        defaultStaleMinutes * time.Minute,
		tomorrowFrom: defaultTomorrowFrom,
		now:          time.Now,
	}
}

func (p *Plugin) Name() string { return "weather" }

func (p *Plugin) Category() string { return plugin.CategoryPersona }

func (p *Plugin) Description() string {
	return "天气感知：按设定的城市定时获取现实天气，作为角色所处环境的一部分每轮注入"
}

// Requires 硬依赖 roleplay：没有角色，一行天气就只是上下文里的噪声。
func (p *Plugin) Requires() []string { return []string{"roleplay"} }

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key: "persona_location", Label: "角色所在的城市", Type: plugin.FieldString,
			Description: "角色身处的城市，如「杭州」「上海」「Hangzhou」。" +
				"角色这边的天气会成为它的环境与状态的一部分。留空则不注入。",
			Default: "",
		},
		{
			Key: "same_city", Label: "与我在同一个城市", Type: plugin.FieldBool,
			Description: "两人同城。开启时只按上面那个城市查一次，下面的「我所在的城市」不生效。",
			Default:     defaultSameCity,
		},
		{
			// 标签用「我」：填的人就是你自己。注入给模型的那一段写「对方所在的」——
			// 提示词是模型以第一人称读的，那里的「我」指它自己。与 roleplay 的
			// 「我的信息」是同一处人称约定。
			Key: "user_location", Label: "我所在的城市", Type: plugin.FieldString,
			Description: "两人不在一处时你所在的城市，角色会知道你那边的天气。" +
				"仅在上一项关闭时生效；留空则不注入。",
			Default: "",
		},
		{
			Key: "refresh_minutes", Label: "刷新间隔（分钟）", Type: plugin.FieldInt,
			Description: "后台每隔多久取一次天气。取得再勤也没有更新的数据，只是白打接口。",
			Default:     defaultRefreshMinutes,
			Min:         plugin.IntPtr(minRefreshMinutes),
			Max:         plugin.IntPtr(1440),
		},
		{
			Key: "tomorrow_from_hour", Label: "几点起看明天的天气", Type: plugin.FieldInt,
			Description: "从这个钟点起，明天的预报才出现在角色眼前——白天的人想的是今天，" +
				"临睡前、计划明天的事时才去看明天。在此之前只有你提到「明天」时它才会看一眼。" +
				"填 0 表示全天都看得到。",
			Default: defaultTomorrowFrom,
			Min:     plugin.IntPtr(0),
			Max:     plugin.IntPtr(23),
		},
		{
			Key: "stale_minutes", Label: "过期不注入的时限（分钟）", Type: plugin.FieldInt,
			Description: "距上次成功取得超过这个时间就不再注入天气——取不到新的时，" +
				"宁可让角色不知道天气，也不要让它把几个小时前的天气当作此刻。" +
				"不得小于刷新间隔。",
			Default: defaultStaleMinutes,
			Min:     plugin.IntPtr(minRefreshMinutes),
			Max:     plugin.IntPtr(10080),
		},
	}
}

// Init 应用配置并（重）启动后台刷新循环。可重入：先停掉上一轮循环。
//
// 这里不发网络请求：启动时断网不该让插件启用失败，取不到就是没有天气可注入而已。
func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	personaLoc, userLoc, sameCity := normalizeLocations(cfg)

	refreshMin := plugin.CfgInt(cfg, "refresh_minutes", defaultRefreshMinutes)
	staleMin := plugin.CfgInt(cfg, "stale_minutes", defaultStaleMinutes)
	tomorrowFrom := plugin.CfgInt(cfg, "tomorrow_from_hour", defaultTomorrowFrom)
	if ictx.StateDir == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.weather.no_state_dir",
			"没有可用的持久化目录，无法保存天气观测"))
	}
	if staleMin < refreshMin {
		return fmt.Errorf(i18n.T(ictx.Lang, "plugin.weather.err.stale_lt_refresh",
			"「过期不注入的时限」(%d 分钟) 不能小于「刷新间隔」(%d 分钟)："+
				"那样刚取到的天气也会立刻被判为过期，等于永远不注入"), staleMin, refreshMin)
	}

	p.Stop() // 重入：先停上一轮循环，避免两套定时器并行

	// 只丢掉不再需要的城市的缓存。改个刷新间隔就把手上这份好数据扔掉，
	// 万一此刻正好取不到，就白白空窗一段。
	// 这一段刻意不与下面的 p.mu 嵌套：两把锁全程各用各的，就不存在取锁顺序问题。
	wanted := wantedLocations(personaLoc, userLoc)
	p.dataMu.Lock()
	if p.obs == nil {
		p.obs = map[string]*observation{}
	}
	for loc := range p.obs {
		if !slices.Contains(wanted, loc) {
			delete(p.obs, loc)
		}
	}
	p.dataMu.Unlock()

	// 装回上次的观测：重启不该产生空窗，也不该白烧一次 API 调用
	p.loadInto(ictx.StateDir, wanted)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.stateDir = ictx.StateDir
	p.lang = ictx.Lang
	p.personaLoc, p.userLoc, p.sameCity = personaLoc, userLoc, sameCity
	p.refresh = time.Duration(refreshMin) * time.Minute
	p.stale = time.Duration(staleMin) * time.Minute
	p.tomorrowFrom = tomorrowFrom
	p.client = &http.Client{Timeout: requestTimeout}
	if len(wanted) == 0 {
		return nil // 一个城市都没填就不起循环，也不注入任何东西
	}

	// 循环起在锁内，与 cancel 的记录是同一步：分成两步的话，中间来一次 Stop
	// 会看到 cancel 还是 nil，于是这个刚起来的循环就没人停得掉了。
	var ctx context.Context
	ctx, p.cancel = context.WithCancel(context.Background())
	p.wg.Add(1)
	go p.loop(ctx, p.client, wanted, p.refresh, p.stateDir)
	return nil
}

// normalizeLocations 把配置里的两处地点规整成「角色所在 / 我所在 / 是否同城」。
func normalizeLocations(cfg map[string]any) (personaLoc, userLoc string, sameCity bool) {
	personaLoc = strings.TrimSpace(plugin.CfgString(cfg, "persona_location", ""))
	sameCity = plugin.CfgBool(cfg, "same_city", defaultSameCity)
	if !sameCity {
		userLoc = strings.TrimSpace(plugin.CfgString(cfg, "user_location", ""))
	}
	// 两处填了同一个地方就当同城处理：否则会注入两行一模一样的天气，
	// 读起来像两个地方碰巧天气相同，而事实是它们本来就是一个地方。
	if userLoc != "" && userLoc == personaLoc {
		sameCity, userLoc = true, ""
	}
	return personaLoc, userLoc, sameCity
}

// wantedLocations 返回需要取数的城市（去重、去空）。
func wantedLocations(locs ...string) []string {
	var out []string
	for _, loc := range locs {
		if loc != "" && !slices.Contains(out, loc) {
			out = append(out, loc)
		}
	}
	return out
}

// Stop 停止刷新循环。只做取消与有界等待，符合 Stoppable 契约。
func (p *Plugin) Stop() {
	p.mu.Lock()
	cancel := p.cancel
	p.cancel = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		p.wg.Wait() // 循环里的阻塞点都挂在 ctx 上，取消后即刻返回
	}
}

func (p *Plugin) Tools() []plugin.Tool { return nil }

// settings 是一次调用期间使用的配置快照。
type settings struct {
	lang         string
	personaLoc   string
	userLoc      string
	sameCity     bool
	refresh      time.Duration
	stale        time.Duration
	client       *http.Client
	stateDir     string
	tomorrowFrom int
}

// tomorrowVisible 判断此刻明天的预报该不该出现在角色眼前：到了傍晚的窗口，或对方
// 这一轮提到了明天（主动问，就看一眼）。
func (s settings) tomorrowVisible(now time.Time, input string) bool {
	return s.inTomorrowWindow(now) || mentionsTomorrow(s.lang, input)
}

// inTomorrowWindow 判断此刻是否在「看明天」的时段里。cue 的投递也按它。
func (s settings) inTomorrowWindow(now time.Time) bool {
	return s.tomorrowFrom <= 0 || now.Hour() >= s.tomorrowFrom
}

// mentionsTomorrow 词面判断一段话里有没有说到明天。
// tomorrowWordsZH 是「说到了明天」的判词，一行一个。整表一个键：它按语言给，
// 因为判的是对方**输入**里的说法。英文实例下的中文提问因此认不出来——一个双语用户
// 少一次「看一眼明天」，不值得为它把两种语言的词表并起来（那会让「明」这类短词
// 在另一种语言的正常句子里误命中）。
const tomorrowWordsZH = "明天\n明早\n明晚\n明日\n明儿"

func mentionsTomorrow(lang, s string) bool {
	for _, w := range strings.Split(prompt.T(lang, "prompt.weather.tomorrow_words", tomorrowWordsZH), "\n") {
		if w != "" && strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// configured 表示至少有一处地点可查。
func (s settings) configured() bool { return s.personaLoc != "" || s.userLoc != "" }

// snapshot 取一份配置快照：SetConfig 会在运行时重新 Init，而注入可能正在进行。
func (p *Plugin) snapshot() settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return settings{
		lang:       p.lang,
		personaLoc: p.personaLoc, userLoc: p.userLoc, sameCity: p.sameCity,
		refresh: p.refresh, stale: p.stale, client: p.client,
		stateDir: p.stateDir, tomorrowFrom: p.tomorrowFrom,
	}
}

// langOf 取进程语言。
func (p *Plugin) langOf() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lang
}

// SystemPrompt 注入天气的使用规则，是静态内容。一个城市都没填时整段不注入：
// 那时候永远不会有 [当前天气]，一段解释怎么用它的规则只是白占上下文。
func (p *Plugin) SystemPrompt() string {
	if !p.snapshot().configured() {
		return ""
	}
	return guideFor(p.langOf())
}

// TurnPrompt 注入此刻的天气。取不到或已过期时不注入：提示词里已经写了
// 「没有给出的一方就当作不知道」，让它不知道，好过让它把旧天气当成现在的。
//
// 一边取到、一边取不到时注入取到的那一边——半边可用不该让整块消失。
func (p *Plugin) TurnPrompt(_ context.Context, ev plugin.TurnEvent) (string, error) {
	s := p.snapshot()
	if !s.configured() {
		return "", nil
	}
	now := p.now()
	showTomorrow := s.tomorrowVisible(now, ev.UserInput)

	body := func(r Report) string {
		return renderConditions(s.lang, r) + renderDays(s.lang, r, now, showTomorrow)
	}
	var lines []string
	if s.sameCity {
		if r, ok := p.forTurn(s, s.personaLoc, now, showTomorrow); ok {
			lines = append(lines, prompt.Tf(s.lang, "prompt.weather.line.both",
				"%s（你与对方同在）：%s", r.Place, body(r)))
		}
	} else {
		if r, ok := p.forTurn(s, s.personaLoc, now, showTomorrow); ok {
			lines = append(lines, prompt.Tf(s.lang, "prompt.weather.line.persona",
				"你所在的%s：%s", r.Place, body(r)))
		}
		if r, ok := p.forTurn(s, s.userLoc, now, showTomorrow); ok {
			lines = append(lines, prompt.Tf(s.lang, "prompt.weather.line.user",
				"对方所在的%s：%s", r.Place, body(r)))
		}
	}
	if len(lines) == 0 {
		return "", nil
	}
	return stateHeaderFor(s.lang) + "\n" + strings.Join(lines, "\n"), nil
}

// ---------- 后台刷新 ----------

func (p *Plugin) loop(ctx context.Context, client *http.Client, locs []string, refresh time.Duration, dir string) {
	defer p.wg.Done()

	// 首次刷新的时机由上次观测推算：缓存还新鲜就等到该刷新的时刻，
	// 没有缓存（首次启用，或刚换了城市）则立刻取。
	// 只有第一次这样算，其后是普通的定时器——否则某处地点持续取不到时，
	// 「早就该刷新了」会让循环空转。
	first := time.NewTimer(p.untilRefresh(locs, refresh, time.Now()))
	select {
	case <-ctx.Done():
		first.Stop()
		return
	case <-first.C:
	}
	p.refreshAll(ctx, client, locs)
	p.save(dir)

	t := time.NewTicker(refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.refreshAll(ctx, client, locs)
			p.save(dir)
		}
	}
}

// refreshAll 逐个刷新。串行即可：最多两处，且失败的一处不该拖住另一处。
func (p *Plugin) refreshAll(ctx context.Context, client *http.Client, locs []string) {
	for _, loc := range locs {
		if ctx.Err() != nil {
			return
		}
		p.refreshOne(ctx, client, loc)
	}
}

// refreshOne 取一处地点的天气。失败时保留上一份观测，由过期时限决定它还能不能用。
func (p *Plugin) refreshOne(ctx context.Context, client *http.Client, loc string) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	rep, err := p.observe(ctx, client, loc)
	if err != nil {
		if ctx.Err() == nil {
			p.noteError(loc, err)
		}
		return
	}
	p.dataMu.Lock()
	o := p.entryLocked(loc)
	prev, prevOK := o.cur, o.curOK
	rep.Tomorrow = carryForecast(prev.Tomorrow, rep.Tomorrow, prevOK)
	o.cur, o.curOK, o.lastErr = rep, true, ""
	p.dataMu.Unlock()

	// 按日留档：这次请求已经把昨天与今天的概要带回来了，存下来供日记回头问
	p.recordDays(loc, rep)

	// 前后两次观测对比出值得开口的转变（下起来/停了）时，投递一条开口理由；
	// 明天的预报理由投出去后记在缓存上，刷新不重投
	if p.maybePostCue(loc, prev, prevOK, rep) {
		p.dataMu.Lock()
		if o.curOK && o.cur.Tomorrow.Date == rep.Tomorrow.Date {
			o.cur.Tomorrow.Cued = true
		}
		p.dataMu.Unlock()
	}
}

// observe 取一次观测，地名解析的结果按城市缓存——它几乎不变，不必每次重解析。
func (p *Plugin) observe(ctx context.Context, client *http.Client, loc string) (Report, error) {
	p.dataMu.RLock()
	var (
		place Place
		ok    bool
	)
	if o := p.obs[loc]; o != nil {
		place, ok = o.place, o.placeOK
	}
	p.dataMu.RUnlock()

	if !ok {
		resolved, err := geocode(ctx, p.langOf(), client, loc)
		if err != nil {
			return Report{}, err
		}
		p.dataMu.Lock()
		o := p.entryLocked(loc)
		o.place, o.placeOK = resolved, true
		p.dataMu.Unlock()
		place = resolved
	}
	return fetchCurrent(ctx, p.langOf(), client, place)
}

// entryLocked 取（或建）一处地点的缓存条目。调用方须持 dataMu 写锁。
func (p *Plugin) entryLocked(loc string) *observation {
	if p.obs == nil {
		p.obs = map[string]*observation{}
	}
	o := p.obs[loc]
	if o == nil {
		o = &observation{}
		p.obs[loc] = o
	}
	return o
}

// noteError 记下失败原因供状态行展示。同一处地点的同一个原因只写一次日志：
// 断网期间每半小时刷屏一条同样的错误没有意义。
func (p *Plugin) noteError(loc string, err error) {
	msg := err.Error()
	p.dataMu.Lock()
	o := p.entryLocked(loc)
	repeated := o.lastErr == msg
	o.lastErr = msg
	p.dataMu.Unlock()
	if !repeated {
		i18n.Logf(p.langOf(), "log.weather.error", "weather: %s: %s", loc, msg)
	}
}

// fresh 返回某处地点仍在有效期内的观测。
func (p *Plugin) fresh(loc string, stale time.Duration, now time.Time) (Report, bool) {
	if loc == "" {
		return Report{}, false
	}
	p.dataMu.RLock()
	defer p.dataMu.RUnlock()
	o := p.obs[loc]
	if o == nil || !o.curOK {
		return Report{}, false
	}
	if stale > 0 && now.Sub(o.cur.Fetched) > stale {
		return Report{}, false
	}
	return o.cur, true
}

// forTurn 取一处地点供本轮注入的观测；明天的预报这一轮要出现时，记下它第一次被角色
// 看到的时刻（只在首次写，随观测一起落盘），「早就知道了」的标记从这里起算。
func (p *Plugin) forTurn(s settings, loc string, now time.Time, showTomorrow bool) (Report, bool) {
	r, ok := p.fresh(loc, s.stale, now)
	if !ok {
		return r, false
	}
	if showTomorrow && r.Tomorrow.known() && r.Tomorrow.Seen.IsZero() && p.markTomorrowSeen(loc, r.Tomorrow.Date, now) {
		r.Tomorrow.Seen = now
		p.save(s.stateDir)
	}
	return r, true
}

// markTomorrowSeen 把缓存里明天预报的 Seen 置为 now（仅当仍是同一天的预报且尚未标记）。
func (p *Plugin) markTomorrowSeen(loc, date string, now time.Time) bool {
	p.dataMu.Lock()
	defer p.dataMu.Unlock()
	o := p.obs[loc]
	if o == nil || !o.curOK || o.cur.Tomorrow.Date != date || !o.cur.Tomorrow.Seen.IsZero() {
		return false
	}
	o.cur.Tomorrow.Seen = now
	return true
}

// lastReport 返回某处地点最近一次成功的观测（不判过期）与最近一次失败的原因。
func (p *Plugin) lastReport(loc string) (Report, bool, string) {
	p.dataMu.RLock()
	defer p.dataMu.RUnlock()
	o := p.obs[loc]
	if o == nil {
		return Report{}, false, ""
	}
	return o.cur, o.curOK, o.lastErr
}

// ---------- 注入渲染 ----------

// renderConditions 渲染天气本身（不含地名）。地名与「谁所在」由调用方拼，
// 因为同城与分处两地的措辞不一样，而措辞正是这条注入起不起作用的关键。
//
// 不注入「几分钟前采集」：当前时间由核心的本轮状态块每轮给出，再加一个逐轮变化的
// 采集间隔只是噪声；数据够不够新已经由过期时限判过了。
func renderConditions(lang string, r Report) string {
	var b strings.Builder
	b.WriteString(prompt.Tf(lang, "prompt.weather.now.temp", "%s，%.0f℃",
		conditions.For(lang, r.Condition), r.TempC))
	if diff := r.FeelsC - r.TempC; diff >= 1 || diff <= -1 {
		b.WriteString(prompt.Tf(lang, "prompt.weather.now.feels", "（体感 %.0f℃）", r.FeelsC))
	}
	if r.Humidity > 0 {
		b.WriteString(prompt.Tf(lang, "prompt.weather.now.humidity", "，湿度 %d%%", r.Humidity))
	}
	b.WriteString(prompt.Tf(lang, "prompt.weather.now.wind", "，风速 %.0f km/h。", r.WindKmh))
	return b.String()
}

// renderDays 渲染昨天、今天与明天的概要，接在现况之后。今天的概要全天都在——
// 白天真正影响行为的是「今天下午会不会下雨」；明天的只在 showTomorrow 时出现。
//
// 按日期核对而不是信字段名：观测是缓存的，跨过午夜后缓存里的「明天」其实是今天，
// 照字面注入就是错话。对不上的那一天直接不注入；旧缓存没有这几项时同样整段为空。
func renderDays(lang string, r Report, now time.Time, showTomorrow bool) string {
	const layout = "2006-01-02"
	var parts []string
	if r.Yesterday.known() && r.Yesterday.Date == now.AddDate(0, 0, -1).Format(layout) {
		parts = append(parts, prompt.Tf(lang, "prompt.weather.day.yesterday", "昨天%s，%.0f~%.0f℃",
			conditions.For(lang, r.Yesterday.Condition), r.Yesterday.MinC, r.Yesterday.MaxC))
	}
	if r.Today.known() && r.Today.Date == now.Format(layout) {
		parts = append(parts, prompt.Tf(lang, "prompt.weather.day.today", "今天预计%s，%.0f~%.0f℃",
			conditions.For(lang, r.Today.Condition), r.Today.MinC, r.Today.MaxC))
	}
	if showTomorrow && r.Tomorrow.known() && r.Tomorrow.Date == now.AddDate(0, 0, 1).Format(layout) {
		parts = append(parts, prompt.Tf(lang, "prompt.weather.day.tomorrow", "明天预计%s，%.0f~%.0f℃%s",
			conditions.For(lang, r.Tomorrow.Condition), r.Tomorrow.MinC, r.Tomorrow.MaxC,
			seenNote(lang, r.Tomorrow.Seen, now)))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, prompt.T(lang, "prompt.weather.day.sep", "；")) +
		prompt.T(lang, "prompt.weather.day.end", "。")
}

// seenNote 给明天的预报标上「早就知道了」。预报每轮都在眼前，不标的话模型会把几个
// 小时前就看过的「明天有雨」当成刚得知的消息，一遍遍提醒带伞——与 presence 给旧字段
// 标「N 前记下」是同一个手法：代码算时间，新不新鲜让模型自己判断。
// 三小时内不标：刚看到的预报本来就是新消息。
//
// 只有一档措辞：某一天的预报只在它前一天里算「明天」，第一次被看到也必然在那一天，
// 所以 Seen 与渲染永远落在同一个日历日，不存在「昨天就知道了」；过了午夜「明天」
// 换成新的一天，carryForecast 不再沿用旧标记。
func seenNote(lang string, seen, now time.Time) string {
	if seen.IsZero() || now.Sub(seen) < 3*time.Hour {
		return ""
	}
	return prompt.T(lang, "prompt.weather.day.seen", "（早些时候就知道了）")
}
