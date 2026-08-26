// Package heartbeat 按可动态调整的间隔定时唤醒模型，在最近活跃的会话上跑一轮心跳对话。
//
// 心跳轮次带上系统提示词与全部启用插件的提示词（RunTurn 的天然组装），输出直接落进
// 目标会话。心跳自身不算「活跃」：落点判定只看真人交互的轮次，否则心跳会不断自我续命。
// 动态心跳开启时，节奏由模型自己定：它在对话里用 set_heartbeat_interval 说下次隔多久
// 再开口——该等还是该催，模型在上下文里看得比任何外部判定都清楚。无人聊天时另有一个
// 内置的衰减定时器每 15 分钟把间隔放缓一档，直到最慢间隔：那时没有新的对话可判断，
// 机械退避就够了，为此再打一次模型是白花钱。
//
// 心跳同时是「开口理由」公告板（internal/cue）的消费方：别的插件投递了值得主动
// 说的事（如天气刚下起雨）时，下一拍提前到最快间隔，理由随心跳提示词一并交给模型
// ——主动开口从纯时间驱动变成有事可说的理由驱动。
package heartbeat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"wen/internal/cue"
	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

// 配置默认值。
const (
	defBaseMinutes = 30
	defMinMinutes  = 5
	defMaxMinutes  = 120
)

// defaultPrompt 是心跳提示词的默认值，用户可在设置页改成任何内容。
const defaultPrompt = `【心跳】这是一次定时唤醒，当前没有新的用户消息。请回顾本会话的进展：
- 若有未完成的任务、值得跟进的事项或需要主动提醒用户的内容，直接说给用户听；
- 若确实无事可说，用一句简短的话表明你还在即可，不要编造进展。
若你判断下次该早些或晚些再开口，用 set_heartbeat_interval 定下时间；
对方要去睡或长时间离开时，用 pause_heartbeat 暂停到合适的时候再醒。`

// defaultContexts 是情境提示词的默认值：内置一个「生活」情境作范例——角色扮演
// 场景下让角色在无人时过自己的日子，同时示范配置格式。用户可改可删；清空保存后
// 就没有情境（FieldText 的空串是合法取值，不会长回来）。
//
// 「过自己的日子」不是新机制：心跳轮次本来就带全部启用插件的工具，场景、心情这类
// 状态模型顺手就能更新，缺的只是一句引导。措辞对工具存在与否不作假设——有就用，
// 没有这句话自然落空。
const defaultContexts = `[生活]
【心跳】这是一次定时唤醒，没有新的用户消息。先过自己的日子，再决定要不要开口：
- 回想从上次到现在你自己在做什么、此刻在哪、心情与状态如何——你的生活在对话之外
  也在继续。有可以记录场景、心情、身体状态的工具就顺手把变化更新进去，变化的幅度
  要与实际过去的时间相称。有日程表时你此刻在做什么以[今日安排]为准，不另编行程；
  尚未排定时不要宣布今天的行程。
- 然后看有没有值得对对方说的：惦记的事、答应过的跟进、刚发生的变化。有就自然地
  说，像随手给人发消息那样，一两句就好，不必汇报你刚更新的状态。
- 没有就保持安静，输出空回复即可。不编造进展，不为了说话而说话。
下次该早些或晚些醒就用 set_heartbeat_interval 定；对方要睡或长时间离开时用
pause_heartbeat 暂停到合适的时候。`

// beatTimeout 是单次心跳轮次的时长上限。
const beatTimeout = 10 * time.Minute

// decayEvery 是空闲衰减定时器的周期：无人聊天时每次触发都把间隔放缓一档。
const decayEvery = 15 * time.Minute

// Plugin 是心跳插件。有状态：Init 会被反复调用（SetConfig），所有字段由 mu 保护，
// 重入时先停掉上一轮循环再起新的。
type Plugin struct {
	mu sync.Mutex

	// 配置（Init 写入）
	lang     string // 进程语言，注入片段与工具文本据此取用
	base     time.Duration
	minIv    time.Duration
	maxIv    time.Duration
	prompt   string
	dynamic  bool
	contexts map[string]string // 情境名 → 覆盖用的心跳提示词

	// 能力与路径（Init 写入）
	stateDir   string
	runTurn    plugin.RunTurnFunc
	newSession plugin.NewSessionFunc
	sessions   plugin.SessionQuery // 只读：挑选最近活跃的会话

	// 运行状态
	cur         time.Duration // 当前心跳间隔（持久化到 state.json）
	adjusted    bool          // cur 是动态判定调整出来的，而非基础间隔的副本
	lastActive  time.Time     // 最近一次真人交互轮次的时间
	lastBeat    time.Time
	curContext  string    // 当前情境（模型经工具切换，持久化）
	pausedUntil time.Time // 暂停到某时刻（模型经工具设定；真人一说话即清除）
	pausedAt    time.Time // 暂停设于何时（重启后判断暂停是否仍有效要用）

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	wake   chan struct{}
}

func New() *Plugin { return &Plugin{} }

func (p *Plugin) Name() string { return "heartbeat" }

func (p *Plugin) Category() string { return plugin.CategoryBackground }

func (p *Plugin) Description() string {
	return "定时唤醒模型在最近活跃的会话上执行心跳提示词，节奏可由模型自己按对话情况设定"
}

func (p *Plugin) SystemPrompt() string { return "" }

// TurnPrompt 把当前的主动开口节奏亮给模型。工具描述里写了「话题正热就调短」，
// 但普通对话轮次里模型此前根本看不到当前间隔——看不见现状，判据就永远不会触发，
// 实测它只在「去睡了」「等一小时」这类对话里有明确文本信号的时刻才调节奏。
// 固定节奏时不注入：调整会被工具拒绝，亮出来只会诱导模型去调。
func (p *Plugin) TurnPrompt(_ context.Context, _ plugin.TurnEvent) (string, error) {
	p.mu.Lock()
	lang, cur, dynamic, pausedUntil := p.lang, p.cur, p.dynamic, p.pausedUntil
	p.mu.Unlock()
	if !dynamic || cur <= 0 {
		return "", nil
	}
	if until := time.Until(pausedUntil); until > 0 {
		return prompt.Tf(lang, "prompt.heartbeat.state_paused",
			"[心跳]\n主动开口已暂停，约 %s后恢复常规节奏。", humanDur(lang, until)), nil
	}
	return prompt.Tf(lang, "prompt.heartbeat.state",
		"[心跳]\n无人说话时你约每 %s主动开口一次；与当下聊天的热度不相称时，用 set_heartbeat_interval 调整。",
		humanDur(lang, cur)), nil
}

// Tools 一律返回工具，不按「动态心跳」开关增减：Tools 在插件注册时就会被调用
// （那时还没 Init），按运行期状态增减会让工具名的冲突检查看到一张空表。
// 关掉动态心跳时由工具自己拒绝并说明理由。
func (p *Plugin) Tools() []plugin.Tool {
	return []plugin.Tool{&setIntervalTool{p: p}, &pauseTool{p: p}}
}

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key: "interval_minutes", Label: "基础心跳间隔（分钟）", Type: plugin.FieldInt,
			Default: defBaseMinutes, Min: plugin.IntPtr(1), Max: plugin.IntPtr(1440),
			Description: "启用或重新配置后的初始心跳间隔",
		},
		{
			Key: "prompt", Label: "心跳提示词", Type: plugin.FieldText, Default: defaultPrompt,
			Description: "每次心跳作为输入执行的内容，连同系统提示词与已启用插件的提示词一起发给模型。" +
				"末尾会自动附上「距上次对话」的时长，提示词里不必再写「很久没聊」这类模糊措辞",
		},
		{
			Key: "dynamic", Label: "动态心跳", Type: plugin.FieldBool, Default: true,
			Description: "允许角色自己定下次主动开口的时间、或暂停一段时间，并在无人聊天时逐步放缓到最慢间隔；关闭则固定按基础间隔，角色也改不动",
		},
		{
			Key: "context_prompts", Label: "情境提示词", Type: plugin.FieldText, Default: defaultContexts,
			Description: "按情境给心跳配不同的提示词。每段以「[情境名]」单独起一行，之后的行是该情境的内容，" +
				"可写多段（如 [睡前] [闲聊] [干活]），由角色自行切换，说「默认」切回上面的提示词。" +
				"默认内置一个「生活」情境作范例（角色在无人时过自己的日子），可改可删；清空则始终用上面的提示词。",
		},
		{
			Key: "min_minutes", Label: "最快间隔（分钟）", Type: plugin.FieldInt,
			Default: defMinMinutes, Min: plugin.IntPtr(1), Max: plugin.IntPtr(1440),
			Description: "角色把节奏调快时，最快不超过这个",
		},
		{
			Key: "max_minutes", Label: "最慢间隔（分钟）", Type: plugin.FieldInt,
			Default: defMaxMinutes, Min: plugin.IntPtr(1), Max: plugin.IntPtr(2880),
			Description: "角色调慢、以及无人聊天时自动放缓的上限",
		},
	}
}

// Init 应用配置并（重）启动心跳循环。可重入：先停掉上一轮循环。
func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	base := time.Duration(plugin.CfgInt(cfg, "interval_minutes", defBaseMinutes)) * time.Minute
	minIv := time.Duration(plugin.CfgInt(cfg, "min_minutes", defMinMinutes)) * time.Minute
	maxIv := time.Duration(plugin.CfgInt(cfg, "max_minutes", defMaxMinutes)) * time.Minute
	if minIv > base || base > maxIv {
		return fmt.Errorf(i18n.T(ictx.Lang, "plugin.heartbeat.err.range",
			"心跳间隔需满足 最快(%v) ≤ 基础(%v) ≤ 最慢(%v)"), minIv, base, maxIv)
	}
	if ictx.StateDir == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.heartbeat.no_state_dir",
			"没有可用的持久化目录，无法记录心跳状态"))
	}
	if ictx.RunTurn == nil {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.heartbeat.no_run_turn",
			"当前环境不支持插件发起对话轮次"))
	}
	if ictx.Sessions == nil {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.heartbeat.no_sessions",
			"当前环境不支持会话查询"))
	}

	p.Stop() // 重入：先停上一轮循环，避免两套定时器并行

	p.mu.Lock()
	defer p.mu.Unlock()
	p.lang = ictx.Lang
	p.base, p.minIv, p.maxIv = base, minIv, maxIv
	p.prompt = plugin.CfgString(cfg, "prompt", defaultPrompt)
	p.dynamic = plugin.CfgBool(cfg, "dynamic", true)
	p.contexts = parseContexts(cfgRawText(cfg, "context_prompts", defaultContexts))
	p.stateDir = ictx.StateDir
	p.runTurn = ictx.RunTurn
	p.newSession = ictx.NewSession
	p.sessions = ictx.Sessions

	// 间隔与倒计时起点都接着上次：只存间隔是不够的，「每 60 分钟一次」还得知道
	// 「上次是几点」才推得出下一次
	st := p.loadStateLocked()
	p.cur, p.adjusted = st.resolve(p.base, p.dynamic, p.normalize)
	p.lastActive = p.probeLastActiveLocked()
	p.lastBeat = p.resumeLastBeat(st.LastBeat, p.lastActive, time.Now())
	// 情境跟着状态走；配置里已经删掉的情境名不保留，否则 beat 会去查一个不存在的键
	if _, ok := p.contexts[st.Context]; ok {
		p.curContext = st.Context
	} else {
		p.curContext = ""
	}
	// 暂停同属动态节奏：关掉动态心跳就没有「模型定的暂停」可言。
	// 设下暂停之后真人说过话的话，说话那刻已把暂停清掉——重启不该让它复活，
	// 所以要对照「暂停设于何时」而不能只看「暂停到何时」。
	p.pausedUntil, p.pausedAt = time.Time{}, time.Time{}
	if p.dynamic && st.PausedUntil.After(time.Now()) && !p.lastActive.After(st.PausedAt) {
		p.pausedUntil, p.pausedAt = st.PausedUntil, st.PausedAt
	} else if p.dynamic && !st.PausedUntil.IsZero() && !p.lastActive.After(st.PausedAt) {
		// 暂停在停机期间到点了：等同「睡醒」（见 beat），节奏回基础值。
		// 不走这条的话，睡前的节奏会越过一整个暂停期存活下来。
		p.cur, p.adjusted = p.base, false
	}

	// 起点当场落盘，别等第一次心跳才写：否则「启用后还没跳过一次就重启」这一段
	// 时间里状态文件仍是空的，倒计时又会从头开始。persistState 不碰 p.mu，
	// 在锁内直接调是安全的，且写完即返回，不会脱离 Init 的生命周期。
	persistState(p.snapshotStateLocked())

	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.wake = make(chan struct{}, 1)
	p.wg.Add(1)
	go p.loop(p.ctx)
	// 公告板来了新理由就叫醒循环重算下一拍。锁序恒为 p.mu → cue 内部锁
	// （Post 的回调在 cue 锁外发起），不成环
	cue.SetNotify(p.onCue)
	return nil
}

// onCue 在公告板有新理由时叫醒循环，让 nextBeatLocked 的提前逻辑立即生效。
func (p *Plugin) onCue() {
	p.mu.Lock()
	wake := p.wake
	p.mu.Unlock()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

// Stop 停止心跳循环。只做取消与有界等待，符合 Stoppable 契约。
// 叫醒回调一并卸下：插件已停，公告板再叫也没有循环可醒。
func (p *Plugin) Stop() {
	cue.SetNotify(nil)
	p.mu.Lock()
	cancel := p.cancel
	p.cancel = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		p.wg.Wait() // 循环体内所有阻塞点都挂在 ctx 上，取消后即刻返回
	}
}

// ---------- 状态持久化（state.json） ----------

// state 是重启后要接着用的东西：当前间隔，以及上一次心跳发生在什么时刻。
//
// 此前只存了间隔，Init 把倒计时清零重算，于是重启比心跳间隔更频繁时，心跳一次都
// 不会触发。定时类状态一律记「上一次发生的时刻」而不是「还剩多久」——进程内的
// 定时器只是执行手段，不是状态载体。
type state struct {
	IntervalSeconds int `json:"interval_seconds"`
	// Adjusted 表示上面那个间隔是动态判定**调整出来**的，而不只是当时基础间隔的
	// 一份副本。少了这一位，两者在文件里长得一模一样，于是「上一次的基础间隔」
	// 会盖住新配的基础间隔——设置页上那一项就永远改不动了。
	Adjusted bool      `json:"adjusted,omitempty"`
	LastBeat time.Time `json:"last_beat,omitempty"`
	// Context 是模型切到的情境（对应配置里的情境提示词名）。
	Context string `json:"context,omitempty"`
	// PausedUntil / PausedAt：暂停到何时、设于何时。要恢复暂停必须两个都看——
	// 设下之后真人说过话的话暂停已被清除，只看「到何时」会让它跨重启复活。
	PausedUntil time.Time `json:"paused_until,omitempty"`
	PausedAt    time.Time `json:"paused_at,omitempty"`
}

// resolve 定出本次启动的心跳间隔，以及它是不是一个动态判定挣来的节奏。
//
// 只有「开着动态心跳」且「确实调整过」时才沿用持久化值——那才是需要跨重启保住的
// 东西。关掉动态心跳、或者根本没调整过，一律跟随基础间隔：此时没有任何节奏可保，
// 沿用旧值只会让用户改了基础间隔却看不到变化。
func (st state) resolve(base time.Duration, dynamic bool, normalize func(time.Duration) time.Duration) (time.Duration, bool) {
	if !dynamic || !st.Adjusted || st.IntervalSeconds <= 0 {
		return base, false
	}
	return normalize(time.Duration(st.IntervalSeconds) * time.Second), true
}

// startupGrace 是重启后补心跳的宽限期。
//
// 关机时长超过心跳间隔时，恢复后「早就该心跳了」，循环会立刻补上一次。补是对的
// ——心跳的语义是隔一阵子主动开口，提示词里还会带上真实间隔——但不该在服务刚起来
// 的那一秒就开口：那时人多半正打算自己说话，调试期间反复重启尤其明显。
const startupGrace = 2 * time.Minute

// resumeLastBeat 定出重启后的倒计时起点。
//
// 取「上次心跳」与「上次真人对话」中较晚的一个：真人聊完会重置心跳时钟
// （见 OnTurnEnd），而那个时刻记在会话元数据里，因此不必在轮次收尾的同步路径上
// 写盘——那条路径要求快速返回，每轮多一次写盘不值得。
func (p *Plugin) resumeLastBeat(lastBeat, lastActive, now time.Time) time.Time {
	last := lastBeat
	if lastActive.After(last) {
		last = lastActive
	}
	if last.IsZero() {
		return now // 没有先前状态（首次启用）：等满一个完整间隔，而不是立刻心跳
	}
	if earliest := now.Add(startupGrace - p.cur); last.Before(earliest) {
		return earliest // 已经过期，补一次，但至少等到宽限期之后
	}
	return last
}

func (p *Plugin) statePath() string { return filepath.Join(p.stateDir, "state.json") }

// loadStateLocked 读上次的状态；文件缺失或损坏时返回零值，由调用方回退。
func (p *Plugin) loadStateLocked() state {
	raw, err := os.ReadFile(p.statePath())
	if err != nil {
		return state{}
	}
	var st state
	if json.Unmarshal(raw, &st) != nil {
		return state{}
	}
	return st
}

// snapshotStateLocked 取出该落盘的内容。调用方需持有 p.mu，写盘在锁外做——
// 持锁写盘会让调用方只能另起 goroutine 绕开自锁，而那种写会脱离插件的生命周期，
// 在插件停掉之后才落地（测试里表现为临时目录清理时「目录非空」）。
func (p *Plugin) snapshotStateLocked() (string, state) {
	return p.stateDir, state{
		IntervalSeconds: int(p.cur / time.Second),
		Adjusted:        p.adjusted,
		LastBeat:        p.lastBeat,
		Context:         p.curContext,
		PausedUntil:     p.pausedUntil,
		PausedAt:        p.pausedAt,
	}
}

// persistState 写状态文件。失败只影响下次启动的初值，不值得让调用方失败。
func persistState(dir string, st state) {
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	raw, _ := json.Marshal(st)
	_ = os.WriteFile(filepath.Join(dir, "state.json"), raw, 0o644)
}

// normalize 把一个算出来的间隔规整成像样的心跳节奏：先取整到分钟，再限幅到
// 配置的最快与最慢之间。所有对 p.cur 的写入都必须经过它。
//
// 取整不是美观问题。衰减每次乘 1.5，而 Duration 是纳秒精度的整数，于是
// 5m → 7m30s → 11m15s → 16m52.5s → 37m58.125s，每衰减一次多一位精度，最后落到
// 一个谁也没配过、也读不出意义的数上。三个间隔配置项的单位都是分钟、最小 1 分钟，
// 亚分钟的精度用户根本表达不出来，保留它只是把浮点噪声当成了信息。加快那一路的
// 对折同理。取整同时让状态文件里的旧值（按秒存）在加载时一并规整。
//
// 顺序是先取整后限幅：反过来的话取整可能把值推到边界之外。
func (p *Plugin) normalize(iv time.Duration) time.Duration {
	iv = iv.Round(time.Minute)
	if iv < p.minIv {
		return p.minIv
	}
	if iv > p.maxIv {
		return p.maxIv
	}
	return iv
}

// probeLastActiveLocked 从会话元数据推最近一次真人交互时间，进程重启后衰减判定不失忆。
func (p *Plugin) probeLastActiveLocked() time.Time {
	last, err := p.sessions.LastInteraction()
	if err != nil {
		return time.Time{}
	}
	return last
}
