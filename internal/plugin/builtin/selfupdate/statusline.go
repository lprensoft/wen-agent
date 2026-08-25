package selfupdate

import (
	"fmt"
	"time"

	"wen/internal/updater"
	"wen/internal/version"
)

// StatusLines 报一行更新状况。
//
// 「有没有新版」与「还知不知道有没有新版」是两件事：自动检查被关掉、上一次检查失败、
// 从来没查过，在界面上看起来都是「没提示更新」。状态行是唯一能把它们区分开的地方。
func (p *Plugin) StatusLines(lang string) []string {
	s := p.snapshot()
	const prefix = "⬆ 程序更新："

	if tag := s.newerTag(); tag != "" {
		return []string{fmt.Sprintf("%s有新版 %s 可更新（当前 %s），在设置页的「程序更新」里一键更新",
			prefix, tag, version.Version)}
	}

	line := prefix + "当前 " + version.Version
	if cur, ok := updater.ParseVersion(version.Version); ok && cur.Dev {
		line += "（开发版）"
	}
	switch {
	case s.st.LastCheck.IsZero():
		line += "，尚未检查过"
	case s.st.Latest != "":
		line += fmt.Sprintf("，已是最新（%s检查）", humanizeAge(p.now().Sub(s.st.LastCheck)))
	default:
		line += fmt.Sprintf("（%s检查）", humanizeAge(p.now().Sub(s.st.LastCheck)))
	}
	if !s.autoCheck {
		line += "，自动检查已关闭"
	} else {
		line += "，" + humanizeInterval(s.interval) + "查一次"
	}
	return []string{line}
}

// humanizeAge 把间隔说成人话。状态行是给人扫一眼的，不要精确到秒。
func humanizeAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "刚刚"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 天前", int(d.Hours()/24))
	}
}

func humanizeInterval(d time.Duration) string {
	switch {
	case d == 24*time.Hour:
		return "每天"
	case d > 24*time.Hour && d%(24*time.Hour) == 0:
		return fmt.Sprintf("每 %d 天", int(d.Hours()/24))
	default:
		return fmt.Sprintf("每 %d 小时", int(d.Hours()))
	}
}
