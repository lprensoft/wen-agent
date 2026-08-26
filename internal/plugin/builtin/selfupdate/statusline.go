package selfupdate

import (
	"time"

	"wen/internal/i18n"
	"wen/internal/prompt"

	"wen/internal/updater"
	"wen/internal/version"
)

// StatusLines 报一行更新状况。
//
// 「有没有新版」与「还知不知道有没有新版」是两件事：自动检查被关掉、上一次检查失败、
// 从来没查过，在界面上看起来都是「没提示更新」。状态行是唯一能把它们区分开的地方。
func (p *Plugin) StatusLines(lang string) []string {
	s := p.snapshot()
	prefix := i18n.T(lang, "plugin.self_update.status.prefix", "⬆ 程序更新：")

	if tag := s.newerTag(); tag != "" {
		return []string{prefix + i18n.Tf(lang, "plugin.self_update.status.available",
			"有新版 %s 可更新（当前 %s），在设置页的「程序更新」里一键更新", tag, version.Version)}
	}

	line := prefix + i18n.Tf(lang, "plugin.self_update.status.current", "当前 %s", version.Version)
	if cur, ok := updater.ParseVersion(version.Version); ok && cur.Dev {
		line += i18n.T(lang, "plugin.self_update.status.dev", "（开发版）")
	}
	switch {
	case s.st.LastCheck.IsZero():
		line += i18n.T(lang, "plugin.self_update.status.never", "，尚未检查过")
	case s.st.Latest != "":
		line += i18n.Tf(lang, "plugin.self_update.status.latest", "，已是最新（%s检查）",
			humanizeAge(lang, p.now().Sub(s.st.LastCheck)))
	default:
		line += i18n.Tf(lang, "plugin.self_update.status.checked", "（%s检查）",
			humanizeAge(lang, p.now().Sub(s.st.LastCheck)))
	}
	if !s.autoCheck {
		line += i18n.T(lang, "plugin.self_update.status.auto_off", "，自动检查已关闭")
	} else {
		line += i18n.Tf(lang, "plugin.self_update.status.every", "，%s查一次",
			humanizeInterval(lang, s.interval))
	}
	return []string{line}
}

// humanizeAge 把间隔说成人话。状态行是给人扫一眼的，不要精确到秒。
func humanizeAge(lang string, d time.Duration) string {
	if d < time.Minute {
		return i18n.T(lang, "plugin.self_update.status.just_now", "刚刚")
	}
	// 时长本身走全程序共用的口径，这里只管「……前」这层壳
	return i18n.Tf(lang, "plugin.self_update.status.ago", "%s前", prompt.Duration(lang, d))
}

func humanizeInterval(lang string, d time.Duration) string {
	switch {
	case d == 24*time.Hour:
		return i18n.T(lang, "plugin.self_update.status.daily", "每天")
	case d > 24*time.Hour && d%(24*time.Hour) == 0:
		return i18n.Tf(lang, "plugin.self_update.status.every_n", "每 %s", prompt.Days(lang, int(d.Hours()/24)))
	default:
		return i18n.Tf(lang, "plugin.self_update.status.every_n", "每 %s", prompt.Hours(lang, int(d.Hours())))
	}
}
