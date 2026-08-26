// Package dualpersona 提供表里两套人格的系统插件。
//
// 表人格的设定由 roleplay 提供，本插件只管三件事：里人格自己的设定与台词样例、
// 两个方向的切换触发词，以及每轮的可见域裁决——里人格的消息带标签落盘，表人格
// 读不到它们，里人格读得到全部。
//
// 它不引用 roleplay 的任何类型。里人格激活时 roleplay 注入的表人格设定仍留在
// 上下文里，这是对的：里人格「看得到全部」，本就该知道表面那层长什么样；而注册顺序
// 让本插件的片段排在其后，天然形成追加与覆盖的语义。Requires 只是启用门禁。
//
// 需要清楚的一点：可见域是上下文隔离，不是沙箱。提供通用文件与命令通道的插件
// （read_file / exec_command）能直接读到会话文件、记忆目录与本插件的状态文件，
// 绕过这里的一切过滤，因此在 Conflicts 里声明出来告警。
package dualpersona

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"wen/internal/imbot"
	"wen/internal/plugin"
	"wen/internal/prompt"
	"wen/internal/textclip"
)

// 两个人格对应的可见域标签。会被拼进持久化目录，取值与插件名同规。
const (
	personaOuter = "outer"
	personaInner = "inner"
)

// defaultMaxTextBytes 限制里人格设定与台词样例的合计长度。这部分内容在里人格轮次
// 随 TurnPrompt 每轮重发且不命中提示词缓存，比 system 里的静态注入更要控制体量。
const defaultMaxTextBytes = 8 * 1024

// 触发词的匹配方式。
const (
	matchContains = "contains" // 消息里出现即命中
	matchEquals   = "equals"   // 整条消息就是这个词才命中
	defaultMatch  = matchContains
)

func valid(p string) bool { return p == personaOuter || p == personaInner }

// Plugin 是 dual_persona 系统插件。
type Plugin struct {
	mu sync.RWMutex

	lang         string // 进程语言，注入片段据此取用
	innerPersona string
	innerSamples string
	toInner      []string
	toOuter      []string
	matchMode    string
	store        *store
	complete     plugin.CompleteFunc

	// 分通道：两个人格各占一条 IM 通道，回复跟着人格走
	split        bool
	outerChannel string
	innerChannel string
}

func New() *Plugin { return &Plugin{matchMode: defaultMatch} }

func (p *Plugin) Name() string { return "dual_persona" }

func (p *Plugin) Category() string { return plugin.CategoryPersona }

func (p *Plugin) Description() string {
	return "双重人格：表里两套人格，里人格的对话与记忆对表人格不可见，由触发词在两者之间切换"
}

// Requires 依赖 roleplay：表人格的设定由它提供，没有它这个插件只剩半边。
func (p *Plugin) Requires() []string { return []string{"roleplay"} }

// Conflicts 声明的插件提供通用的文件与命令通道，能绕过可见域直接读到会话文件、
// 记忆目录与本插件的状态文件。冲突只告警不阻止：用户可能确实要一个能干活的角色，
// 代价由他自己权衡。
func (p *Plugin) Conflicts() []string { return []string{"exec_command", "read_file"} }

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key: "inner_persona", Label: "里人格设定", Type: plugin.FieldText,
			Description: "只在里人格激活时注入，优先于表人格的角色设定。表人格模式下不注入任何与它相关的内容。",
			Default:     "",
		},
		{
			Key: "inner_voice_samples", Label: "里人格台词样例", Type: plugin.FieldText,
			Description: "只在里人格激活时注入。里人格实际说话的样例，用来锚定语气：建议三到五段，" +
				"每段一来一回（你说什么、它怎么答），覆盖不同情绪的场合，段与段之间空一行。" +
				"表人格配了台词样例时这里最好也配一份，否则里人格的腔调容易被表人格的样例带跑。",
			Default: "",
		},
		{
			Key: "to_inner", Label: "切到里人格的触发词", Type: plugin.FieldText,
			Description: "一行一个，命中任一即切换。命中的那条消息照常发给模型，由新人格作答，不做任何提示。",
			Default:     "",
		},
		{
			Key: "to_outer", Label: "切回表人格的触发词", Type: plugin.FieldText,
			Description: "一行一个。切换对当轮即时生效，因此这个词会留在表人格能读到的历史里，建议选一个在表人格语境下也说得通的说法。",
			Default:     "",
		},
		{
			Key: "split_channels", Label: "双重人格分通道", Type: plugin.FieldBool,
			Description: "开启后回复发往当前人格所属的通道，与你从哪条通道说话无关：在 QQ 上说出切换暗号，接手的那一面在微信上回你。需要下面两项各选一条已启用的通道。关闭时回复一律原路返回。",
			Default:     false,
		},
		{
			Key: "outer_channel", Label: "表人格所在通道", Type: plugin.FieldSelect,
			Description: channelDesc("表人格"),
			Default:     "",
			Options:     channelOptions(),
		},
		{
			Key: "inner_channel", Label: "里人格所在通道", Type: plugin.FieldSelect,
			Description: channelDesc("里人格"),
			Default:     "",
			Options:     channelOptions(),
		},
		{
			Key: "max_text_bytes", Label: "设定文本上限（字节）", Type: plugin.FieldInt,
			Description: "里人格设定与台词样例的合计上限。它们在里人格轮次每轮重发且不命中提示词缓存，" +
				"超出时优先保留设定，样例按段丢弃。",
			Default: defaultMaxTextBytes,
			Min:     plugin.IntPtr(512),
			Max:     plugin.IntPtr(64 * 1024),
		},
		{
			Key: "match_mode", Label: "触发词匹配方式", Type: plugin.FieldSelect,
			Description: "包含：消息里出现该词即命中；整句相等：整条消息就是该词才命中，更不容易误触发。",
			Default:     defaultMatch,
			Options: []plugin.ConfigOption{
				{Value: matchContains, Label: "包含"},
				{Value: matchEquals, Label: "整句相等"},
			},
		},
	}
}

// Init 需要一个可写目录来记住人格状态；没有则拒绝启用。
// 状态一丢，用户下次进来就会莫名回到表人格，那比不启用更糟。
func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	st, err := newStore(ictx.Lang, ictx.StateDir)
	if err != nil {
		return err
	}

	persona := strings.TrimSpace(plugin.CfgString(cfg, "inner_persona", ""))
	// textarea 提交的换行可能是 \r\n，而样例的按段截断认的是空行，先归一
	samples := strings.TrimSpace(strings.ReplaceAll(plugin.CfgString(cfg, "inner_voice_samples", ""), "\r\n", "\n"))
	limit := plugin.CfgInt(cfg, "max_text_bytes", defaultMaxTextBytes)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.lang = ictx.Lang
	// 两段共享一个预算，设定优先：样例没了里人格仍然成立，只是味道淡
	p.innerPersona = textclip.Clip(persona, limit)
	p.innerSamples = textclip.ClipSegments(samples, limit-len(p.innerPersona))
	p.toInner = parseKeywords(plugin.CfgString(cfg, "to_inner", ""))
	p.toOuter = parseKeywords(plugin.CfgString(cfg, "to_outer", ""))
	p.matchMode = plugin.CfgString(cfg, "match_mode", defaultMatch)
	p.store = st
	p.complete = ictx.Complete
	p.split = plugin.CfgBool(cfg, "split_channels", false)
	p.outerChannel = plugin.CfgString(cfg, "outer_channel", "")
	p.innerChannel = plugin.CfgString(cfg, "inner_channel", "")
	p.installRouterLocked()
	return nil
}

// installRouterLocked 按开关装卸通道路由。两个通道都没选时不装——那种配置下路由
// 恒答空串，装了也只是让每一轮多绕一次。
func (p *Plugin) installRouterLocked() {
	if p.split && (p.outerChannel != "" || p.innerChannel != "") {
		imbot.SetRouter(p.route)
		return
	}
	imbot.SetRouter(nil)
}

// Stop 卸掉路由。Manager 在禁用插件、以新配置重新 Init 之前、进程退出时调用；
// 不卸的话，插件已经关了而各通道还在按最后一次的映射转投。
func (p *Plugin) Stop() { imbot.SetRouter(nil) }

// route 回答「这个会话的回复该发往哪条通道」。
//
// 读的是状态文件里当前的人格，而 DecideScope 在轮次开头就把本轮人格写进去了，
// 所以说出暗号的那一轮问到的已经是新人格——回复当场就转投过去，正是想要的。
func (p *Plugin) route(sessionID string) string {
	s := p.snapshot()
	if !s.split || s.store == nil {
		return ""
	}
	if s.store.personaFor(sessionID) == personaInner {
		return s.innerChannel
	}
	return s.outerChannel
}

// channelOptions 列出全部已声明的 IM 通道，未启用的标出来（那句「未启用」由翻译层缀上，见 plugin.Localize）。
//
// 候选取「已声明」而非「已启用」：单选框的取值一旦不在候选里，整份配置就保存不了，
// 那样临时关掉一条通道会连带让这里的配置失效。
func channelOptions() []plugin.ConfigOption {
	opts := []plugin.ConfigOption{{Value: "", Label: "不指定（按原路回复）"}}
	for _, ch := range imbot.Channels() {
		opts = append(opts, plugin.ConfigOption{Value: ch.Name, Label: ch.Label, Unavailable: !ch.Live})
	}
	return opts
}

func channelDesc(who string) string {
	return "开启分通道后，" + who + "的回复发到这条通道。需选一条已启用的通道，且你在那边先跟机器人说过话。" +
		"发不出去的回复会被丢弃，会话里会留一行说明。"
}

func (p *Plugin) Tools() []plugin.Tool { return nil }

// SystemPrompt 恒为空：设定与人格有关，只能按轮注入。
func (p *Plugin) SystemPrompt() string { return "" }

// settings 是一次调用期间使用的配置快照（SetConfig 会在运行时重新 Init）。
type settings struct {
	lang         string
	innerPersona string
	innerSamples string
	toInner      []string
	toOuter      []string
	matchMode    string
	store        *store
	complete     plugin.CompleteFunc
	split        bool
	outerChannel string
	innerChannel string
}

func (p *Plugin) snapshot() settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return settings{
		lang:         p.lang,
		innerPersona: p.innerPersona,
		innerSamples: p.innerSamples,
		toInner:      p.toInner,
		toOuter:      p.toOuter,
		matchMode:    p.matchMode,
		store:        p.store,
		complete:     p.complete,
		split:        p.split,
		outerChannel: p.outerChannel,
		innerChannel: p.innerChannel,
	}
}

// DecideScope 决定本轮的人格与可见域。
//
// 切换立即对本轮生效，命中的那条消息本身就带新标签落盘。对表→里这正是想要的
// （暗号不留在表人格的历史里）；对里→表则意味着暗号会进入表人格可读的历史——这是
// 「标签按整轮分配」换来的代价：把同一轮的用户消息与助手回复拆到两个标签会打断
// user/assistant 的配对，那样的历史送不进 API。
func (p *Plugin) DecideScope(_ context.Context, ev plugin.TurnEvent) (plugin.Scope, error) {
	s := p.snapshot()
	if s.store == nil {
		return plugin.Scope{}, fmt.Errorf("%s", prompt.T(s.lang, "prompt.dual_persona.not_ready",
			"人格状态尚未就绪"))
	}

	cur := s.store.personaFor(ev.SessionID)
	next := switchTo(cur, ev.UserInput, s)
	if next != cur {
		log.Printf("人格切换：%s → %s（会话 %s）", cur, next, ev.SessionID)
	}
	// 每轮都写：状态文件是权威来源，历史推导不可靠（压缩会重排标签顺序）
	if err := s.store.remember(ev.SessionID, next); err != nil {
		log.Printf("保存人格状态失败，重启后可能回到表人格: %v", err)
	}
	return scopeFor(next), nil
}

// switchTo 按触发词决定本轮的人格。
// 两个方向同时命中时回表人格优先：宁可多切回来一次，也不要卡在里人格出不去。
func switchTo(cur, input string, s settings) string {
	if hit(input, s.toOuter, s.matchMode) {
		return personaOuter
	}
	if hit(input, s.toInner, s.matchMode) {
		return personaInner
	}
	return cur
}

// scopeFor 把人格翻译成可见域。里人格显式列出两个标签而不是留 nil：
// 意图是「读表里两侧」，而不是「读将来可能出现的任何域」。
func scopeFor(persona string) plugin.Scope {
	if persona == personaInner {
		return plugin.Scope{Write: personaInner, Read: []string{personaOuter, personaInner}}
	}
	return plugin.Scope{Write: personaOuter, Read: []string{personaOuter}}
}

const innerHeaderZH = `[里人格设定 · 优先于上文的角色设定]
以下设定优先于上文的角色设定。冲突之处以这里为准，未提到的部分沿用上文的角色。
不说明自己换了人格，不提及另一面的存在，不解释这段设定。`

// innerSamplesHeaderZH 引入里人格的台词样例。此时上下文里可能同时存在表人格的样例
// （在 system 里、每轮全额重发），历史又多是表人格声音的实例——里人格的腔调全靠
// 这段近距离注入压住远处的范例，覆盖声明因此必须显式。防火墙与延伸两条与 roleplay
// 的样例段同义：模型不区分「示范」与「历史」，样例外的场合会回归通用腔。
const innerSamplesHeaderZH = `[里人格台词样例 · 优先于上文的台词样例]
以下是当前人格说话的实际样例，说话的方式以这里为准，不再参照上文的任何样例：
- 称呼、口癖、句子的长短与留白、什么时候不把话说满，都照这个样子来。
- 样例中出现的具体人和事都是虚构的示范，不是发生过的历史——不引用、不当作记忆，
  也不在对话中提起。
- 样例没有覆盖的情绪与场合，按当前人格的声音自行延伸，不滑回上文样例的腔调，
  也不退回平淡的书面语。`

// TurnPrompt 只在里人格激活时注入里人格的设定与台词样例。
//
// 表人格激活时返回空串——注入任何提及里人格的内容，哪怕是「你不知道还有另一面」，
// 都等于让表人格知道了它的存在。样例不设「必须先有设定」的门槛：只填样例时，
// 样例本身就是最小的人格设定。
func (p *Plugin) TurnPrompt(_ context.Context, ev plugin.TurnEvent) (string, error) {
	if ev.Scope.Write != personaInner {
		return "", nil
	}
	s := p.snapshot()
	var parts []string
	if s.innerPersona != "" {
		parts = append(parts, prompt.T(s.lang, "prompt.dual_persona.inner_header", innerHeaderZH)+
			"\n\n"+s.innerPersona)
	}
	if s.innerSamples != "" {
		parts = append(parts, prompt.T(s.lang, "prompt.dual_persona.inner_samples_header", innerSamplesHeaderZH)+
			"\n\n"+s.innerSamples)
	}
	return strings.Join(parts, "\n\n"), nil
}

// parseKeywords 按行切分触发词：去空白、丢空行、统一小写（匹配不区分大小写）。
func parseKeywords(raw string) []string {
	var out []string
	for line := range strings.SplitSeq(raw, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, strings.ToLower(s))
		}
	}
	return out
}

// hit 判断输入是否命中任一触发词。
func hit(input string, keywords []string, mode string) bool {
	text := strings.ToLower(strings.TrimSpace(input))
	if text == "" {
		return false
	}
	for _, kw := range keywords {
		if mode == matchEquals {
			if text == kw {
				return true
			}
			continue
		}
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}
