package bodysense

import (
	"time"

	"wen/internal/prompt"
)

// 困意：由「醒了多久」与「几点了」算出来，模型不上报、也调不动。
//
// 与唤起、疲劳不同，它不是模型记的账，而是一个模型自己看不见的真实量：会话上下文
// 离自动压缩还有多远。上下文越长，模型对其中细节的把握确实越差，所以这份「困」
// 不是演出来的；压缩之后上下文变短，人也确实清醒了——睡一觉对应的正是压缩。
// 结构照搬睡眠的双过程模型：醒着的时长攒出睡眠压力，钟点给出昼夜节律，两者相加。
//
// 不落盘：两个输入在每轮开头都能现取，存一份只会多一处与真相对不上的地方。
// 「一阵困意」的冷却记在内存里，重启丢掉的只是一次冷却。

const (
	defaultSleepinessOnset = 50 // 上下文用量到自动压缩点的这个百分比之后才开始计入

	// pressureWeight 是睡眠压力的满分：到了自动压缩点就是「很困」那一档的下沿，
	// 再叠上深夜就撑不住了。
	pressureWeight = 70

	// waveCooldown 是两阵困意之间的最短间隔。困意是一阵一阵来的，不冷却的话
	// 「很困」那一档几乎每轮都在打哈欠。
	waveCooldown = 10 * time.Minute
)

// clockDrowsiness 是昼夜节律给的困意：深夜最重，午后有个小低谷，白天为 0。
var clockDrowsiness = [24]int{
	30, 35, 45, 45, 45, 35, 15, // 0-6 点
	0, 0, 0, 0, 0, 0, // 7-12 点
	10, 10, // 13-14 点
	0, 0, 0, 0, 0, 0, 0, // 15-21 点
	10, 20, // 22-23 点
}

// sleepiness 是一轮的困意：总分与两个来源各占多少，起因的措辞要靠后两者区分。
type sleepiness struct {
	Score    int
	Pressure int // 醒了很久
	Clock    int // 钟点
	Hour     int
}

// computeSleepiness 按上下文负载（用量 / 自动压缩点）、起算百分比与本地钟点算困意。
// 负载在起算点以下不计；起算点到压缩点之间线性涨到满分；超过压缩点按满分算。
func computeSleepiness(load float64, onsetPct, hour int) sleepiness {
	s := sleepiness{Hour: hour, Clock: clockDrowsiness[((hour%24)+24)%24]}
	if onset := float64(onsetPct) / 100; onset < 1 && load > onset {
		s.Pressure = int(min((load-onset)/(1-onset), 1) * pressureWeight)
	}
	s.Score = min(s.Pressure+s.Clock, stateMax)
	return s
}

// sleepinessLevel 把总分分成五档：0 不注入，1-4 由轻到重。
func sleepinessLevel(score int) int {
	switch {
	case score < 20:
		return 0
	case score < 45:
		return 1
	case score < 70:
		return 2
	case score < 90:
		return 3
	default:
		return 4
	}
}

// waveChance 是各档每轮冒出「一阵困意」的概率：越困越频繁。
func waveChance(level int) float64 {
	return [...]float64{0, 0.15, 0.3, 0.5, 0.7}[level]
}

func sleepinessWord(lang string, level int) string {
	switch level {
	case 1:
		return prompt.T(lang, "prompt.body_sense.sleepiness.drowsy", "有点困")
	case 2:
		return prompt.T(lang, "prompt.body_sense.sleepiness.sleepy", "困了")
	case 3:
		return prompt.T(lang, "prompt.body_sense.sleepiness.heavy", "很困，眼皮发沉")
	default:
		return prompt.T(lang, "prompt.body_sense.sleepiness.spent", "困得快撑不住了")
	}
}

// sleepinessCause 给出困意从哪儿来。只说模型能演的那一面——「醒了很久」与钟点，
// 不提上下文与 token：那是机制，不是角色的感受。
func sleepinessCause(lang string, s sleepiness) string {
	switch {
	case s.Pressure >= 15 && s.Clock >= 15:
		return prompt.T(lang, "prompt.body_sense.sleepiness.cause.both", "醒了很久，又是这个钟点")
	case s.Pressure >= s.Clock:
		return prompt.T(lang, "prompt.body_sense.sleepiness.cause.awake", "醒了很久")
	case s.Hour == 6:
		return prompt.T(lang, "prompt.body_sense.sleepiness.cause.dawn", "天才刚亮")
	case s.Hour >= 13 && s.Hour <= 14:
		return prompt.T(lang, "prompt.body_sense.sleepiness.cause.afternoon", "午后")
	default:
		return prompt.T(lang, "prompt.body_sense.sleepiness.cause.night", "夜深了")
	}
}

// waveLineZH 是「一阵困意」的措辞。[困意] 的判据逐字引用它，TestSleepGuideQuotesWave 盯着两处对得上。
const waveLineZH = "这会儿一阵困意涌上来。"

// renderSleepiness 渲染一行困意，不到一档时返回空串。
func renderSleepiness(lang string, s sleepiness, wave bool) string {
	level := sleepinessLevel(s.Score)
	if level == 0 {
		return ""
	}
	out := prompt.Tf(lang, "prompt.body_sense.sleepiness.line", "困意：%s（%s）。",
		sleepinessWord(lang, level), sleepinessCause(lang, s))
	if wave {
		out += prompt.T(lang, "prompt.body_sense.sleepiness.wave", waveLineZH)
	}
	return out
}
