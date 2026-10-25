// Package growth 让角色在相处中慢慢长出东西来，又始终是原来那个人。
//
// 思路取自 project-sue- 的 self-growth：人格分三层。根是用户写的角色设定与台词样例，
// 本插件永远不改写它们——成长是叠在上面的一层，不是编辑；核心是性格、待人与说话方式
// 的整体变化，由角色在反思时提议、人在设置页批准才生效，同一时间只有一条在等；枝叶是
// 喜好、习惯、口头禅、生活里的小细节，角色自己改、留痕，每天有限额、总数有上限。
//
// 判断只在每天一次的反思轮次里做（新的一天第一轮对话或心跳结束后，见 reflect.go），
// 两个改动工具在别的轮次里一律拒绝：平常聊天时随手改自己，正是漂移的来路。护栏都在
// 代码里，不只写在提示词里——限额、上限、一次一条、必填的「为什么这还是我」。
//
// 它只管「角色自己身上长出来的」：对方的事归 memory，两人之间的默契归 relationship，
// 此刻的状态归 mood 与 body_sense。按可见域各存一份——成长属于人格，表里两面各长各的。
package growth

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

const (
	defMaxDetails     = 12
	defMaxCore        = 6
	defDailyChanges   = 3
	defMaxInjectBytes = 1536

	// dayStartHour 之前的时段算前一天：凌晨两点的那轮对话还是「昨天」的事。
	dayStartHour = 5
	dateLayout   = "2006-01-02"

	// 一条成长的长度上限（按字计）。超了报错让模型压缩，不截断——截掉半句的习惯，
	// 意思可能正好变了。
	maxTextRunes  = 60
	maxBasisRunes = 80
	maxWhyRunes   = 80
)

// Plugin 是 growth 插件。有状态：Init 会被反复调用，字段由 mu 保护。
type Plugin struct {
	mu sync.RWMutex

	lang           string
	base           string // 基准目录（= 共享域）
	reflect        bool
	maxDetails     int
	maxCore        int
	dailyChanges   int
	maxInjectBytes int

	runTurn plugin.RunTurnFunc
	notice  plugin.NoticeFunc

	now        func() time.Time // 测试替换
	settle     time.Duration    // 触发轮次收尾后等多久再起反思
	busyRetry  time.Duration
	busyMax    int
	runTimeout time.Duration

	storesMu sync.Mutex
	stores   map[string]*Store

	// 反思的生命周期：Init 起、Stop 停；reflecting 防同一个域重复起。
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	reflecting map[string]bool
	// claims 是「这个会话上的反思轮次认领了哪个域」，claimed 是被认领的域：反思落在
	// 哪个人格上由那一轮在裁决之后自己认领（见 runReflect），工具只接受已认领的域。
	claims  map[string]string
	claimed map[string]bool
	// changed 记下反思轮次里每个域做了哪些改动，轮次结束后汇成一条会话注记。
	changed map[string][]string
	// lastActive 是每个域最近一次真人对话的时刻（只在内存里）：反思收尾时只消费它
	// 开始之前的那些。
	lastActive map[string]time.Time

	actMu    sync.Mutex
	actState map[string]plugin.ActionState
	shown    string // 设置页最后一次展示的待批准提议（域/编号），批准与驳回只认它
}

func New() *Plugin {
	return &Plugin{
		reflect: true, maxDetails: defMaxDetails, maxCore: defMaxCore,
		dailyChanges: defDailyChanges, maxInjectBytes: defMaxInjectBytes,
		now: time.Now, settle: 20 * time.Second, busyRetry: time.Minute, busyMax: 3,
		runTimeout: 5 * time.Minute,
		stores:     map[string]*Store{}, reflecting: map[string]bool{}, changed: map[string][]string{},
		claims: map[string]string{}, claimed: map[string]bool{}, lastActive: map[string]time.Time{},
		actState: map[string]plugin.ActionState{},
	}
}

func (p *Plugin) Name() string { return "growth" }

func (p *Plugin) Category() string { return plugin.CategoryPersona }

func (p *Plugin) Description() string {
	return "成长：角色在相处中慢慢长出新的喜好、习惯与小变化，大的变化要你批准；角色设定本身不会被改动"
}

// Requires：根是 roleplay 的角色设定，没有角色就没有谁在成长；反思要读 memory 的
// [最近几天]，提示词只能引用依赖方的块名。
func (p *Plugin) Requires() []string { return []string{"roleplay", "memory"} }

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key: "daily_reflect", Label: "每天反思一次", Type: plugin.FieldBool, Default: true,
			Description: "新的一天第一轮对话或心跳结束后，角色回头看看昨天，决定有没有长出什么（每天多一轮模型调用）。" +
				"前一天没有人来聊过就跳过。关掉后已有的成长照常生效，只是不再长出新的",
		},
		{
			Key: "max_details", Label: "小变化上限（条）", Type: plugin.FieldInt, Default: defMaxDetails,
			Min: plugin.IntPtr(1), Max: plugin.IntPtr(30),
			Description: "喜好、习惯、口头禅这类小变化最多留几条，满了角色只能替换或放下旧的",
		},
		{
			Key: "max_core", Label: "大变化上限（条）", Type: plugin.FieldInt, Default: defMaxCore,
			Min: plugin.IntPtr(1), Max: plugin.IntPtr(12),
			Description: "已批准的性格、待人、说话方式上的变化最多留几条，满了就不再接受新提议",
		},
		{
			Key: "daily_changes", Label: "每天最多改几条小变化", Type: plugin.FieldInt, Default: defDailyChanges,
			Min: plugin.IntPtr(1), Max: plugin.IntPtr(10),
			Description: "新增、修改、放下都算一次。限额是防漂移的护栏：一天里变太多，就不像同一个人了",
		},
		{
			Key: "max_inject_bytes", Label: "注入字节上限", Type: plugin.FieldInt, Default: defMaxInjectBytes,
			Min: plugin.IntPtr(256), Max: plugin.IntPtr(8 * 1024),
			Description: "成长随每轮对话注入，因此需要上限。超出时先省略日期，再只留最近的几条并注明另有几条",
		},
	}
}

// Init 需要可写目录；起反思用的生命周期。可重入：先停旧的。
func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	if ictx.StateDir == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.growth.no_state_dir",
			"没有可用的持久化目录，无法保存成长记录"))
	}
	p.Stop()

	p.storesMu.Lock()
	p.stores = map[string]*Store{}
	p.storesMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.lang = ictx.Lang
	p.base = filepath.Join(ictx.StateDir, "growth")
	p.reflect = plugin.CfgBool(cfg, "daily_reflect", true)
	p.maxDetails = plugin.CfgInt(cfg, "max_details", defMaxDetails)
	p.maxCore = plugin.CfgInt(cfg, "max_core", defMaxCore)
	p.dailyChanges = plugin.CfgInt(cfg, "daily_changes", defDailyChanges)
	p.maxInjectBytes = plugin.CfgInt(cfg, "max_inject_bytes", defMaxInjectBytes)
	p.runTurn = ictx.RunTurn
	p.notice = ictx.Notice
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.reflecting = map[string]bool{}
	p.claims, p.claimed, p.lastActive = map[string]string{}, map[string]bool{}, map[string]time.Time{}
	p.changed = map[string][]string{}
	return nil
}

// Stop 取消正在等待或进行中的反思，并等它们退出（RunTurn 跟着 ctx 一起结束）。
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

func (p *Plugin) SystemPrompt() string { return "" }

func (p *Plugin) Tools() []plugin.Tool {
	return []plugin.Tool{&detailTool{p: p}, &proposeTool{p: p}}
}

// settings 是一次调用期间使用的配置快照。
type settings struct {
	lang           string
	base           string
	reflect        bool
	maxDetails     int
	maxCore        int
	dailyChanges   int
	maxInjectBytes int
	runTurn        plugin.RunTurnFunc
	notice         plugin.NoticeFunc
}

func (p *Plugin) snapshot() settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return settings{
		lang: p.lang, base: p.base, reflect: p.reflect, maxDetails: p.maxDetails, maxCore: p.maxCore,
		dailyChanges: p.dailyChanges, maxInjectBytes: p.maxInjectBytes, runTurn: p.runTurn, notice: p.notice,
	}
}

func (p *Plugin) langOf() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lang
}

// logicalDay 是 t 所在的逻辑日：dayStartHour 之前算前一天。
func logicalDay(t time.Time) string {
	return t.Add(-dayStartHour * time.Hour).Format(dateLayout)
}

// storeFor 返回某个可见域的库（惰性创建）。未初始化时返回 nil。
func (p *Plugin) storeFor(tag string) *Store {
	s := p.snapshot()
	if s.base == "" {
		return nil
	}
	p.storesMu.Lock()
	defer p.storesMu.Unlock()
	if st, ok := p.stores[tag]; ok {
		return st
	}
	st := NewStore(s.lang, plugin.DomainDir(s.base, tag))
	p.stores[tag] = st
	return st
}

// TurnPrompt 注入本轮写入域的成长。没有任何成长时不注入——没长出东西之前，这个插件
// 对上下文零开销。
//
// 只读写入域那一份，不像 body_sense 那样跨可读域合并：成长属于人格，里人格长出来的
// 习惯不是表人格的习惯。
func (p *Plugin) TurnPrompt(ctx context.Context, ev plugin.TurnEvent) (string, error) {
	if inReflection(ctx) {
		return p.reflectionPrompt(ctx, ev.SessionID)
	}
	s := p.snapshot()
	store := p.storeFor(plugin.ScopeFrom(ctx).Write)
	if store == nil {
		return "", nil
	}
	st, err := store.Load()
	if err != nil {
		return "", err
	}
	if len(st.Details) == 0 && len(st.Core) == 0 {
		return "", nil
	}
	return renderBlock(s.lang, st, s.maxInjectBytes), nil
}

// renderBlock 按预算渲染注入块，三级降级：全列（带起始日期）→ 去掉日期 → 核心照留、
// 小变化只留最近几条并注明另有几条。核心条数少且分量重，永远保留。
func renderBlock(lang string, st State, maxBytes int) string {
	head := prompt.T(lang, "prompt.growth.block", blockZH)
	build := func(withDate bool, details []Entry, omitted int) string {
		var b strings.Builder
		b.WriteString(head)
		if len(st.Core) > 0 {
			b.WriteString("\n" + prompt.T(lang, "prompt.growth.block.core", "已经定下来的："))
			for _, e := range st.Core {
				b.WriteString("\n- " + e.Text)
			}
		}
		if len(details) > 0 {
			b.WriteString("\n" + prompt.T(lang, "prompt.growth.block.details", "近来的小变化："))
			for _, e := range details {
				b.WriteString("\n- " + e.Text)
				if withDate && !e.Since.IsZero() {
					b.WriteString(prompt.Tf(lang, "prompt.growth.block.since", "（%s 起）", e.Since.Format(dateLayout)))
				}
			}
		}
		if omitted > 0 {
			b.WriteString("\n" + prompt.Tf(lang, "prompt.growth.block.omitted", "（另有 %d 条较早的小变化）", omitted))
		}
		return b.String()
	}
	if out := build(true, st.Details, 0); len(out) <= maxBytes {
		return out
	}
	if out := build(false, st.Details, 0); len(out) <= maxBytes {
		return out
	}
	for keep := len(st.Details) - 1; keep >= 0; keep-- {
		if out := build(false, st.Details[len(st.Details)-keep:], len(st.Details)-keep); len(out) <= maxBytes || keep == 0 {
			return out
		}
	}
	return build(false, nil, len(st.Details))
}

// blockZH 是注入块的抬头与用法。说清两件事：这是长在设定之上的，冲突以设定为准；
// 以及怎么用——自然地带出来，而不是宣告「我变了」，后者是这类功能最常见的出戏方式。
const blockZH = `[这些日子的变化]
下面是你在角色设定之上、在相处中慢慢长出来的东西，和角色设定冲突时以设定为准。
它们已经是你的一部分：在合适的时候自然地流露，不要宣告自己变了，也不要逐条复述。`
