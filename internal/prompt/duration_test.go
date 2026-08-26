package prompt

import (
	"testing"
	"time"
)

// 中文的分档与措辞是既有行为，这里钉住：核心报「距上一条消息多久」、现场快照报
// 「多久前记下的」、身体感知报「多久前被碰过」共用这一份，改了会一起变。
func TestDurationZH(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{90 * time.Second, "1 分钟"},
		{2 * time.Hour, "2 小时"},
		{50 * time.Hour, "2 天"},
		{45 * 24 * time.Hour, "1 个月"},
		{400 * 24 * time.Hour, "1 年"},
	}
	for _, c := range cases {
		if got := Duration("", c.d); got != c.want {
			t.Errorf("Duration(%v) = %q，期望 %q", c.d, got, c.want)
		}
	}
}

// 英语里「1 hours」是错的，而这几处都真的会取到 1，所以每个单位有单复数两个键。
func TestDurationENSingularAndPlural(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{90 * time.Second, "1 minute"},
		{30 * time.Minute, "30 minutes"},
		{time.Hour, "1 hour"},
		{2 * time.Hour, "2 hours"},
		{25 * time.Hour, "1 day"},
		{50 * time.Hour, "2 days"},
		{45 * 24 * time.Hour, "1 month"},
		{75 * 24 * time.Hour, "2 months"},
		{400 * 24 * time.Hour, "1 year"},
		{800 * 24 * time.Hour, "2 years"},
	}
	for _, c := range cases {
		if got := Duration("en", c.d); got != c.want {
			t.Errorf("Duration(en, %v) = %q，期望 %q", c.d, got, c.want)
		}
	}
}
