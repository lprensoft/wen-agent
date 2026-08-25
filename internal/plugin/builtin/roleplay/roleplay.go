// Package roleplay 提供角色扮演的系统插件：把用户配置的角色设定、台词样例与用户自己的
// 信息作为最高优先级提示词注入，附一套让中文表达脱离机械感的规则、一套以【】做场景与
// 动作演绎的规则，以及时间一致性约束。
//
// 注意界面与提示词的人称是相反的：设置页上那一项叫「我的信息」（填的人是用户自己），
// 而注入给模型的段落写作「对方信息」——提示词是模型以第一人称读的，那里的「我」指它。
// 两处不要统一。
//
// 本插件不提供工具，也不落盘。它硬依赖 memory 与 session_search：一个有连续性的
// 角色需要记得住跨会话的事、查得到之前说过的原话，缺了这两样，角色每开一个新会话
// 就重新变成陌生人。
package roleplay

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

const (
	defaultInteraction = true
	defaultHumanize    = true
	defaultTimeRules   = true
	defaultMemoryRules = true
	defaultBoundaries  = true
	// defaultMaxTextBytes 限制角色设定与用户信息的合计长度。这些内容每轮随 system
	// 消息全额重发、且不参与预算裁剪，还计入自动压缩判据，必须有硬上限。
	defaultMaxTextBytes = 8 * 1024
)

// Plugin 是 roleplay 系统插件。
type Plugin struct {
	mu sync.RWMutex

	lang              string
	persona           string
	userProfile       string
	voiceSamples      string
	interaction       bool
	humanize          bool
	timeRules         bool
	memoryRules       bool
	boundaries        bool
	maxTextBytes      int
	translateFailures bool
	failureLine       string
	complete          plugin.CompleteFunc
}

func New() *Plugin {
	return &Plugin{
		interaction:       defaultInteraction,
		humanize:          defaultHumanize,
		timeRules:         defaultTimeRules,
		memoryRules:       defaultMemoryRules,
		boundaries:        defaultBoundaries,
		maxTextBytes:      defaultMaxTextBytes,
		translateFailures: defaultTranslateFailures,
		failureLine:       defaultFailureLine,
	}
}

func (p *Plugin) Name() string { return "roleplay" }

func (p *Plugin) Category() string { return plugin.CategoryPersona }
func (p *Plugin) Description() string {
	return "按设定扮演角色：注入角色设定、台词样例与我的信息，以【】演绎场景动作，约束表达方式与时间一致性"
}

// Requires 硬依赖记忆与会话检索：角色的连续性建立在「记得住」与「查得到」之上。
func (p *Plugin) Requires() []string { return []string{"memory", "session_search"} }

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key: "persona", Label: "角色设定", Type: plugin.FieldText,
			Description: "扮演谁、什么性格、什么处境、怎么说话。作为最高优先级提示词每轮注入。留空则不注入设定，只保留表达规则。",
			Default:     defaultPersona,
		},
		{
			// 标签用「我的信息」：填的人就是你自己。注入给模型的那一段仍写作
			// 「对方信息」——提示词是模型以第一人称读的，那里的「我」指它自己。
			Key: "user_profile", Label: "我的信息", Type: plugin.FieldText,
			Description: "你的基本情况，作为角色一开始就知道的信息。可以留空，改在对话中告诉它，由记忆插件记下来。",
			Default:     "",
		},
		{
			Key: "voice_samples", Label: "台词样例", Type: plugin.FieldText,
			Description: "角色实际说话的样例，用来锚定语气——比性格描述管用得多。建议三到五段，" +
				"每段一来一回（你说什么、角色怎么答），段与段之间空一行；覆盖不同情绪的场合" +
				"（高兴时、被烦到时、认真说事时），开了【】演绎就在样例里也带上【】。" +
				"样例里的人和事只是示范，不会被角色当成真实经历。",
			Default: defaultVoiceSamples,
		},
		{
			Key: "interaction", Label: "启用【】互动演绎", Type: plugin.FieldBool,
			Description: "每次回复先以【】写场景、动作与表情，再说话；一轮中可多次出现。",
			Default:     defaultInteraction,
		},
		{
			Key: "boundaries", Label: "启用边界表达", Type: plugin.FieldBool,
			Description: "角色可以以自己的身份表达不愿意：轻则带开话头，再则说出感受请求停顿，" +
				"极限处直接拒绝——全部第一人称、从感受出发。边界划在哪里由角色设定与演绎决定。",
			Default: defaultBoundaries,
		},
		{
			Key: "humanize", Label: "启用自然表达规则", Type: plugin.FieldBool,
			Description: "抑制「不是…而是…」、先总后分再总结、过渡套话、结尾客套等让中文显得机械的表达。",
			Default:     defaultHumanize,
		},
		{
			Key: "time_rules", Label: "启用时间一致性约束", Type: plugin.FieldBool,
			Description: "说到时间时以当前真实时间为准，不沿用历史里出现过的时刻；运行环境（系统、目录等）不进入角色的自我认知。",
			Default:     defaultTimeRules,
		},
		{
			Key: "memory_rules", Label: "启用记忆与回想约束", Type: plugin.FieldBool,
			Description: "让角色像人一样记：生活琐事会随时间淡忘，说法变了就更新旧记忆；" +
				"回想时只说要点，不逐字复述历史原文，记不清就承认记不清。",
			Default: defaultMemoryRules,
		},
		{
			Key: "translate_failures", Label: "失败转译", Type: plugin.FieldBool,
			Description: "模型调用失败（如内容被提供商安全策略拦截）时，以角色口吻转成一句走神般的回复，" +
				"原始错误转入会话注记；配置类错误（密钥、地址）仍原样报出。需要已填写角色设定。",
			Default: defaultTranslateFailures,
		},
		{
			Key: "failure_line", Label: "转译兜底台词", Type: plugin.FieldString,
			Description: "转译时连生成一句台词也失败（如提供商完全不可用）的兜底回复。",
			Default:     defaultFailureLine,
		},
		{
			Key: "max_text_bytes", Label: "设定文本上限（字节）", Type: plugin.FieldInt,
			Description: "角色设定、我的信息与台词样例的合计上限。它们每轮都完整发给模型，" +
				"超出时按 设定 > 我的信息 > 样例 的顺序保留。",
			Default: defaultMaxTextBytes,
			Min:     plugin.IntPtr(512),
			Max:     plugin.IntPtr(64 * 1024),
		},
	}
}

func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	persona := strings.TrimSpace(plugin.CfgString(cfg, "persona", ""))
	profile := strings.TrimSpace(plugin.CfgString(cfg, "user_profile", ""))
	// textarea 提交的换行可能是 \r\n，而样例的按段截断认的是空行，先归一
	samples := strings.TrimSpace(strings.ReplaceAll(plugin.CfgString(cfg, "voice_samples", ""), "\r\n", "\n"))
	limit := plugin.CfgInt(cfg, "max_text_bytes", defaultMaxTextBytes)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.lang = ictx.Lang
	// 三段共享一个预算，截断顺序 设定 > 我的信息 > 样例：设定是这个插件存在的
	// 理由，样例没了角色仍然成立，只是味道淡
	p.persona, p.userProfile, p.voiceSamples = clipTriple(persona, profile, samples, limit)
	p.interaction = plugin.CfgBool(cfg, "interaction", defaultInteraction)
	p.humanize = plugin.CfgBool(cfg, "humanize", defaultHumanize)
	p.timeRules = plugin.CfgBool(cfg, "time_rules", defaultTimeRules)
	p.memoryRules = plugin.CfgBool(cfg, "memory_rules", defaultMemoryRules)
	p.boundaries = plugin.CfgBool(cfg, "boundaries", defaultBoundaries)
	p.maxTextBytes = limit
	p.translateFailures = plugin.CfgBool(cfg, "translate_failures", defaultTranslateFailures)
	p.failureLine = strings.TrimSpace(plugin.CfgString(cfg, "failure_line", defaultFailureLine))
	p.complete = ictx.Complete
	return nil
}

func (p *Plugin) Tools() []plugin.Tool { return nil }

// settings 是一次调用期间使用的配置快照。
type settings struct {
	lang              string
	persona           string
	userProfile       string
	voiceSamples      string
	interaction       bool
	humanize          bool
	timeRules         bool
	memoryRules       bool
	boundaries        bool
	translateFailures bool
	failureLine       string
	complete          plugin.CompleteFunc
}

// snapshot 取一份配置快照：SetConfig 会在运行时重新 Init，而提示词可能正在生成。
func (p *Plugin) snapshot() settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return settings{
		lang:              p.lang,
		persona:           p.persona,
		userProfile:       p.userProfile,
		voiceSamples:      p.voiceSamples,
		interaction:       p.interaction,
		humanize:          p.humanize,
		timeRules:         p.timeRules,
		memoryRules:       p.memoryRules,
		boundaries:        p.boundaries,
		translateFailures: p.translateFailures,
		failureLine:       p.failureLine,
		complete:          p.complete,
	}
}

// SystemPrompt 拼装各段设定与规则。全部内容都是静态的——设定来自配置、规则是常量、
// 当前时间由核心的本轮状态块每轮提供——因此不需要按轮生成。
func (p *Plugin) SystemPrompt() string {
	s := p.snapshot()
	var parts []string
	if s.persona != "" {
		parts = append(parts, prompt.T(s.lang, "prompt.roleplay.persona_header", personaHeaderZH)+"\n\n"+s.persona)
	}
	// 紧跟角色设定：样例是「这个角色是谁」的延伸。不设 persona 非空的门槛——
	// 只填样例不填设定时，样例本身就是最小的角色设定
	if s.voiceSamples != "" {
		parts = append(parts, prompt.T(s.lang, "prompt.roleplay.samples_header", samplesHeaderZH)+"\n\n"+s.voiceSamples)
	}
	if s.userProfile != "" {
		parts = append(parts, prompt.T(s.lang, "prompt.roleplay.user_header", userHeaderZH)+"\n\n"+s.userProfile)
	}
	if s.interaction {
		parts = append(parts, prompt.T(s.lang, "prompt.roleplay.interaction", interactionRulesZH))
		// 身高规则依附于动作描写，不做演绎时它没有作用对象
		if s.persona != "" && s.userProfile != "" {
			parts = append(parts, prompt.T(s.lang, "prompt.roleplay.height", heightRulesZH))
		}
	}
	if s.boundaries {
		parts = append(parts, prompt.T(s.lang, "prompt.roleplay.boundary", boundaryRulesZH))
	}
	if s.humanize {
		parts = append(parts, prompt.T(s.lang, "prompt.roleplay.humanize", humanizeRulesZH))
	}
	if s.timeRules {
		parts = append(parts, prompt.T(s.lang, "prompt.roleplay.time", timeRulesZH))
	}
	if s.memoryRules {
		parts = append(parts, prompt.T(s.lang, "prompt.roleplay.memory", memoryRulesZH))
	}
	return strings.Join(parts, "\n\n")
}

// hasCharacter 判断是否配置了一个可辨认的角色：设定或台词样例任一非空。
// 只开了表达规则而没有角色时，摘要要求与语气样本都没有对象。
func (s settings) hasCharacter() bool {
	return s.persona != "" || s.voiceSamples != ""
}

// CompactPrompt 给压缩摘要追加角色扮演的要求。核心的基础要求把「寒暄客套」列为
// 丢弃项，而关系的变化恰恰藏在那些看似寒暄的来回里——按任务型的摘要压一次，
// 角色对对方的态度、两人之间悬着的事、彼此的叫法就都没了，之后的角色只剩设定。
func (p *Plugin) CompactPrompt(context.Context) string {
	if !p.snapshot().hasCharacter() {
		return ""
	}
	return prompt.T(p.snapshot().lang, "prompt.roleplay.compact", compactRequirementsZH)
}

// OnCompact 在历史被物理删除前抽出两样摘要保不住的东西，作为注记留在摘要末尾：
// 最后一处【】原文（场景与姿态）和角色最近的几句原话（声音）。
//
// 压缩会把整段历史换成一份摘要，而摘要是对内容的概述，场景与语气这类细节正是它会
// 丢掉的部分——压缩一次场景就断了、腔调就散了。台词样例是静态示范，近期原话才是
// 「这几天她是这么说话的」；压缩刚过时历史最薄，正是最需要这份范本的时刻。
// 这里只做文本提取，不发起模型调用。
func (p *Plugin) OnCompact(_ context.Context, ev plugin.CompactEvent) (string, error) {
	s := p.snapshot()
	var notes []string
	if s.interaction {
		if scene := lastScene(ev.History); scene != "" {
			notes = append(notes, fmt.Sprintf("（压缩前的最后一处场景演绎，后续对话应从此处继续：%s）", scene))
		}
	}
	if s.hasCharacter() {
		if lines := recentVoice(ev.History); len(lines) > 0 {
			notes = append(notes, "（压缩前角色最近的几句原话，之后保持同一个声音："+joinQuoted(lines)+"）")
		}
	}
	return strings.Join(notes, "\n"), nil
}
