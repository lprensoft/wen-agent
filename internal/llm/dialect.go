package llm

import "wen/internal/i18n"

import "slices"

// 思考参数方言：OpenAI 兼容协议只统一了 messages/tools/stream 这一层，
// 思考（推理）相关参数是各家的私有扩展，互不兼容，按提供商配置。
//
// 方言按**厂商**而不是按线上格式划分，因为两家即便写法一样，接受的档位也未必一样：
// 智谱与 DeepSeek 都是 thinking + reasoning_effort，但前者只认 low/high/max，
// 后者五档全收。把档位并进方言，界面上的六档才有地方收敛（见 snapEffort）。
const (
	DialectDeepSeek = "deepseek" // thinking:{enabled|disabled} + reasoning_effort（默认）
	DialectGLM      = "glm"      // 同上，但只认 low/high/max
	DialectMiniMax  = "minimax"  // thinking:{adaptive} + reasoning_split，无档位、关不掉
	DialectQwen     = "qwen"     // enable_thinking 布尔开关
	DialectKimi     = "kimi"     // 仅 reasoning_effort，只认 low/high/max；关闭落到 low
	DialectOpenAI   = "openai"   // 仅 reasoning_effort；拒绝采样参数，且要 max_completion_tokens
	DialectGemini   = "gemini"   // 仅 reasoning_effort，只认 low/medium/high
	DialectXAI      = "xai"      // 仅 reasoning_effort，只认 low/medium/high/xhigh
	DialectEffort   = "effort"   // 仅 reasoning_effort，六档原样透传
	DialectNone     = "none"     // 不发送任何思考参数
)

// 线上写法。多个方言可以共用同一种写法，差别只在接受的档位。
type thinkShape int

const (
	shapeThinkingEffort thinkShape = iota // thinking:{enabled|disabled} + reasoning_effort
	shapeAdaptive                         // thinking:{adaptive} + reasoning_split，不发 disabled
	shapeEnableThinking                   // enable_thinking 布尔
	shapeEffortOnly                       // 仅 reasoning_effort
	shapeNothing                          // 什么都不发
)

// dialectSpec 是一门方言的全部差异：怎么写、接受哪几档、要不要避开某些通用字段。
type dialectSpec struct {
	shape thinkShape
	// levels 是对方接受的 reasoning_effort 取值，从低到高；
	// nil 表示这门方言不带档位（只有开关，或什么都不发）。
	levels []string
	// offLevel 非空时，「关闭思考」表达成这一档而不是「不发 effort」。
	offLevel string
	// noSampling 为真时任何情况下都不发 temperature —— OpenAI 系推理模型会拒绝它。
	noSampling bool
	// maxCompletionTokens 为真时用 max_completion_tokens 而不是 max_tokens。
	// GPT-5 起的模型在 /chat/completions 上明确拒绝后者。
	maxCompletionTokens bool
}

var dialectSpecs = map[string]dialectSpec{
	DialectDeepSeek: {shape: shapeThinkingEffort, levels: []string{"low", "medium", "high", "xhigh", "max"}},
	DialectGLM:      {shape: shapeThinkingEffort, levels: []string{"low", "high", "max"}},
	// MiniMax 与 Kimi 都已把 max_tokens 标为废弃，改用 max_completion_tokens
	DialectMiniMax: {shape: shapeAdaptive, maxCompletionTokens: true},
	DialectQwen:    {shape: shapeEnableThinking},
	// Kimi K3 的思考关不掉，「关闭」落到最低一档 low；它的采样参数是固定值，
	// 官方要求不发（temperature 等）
	DialectKimi: {shape: shapeEffortOnly, levels: []string{"low", "high", "max"},
		offLevel: "low", noSampling: true, maxCompletionTokens: true},
	DialectOpenAI: {shape: shapeEffortOnly, levels: []string{"low", "medium", "high", "xhigh", "max"},
		offLevel: "none", noSampling: true, maxCompletionTokens: true},
	DialectGemini: {shape: shapeEffortOnly, levels: []string{"low", "medium", "high"}},
	DialectXAI:    {shape: shapeEffortOnly, levels: []string{"low", "medium", "high", "xhigh"}},
	DialectEffort: {shape: shapeEffortOnly, levels: []string{"low", "medium", "high", "xhigh", "max"}},
	DialectNone:   {shape: shapeNothing},
}

// KnownDialects 是支持的思考参数方言（配置校验与界面下拉共用）。
// 顺序即界面下拉的顺序，故不是从 map 里取。
var KnownDialects = []string{
	DialectDeepSeek, DialectGLM, DialectMiniMax, DialectQwen, DialectKimi,
	DialectOpenAI, DialectGemini, DialectXAI, DialectEffort, DialectNone,
}

// specOf 取方言定义；空串等同 DeepSeek（兼容既有配置）。
func specOf(d string) dialectSpec {
	if d == "" {
		return dialectSpecs[DialectDeepSeek]
	}
	return dialectSpecs[d]
}

// DialectLabel 返回方言的显示名。它进设置页的下拉框，按请求语言取。
func DialectLabel(lang, d string) string {
	switch d {
	case DialectDeepSeek:
		return i18n.T(lang, "llm.dialect.deepseek", "DeepSeek（thinking + reasoning_effort）")
	case DialectGLM:
		return i18n.T(lang, "llm.dialect.glm", "智谱 GLM（thinking + effort，只认 low/high/max）")
	case DialectMiniMax:
		return i18n.T(lang, "llm.dialect.minimax", "MiniMax（adaptive + reasoning_split）")
	case DialectQwen:
		return i18n.T(lang, "llm.dialect.qwen", "Qwen（enable_thinking）")
	case DialectKimi:
		return i18n.T(lang, "llm.dialect.kimi", "Kimi（reasoning_effort，只认 low/high/max）")
	case DialectOpenAI:
		return i18n.T(lang, "llm.dialect.openai", "OpenAI（reasoning_effort，不发采样参数）")
	case DialectGemini:
		return i18n.T(lang, "llm.dialect.gemini", "Gemini（reasoning_effort，只认 low/medium/high）")
	case DialectXAI:
		return i18n.T(lang, "llm.dialect.xai", "xAI Grok（reasoning_effort，无 max 档）")
	case DialectEffort:
		return i18n.T(lang, "llm.dialect.effort", "通用 reasoning_effort（六档原样发出）")
	case DialectNone:
		return i18n.T(lang, "llm.dialect.none", "不发送思考参数")
	}
	return d
}

// IsKnownDialect 校验方言取值；空串合法（等同 DeepSeek，兼容既有配置）。
func IsKnownDialect(d string) bool {
	return d == "" || slices.Contains(KnownDialects, d)
}

// effortOrder 是界面上那六档由低到高的次序，snapEffort 按它算远近。
var effortOrder = []string{"off", "low", "medium", "high", "xhigh", "max"}

// snapEffort 把用户选的档位收敛到对方真正接受的一档。
//
// 规则是**向上取整**：取不低于所选档位的最低一档，没有更高的就取最高档。
// 选了 medium 的人要的是「比 low 多想一点」，给他 low 是给少了；而各家的档位
// 本来就疏密不一，宁可多想一点也不要少想。DeepSeek 服务端把 medium 归并成
// high，与这条一致。
//
// 这个函数存在的理由是：界面上那六档取自 Anthropic，别家没一家全收——
// 智谱与 Kimi 只认 low/high/max，Gemini 只认 low/medium/high。原样发出去的
// 结果是一个 400，而错误文案来自对方服务器，用户多半看不懂。
func snapEffort(levels []string, want string) string {
	if len(levels) == 0 {
		return ""
	}
	if slices.Contains(levels, want) {
		return want
	}
	wantIdx := slices.Index(effortOrder, want)
	if wantIdx < 0 {
		return levels[len(levels)-1]
	}
	for _, lv := range levels {
		if slices.Index(effortOrder, lv) >= wantIdx {
			return lv
		}
	}
	return levels[len(levels)-1]
}

// ---------- <think> 标签剥离 ----------

// thinkFilter 把内联在正文里的 <think>…</think> 思考内容从文本增量流中剥出来。
// MiniMax 等提供商在不带 reasoning_split 或历史模型上会把思考混进 content，
// 这里做兜底，即使方言配置不准也不至于让思考泄漏到正文。
// 标签可能被流式切在任意位置，末尾可能构成半个标签的部分先暂存不吐。
type thinkFilter struct {
	inThink bool
	pend    string
}

const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// feed 消化一段增量，返回应作为正文与思考分别下发的部分。
func (f *thinkFilter) feed(chunk string) (content, reasoning string) {
	buf := f.pend + chunk
	f.pend = ""
	var out, think []byte
	for buf != "" {
		tag := thinkOpen
		if f.inThink {
			tag = thinkClose
		}
		if i := indexOf(buf, tag); i >= 0 {
			if f.inThink {
				think = append(think, buf[:i]...)
			} else {
				out = append(out, buf[:i]...)
			}
			f.inThink = !f.inThink
			buf = buf[i+len(tag):]
			continue
		}
		keep := tagPrefixSuffixLen(buf, tag)
		emit := buf[:len(buf)-keep]
		if f.inThink {
			think = append(think, emit...)
		} else {
			out = append(out, emit...)
		}
		f.pend = buf[len(buf)-keep:]
		break
	}
	return string(out), string(think)
}

// flush 在流结束时把暂存的尾巴按当前状态吐出（半个标签按原文处理）。
func (f *thinkFilter) flush() (content, reasoning string) {
	p := f.pend
	f.pend = ""
	if p == "" {
		return "", ""
	}
	if f.inThink {
		return "", p
	}
	return p, ""
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// tagPrefixSuffixLen 返回 s 的末尾与 tag 的开头能重叠的最大长度（不含完整 tag）。
func tagPrefixSuffixLen(s, tag string) int {
	maxK := len(tag) - 1
	if len(s) < maxK {
		maxK = len(s)
	}
	for k := maxK; k > 0; k-- {
		if s[len(s)-k:] == tag[:k] {
			return k
		}
	}
	return 0
}
