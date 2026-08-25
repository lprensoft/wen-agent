package agent

import (
	"testing"
	"time"
)

// 中文那条路的措辞不能因为加了语言维度就变——它是这次改动唯一的硬指标：
// 中文用户感觉不到任何区别。这里钉死几处最容易在重构中漂移的排版（星期的位置、
// 冒号后的空格、括号的全角半角）。
func TestChineseWordingUnchanged(t *testing.T) {
	tz := time.FixedZone("CST", 8*3600)
	at := time.Date(2026, 8, 20, 14, 7, 0, 0, tz)

	for _, c := range []struct{ got, want string }{
		{dayMark("", at), "（以下是 2026-08-20 星期四 的对话）"},
		{formatNow("", at), "2026-08-20 14:07 星期四 +08:00"},
		{gapNote("", at.Add(-14*time.Hour), at), "（此处距上一条消息约 14 小时）\n"},
		{gapNote("", at.Add(-5*time.Minute), at), ""},
		{humanizeGap("", time.Minute), "约 1 分钟"},
		{humanizeGap("", 40*24*time.Hour), "约 1 个月"},
		{turnStateOpen(""), "<本轮状态>"},
		{turnStateClose(""), "</本轮状态>"},
	} {
		if c.got != c.want {
			t.Errorf("中文措辞变了:\n实际 %q\n期望 %q", c.got, c.want)
		}
	}

	block := turnStateBlock("", at, 2*time.Hour, []string{"[当前心情]\n平静"})
	want := "<本轮状态>\n" +
		"以下由程序在每轮对话前重新生成，不是对方说的话。其中的信息与历史对话里出现过的同类说法不一致时，一律以这里为准。\n\n" +
		"当前时间: 2026-08-20 14:07 星期四 +08:00\n" +
		"距上一条消息: 约 2 小时\n\n" +
		"[当前心情]\n平静\n" +
		"</本轮状态>"
	if block != want {
		t.Errorf("本轮状态块变了:\n实际:\n%s\n期望:\n%s", block, want)
	}
}
