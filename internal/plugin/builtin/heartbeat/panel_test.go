package heartbeat

import (
	"testing"
	"time"

	"wen/internal/availability"
	"wen/internal/plugin"
)

func TestStatePinnedSummary(t *testing.T) {
	p := New()
	p.lang = "zh"
	p.cur = 30 * time.Minute
	p.dynamic = true
	p.lastBeat = time.Now().Add(-10 * time.Minute)
	p.curContext = "闲聊"

	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 {
		t.Fatalf("State = %+v", secs)
	}
	got := secs[0]
	if got.Title != "心跳" || !got.Pinned || got.PinnedOrder != 3 || !got.Global || got.Summary == "" {
		t.Errorf("section = %+v", got)
	}
	if len(got.Rows) != 3 {
		t.Errorf("rows = %+v", got.Rows)
	}
}

func TestStateAsleep(t *testing.T) {
	p := New()
	p.lang = "zh"
	p.cur = 30 * time.Minute
	p.asleep = true
	p.pausedUntil = time.Now().Add(time.Hour)

	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 || secs[0].Summary == "" {
		t.Fatalf("State = %+v", secs)
	}
}

// 状态页要按调度器的真实规则报下一拍：重忙时压到忙完之后，同时不能
// 把这次只读查询的忙碌观察写回插件。
func TestStateUsesBusyHoldoffReadOnly(t *testing.T) {
	cleanBoard(t)
	p := New()
	p.lang = "zh"
	p.cur = 30 * time.Minute
	p.minIv = 5 * time.Minute
	p.dynamic = true
	p.lastBeat = time.Now()

	until := time.Now().Add(time.Hour)
	availability.Set(availability.State{
		Source: "agenda", Activity: "开会", Level: availability.Heavy, Until: until,
	})
	beforeSeen, beforeUntil, beforeEnded := p.busySeen, p.busyUntil, p.busyEndedAt

	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 {
		t.Fatalf("State = %+v", secs)
	}
	if secs[0].Summary == "即将触发" {
		t.Fatalf("重忙时不该报即将触发: %+v", secs[0])
	}
	if p.busySeen != beforeSeen || p.busyUntil != beforeUntil || p.busyEndedAt != beforeEnded {
		t.Fatalf("状态查询改写了忙碌观察：before=%v/%v/%v after=%v/%v/%v",
			beforeSeen, beforeUntil, beforeEnded, p.busySeen, p.busyUntil, p.busyEndedAt)
	}
}
