package stylewatch

import (
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

func TestStateShowsToday(t *testing.T) {
	now := time.Date(2026, 3, 5, 14, 0, 0, 0, time.Local)
	p := New()
	p.statsMu.Lock()
	p.st = stats{Days: []dayStat{{
		Date: dateOf(now),
		Interactive: bucket{
			Turns: 2, HitTurns: 1, Hits: map[string]int{"emoji": 1},
			Runes: 120, BracketSum: 0.6,
		},
		Background: bucket{Turns: 1, Hits: map[string]int{"closing_cliche": 1}, Runes: 40},
	}}}
	p.statsMu.Unlock()

	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 {
		t.Fatalf("State = %+v", secs)
	}
	got := secs[0]
	if got.Title != "文风" || !got.Global || got.Summary != "今日" {
		t.Errorf("section = %+v", got)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("rows = %+v", got.Rows)
	}
	if got.Rows[0].Label != "真人对话" || !strings.Contains(got.Rows[0].Value, "2 轮") {
		t.Errorf("真人行 = %+v", got.Rows[0])
	}
	if got.Rows[1].Label != "后台轮次" || got.Rows[1].Tone != plugin.StateToneMuted {
		t.Errorf("后台行 = %+v", got.Rows[1])
	}
}

func TestStateEmpty(t *testing.T) {
	now := time.Date(2026, 3, 5, 14, 0, 0, 0, time.Local)
	p := New()
	p.statsMu.Lock()
	p.st = stats{}
	p.statsMu.Unlock()

	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 || secs[0].Empty == "" || len(secs[0].Rows) != 0 {
		t.Fatalf("State = %+v，期望一块带 Empty 的空块", secs)
	}
}

func TestStatePinnedOnlySkipsEverything(t *testing.T) {
	now := time.Date(2026, 3, 5, 14, 0, 0, 0, time.Local)
	p := New()
	if secs := p.State(plugin.NewStateQuery("zh", true, now)); len(secs) != 0 {
		t.Fatalf("PinnedOnly = %+v", secs)
	}
}
