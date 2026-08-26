package bodysense

import "wen/internal/prompt"

// 熟悉的快慢。它作用在次数上而不是阈值上：一次乘法，没有取整歧义，
// 也不会因为缩放让某一档的阈值塌成 0。
const (
	paceSlow   = "slow"
	paceNormal = "normal"
	paceFast   = "fast"
)

// 五档熟悉度。分档取对数形状——人的适应本来就是对数的，头几次的差别最大，
// 到二十次以后再多几次已经没有分别。
//
// 「初次」必须单独成档：需求里最要紧的信号就是「第一次被碰到会僵住」，
// 把 1 和 2 合成一档正好把它糊掉。
//
// 五个档名会同时出现在注入的准则里与工具回显里，因此声明成 prompt.Piece：
// 取用处只剩一次 For(lang)，而键与中文原文仍是声明处的两个字面量，覆盖检查扫得到。
var (
	stageFirst    = prompt.Text("prompt.body_sense.stage.first", "初次")
	stageStrange  = prompt.Text("prompt.body_sense.stage.strange", "生疏")
	stageAdapting = prompt.Text("prompt.body_sense.stage.adapting", "适应")
	stageFamiliar = prompt.Text("prompt.body_sense.stage.familiar", "熟悉")
	stageUsual    = prompt.Text("prompt.body_sense.stage.usual", "习以为常")
)

// stageOrder 是注入时按阶段归并的顺序（最熟悉的在前）。
var stageOrder = []prompt.Piece{stageUsual, stageFamiliar, stageAdapting, stageStrange, stageFirst}

// effectiveCount 按快慢缩放实际次数。慢档至少留 1，否则第一次触碰会算不出阶段。
func effectiveCount(count int, pace string) int {
	if count <= 0 {
		return 0
	}
	switch pace {
	case paceFast:
		return count * 2
	case paceSlow:
		if n := count / 2; n >= 1 {
			return n
		}
		return 1
	default:
		return count
	}
}

// stageOf 按累计次数给出熟悉阶段。次数为 0 时返回零值——没记录过的部位不该出现在
// 任何输出里，调用方按 Key() 为空判断。
func stageOf(count int, pace string) prompt.Piece {
	switch n := effectiveCount(count, pace); {
	case n <= 0:
		return prompt.Piece{}
	case n == 1:
		return stageFirst
	case n <= 3:
		return stageStrange
	case n <= 9:
		return stageAdapting
	case n <= 19:
		return stageFamiliar
	default:
		return stageUsual
	}
}
