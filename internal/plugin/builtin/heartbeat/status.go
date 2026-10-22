package heartbeat

import (
	"time"

	"wen/internal/i18n"
	"wen/internal/prompt"
)

// StatusLines 在状态命令里报一行当前节奏：间隔、模式与下次心跳的大致时刻。
// 只报「大致」——心跳会被真人对话重置、被动态判定改写，报到秒既没有意义也会误导。
func (p *Plugin) StatusLines(lang string) []string {
	p.mu.Lock()
	cur, dynamic, lastBeat := p.cur, p.dynamic, p.lastBeat
	pausedUntil, curContext, asleep := p.pausedUntil, p.curContext, p.asleep
	p.mu.Unlock()

	if cur <= 0 { // 尚未 Init（理论上不会被问到），没有可报的节奏
		return nil
	}
	mode := i18n.T(lang, "plugin.heartbeat.status.fixed", "固定")
	if dynamic {
		mode = i18n.T(lang, "plugin.heartbeat.status.dynamic", "动态")
	}
	if curContext != "" {
		mode += " · " + curContext // 情境名是用户自己写的，不翻译
	}
	if until := time.Until(pausedUntil); until > 0 && asleep {
		return []string{i18n.Tf(lang, "plugin.heartbeat.status.asleep",
			"💓 心跳：睡着了，约 %s后醒（每 %s，%s）",
			humanDur(lang, until), humanDur(lang, cur), mode)}
	}
	if until := time.Until(pausedUntil); until > 0 {
		return []string{i18n.Tf(lang, "plugin.heartbeat.status.paused",
			"💓 心跳：暂停中，约 %s后恢复（每 %s，%s）",
			humanDur(lang, until), humanDur(lang, cur), mode)}
	}
	return []string{i18n.Tf(lang, "plugin.heartbeat.status.line", "💓 心跳：每 %s（%s），%s",
		humanDur(lang, cur), mode, nextBeatDesc(lang, lastBeat.Add(cur)))}
}

// nextBeatDesc 描述下次心跳的时机。已到期说明轮次正忙或刚被叫醒，此时报「即将」
// 比报一个负数或过去的时刻更贴近实际。
func nextBeatDesc(lang string, next time.Time) string {
	left := time.Until(next)
	if left < time.Minute {
		return i18n.T(lang, "plugin.heartbeat.status.imminent", "即将触发")
	}
	return i18n.Tf(lang, "plugin.heartbeat.status.next", "下次约 %s后", humanDur(lang, left))
}

// humanDur 描述时长：心跳的配置单位是分钟，秒级精度只会让输出抖动。
// 逐级降精度——隔了三天还报到分钟（“72 小时 13 分钟”）既难读也没有意义。
func humanDur(lang string, d time.Duration) string {
	// 单个单位的说法与全程序共用一份（含英语的单复数），这里只管拼两段
	join := func(a, b string) string {
		return prompt.Tf(lang, "prompt.heartbeat.dur.join", "%s %s", a, b)
	}
	mins := int(d.Round(time.Minute) / time.Minute)
	if mins < 60 {
		return prompt.Minutes(lang, mins)
	}
	h, m := mins/60, mins%60
	if h < 24 {
		if m == 0 {
			return prompt.Hours(lang, h)
		}
		return join(prompt.Hours(lang, h), prompt.Minutes(lang, m))
	}
	days, rh := h/24, h%24
	if rh == 0 {
		return prompt.Days(lang, days)
	}
	return join(prompt.Days(lang, days), prompt.Hours(lang, rh))
}
