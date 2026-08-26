package heartbeat

import (
	"strings"
	"testing"
	"time"
)

// 状态行报出间隔、模式与下次心跳的大致时机。
func TestStatusLines(t *testing.T) {
	p := &Plugin{cur: 30 * time.Minute, dynamic: true, lastBeat: time.Now().Add(-10 * time.Minute)}
	lines := p.StatusLines("")
	if len(lines) != 1 {
		t.Fatalf("应只报一行，得到 %v", lines)
	}
	got := lines[0]
	for _, want := range []string{"心跳", "每 30 分钟", "动态", "下次约 20 分钟后"} {
		if !strings.Contains(got, want) {
			t.Fatalf("状态行 %q 缺少 %q", got, want)
		}
	}

	// 动态关闭时报「固定」
	p.dynamic = false
	if got := p.StatusLines("")[0]; !strings.Contains(got, "固定") {
		t.Fatalf("动态关闭时应报固定，得到 %q", got)
	}

	// 已到期（轮次占用或刚被叫醒）报「即将触发」，不报负数
	p.lastBeat = time.Now().Add(-time.Hour)
	if got := p.StatusLines("")[0]; !strings.Contains(got, "即将触发") {
		t.Fatalf("已到期应报即将触发，得到 %q", got)
	}

	// 未 Init（cur 为零）时不报
	if lines := (&Plugin{}).StatusLines(""); lines != nil {
		t.Fatalf("未启动时不应有状态行，得到 %v", lines)
	}
}

// 时长描述按分钟粒度，超过一小时拆成小时与分钟。
func TestHumanDur(t *testing.T) {
	cases := map[time.Duration]string{
		5 * time.Minute:    "5 分钟",
		59 * time.Minute:   "59 分钟",
		60 * time.Minute:   "1 小时",
		90 * time.Minute:   "1 小时 30 分钟",
		120 * time.Minute:  "2 小时",
		90*time.Second + 1: "2 分钟", // 四舍五入到分钟
		24 * time.Hour:     "1 天",
		30 * time.Hour:     "1 天 6 小时",
		72 * time.Hour:     "3 天", // 隔天数的间隔不再报到分钟
	}
	for d, want := range cases {
		if got := humanDur("", d); got != want {
			t.Errorf("humanDur(%v) = %q，期望 %q", d, got, want)
		}
	}
}
