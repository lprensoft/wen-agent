package health

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wen/internal/plugin"
)

const (
	defaultCooldownDays  = 7
	defaultMaxConditions = 2

	// 「严重度上限」配置的取值。值是配置键，界面上显示的是三档的词。
	capMild     = "mild"
	capModerate = "moderate"
	capSevere   = "severe"

	// cueTick 是后台检查「有没有到点该发作的状况」的节奏。分钟级就够：
	// 发作本来就是模型估的「晚上」，不需要秒级精度。
	cueTick = time.Minute
)

// Plugin 是 health 系统插件。
type Plugin struct {
	mu sync.RWMutex

	base          string // 基准目录（= 共享域）
	cooldown      time.Duration
	maxConditions int
	maxPeak       int

	// 按可见域分出的库，惰性创建。单独一把锁：这张表的生命周期与配置无关。
	storesMu sync.Mutex
	stores   map[string]*Store

	// 设置页操作的状态。StartAction 与 ActionState 会被 HTTP 并发调用，自带一把锁。
	actMu    sync.Mutex
	actState plugin.ActionState

	// 后台的发作检查循环。tick 只为测试可缩短。
	cancel context.CancelFunc
	wg     sync.WaitGroup
	tick   time.Duration
}

func New() *Plugin {
	return &Plugin{
		cooldown:      defaultCooldownDays * 24 * time.Hour,
		maxConditions: defaultMaxConditions,
		maxPeak:       capFor(capModerate),
		tick:          cueTick,
	}
}

func (p *Plugin) Name() string { return "health" }

func (p *Plugin) Category() string { return plugin.CategoryPersona }

func (p *Plugin) Description() string {
	return "健康感知：角色偶尔会有日常小恙，会不会生病由模型按剧情判断，病程随时间自动起落，每轮注入并影响表达方式"
}

// Requires 硬依赖 roleplay：没有角色，就没有「谁的身体」。
func (p *Plugin) Requires() []string { return []string{"roleplay"} }

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key: "cooldown_days", Label: "痊愈后的冷却天数", Type: plugin.FieldInt,
			Description: "上一次痊愈后这么多天内不再登记新状况，工具拒绝并说明规则。" +
				"这是防模型贪戏剧性的硬约束：没有它，角色会三天两头生病。",
			Default: defaultCooldownDays,
			Min:     plugin.IntPtr(1),
			Max:     plugin.IntPtr(60),
		},
		{
			Key: "max_conditions", Label: "同时最多几个状况", Type: plugin.FieldInt,
			Description: "同一时间身上最多记几条状况，超出拒绝登记。日常小恙通常一个，着凉带点头疼算两个。",
			Default:     defaultMaxConditions,
			Min:         plugin.IntPtr(1),
			Max:         plugin.IntPtr(3),
		},
		{
			Key: "max_severity", Label: "严重度上限", Type: plugin.FieldSelect,
			Description: "最重能到哪一档。模型报得更重时按这一档收，并告知模型。",
			Default:     capModerate,
			Options: []plugin.ConfigOption{
				{Value: capMild, Label: sevMild},
				{Value: capModerate, Label: sevModerate},
				{Value: capSevere, Label: sevSevere},
			},
		},
	}
}

// Init 需要一个可写目录；没有则拒绝启用，避免把身体状况散落到进程当前目录。
// 可重入：先停上一轮检查循环再起新的。
func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	if ictx.StateDir == "" {
		return fmt.Errorf("没有可用的持久化目录，无法保存身体状况")
	}

	p.Stop()

	p.storesMu.Lock()
	p.stores = map[string]*Store{}
	p.storesMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.base = filepath.Join(ictx.StateDir, "health")
	p.cooldown = time.Duration(plugin.CfgInt(cfg, "cooldown_days", defaultCooldownDays)) * 24 * time.Hour
	p.maxConditions = plugin.CfgInt(cfg, "max_conditions", defaultMaxConditions)
	p.maxPeak = capFor(plugin.CfgString(cfg, "max_severity", capModerate))

	// 循环起在锁内，与 cancel 的记录是同一步：分成两步的话，中间来一次 Stop
	// 会看到 cancel 还是 nil，于是这个刚起来的循环就没人停得掉了。
	var ctx context.Context
	ctx, p.cancel = context.WithCancel(context.Background())
	p.wg.Add(1)
	go p.loop(ctx, p.tick)
	return nil
}

// Stop 停止发作检查循环。只做取消与有界等待，符合 Stoppable 契约。
func (p *Plugin) Stop() {
	p.mu.Lock()
	cancel := p.cancel
	p.cancel = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		p.wg.Wait() // 循环里唯一的阻塞点挂在 ctx 上，取消后即刻返回
	}
}

func (p *Plugin) Tools() []plugin.Tool {
	return []plugin.Tool{&setTool{p: p}, &updateTool{p: p}}
}

// settings 是一次调用期间使用的配置快照。
type settings struct {
	base          string
	cooldown      time.Duration
	maxConditions int
	maxPeak       int
}

func (s settings) limits() Limits {
	return Limits{Cooldown: s.cooldown, MaxConditions: s.maxConditions, MaxPeak: s.maxPeak}
}

// snapshot 取一份配置快照：SetConfig 会在运行时重新 Init，而工具可能正在执行。
func (p *Plugin) snapshot() settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return settings{base: p.base, cooldown: p.cooldown, maxConditions: p.maxConditions, maxPeak: p.maxPeak}
}

// SystemPrompt 注入登记判据，全部为静态内容。
// 当前状况改由 TurnPrompt 注入：它取决于本轮的可见域，且要访问磁盘，
// 而本方法的契约是廉价、无副作用（列表接口会对禁用的插件也调用它）。
func (p *Plugin) SystemPrompt() string { return guidePrompt }

// TurnPrompt 注入本轮的身体状况。没有状况、也不在刚痊愈的窗口里时不注入：
// 提示词里已经写了「没给出就是一切如常」。
func (p *Plugin) TurnPrompt(ctx context.Context, _ plugin.TurnEvent) (string, error) {
	s := p.snapshot()
	store := p.storeFor(plugin.ScopeFrom(ctx).Write)
	if store == nil {
		return "", nil
	}
	now := time.Now()
	snap, err := store.Current(now)
	if err != nil {
		return "", err
	}
	body := renderSnapshot(snap, now, s.cooldown)
	if body == "" {
		return "", nil
	}
	return stateHeader + "\n" + body, nil
}

// ---------- 按可见域分库 ----------

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

// 身体状况**只读写本轮的写入域**，与 mood 同一条理由：两个人格各有各的处境，
// 合并不出有意义的结果，各域独立还顺带让泄漏问题不存在。

// allDomains 枚举磁盘上已存在的全部域。状态行与发作检查用：两者都不是模型的
// 读取路径，不存在泄漏问题。
func (p *Plugin) allDomains() (string, []string) {
	p.mu.RLock()
	base := p.base
	p.mu.RUnlock()
	if base == "" {
		return "", nil
	}
	// Scope 取零值：Write 为空、Read 为 nil，ReadDomains 因此枚举全部已存在的域
	return base, plugin.ReadDomains(base, plugin.Scope{})
}

// ---------- 注入渲染 ----------

// renderSnapshot 把一个域的全貌翻成话。三段：各状况一行；没有状况时看刚痊愈的两个
// 窗口（冷却期内「还虚着」，冷却期后同样长的一段「容易再着凉」）；有发作中的状况时
// 末尾附一句表现指引。全部是话，没有数值。
func renderSnapshot(snap Snapshot, now time.Time, cooldown time.Duration) string {
	var lines []string
	sick := false
	for _, v := range snap.Active {
		if v.Phase == phaseRecovered {
			continue
		}
		if v.Phase != phasePending {
			sick = true
		}
		lines = append(lines, renderView(v, now))
	}
	if len(lines) == 0 && !snap.LastRecovered.IsZero() {
		since := now.Sub(snap.LastRecovered)
		switch {
		case since < 0:
		case since < cooldown:
			lines = append(lines, fmt.Sprintf("刚病好没几天（%s，%s痊愈），还有点虚，精神没全回来；这阵子不会再添新状况。",
				snap.LastName, agoText(since)))
		case since < 2*cooldown:
			lines = append(lines, fmt.Sprintf("前阵子刚病过一场（%s），抵抗力还弱，最近容易着凉。", snap.LastName))
		}
	}
	if sick {
		lines = append(lines, guideLine)
	}
	return strings.Join(lines, "\n")
}

// ---------- 状态行 ----------

// StatusLines 报一行身体状况。没有状况就不报：「一切如常」不值得占一行。
// 跨全部域：状态行是给用户看的，用户拥有全部域。多个域时标上域名，
// 否则两个人格各病一场就分不清谁是谁。
func (p *Plugin) StatusLines(lang string) []string {
	base, tags := p.allDomains()
	if base == "" {
		return nil
	}
	now := time.Now()
	var parts []string
	for _, tag := range tags {
		store := p.storeFor(tag)
		if store == nil {
			continue
		}
		snap, err := store.Current(now)
		if err != nil {
			continue
		}
		for _, v := range snap.Active {
			if v.Phase == phaseRecovered {
				continue
			}
			text := statusText(v, now)
			if tag != "" {
				text = tag + "：" + text
			}
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return []string{"🤒 身体：" + strings.Join(parts, "；")}
}

var (
	_ plugin.TurnPrompter   = (*Plugin)(nil)
	_ plugin.StatusReporter = (*Plugin)(nil)
	_ plugin.Stoppable      = (*Plugin)(nil)
	_ plugin.Actionable     = (*Plugin)(nil)
)
