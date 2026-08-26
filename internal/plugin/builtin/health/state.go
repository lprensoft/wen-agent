// Package health 提供健康的系统插件：角色会像真人一样偶尔有些小恙。
//
// 分工只有一条：代码管时间，模型管判断。会不会生病、生什么、多重、怎么处理，
// 全是叙事判断，由模型在事发的那一轮经工具记下；之后严重度随时间按病程曲线自动
// 起、峰、落，恢复快慢由处理方式决定，读取时按落盘时刻折算、不回写。插件不做
// 疾病模拟器，没有随机事件，也没有 HP 条——注入给模型看的是处境，不是数值。
//
// 本插件依赖 roleplay：没有角色，就没有「谁的身体」。
package health

import (
	"math"
	"time"

	"wen/internal/i18n"
	"wen/internal/prompt"
)

// 严重度三档。模型只跟这三个词打交道，内部的 0-100 数值只用来推曲线。
//
// 它不落盘（写入时就换算成了 Peak 数值），所以只需要 Piece；处理方式那组要落盘，
// 因此走 prompt.Enum，见下。
var (
	sevMild     = prompt.Text("prompt.health.severity.mild", "有点不舒服")
	sevModerate = prompt.Text("prompt.health.severity.moderate", "难受")
	sevSevere   = prompt.Text("prompt.health.severity.severe", "病倒了")

	severities = prompt.NewEnum(sevMild, sevModerate, sevSevere)
)

// 处理方式三种，决定恢复的快慢。
//
// 这一组**会原样落盘**（Condition.Care），所以中文是规范形式：Schema 按语言给出
// 候选项，模型报回来的值先经 careKinds.Canonical 换回中文再存。已经写下的记录
// 因此不需要迁移，换一次语言也不会让同一个字段有两种写法。
const (
	careTough  = "硬扛"
	careMeds   = "吃药"
	careDoctor = "就医"
)

var careKinds = prompt.NewEnum(
	prompt.Text("prompt.health.care.tough", careTough),
	prompt.Text("prompt.health.care.meds", careMeds),
	prompt.Text("prompt.health.care.doctor", careDoctor),
)

const (
	// riseEnd 是病程里加重段的占比：前三成在往重里走，峰值落在这里，之后转好。
	riseEnd = 0.3
	// onsetRatio 是刚发作时的严重度占峰值的比例——发作不是从零起跳的。
	onsetRatio = 0.25

	// maxNameRunes 限制状况名的长度。它由模型写入又逐轮进上下文，必须限死。
	maxNameRunes = 12
	// 病程天数的取值范围。日常小恙不会拖过两周，过了那就不是小恙。
	minDays, maxDays = 1, 14
	// 延迟发作最多两天：「下午淋雨晚上难受」是两三个小时，不是下个星期。
	maxOnsetHours = 48
)

// Condition 是一条身体状况。
//
// Progress / ProgressAt 是病程进度的折算基准：改处理方式时先把进度折算到此刻再
// 换速率，读取路径只折算不回写。刚登记时基准就是发作时刻、进度为 0。
type Condition struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Peak       int       `json:"peak"`  // 峰值严重度 0-100
	Onset      time.Time `json:"onset"` // 预计（或实际）发作时刻
	Days       int       `json:"days"`  // 硬扛着走完整个病程要几天
	Care       string    `json:"care"`
	Progress   float64   `json:"progress"`
	ProgressAt time.Time `json:"progress_at"`
	Cued       bool      `json:"cued,omitempty"` // 发作的开口理由是否已投递（或已错过）
}

// 阶段。
const (
	phasePending   = "pending"   // 还没发作，只是苗头
	phaseRising    = "rising"    // 在加重
	phaseFalling   = "falling"   // 在好转
	phaseRecovered = "recovered" // 已走完病程
)

// View 是一条状况折算到某一时刻的样子，渲染与状态行都从它取。
type View struct {
	Cond      Condition
	Phase     string
	Severity  int           // 此刻的严重度 0-100
	Day       int           // 第几天（按日历日，发作当天为第 1 天）
	Remaining time.Duration // 距预计痊愈还有多久
}

// peakFor 把严重度的词换成峰值数值；不认识的词返回 false。
// 传进来的可以是任意语言的字面量，先换回中文规范值再比。
func peakFor(word string) (int, bool) {
	zh, ok := severities.Canonical(word)
	if !ok {
		return 0, false
	}
	switch zh {
	case sevMild.ZH():
		return 30, true
	case sevModerate.ZH():
		return 55, true
	}
	return 85, true
}

// capFor 把「严重度上限」的配置值换成峰值封顶。
func capFor(value string) int {
	switch value {
	case capMild:
		return 35
	case capSevere:
		return 100
	}
	return 65
}

// band 按数值给出严重度的词。分档线与 capFor 的封顶一致：封在「难受」档就永远到不了
// 「病倒了」那个词。
func band(lang string, v int) string {
	switch {
	case v <= 0:
		return ""
	case v <= 35:
		return sevMild.For(lang)
	case v <= 65:
		return sevModerate.For(lang)
	}
	return sevSevere.For(lang)
}

// careFactor 是各处理方式在好转段的速率倍数。加重段不受它影响——药能让人好得快，
// 拦不住病先发出来。
func careFactor(care string) float64 {
	switch care {
	case careMeds:
		return 1.6
	case careDoctor:
		return 2.5
	}
	return 1
}

func validCare(care string) bool { return careKinds.Has(care) }

func (c Condition) course() time.Duration {
	return time.Duration(max(c.Days, minDays)) * 24 * time.Hour
}

// progressAt 把病程进度折算到 now。基准时刻之前（还没发作）原样返回。
func (c Condition) progressAt(now time.Time) float64 {
	p := c.Progress
	elapsed := now.Sub(c.ProgressAt)
	if elapsed <= 0 || p >= 1 {
		return min(p, 1)
	}
	course := c.course()
	if p < riseEnd {
		need := time.Duration((riseEnd - p) * float64(course))
		if elapsed < need {
			return p + float64(elapsed)/float64(course)
		}
		p, elapsed = riseEnd, elapsed-need
	}
	p += careFactor(c.Care) * float64(elapsed) / float64(course)
	return min(p, 1)
}

// recoveryAt 给出按当前处理方式走完病程的时刻。
func (c Condition) recoveryAt() time.Time {
	p := c.Progress
	if p >= 1 {
		return c.ProgressAt
	}
	course := float64(c.course())
	var d float64
	if p < riseEnd {
		d += (riseEnd - p) * course
		p = riseEnd
	}
	d += (1 - p) * course / careFactor(c.Care)
	return c.ProgressAt.Add(time.Duration(d))
}

// severityAt 把进度换成严重度：加重段从峰值的一部分线性升到峰值，好转段线性落到 0。
func severityAt(peak int, p float64) int {
	switch {
	case p <= 0:
		return 0
	case p >= 1:
		return 0
	case p < riseEnd:
		return int(math.Round(float64(peak) * (onsetRatio + (1-onsetRatio)*p/riseEnd)))
	}
	return int(math.Round(float64(peak) * (1 - p) / (1 - riseEnd)))
}

// view 折算一条状况到 now。
func (c Condition) view(now time.Time) View {
	v := View{Cond: c}
	if now.Before(c.Onset) {
		v.Phase = phasePending
		v.Remaining = c.recoveryAt().Sub(now)
		return v
	}
	p := c.progressAt(now)
	v.Severity = severityAt(c.Peak, p)
	v.Day = calendarDays(c.Onset, now) + 1
	v.Remaining = c.recoveryAt().Sub(now)
	switch {
	case p >= 1:
		v.Phase = phaseRecovered
	case p < riseEnd:
		v.Phase = phaseRising
	default:
		v.Phase = phaseFalling
	}
	return v
}

// calendarDays 算两个时刻隔了几个日历日。按日历日而不是按 24 小时：昨晚发作的病
// 今天早上就是「第 2 天」。倒流时按 0 算。
func calendarDays(from, to time.Time) int {
	day := func(x time.Time) time.Time {
		y, m, d := x.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, x.Location())
	}
	days := int(day(to).Sub(day(from.In(to.Location()))).Hours() / 24)
	return max(days, 0)
}

// ---------- 翻成话 ----------

// careText 是状况行里处理方式的说法。入参是落盘的中文规范值。
func careText(lang, care string) string {
	switch care {
	case careMeds:
		return prompt.T(lang, "prompt.health.care_text.meds", "吃了药")
	case careDoctor:
		return prompt.T(lang, "prompt.health.care_text.doctor", "看过医生")
	}
	return prompt.T(lang, "prompt.health.care_text.tough", "硬扛着")
}

// untilText 把「还有多久发作」说成话。
func untilText(lang string, d time.Duration) string {
	h := d.Hours()
	switch {
	case h < 1:
		return prompt.T(lang, "prompt.health.until.within_hour", "不到一小时后")
	case h < 24:
		return prompt.Tf(lang, "prompt.health.until.hours", "约 %d 小时后", int(math.Round(h)))
	}
	return prompt.Tf(lang, "prompt.health.until.days", "约 %d 天后", int(math.Round(h/24)))
}

// recoverText 把「还有多久好」说成话。说的是区间而不是精确值：病程本来就是估的，
// 「预计再 2.4 天好」这种精度是假的。
func recoverText(lang string, d time.Duration) string {
	h := d.Hours()
	switch {
	case h <= 0:
		return prompt.T(lang, "prompt.health.recover.due", "该好了")
	case h < 12:
		return prompt.T(lang, "prompt.health.recover.today", "预计今天内就好")
	case h < 36:
		return prompt.T(lang, "prompt.health.recover.tomorrow", "预计明天就好")
	}
	days := int(math.Ceil(h / 24))
	var span string
	switch {
	case days <= 2:
		span = prompt.T(lang, "prompt.health.span.two_three", "两三天")
	case days == 3:
		span = prompt.T(lang, "prompt.health.span.three_four", "三四天")
	case days == 4:
		span = prompt.T(lang, "prompt.health.span.four_five", "四五天")
	case days <= 6:
		span = prompt.T(lang, "prompt.health.span.five_six", "五六天")
	case days <= 9:
		span = prompt.T(lang, "prompt.health.span.about_week", "一周左右")
	default:
		span = prompt.Tf(lang, "prompt.health.span.days", " %d 天左右", days)
	}
	return prompt.Tf(lang, "prompt.health.recover.in", "预计再%s好", span)
}

// renderView 渲染一条状况。四种形态：苗头 / 在加重 / 停在峰值档 / 从峰值档转轻。
// 数值一个都不出现：模型看到的是处境，不是 HP。
func renderView(lang string, v View, now time.Time) string {
	c := v.Cond
	switch v.Phase {
	case phasePending:
		return prompt.Tf(lang, "prompt.health.view.pending", "%s的苗头：预计%s发作，到时大概会%s。",
			c.Name, untilText(lang, c.Onset.Sub(now)), band(lang, c.Peak))
	case phaseRising:
		return prompt.Tf(lang, "prompt.health.view.rising", "%s第 %d 天，%s，还在往重里走（%s），%s。",
			c.Name, v.Day, band(lang, v.Severity), careText(lang, c.Care), recoverText(lang, v.Remaining))
	}
	cur, peak := band(lang, v.Severity), band(lang, c.Peak)
	if cur == peak {
		return prompt.Tf(lang, "prompt.health.view.steady", "%s第 %d 天，%s（%s），%s。",
			c.Name, v.Day, cur, careText(lang, c.Care), recoverText(lang, v.Remaining))
	}
	return prompt.Tf(lang, "prompt.health.view.easing", "%s第 %d 天，从%s转成%s（%s），%s。",
		c.Name, v.Day, peak, cur, careText(lang, c.Care), recoverText(lang, v.Remaining))
}

// statusText 是状态行里一条状况的说法。它给人看，因此走 i18n 而不是 prompt。
func statusText(lang string, v View, now time.Time) string {
	c := v.Cond
	if v.Phase == phasePending {
		return i18n.Tf(lang, "plugin.health.status.pending", "%s（预计%s发作）",
			c.Name, untilStatus(lang, c.Onset.Sub(now)))
	}
	return i18n.Tf(lang, "plugin.health.status.day", "%s第 %d 天（%s）",
		c.Name, v.Day, careStatus(lang, c.Care))
}

// careStatus 是状态行里处理方式的说法。
func careStatus(lang, care string) string {
	switch care {
	case careMeds:
		return i18n.T(lang, "plugin.health.care_status.meds", "吃药中")
	case careDoctor:
		return i18n.T(lang, "plugin.health.care_status.doctor", "看过医生")
	}
	return i18n.T(lang, "plugin.health.care_status.tough", "硬扛中")
}

// untilStatus 与 untilText 说的是同一件事，但落在状态行上，因此走 i18n。
func untilStatus(lang string, d time.Duration) string {
	h := d.Hours()
	switch {
	case h < 1:
		return i18n.T(lang, "plugin.health.until.within_hour", "不到一小时后")
	case h < 24:
		return i18n.Tf(lang, "plugin.health.until.hours", "约 %d 小时后", int(math.Round(h)))
	}
	return i18n.Tf(lang, "plugin.health.until.days", "约 %d 天后", int(math.Round(h/24)))
}
