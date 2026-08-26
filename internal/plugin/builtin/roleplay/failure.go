package roleplay

import (
	"context"
	"strings"
	"time"

	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

// 失败转译：模型调用失败（最常见的是内容被提供商安全策略拦截）时，不把厂商的
// 原始错误怼给正沉浸在对话里的人，而是以角色口吻转成一句走神般的停顿——拒绝
// 显现为角色的状态，而不是系统的墙。真实错误由核心转入会话注记，真相不丢。

const (
	defaultTranslateFailures = true
	defaultFailureLine       = "（走神了一会儿）……抱歉，刚才想说什么来着？"
	// failureCompleteTimeout 限制生成台词的辅助调用。失败路径上用户已经在等，
	// 不能再让一次卡住的调用把等待翻倍。
	failureCompleteTimeout = 20 * time.Second
	// failureLineMaxRunes 是台词的长度上限：它要冒充一句自然的停顿，长篇大论
	// 本身就出戏。
	failureLineMaxRunes = 120
)

const failurePromptZH = `下面是一段角色设定：

%s

你正以这个角色的身份与人对话。刚才你想说的话没能说出口。请以这个角色的口吻写一句简短自然的话，表示自己刚才走神、愣了一下或没接上话，并轻轻把话头接回来。
要求：只输出这句话本身，不加引号与任何解释；不出现「系统」「模型」「错误」「抱歉给您」这类字眼；不超过 60 字。`

// TranslateFailure 实现 plugin.FailureTranslator。
func (p *Plugin) TranslateFailure(ctx context.Context, ev plugin.TurnFailure) (string, bool) {
	s := p.snapshot()
	// 没有角色设定就没有「以谁的口吻」可言，此时包装错误只会掩盖问题。
	if !s.translateFailures || s.persona == "" {
		return "", false
	}
	// 配置类错误（密钥、地址、模型名）必须让用户看到原文才修得好。
	if llm.IsConfigError(ev.Err) {
		return "", false
	}
	if s.complete != nil {
		cctx, cancel := context.WithTimeout(ctx, failureCompleteTimeout)
		defer cancel()
		// 失败原因刻意不进提示词：拦截类失败把原文回传给同一个后端只会再被拦一次。
		out, err := s.complete(cctx, prompt.Tf(s.lang, "prompt.roleplay.failure_prompt", failurePromptZH, s.persona))
		if err == nil {
			if line := clipLine(out); line != "" {
				return line, true
			}
		}
		// 生成失败（提供商很可能整体不可用）落到静态兜底，不再重试。
	}
	if s.failureLine == "" {
		return "", false
	}
	return s.failureLine, true
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
