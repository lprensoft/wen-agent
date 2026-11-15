package heartbeat

import (
	"testing"
	"time"

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
	if got.Title != "心跳" || !got.Pinned || got.PinnedOrder != 3 || got.Summary == "" {
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
