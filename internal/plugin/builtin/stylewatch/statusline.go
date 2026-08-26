package stylewatch

import (
	"time"

	"wen/internal/i18n"
)

// StatusLines 报一行今天的文风：真人轮次的数量、助手腔命中、平均长度与演绎占比。
// 后台轮次（心跳、定时任务）另计，有才报——它们的腔调是另一回事，混在一起会把
// 真人对话的数字冲淡。
func (p *Plugin) StatusLines(lang string) []string {
	p.statsMu.Lock()
	today, ok := p.st.find(dateOf(time.Now()))
	p.statsMu.Unlock()
	if !ok || (today.Interactive.Turns == 0 && today.Background.Turns == 0) {
		return []string{i18n.T(lang, "plugin.style_watch.status.none", "✍️ 文风：今日尚无数据")}
	}
	if today.Interactive.Turns == 0 {
		return []string{i18n.Tf(lang, "plugin.style_watch.status.bg_only",
			"✍️ 文风：今日真人对话尚无数据；后台 %s", today.Background.summary(lang))}
	}
	line := i18n.Tf(lang, "plugin.style_watch.status.today", "✍️ 文风：今日 %s", today.Interactive.summary(lang))
	if bg := today.Background; bg.Turns > 0 {
		line += i18n.Tf(lang, "plugin.style_watch.status.bg", "；后台 %d 轮 %d 次", bg.Turns, bg.totalHits())
	}
	return []string{line}
}
