package agent

import (
	"strings"
	"testing"
	"time"
)

func TestFormatNowIsChineseAndCarriesZone(t *testing.T) {
	// 时间是模型唯一无法自行获知却又必须与之一致的事实，格式上不该再给它解析负担；
	// 不写时区的话，跨时区的相对时间推算会悄悄错开。
	tz := time.FixedZone("CST", 8*3600)
	got := formatNow("", time.Date(2026, 8, 17, 21, 5, 0, 0, tz))
	if got != "2026-08-17 21:05 星期一 +08:00" {
		t.Errorf("formatNow = %q", got)
	}
	if got := formatNow("", time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)); !strings.Contains(got, "星期日") {
		t.Errorf("周日应写作星期日: %q", got)
	}
}

// 本轮状态块要能被模型认出是程序给的、且宣告自己比历史里的说法更可信——
// 只给一行时间而不说明权威性的话，模型会继续采信对话文本里出现过的旧时刻。
func TestTurnStateBlockDeclaresAuthority(t *testing.T) {
	got := turnStateBlock("", time.Date(2026, 8, 19, 14, 7, 0, 0, time.UTC), 0, nil)
	for _, want := range []string{turnStateOpenZH, turnStateCloseZH, "当前时间: 2026-08-19 14:07", "以这里为准"} {
		if !strings.Contains(got, want) {
			t.Errorf("本轮状态块缺少 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "距上一条消息") {
		t.Error("间隔为零时不该出现间隔行")
	}
}

func TestTurnStateBlockCarriesGapAndPluginParts(t *testing.T) {
	got := turnStateBlock("", time.Now(), 14*time.Hour, []string{"[当前心情]\n平静", "   ", "[当前天气]\n小雨"})
	if !strings.Contains(got, "距上一条消息: 约 14 小时") {
		t.Errorf("缺少间隔行:\n%s", got)
	}
	for _, want := range []string{"[当前心情]", "[当前天气]"} {
		if !strings.Contains(got, want) {
			t.Errorf("插件片段缺失 %q:\n%s", want, got)
		}
	}
	// 空片段不留下空行，否则每轮的字节内容会因为插件启停而无意义地抖动
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("空白片段应当被跳过:\n%q", got)
	}
}

func TestGapNoteOnlyMarksLongGaps(t *testing.T) {
	base := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)
	if got := gapNote("", base, base.Add(5*time.Minute)); got != "" {
		t.Errorf("短间隔不该标记，实际 %q", got)
	}
	got := gapNote("", base, base.Add(14*time.Hour))
	if !strings.Contains(got, "约 14 小时") || !strings.HasSuffix(got, "\n") {
		t.Errorf("长间隔标记 = %q", got)
	}
	// 缺时间戳的历史（升级前写下的记录）不能凭空造出间隔
	if got := gapNote("", time.Time{}, base); got != "" {
		t.Errorf("缺时间戳时应当不标记，实际 %q", got)
	}
	if got := gapNote("", base, time.Time{}); got != "" {
		t.Errorf("缺时间戳时应当不标记，实际 %q", got)
	}
}

func TestHumanizeGap(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{45 * time.Minute, "约 45 分钟"},
		{3 * time.Hour, "约 3 小时"},
		{50 * time.Hour, "约 2 天"},
		{100 * 24 * time.Hour, "约 3 个月"},
	}
	for _, c := range cases {
		if got := humanizeGap("", c.d); got != c.want {
			t.Errorf("humanizeGap(%v) = %q，期望 %q", c.d, got, c.want)
		}
	}
}
