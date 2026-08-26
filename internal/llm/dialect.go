package llm

import "wen/internal/i18n"

import "slices"

// 思考参数方言：OpenAI 兼容协议只统一了 messages/tools/stream 这一层，
// 思考（推理）相关参数是各家的私有扩展，互不兼容，按提供商配置。
const (
	DialectDeepSeek = "deepseek" // thinking:{enabled|disabled} + reasoning_effort（默认）
	DialectMiniMax  = "minimax"  // thinking:{adaptive|disabled} + reasoning_split，无 effort
	DialectQwen     = "qwen"     // enable_thinking 布尔开关
	DialectEffort   = "effort"   // 仅 reasoning_effort（OpenAI / Kimi 风格）
	DialectNone     = "none"     // 不发送任何思考参数
)

// KnownDialects 是支持的思考参数方言（配置校验与界面下拉共用）。
var KnownDialects = []string{DialectDeepSeek, DialectMiniMax, DialectQwen, DialectEffort, DialectNone}

// DialectLabel 返回方言的中文显示名。
func DialectLabel(lang, d string) string {
	switch d {
	case DialectDeepSeek:
		return i18n.T(lang, "llm.dialect.deepseek", "DeepSeek（thinking + reasoning_effort）")
	case DialectMiniMax:
		return i18n.T(lang, "llm.dialect.minimax", "MiniMax（adaptive + reasoning_split）")
	case DialectQwen:
		return i18n.T(lang, "llm.dialect.qwen", "Qwen（enable_thinking）")
	case DialectEffort:
		return i18n.T(lang, "llm.dialect.effort", "OpenAI / Kimi（reasoning_effort）")
	case DialectNone:
		return i18n.T(lang, "llm.dialect.none", "不发送思考参数")
	}
	return d
}

// IsKnownDialect 校验方言取值；空串合法（等同 DeepSeek，兼容既有配置）。
func IsKnownDialect(d string) bool {
	return d == "" || slices.Contains(KnownDialects, d)
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
