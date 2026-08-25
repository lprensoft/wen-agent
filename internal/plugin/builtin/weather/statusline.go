package weather

import (
	"fmt"
	"strings"
	"time"

	"wen/internal/plugin"
)

// StatusLines 报一行天气的运行状况。
//
// 「注入了什么」与「为什么没注入」在这里是同一个问题：没配城市、取不到、取到了但
// 已经过期，三种情况在对话里看起来都一样——角色都是不提天气。状态行是唯一能把它们
// 区分开的地方，所以三种都说，并且过期标在具体那一处上：两处地点时，一处失效
// 不代表另一处也没在用。
func (p *Plugin) StatusLines(lang string) []string {
	s := p.snapshot()
	if !s.configured() {
		return []string{"🌦 天气：未设置城市，不注入"}
	}
	now := time.Now()

	var (
		parts  []string
		newest time.Time
	)
	add := func(prefix, loc string) {
		if loc == "" {
			return
		}
		rep, ok, lastErr := p.lastReport(loc)
		if !ok {
			text := prefix + loc + " 尚未取得"
			if lastErr != "" {
				text += "（" + lastErr + "）"
			}
			parts = append(parts, text)
			return
		}
		if rep.Fetched.After(newest) {
			newest = rep.Fetched
		}
		text := fmt.Sprintf("%s%s %s %.0f℃", prefix, shortPlace(rep.Place), rep.Condition, rep.TempC)
		if s.stale > 0 && now.Sub(rep.Fetched) > s.stale {
			text += "（已过期，不注入）"
		}
		parts = append(parts, text)
	}

	if s.sameCity {
		add("", s.personaLoc)
	} else {
		add("角色 ", s.personaLoc)
		add("我 ", s.userLoc)
	}

	line := "🌦 天气：" + strings.Join(parts, " ／ ")
	if !newest.IsZero() {
		line += fmt.Sprintf("（%s，每 %d 分钟刷新）", humanizeAge(now.Sub(newest)), int(s.refresh.Minutes()))
	}
	return []string{line}
}

var _ plugin.StatusReporter = (*Plugin)(nil)

// shortPlace 取地名的第一段。状态行是给人扫一眼的，「杭州 · 浙江 · 中国」太长。
func shortPlace(name string) string {
	if i := strings.Index(name, " · "); i > 0 {
		return name[:i]
	}
	return name
}

// humanizeAge 把间隔说成人话。
func humanizeAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "刚刚更新"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前更新", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时前更新", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 天前更新", int(d.Hours()/24))
	}
}
