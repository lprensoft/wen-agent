package weather

import (
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/prompt"
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
		return []string{i18n.T(lang, "plugin.weather.status.unset", "🌦 天气：未设置城市，不注入")}
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
			text := i18n.Tf(lang, "plugin.weather.status.pending", "%s%s 尚未取得", prefix, loc)
			if lastErr != "" {
				text += i18n.Tf(lang, "plugin.weather.status.reason", "（%s）", lastErr)
			}
			parts = append(parts, text)
			return
		}
		if rep.Fetched.After(newest) {
			newest = rep.Fetched
		}
		text := i18n.Tf(lang, "plugin.weather.status.reading", "%s%s %s %.0f℃",
			prefix, shortPlace(rep.Place), conditions.For(lang, rep.Condition), rep.TempC)
		if s.stale > 0 && now.Sub(rep.Fetched) > s.stale {
			text += i18n.T(lang, "plugin.weather.status.stale", "（已过期，不注入）")
		}
		parts = append(parts, text)
	}

	if s.sameCity {
		add("", s.personaLoc)
	} else {
		add(i18n.T(lang, "plugin.weather.status.persona", "角色 "), s.personaLoc)
		add(i18n.T(lang, "plugin.weather.status.user", "我 "), s.userLoc)
	}

	line := i18n.T(lang, "plugin.weather.status.prefix", "🌦 天气：") + strings.Join(parts, " ／ ")
	if !newest.IsZero() {
		line += i18n.Tf(lang, "plugin.weather.status.age", "（%s，每 %d 分钟刷新）",
			humanizeAge(lang, now.Sub(newest)), int(s.refresh.Minutes()))
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
func humanizeAge(lang string, d time.Duration) string {
	if d < time.Minute {
		return i18n.T(lang, "plugin.weather.status.just_now", "刚刚更新")
	}
	// 时长本身走全程序共用的口径，这里只管「……前更新」这层壳
	return i18n.Tf(lang, "plugin.weather.status.updated_ago", "%s前更新", prompt.Duration(lang, d))
}
