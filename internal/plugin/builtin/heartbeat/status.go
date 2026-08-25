package heartbeat

import (
	"fmt"
	"time"
)

// StatusLines 在状态命令里报一行当前节奏：间隔、模式与下次心跳的大致时刻。
// 只报「大致」——心跳会被真人对话重置、被动态判定改写，报到秒既没有意义也会误导。
func (p *Plugin) StatusLines(lang string) []string {
	p.mu.Lock()
	cur, dynamic, lastBeat := p.cur, p.dynamic, p.lastBeat
	pausedUntil, curContext := p.pausedUntil, p.curContext
	p.mu.Unlock()

	if cur <= 0 { // 尚未 Init（理论上不会被问到），没有可报的节奏
		return nil
	}
	mode := "固定"
	if dynamic {
		mode = "动态"
	}
	if curContext != "" {
		mode += " · " + curContext
	}
	if until := time.Until(pausedUntil); until > 0 {
		return []string{fmt.Sprintf("💓 心跳：暂停中，约 %s后恢复（每 %s，%s）",
			humanDur(until), humanDur(cur), mode)}
	}
	return []string{fmt.Sprintf("💓 心跳：每 %s（%s），%s",
		humanDur(cur), mode, nextBeatDesc(lastBeat.Add(cur)))}
}

// nextBeatDesc 描述下次心跳的时机。已到期说明轮次正忙或刚被叫醒，此时报「即将」
// 比报一个负数或过去的时刻更贴近实际。
func nextBeatDesc(next time.Time) string {
	left := time.Until(next)
	if left < time.Minute {
		return "即将触发"
	}
	return "下次约 " + humanDur(left) + "后"
}

// humanDur 描述时长：心跳的配置单位是分钟，秒级精度只会让输出抖动。
// 逐级降精度——隔了三天还报到分钟（“72 小时 13 分钟”）既难读也没有意义。
func humanDur(d time.Duration) string {
	mins := int(d.Round(time.Minute) / time.Minute)
	if mins < 60 {
		return fmt.Sprintf("%d 分钟", mins)
	}
	h, m := mins/60, mins%60
	if h < 24 {
		if m == 0 {
			return fmt.Sprintf("%d 小时", h)
		}
		return fmt.Sprintf("%d 小时 %d 分钟", h, m)
	}
	days, rh := h/24, h%24
	if rh == 0 {
		return fmt.Sprintf("%d 天", days)
	}
	return fmt.Sprintf("%d 天 %d 小时", days, rh)
}
