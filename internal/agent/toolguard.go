package agent

import (
	"encoding/json"
	"errors"

	"wen/internal/i18n"
	"wen/internal/llm"
	"wen/internal/prompt"
)

// 工具循环护栏。
//
// max_turns 是唯一的闸门时，闸门开得太晚：一个后台轮次曾把同一个工具提交了十三次
// （每次只微调一点参数），每一次都重发两万多 token 的上下文，两分半烧掉三十万
// input token，而它本来要跑到第二十次才会停。护栏不等到那时候：重复的调用直接不执行，
// 把「这件事已经做完了」当场告诉模型——它多半就此收尾。
//
// 三条判据，一条比一条宽：
//   - 与本轮里任何一次调用**完全相同**（同名同参）：结果不会变，不必再跑一遍。
//   - 同一个工具在本轮里调用超过 maxSameTool 次：参数每次都变一点的那种绕圈。
//   - 前两条累计触发 maxGuardHits 次：模型没有听懂，结束这一轮，交给发起方处理
//     （前台由 FailureTranslator 演成一句走神，后台由发起方记一次失败）。
//
// 判据刻意只在一轮之内计数：跨轮的重复是正常的（每天都要排一次表）。
const (
	maxSameTool  = 6
	maxGuardHits = 3
)

// errToolLoopZH 是护栏最终放弃时那句话的中文原文。措辞面向日志与注记，不面向模型。
const errToolLoopZH = "同一工具反复调用且不收尾，已结束本轮"

// errToolLoop 用中文原文建，身份用它比对；给人看的那一份在 toolLoopError 里按语言取。
var errToolLoop = errors.New(errToolLoopZH)

// toolLoopError 是同一件事按语言说出来的版本，写进会话注记与日志。
//
// 它必须仍然能被 errors.Is(err, errToolLoop) 认出来：哨兵存在的意义就是被比对，
// 换一门语言把它换成一个新错误，比对就静默失效了——那是翻译不该造成的回归。
func toolLoopError(lang string) error {
	return &localizedErr{msg: i18n.T(lang, "agent.tool_guard.loop", errToolLoopZH), is: errToolLoop}
}

// localizedErr 把「给人看的那句话」与「用来比对的身份」分开拿着。
type localizedErr struct {
	msg string
	is  error
}

func (e *localizedErr) Error() string        { return e.msg }
func (e *localizedErr) Is(target error) bool { return target == e.is }
func (e *localizedErr) Unwrap() error        { return e.is }

// 两条拦截回执的中文原文。它们作为工具结果交回模型，因此归提示词语言包。
const guardDuplicateZH = "这次调用与本轮之前的某一次完全相同，没有执行——同样的参数得到同样的结果。" +
	"已经做成的事不必重复提交：要改就改不一样的地方，没有要改的就直接给出回复结束本轮。"

const guardTooManyZH = "本轮已经调用过 %s %d 次，这次没有执行。请不要再调用它，" +
	"直接给出回复结束本轮。"

// toolGuard 是一轮之内的调用记账。零值可用，非并发（工具循环是同步的）。
type toolGuard struct {
	seen   map[string]bool // 同名同参的指纹
	counts map[string]int  // 工具名 → 本轮调用次数
	hits   int             // 护栏拦下的次数
}

// check 判断这次调用要不要拦。返回的字符串非空时表示拦下，它会作为工具结果
// 交回模型——工具协议要求每个 tool_call 都有对应的结果，拦下也不例外。
func (g *toolGuard) check(lang string, call llm.ToolCall) string {
	if g.seen == nil {
		g.seen, g.counts = map[string]bool{}, map[string]int{}
	}
	key := call.Name + "\x00" + canonicalArgs(call.Arguments)
	switch {
	case g.seen[key]:
		g.hits++
		return prompt.T(lang, "prompt.core.tool_guard.duplicate", guardDuplicateZH)
	case g.counts[call.Name] >= maxSameTool:
		g.hits++
		return prompt.Tf(lang, "prompt.core.tool_guard.too_many", guardTooManyZH,
			call.Name, g.counts[call.Name])
	}
	g.seen[key] = true
	g.counts[call.Name]++
	return ""
}

// exhausted 报告是否该放弃这一轮。
func (g *toolGuard) exhausted() bool { return g.hits >= maxGuardHits }

// canonicalArgs 把参数归一成可比较的形式：Go 的 json.Marshal 对 map 的键排序，
// 因此键序与空白的差异不会让两次相同的调用看起来不同。解析不了就用原文——
// 指纹算不准只是让护栏漏掉一次，不该让工具调用本身失败。
func canonicalArgs(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}
