package clock

import (
	"testing"
	"time"
	_ "time/tzdata" // Windows 与精简容器里没有系统 zoneinfo，夏令时用例靠它兜底
)

// 区间不前进时兜底扫描偏移变化；伦敦 2029-03-25 01:00 UTC 进入 BST。
func TestNextOffsetChangeFindsTransition(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("没有时区数据: %v", err)
	}
	from := time.Date(2029, 3, 25, 0, 0, 0, 0, time.UTC)
	got := nextOffsetChange(from, from.Add(48*time.Hour), london)
	want := time.Date(2029, 3, 25, 1, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("nextOffsetChange = %v，期望 %v", got.UTC(), want)
	}
}

// 整日被跳过的日期（萨摩亚 2011-12-30 不存在）要如实回答「没有这一天」，
// 不能退回前一天 23:00——调用方据此决定怎么补。
func TestFirstInstantOfDateMissingDay(t *testing.T) {
	apia, err := time.LoadLocation("Pacific/Apia")
	if err != nil {
		t.Skipf("没有时区数据: %v", err)
	}
	if got, ok := FirstInstantOfDate("2011-12-30", apia); ok {
		t.Fatalf("FirstInstantOfDate = %v, true，期望判为不存在", got)
	}
	next, ok := FirstInstantOfDate("2011-12-31", apia)
	if !ok {
		t.Fatal("2011-12-31 应当存在")
	}
	// 跳过的那天之后，前一天的后沿正好接上这一天的起点：时间轴是连续的。
	prev, ok := FirstInstantOfDate("2011-12-29", apia)
	if !ok || !next.After(prev) {
		t.Fatalf("2011-12-29 = %v, %v；2011-12-31 = %v", prev, ok, next)
	}
}
