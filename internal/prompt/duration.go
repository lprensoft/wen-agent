package prompt

import (
	"fmt"
	"time"
)

// 时长的说法。整个程序共用一份：核心报「距上一条消息多久」，现场快照报某一项
// 「多久前记下的」，身体感知报某处「多久前被碰过」——三处此前各写了一个
// humanizeGap，逐行相同，措辞早晚会分叉。
//
// 每个单位两个键，单数一个复数一个：英语里「1 hours」是错的，而这几处都真的会
// 取到 1。中文两个键取同一句原文，要分的只有语言包里的英文那份。
const (
	durMinutesZH = "%d 分钟"
	durHoursZH   = "%d 小时"
	durDaysZH    = "%d 天"
	durMonthsZH  = "%d 个月"
	durYearsZH   = "%d 年"
)

// Duration 把一段时长说成人话，只给到一个单位的精度。
//
// 精确到分钟反而会让模型拿它去做不必要的推算——这里要传达的只是「隔了多久」
// 这个量级。
//
// 两个键都写在调用点上，没有抽成按单位查表的循环：拼出来的键静态扫不到，
// 覆盖检查就看不见它们（见 cmd/wen 的 scanRepo）。
func Duration(lang string, d time.Duration) string {
	switch {
	case d < time.Hour:
		return Minutes(lang, int(d.Minutes()))
	case d < 24*time.Hour:
		return Hours(lang, int(d.Hours()))
	case d < 30*24*time.Hour:
		return Days(lang, int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return plural(lang, int(d.Hours()/24/30),
			T(lang, "prompt.duration.month_one", durMonthsZH),
			T(lang, "prompt.duration.months", durMonthsZH))
	default:
		return plural(lang, int(d.Hours()/24/365),
			T(lang, "prompt.duration.year_one", durYearsZH),
			T(lang, "prompt.duration.years", durYearsZH))
	}
}

// Minutes / Hours / Days 是单个单位的说法，供要自己拼两段的地方用（心跳的
// 「2 小时 30 分钟」——Duration 只给一个单位的精度，而「下次什么时候开口」
// 差半小时是看得出来的）。三对键与 Duration 共用，翻译只有一份。
func Minutes(lang string, n int) string {
	return plural(lang, n, T(lang, "prompt.duration.minute_one", durMinutesZH),
		T(lang, "prompt.duration.minutes", durMinutesZH))
}

func Hours(lang string, n int) string {
	return plural(lang, n, T(lang, "prompt.duration.hour_one", durHoursZH),
		T(lang, "prompt.duration.hours", durHoursZH))
}

func Days(lang string, n int) string {
	return plural(lang, n, T(lang, "prompt.duration.day_one", durDaysZH),
		T(lang, "prompt.duration.days", durDaysZH))
}

func plural(_ string, n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf(one, n)
	}
	return fmt.Sprintf(many, n)
}
