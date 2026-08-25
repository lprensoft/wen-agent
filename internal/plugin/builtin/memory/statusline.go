package memory

import (
	"fmt"
	"strings"
)

// StatusLines 报一行当前的记忆行为：多久提炼一次、会不会淡忘、时限是多少。
//
// 刻意不报条数。StatusLines 拿不到本轮的可见域（接口没有这个参数，它在 Manager
// 持锁的路径上被调用），而条数本身就会泄漏「存在什么」——启用 dual_persona 时，
// 一个人格的状态输出里出现另一个人格的记忆条数，藏住的就只是内容而不是存在性了。
// 要看有多少条，list_memories 是按可见域算的。
func (p *Plugin) StatusLines(lang string) []string {
	s := p.snapshot()
	if s.store == nil {
		return nil
	}
	var parts []string
	switch {
	case s.turnExtract && s.autoExtract:
		parts = append(parts, fmt.Sprintf("每 %d 轮及压缩前提炼", s.turnEvery))
	case s.turnExtract:
		parts = append(parts, fmt.Sprintf("每 %d 轮提炼", s.turnEvery))
	case s.autoExtract:
		parts = append(parts, "压缩前提炼")
	default:
		parts = append(parts, "仅模型主动保存")
	}
	if s.decay {
		parts = append(parts, fmt.Sprintf("%d 天淡忘、%d 天移出", s.blurDays, s.forgetDays))
	}
	return []string{"🧠 记忆：" + strings.Join(parts, " · ")}
}
