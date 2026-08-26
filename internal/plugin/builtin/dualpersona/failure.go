package dualpersona

import (
	"context"
	"fmt"
	"strings"
	"time"

	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

// 失败转译的里人格侧：里人格轮次的走神台词按里人格设定生成，口吻才对得上。
// 表人格轮次一律不接手，落回 roleplay——转译按逆注册序征询，本插件排在 roleplay
// 之后，正好先于它拿到里人格的轮次。
//
// 提示词与长度规整跟 roleplay 的实现同形状但各写各的：本插件按约定不引用
// roleplay 的任何类型（见包注释），一段小常量不值得为此破例。

const (
	failureCompleteTimeout = 20 * time.Second
	failureLineMaxRunes    = 120
)

const failurePromptZH = `下面是一段角色设定：

%s

你正以这个角色的身份与人对话。刚才你想说的话没能说出口。请以这个角色的口吻写一句简短自然的话，表示自己刚才走神、愣了一下或没接上话，并轻轻把话头接回来。
要求：只输出这句话本身，不加引号与任何解释；不出现「系统」「模型」「错误」这类字眼；不超过 60 字。`

// TranslateFailure 实现 plugin.FailureTranslator。
func (p *Plugin) TranslateFailure(ctx context.Context, ev plugin.TurnFailure) (string, bool) {
	s := p.snapshot()
	// 只接里人格的轮次；本轮的域在轮次开头已裁决并写进 ctx。
	if plugin.ScopeFrom(ctx).Write != personaInner || s.innerPersona == "" {
		return "", false
	}
	// 配置类错误得自己拦一道：本插件先被征询，接手了就轮不到 roleplay 去分流。
	if llm.IsConfigError(ev.Err) {
		return "", false
	}
	// 从这往下的失败一律返回不接手：roleplay 的兜底台词会续上，
	// 一句中性的走神停顿两个人格都说得通，不值得再配一份。
	if s.complete == nil {
		return "", false
	}
	cctx, cancel := context.WithTimeout(ctx, failureCompleteTimeout)
	defer cancel()
	out, err := s.complete(cctx, fmt.Sprintf(
		prompt.T(s.lang, "prompt.dual_persona.failure_prompt", failurePromptZH), s.innerPersona))
	if err != nil {
		return "", false
	}
	if line := clipLine(out); line != "" {
		return line, true
	}
	return "", false
}

// clipLine 把模型产出规整成一句台词：去首尾空白与包裹引号，限制长度。
func clipLine(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"“”「」『』")
	if r := []rune(s); len(r) > failureLineMaxRunes {
		s = string(r[:failureLineMaxRunes])
	}
	return strings.TrimSpace(s)
}
