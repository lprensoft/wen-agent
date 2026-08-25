package agent

import (
	"fmt"
	"strings"
	"time"

	"wen/internal/prompt"
)

// 本轮状态块的包裹标签。用成对标签而不是 [标题] 是因为它拼在用户输入之前发出，
// 需要一个明确的收尾位置把「程序生成的信息」与「对方说的话」分开。
//
// 标签名随语言切换，引用它的提示词（核心的 [历史与时间]、roleplay 的 [时间一致性]）
// 必须同语言引用同一个名字。语言是进程级的，两边取的是同一个 lang。
const (
	turnStateOpenZH  = "<本轮状态>"
	turnStateCloseZH = "</本轮状态>"
)

func turnStateOpen(lang string) string {
	return prompt.T(lang, "prompt.core.turn_state.open", turnStateOpenZH)
}

func turnStateClose(lang string) string {
	return prompt.T(lang, "prompt.core.turn_state.close", turnStateCloseZH)
}

// turnStateLeadZH 是块内的第一句话。「以这里为准」这一句是必须的：模型判断「现在」
// 时会优先采信对话文本里出现过的时刻（那是叙事事实，且离生成位置更近），
// 不明确宣告权威性的话，一段深夜的对话会让它在第二天下午继续演深夜。
const turnStateLeadZH = "以下由程序在每轮对话前重新生成，不是对方说的话。" +
	"其中的信息与历史对话里出现过的同类说法不一致时，一律以这里为准。"

// 块内两个字段名。roleplay 的 [时间一致性] 会引用「当前时间」这个名字，两处同语言。
const (
	turnStateNowZH = "当前时间: "
	turnStateGapZH = "距上一条消息: "
)

// gapThreshold 以下的间隔不在历史里作标记。每条消息都带时间戳的话噪声太大，
// 而模型真正缺的信息只有一个：两条消息之间是不是隔了很久。
const gapThreshold = 30 * time.Minute

// nowGapThreshold 是本轮那一行的阈值，比历史里的低得多：它每轮只有一行，
// 「隔了五分钟」与「刚说完」对接话方式的影响是实打实的，不算噪声。
// 低于一分钟才不说——连续对话里报「刚刚」是废话。
const nowGapThreshold = time.Minute

// turnStateBlock 组装放在历史之后、本轮用户输入之前的易变段：当前时间、距上一条
// 消息的间隔，以及各插件的每轮提示词（心情、天气、记忆索引这类）。
//
// 放在这个位置有两个理由，且是同一处改动同时解决的：其一，它离生成位置最近，
// 模型不再需要跨几千 token 主动比对才想起「现在」是几点；其二，system 与历史因此
// 成为整轮之间字节一致的前缀，提示词缓存才可能命中（见 llm 层的缓存断点）。
func turnStateBlock(lang string, now time.Time, sinceLast time.Duration, parts []string) string {
	var b strings.Builder
	b.WriteString(turnStateOpen(lang))
	b.WriteString("\n")
	b.WriteString(prompt.T(lang, "prompt.core.turn_state.lead", turnStateLeadZH))
	b.WriteString("\n\n")
	b.WriteString(prompt.T(lang, "prompt.core.turn_state.now", turnStateNowZH))
	b.WriteString(formatNow(lang, now))
	if sinceLast >= nowGapThreshold {
		b.WriteString("\n")
		b.WriteString(prompt.T(lang, "prompt.core.turn_state.gap", turnStateGapZH))
		b.WriteString(humanizeGap(lang, sinceLast))
	}
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			b.WriteString("\n\n")
			b.WriteString(s)
		}
	}
	b.WriteString("\n")
	b.WriteString(turnStateClose(lang))
	return b.String()
}

// gapNoteZH 是历史里那行间隔标注的格式串。它被 [历史与时间] 逐字引述，两处同语言。
const gapNoteZH = "（此处距上一条消息%s）\n"

func gapNoteFmt(lang string) string {
	return prompt.T(lang, "prompt.core.gap_note", gapNoteZH)
}

// gapNote 返回插在历史某条消息之前的时间间隔提示，不需要标记时返回空串。
//
// 历史里没有任何时间流逝的信号：昨夜 23:30 说的话与今天下午的提问在上下文里紧挨着，
// 中间那十几个小时不存在，模型只能从对话文本里找时刻，于是找到的永远是旧的那个。
// 消息的时间戳本来就落了盘（session.StoredMessage.TS），补上这一行即可。
func gapNote(lang string, prev, cur time.Time) string {
	if prev.IsZero() || cur.IsZero() {
		return ""
	}
	d := cur.Sub(prev)
	if d < gapThreshold {
		return ""
	}
	return fmt.Sprintf(gapNoteFmt(lang), humanizeGap(lang, d))
}

// 间隔的说法。只给到「约」的精度：这里要传达的是「隔了多久」，精确到分钟反而会让
// 模型拿它去做不必要的推算。
//
// 每个单位两个键，单数一个复数一个：英语里「about 1 hours」是错的，而这个函数在
// n 为 1 时真的会走到（本轮那一行的阈值只有一分钟）。中文两个键取同一句原文，
// 要分的只有语言包里的英文那份。
const (
	gapMinutesZH = "约 %d 分钟"
	gapHoursZH   = "约 %d 小时"
	gapDaysZH    = "约 %d 天"
	gapMonthsZH  = "约 %d 个月"
)

// humanizeGap 把间隔说成人话。
//
// 两个键都在调用点上写全，没有抽成一个按单位查表的循环：拼出来的键静态扫不到，
// 覆盖检查就看不见它们（见 cmd/wen 的 scanRepo）。
func humanizeGap(lang string, d time.Duration) string {
	switch {
	case d < time.Hour:
		return gapText(int(d.Minutes()),
			prompt.T(lang, "prompt.core.gap.minute_one", gapMinutesZH),
			prompt.T(lang, "prompt.core.gap.minutes", gapMinutesZH))
	case d < 24*time.Hour:
		return gapText(int(d.Hours()),
			prompt.T(lang, "prompt.core.gap.hour_one", gapHoursZH),
			prompt.T(lang, "prompt.core.gap.hours", gapHoursZH))
	case d < 30*24*time.Hour:
		return gapText(int(d.Hours()/24),
			prompt.T(lang, "prompt.core.gap.day_one", gapDaysZH),
			prompt.T(lang, "prompt.core.gap.days", gapDaysZH))
	default:
		return gapText(int(d.Hours()/24/30),
			prompt.T(lang, "prompt.core.gap.month_one", gapMonthsZH),
			prompt.T(lang, "prompt.core.gap.months", gapMonthsZH))
	}
}

// gapText 在单数与复数两句里挑一句。
func gapText(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf(one, n)
	}
	return fmt.Sprintf(many, n)
}

// weekdaysZH 是七个星期名，一行一个，从星期日排起。
//
// 整表一个键而不是七个：七个键要靠拼串取用，而拼出来的键静态扫不到，覆盖检查就
// 看不见它们（bodysense 的部位表是同样的形状）。
const weekdaysZH = "星期日\n星期一\n星期二\n星期三\n星期四\n星期五\n星期六"

func weekdayName(lang string, t time.Time) string {
	names := strings.Split(prompt.T(lang, "prompt.core.weekdays", weekdaysZH), "\n")
	if i := int(t.Weekday()); i < len(names) {
		return names[i]
	}
	return t.Weekday().String()
}

// dayMarkZH 是历史里那行日界线的格式串（日期、星期）。它被 [历史与时间] 逐字引述。
const dayMarkZH = "（以下是 %s %s 的对话）"

func dayMarkFmt(lang string) string {
	return prompt.T(lang, "prompt.core.day_mark", dayMarkZH)
}

// dayMark 是历史里的日界线：日期翻篇处标一行绝对日期。
//
// gapNote 给的是增量（「距上一条消息约 12 小时」），要定位历史里某句「明天」指的是
// 哪一天，模型得把那条消息到末尾的间隔逐段累加——一条它做不可靠的算术链。日界线把
// 这件事变成一次查表：那句话写在哪一天下面，「明天」就是那天的次日。
//
// 日期格式与 formatNow 的日期部分同形，使「历史里那天」与「当前时间」可以直接比对，
// 不必再做一次格式换算。
//
// 一天只有一行，且由已落盘的时间戳推出、生成后再不变化，因此对提示词缓存无害——
// 它与本轮状态块那种每轮重算的内容不是一回事。
func dayMark(lang string, t time.Time) string {
	return fmt.Sprintf(dayMarkFmt(lang), t.Format("2006-01-02"), weekdayName(lang, t))
}

// nowFormatZH 是当前时间的排法：日期时间、星期、时区偏移。
//
// 星期用本地写法而不是 Mon 这类英文缩写，并带上时区偏移。时间是模型唯一无法自行
// 获知却又必须与之保持一致的事实，格式上不该再给它额外的解析负担；不写时区的话，
// 跨时区的相对时间推算会悄悄错开。
const nowFormatZH = "%s %s %s"

func formatNow(lang string, t time.Time) string {
	return fmt.Sprintf(prompt.T(lang, "prompt.core.now_format", nowFormatZH),
		t.Format("2006-01-02 15:04"), weekdayName(lang, t), t.Format("Z07:00"))
}
