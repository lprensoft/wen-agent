package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"wen/internal/clock"
)

// 评测把整场对话放到一条自己推进的时间线上。这里守住三件事：消息按虚拟时刻落盘、
// 下一轮读得到「隔了多久」、以及虚拟时刻不会被真实耗时污染。
func TestVirtualClockStampsMessages(t *testing.T) {
	store, id := newTestSession(t)
	mp := &mockProvider{turns: []mockTurn{{content: "嗯"}, {content: "在"}}}
	ag := New(mp, newTestManager(t), store, Options{Model: "m"})

	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.Local)
	clk := clock.NewManual(start)
	ctx := clock.With(context.Background(), clk)

	ag.Run(ctx, id, "早", func(Event) {})
	clk.Advance(72 * time.Hour)
	ag.Run(ctx, id, "我回来了", func(Event) {})

	_, hist, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) < 4 {
		t.Fatalf("历史条数 = %d", len(hist))
	}
	// 第 1 轮：用户与回复都在起点；第 2 轮：用户与回复都在三天后。
	for _, c := range []struct {
		i    int
		want time.Time
	}{{0, start}, {1, start}, {2, start.Add(72 * time.Hour)}, {3, start.Add(72 * time.Hour)}} {
		if got := hist[c.i].TS; !got.Equal(c.want) {
			t.Errorf("第 %d 条时间戳 = %v，期望 %v", c.i, got, c.want)
		}
	}

	// 第二轮的请求里，历史与时间信号都该说出这三天。
	last := mp.reqs[len(mp.reqs)-1]
	var joined strings.Builder
	for _, m := range last.Messages {
		joined.WriteString(m.Content)
		joined.WriteString("\n")
	}
	if !strings.Contains(joined.String(), "距上一条消息") {
		t.Errorf("第二轮没带上「距上一条消息」:\n%s", joined.String())
	}
	if !strings.Contains(joined.String(), "2026-03-04") {
		t.Errorf("当前时间没跟着虚拟时钟走:\n%s", joined.String())
	}
}

// 没装钟时一切照旧：生产路径不该因为引入时钟而改变行为。
func TestNoClockUsesWallTime(t *testing.T) {
	store, id := newTestSession(t)
	mp := &mockProvider{turns: []mockTurn{{content: "嗯"}}}
	ag := New(mp, newTestManager(t), store, Options{Model: "m"})
	before := time.Now()
	ag.Run(context.Background(), id, "早", func(Event) {})
	_, hist, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) == 0 || hist[0].TS.Before(before.Add(-time.Second)) {
		t.Fatalf("没装钟时应当用系统时间，得到 %v", hist)
	}
}
