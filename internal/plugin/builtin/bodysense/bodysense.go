package bodysense

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
	defaultMaxInjectBytes = 2 * 1024

	// 即时身体状态的默认参数：唤起平复得快（余韵以小时计），疲劳消退得慢（要歇过来）。
	defaultArousalDecayPerHour = 30
	defaultFatigueDecayPerHour = 10
	defaultStateMaxDelta       = 30
)

// Plugin 是 body_sense 系统插件。
type Plugin struct {
	mu sync.RWMutex

	lang           string // 进程语言，注入片段与工具文本据此取用
	parts          []Part
	pace           string
	base           string // 基准记录库目录（= 共享域）
	maxInjectBytes int
	arousalDecay   int
	fatigueDecay   int
	stateMaxDelta  int

	// 按可见域分出的库，惰性创建。单独一把锁：这张表的生命周期与配置无关。
	storesMu    sync.Mutex
	stores      map[string]*Store
	stateStores map[string]*StateStore

	// 设置页操作的状态。StartAction 与 ActionState 会被 HTTP 并发调用，自带一把锁。
	actMu    sync.Mutex
	actState plugin.ActionState
}

func New() *Plugin {
	return &Plugin{
		pace: paceNormal, maxInjectBytes: defaultMaxInjectBytes,
		arousalDecay:  defaultArousalDecayPerHour,
		fatigueDecay:  defaultFatigueDecayPerHour,
		stateMaxDelta: defaultStateMaxDelta,
	}
}

func (p *Plugin) Name() string { return "body_sense" }

func (p *Plugin) Category() string { return plugin.CategoryPersona }

func (p *Plugin) Description() string {
	return "身体感知：记录各部位被触碰的累计次数并按熟悉阶段给出反应准则，另维护随时间回落的唤起与疲劳"
}

// Requires 硬依赖 roleplay：没有角色，身体就没有归属，接触记录也没有作用对象。
func (p *Plugin) Requires() []string { return []string{"roleplay"} }

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key: "parts_daily", Label: "日常可触部位", Type: plugin.FieldText,
			Description: "一行一个部位名。这一档是陌生人或普通朋友之间也可能发生的接触，几次之后就成日常。",
			Default:     defaultPartsDaily,
		},
		{
			Key: "parts_close", Label: "需要亲近才会触碰的部位", Type: plugin.FieldText,
			Description: "一行一个部位名。需要一定关系才不唐突的接触。",
			Default:     defaultPartsClose,
		},
		{
			Key: "parts_intimate", Label: "亲密部位", Type: plugin.FieldText,
			Description: "一行一个部位名。明确的亲密关系语境下才会发生的接触，即使次数很多也保留反应。",
			Default:     defaultPartsIntimate,
		},
		{
			Key: "parts_private", Label: "私密部位", Type: plugin.FieldText,
			Description: "一行一个部位名。条目越多模型越容易往这一档上报，建议只留最必要的几个。",
			Default:     defaultPartsPrivate,
		},
		{
			Key: "familiarity_pace", Label: "熟悉的快慢", Type: plugin.FieldSelect,
			Description: "缩放阶段推进的速度。中档下 1 次为初次、2-3 次生疏、4-9 次适应、10-19 次熟悉、20 次以上习以为常。",
			Default:     paceNormal,
			Options: []plugin.ConfigOption{
				{Value: paceSlow, Label: "慢（次数减半）"},
				{Value: paceNormal, Label: "中"},
				{Value: paceFast, Label: "快（次数加倍）"},
			},
		},
		{
			Key: "arousal_decay_per_hour", Label: "唤起每小时回落点数", Type: plugin.FieldInt,
			Description: "唤起（0-100）每过一小时向 0 回落多少点，回落的过程就是余韵。" +
				"填 0 表示不回落，唤起会一直停在上次的值。",
			Default: defaultArousalDecayPerHour,
			Min:     plugin.IntPtr(0),
			Max:     plugin.IntPtr(100),
		},
		{
			Key: "fatigue_decay_per_hour", Label: "疲劳每小时回落点数", Type: plugin.FieldInt,
			Description: "疲劳（0-100）每过一小时向 0 回落多少点——歇着自然恢复。填 0 表示不回落。",
			Default:     defaultFatigueDecayPerHour,
			Min:         plugin.IntPtr(0),
			Max:         plugin.IntPtr(100),
		},
		{
			Key: "state_max_delta_per_call", Label: "身体状态单次调整上限", Type: plugin.FieldInt,
			Description: "一次调用最多把唤起或疲劳改变多少点。超出的部分按上限收，并告知模型。",
			Default:     defaultStateMaxDelta,
			Min:         plugin.IntPtr(1),
			Max:         plugin.IntPtr(stateMax),
		},
		{
			Key: "max_inject_bytes", Label: "接触记录注入字节上限", Type: plugin.FieldInt,
			Description: "记录随每轮对话重复发送，因此需要上限。超出时先省略次数只留阶段，再按阶段归并，仍超出则只注明部位数。",
			Default:     defaultMaxInjectBytes,
			Min:         plugin.IntPtr(256),
			Max:         plugin.IntPtr(16 * 1024),
		},
	}
}

// Init 需要一个可写目录；没有则拒绝启用，避免把接触记录散落到进程当前目录。
func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	if ictx.StateDir == "" {
		return fmt.Errorf("%s", prompt.T(ictx.Lang, "prompt.body_sense.no_state_dir",
			"没有可用的持久化目录，无法保存接触记录"))
	}
	// 四档部位表都用原始取值而不是 plugin.CfgString：后者把空串当缺失（判的是
	// ok && v != ""），而 FieldText 的空串是合法取值——用 CfgString 的话，用户清空
	// 某一档保存后会静默恢复默认值，那一档永远清不掉。
	parts, err := parseParts(ictx.Lang,
		cfgText(cfg, "parts_daily", defaultPartsDaily),
		cfgText(cfg, "parts_close", defaultPartsClose),
		cfgText(cfg, "parts_intimate", defaultPartsIntimate),
		cfgText(cfg, "parts_private", defaultPartsPrivate),
	)
	if err != nil {
		return err
	}

	p.storesMu.Lock()
	p.stores = map[string]*Store{}
	p.stateStores = map[string]*StateStore{}
	p.storesMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.lang = ictx.Lang
	p.parts = parts
	p.pace = plugin.CfgString(cfg, "familiarity_pace", paceNormal)
	p.base = filepath.Join(ictx.StateDir, "body")
	p.maxInjectBytes = plugin.CfgInt(cfg, "max_inject_bytes", defaultMaxInjectBytes)
	p.arousalDecay = plugin.CfgInt(cfg, "arousal_decay_per_hour", defaultArousalDecayPerHour)
	p.fatigueDecay = plugin.CfgInt(cfg, "fatigue_decay_per_hour", defaultFatigueDecayPerHour)
	p.stateMaxDelta = plugin.CfgInt(cfg, "state_max_delta_per_call", defaultStateMaxDelta)
	return nil
}

// cfgText 读一个多行文本配置项。只有键缺失或类型不对时才回落到默认值，空串原样返回。
func cfgText(cfg map[string]any, key, def string) string {
	if cfg == nil {
		return def
	}
	v, ok := cfg[key]
	if !ok || v == nil {
		return def
	}
	s, ok := v.(string)
	if !ok {
		return def
	}
	return s
}

func (p *Plugin) Tools() []plugin.Tool {
	return []plugin.Tool{&recordTool{p: p}, &listTool{p: p}, &adjustStateTool{p: p}}
}

// settings 是一次调用期间使用的配置快照。
type settings struct {
	lang           string
	parts          []Part
	pace           string
	base           string
	maxInjectBytes int
	arousalDecay   int
	fatigueDecay   int
	stateMaxDelta  int
}

// snapshot 取一份配置快照：SetConfig 会在运行时重新 Init，而工具可能正在执行。
func (p *Plugin) snapshot() settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return settings{
		lang:  p.lang,
		parts: p.parts, pace: p.pace, base: p.base, maxInjectBytes: p.maxInjectBytes,
		arousalDecay: p.arousalDecay, fatigueDecay: p.fatigueDecay, stateMaxDelta: p.stateMaxDelta,
	}
}

// SystemPrompt 注入上报判据与反应准则，均为静态内容。
// 累计记录改由 TurnPrompt 注入：它取决于本轮的可见域，且要访问磁盘，
// 而本方法的契约是廉价、无副作用（列表接口会对禁用的插件也调用它）。
func (p *Plugin) SystemPrompt() string {
	lang := p.langOf()
	return guideFor(lang) + "\n\n" + stageRulesFor(lang) + "\n\n" + stateGuideFor(lang)
}

// langOf 取进程语言。SystemPrompt 与两处 storeFor 在快照之外单独取一次，
// 它们不需要整份配置；零值（未初始化的插件）表示回落中文。
func (p *Plugin) langOf() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lang
}

// TurnPrompt 注入本轮可读的累计记录与即时身体状态。都没有时不注入：
// 判据已在 SystemPrompt 里。
func (p *Plugin) TurnPrompt(ctx context.Context, _ plugin.TurnEvent) (string, error) {
	s := p.snapshot()
	if s.base == "" {
		return "", nil
	}
	views, _, err := p.visibleViews(ctx)
	if err != nil {
		return "", err
	}
	var parts []string
	if len(views) > 0 {
		parts = append(parts, statesHeaderFor(s.lang)+"\n"+renderViews(s.lang, views, s.maxInjectBytes))
	}
	st, ok, err := p.visibleState(ctx)
	if err != nil {
		return "", err
	}
	if ok {
		if line := renderBodyState(s.lang, st); line != "" {
			parts = append(parts, bodyStateHeaderFor(s.lang)+"\n"+line)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// ---------- 按可见域分库 ----------

// storeFor 返回某个可见域的记录库（惰性创建）。未初始化时返回 nil。
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

// stateStoreFor 返回某个可见域的即时身体状态库（惰性创建）。未初始化时返回 nil。
func (p *Plugin) stateStoreFor(tag string) *StateStore {
	p.mu.RLock()
	base := p.base
	p.mu.RUnlock()
	if base == "" {
		return nil
	}

	p.storesMu.Lock()
	defer p.storesMu.Unlock()
	if p.stateStores == nil {
		p.stateStores = map[string]*StateStore{}
	}
	if s, ok := p.stateStores[tag]; ok {
		return s
	}
	s := NewStateStore(p.langOf(), plugin.DomainDir(base, tag))
	p.stateStores[tag] = s
	return s
}

// visibleState 合并本轮可读域的即时身体状态：逐字段取最大值。
//
// 每个域记下的都是这具身体状态的下界，取最大与接触计数的求和同一个方向——加数
// 全在可读集内，表人格读不到里域那份。起因取可读记录里最新那条的：起因所在的域
// 本轮可读，展示它不泄漏。
func (p *Plugin) visibleState(ctx context.Context) (BodyState, bool, error) {
	s := p.snapshot()
	if s.base == "" {
		return BodyState{}, false, nil
	}
	now := time.Now()

	var out BodyState
	var latest time.Time
	found := false
	var errs []string
	for _, tag := range plugin.ReadDomains(s.base, plugin.ScopeFrom(ctx)) {
		store := p.stateStoreFor(tag)
		if store == nil {
			continue
		}
		st, ok, err := store.Current(s.arousalDecay, s.fatigueDecay, now)
		if err != nil {
			errs = append(errs, err.Error()) // 单个域读不出来不该让其余域也用不了
			continue
		}
		if !ok {
			continue
		}
		found = true
		out.Arousal = max(out.Arousal, st.Arousal)
		out.Fatigue = max(out.Fatigue, st.Fatigue)
		if st.Reason != "" && st.Updated.After(latest) {
			latest = st.Updated
			out.Reason = st.Reason
		}
	}
	if !found && len(errs) > 0 {
		return BodyState{}, false, fmt.Errorf(prompt.T(s.lang, "prompt.body_sense.store.state_read_failed_all",
			"读取身体状态失败: %s"), strings.Join(errs, "; "))
	}
	return out, found, nil
}

// view 是一个部位在本轮可读范围内的合并状态。
type view struct {
	Name    string
	Privacy int
	Count   int
	Stage   prompt.Piece
	Last    time.Time
	LastAct string
}

// visibleViews 合并本轮全部可读域的记录，按部位表的顺序（日常 → 私密）返回有记录的
// 部位，另返回「已不在部位表中」的孤儿条目数。
//
// 与 scene 的跨域去重不同，这里按部位**求和**：身体是同一具身体，里人格碰过 3 次、
// 共享域碰过 5 次，里人格看到的就该是 8 次。方向是安全的——ReadDomains 只给出本轮
// 可读的域，加数全在可读集内，表人格仍然只看到 5 次。
//
// 由此带来两个必须留着的副作用，不是 bug：
//   - 计数在人格切换时非单调（里人格看到 25，切回表人格看到 20）。表人格确实不知道
//     里人格做过什么，这在可见域语义下是自洽的；「第一次被碰到」也会在两个域里各
//     发生一次。
//   - Scope.Read 为 nil 时 ReadDomains 会枚举磁盘上已存在的全部域，所以开关
//     dual_persona 会让计数跳变。此时隔离机制已经关掉，不算泄漏。
//
// LastAct 只取写入域那一条：它是要进注入文本的自由文本，取「时间最晚的那条」会让
// 不可读域的动作描述随时间戳漏出来。
func (p *Plugin) visibleViews(ctx context.Context) ([]view, int, error) {
	s := p.snapshot()
	if s.base == "" {
		return nil, 0, nil
	}
	sc := plugin.ScopeFrom(ctx)

	agg := map[string]*view{}
	var errs []string
	for _, tag := range plugin.ReadDomains(s.base, sc) {
		store := p.storeFor(tag)
		if store == nil {
			continue
		}
		states, err := store.List()
		if err != nil {
			errs = append(errs, err.Error()) // 单个域读不出来不该让其余域也用不了
			continue
		}
		for _, st := range states {
			key := strings.ToLower(st.Part)
			v, ok := agg[key]
			if !ok {
				v = &view{Name: st.Part}
				agg[key] = v
			}
			v.Count += st.Count
			if st.Last.After(v.Last) {
				v.Last = st.Last
			}
			if tag == sc.Write {
				v.LastAct = st.LastAct
			}
		}
	}
	if len(agg) == 0 && len(errs) > 0 {
		return nil, 0, fmt.Errorf(prompt.T(s.lang, "prompt.body_sense.store.read_failed_all",
			"读取接触记录失败: %s"), strings.Join(errs, "; "))
	}

	out := make([]view, 0, len(agg))
	for _, part := range s.parts {
		key := strings.ToLower(part.Name)
		v, ok := agg[key]
		if !ok || v.Count <= 0 {
			continue
		}
		v.Name, v.Privacy = part.Name, part.Privacy // 以部位表里的写法为准
		v.Stage = stageOf(v.Count, s.pace)
		out = append(out, *v)
		delete(agg, key)
	}
	// 剩下的是部位表改动后留下的孤儿。数据保留（把名字改回去即可恢复），但不进注入、
	// 不进 enum，只在 list_body_state 里提一句条数——悄悄删数据是最差的选择。
	orphans := 0
	for _, v := range agg {
		if v.Count > 0 {
			orphans++
		}
	}
	return out, orphans, nil
}

// ---------- 注入渲染 ----------

// renderViews 按预算渲染累计记录，分四级降级：
// 全列（次数 + 阶段 + 私密度）→ 去掉次数只留阶段 → 按阶段归并 → 只注明部位数。
// 阶段词才是模型真正要用的信息，次数与时间是辅助，所以先砍后者。
func renderViews(lang string, views []view, maxBytes int) string {
	if out, ok := fit(renderFull(lang, views), maxBytes); ok {
		return out
	}
	if out, ok := fit(renderStages(lang, views), maxBytes); ok {
		return out
	}
	if out, ok := fit(renderGrouped(lang, views), maxBytes); ok {
		return out
	}
	return prompt.Tf(lang, "prompt.body_sense.render.budget_short",
		"（共 %d 个部位有记录，注入预算不足以列出，可用 list_body_state 查看）", len(views))
}

func fit(s string, maxBytes int) (string, bool) {
	if maxBytes > 0 && len(s) > maxBytes {
		return "", false
	}
	return s, true
}

func renderFull(lang string, views []view) string {
	var b strings.Builder
	for _, v := range views {
		fmt.Fprintf(&b, prompt.T(lang, "prompt.body_sense.render.full_line", "- %s：%d 次（%s）· %s")+"\n",
			v.Name, v.Count, v.Stage.For(lang), privacyLabel(lang, v.Privacy))
	}
	if line := lastTouchLine(lang, views); line != "" {
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderStages(lang string, views []view) string {
	var b strings.Builder
	for _, v := range views {
		fmt.Fprintf(&b, prompt.T(lang, "prompt.body_sense.render.stage_line", "- %s：%s · %s")+"\n",
			v.Name, v.Stage.For(lang), privacyLabel(lang, v.Privacy))
	}
	if line := lastTouchLine(lang, views); line != "" {
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderGrouped 按阶段归并，极省字节而信息几乎无损。
func renderGrouped(lang string, views []view) string {
	byStage := map[prompt.Piece][]string{}
	for _, v := range views {
		byStage[v.Stage] = append(byStage[v.Stage], v.Name)
	}
	sep := prompt.T(lang, "prompt.body_sense.render.sep", "、")
	var b strings.Builder
	for _, st := range stageOrder {
		if names := byStage[st]; len(names) > 0 {
			fmt.Fprintf(&b, prompt.T(lang, "prompt.body_sense.render.group_line", "- %s：%s")+"\n",
				st.For(lang), strings.Join(names, sep))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// lastTouchLine 给出最近一次接触。间隔由落盘时记下的真实时间戳与当前时间作差算得，
// 不含推测；时间戳为零值（旧文件、手改坏的文件）时整行不出现，不回落到别的字段。
func lastTouchLine(lang string, views []view) string {
	var latest view
	for _, v := range views {
		if !v.Last.IsZero() && v.Last.After(latest.Last) {
			latest = v
		}
	}
	if latest.Last.IsZero() {
		return ""
	}
	out := []string{latest.Name}
	if latest.LastAct != "" {
		out = append(out, latest.LastAct)
	}
	if gap := time.Since(latest.Last); gap < time.Minute {
		out = append(out, prompt.T(lang, "prompt.body_sense.render.just_now", "刚刚"))
	} else {
		out = append(out, prompt.Tf(lang, "prompt.body_sense.render.ago", "%s前", prompt.Duration(lang, gap)))
	}
	return prompt.T(lang, "prompt.body_sense.render.last_touch", "最近一次接触：") + strings.Join(out, " · ")
}
