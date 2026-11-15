package relationship

import (
	"path/filepath"
	"testing"
	"time"

	"wen/internal/plugin"
)

func stateTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	p := New()
	p.lang = "zh"
	p.base = filepath.Join(t.TempDir(), "relationship")
	return p
}

func TestStateRowsAndPinnedOnly(t *testing.T) {
	p := stateTestPlugin(t)
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.Local)
	stage, my, their, recent := "熟悉", "小名", "你", "一起吃了饭"
	if _, _, err := p.storeFor("").Apply(Update{
		Stage: &stage, MyCall: &my, TheirCall: &their, Recent: &recent,
	}, now); err != nil {
		t.Fatal(err)
	}

	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 {
		t.Fatalf("State = %+v", secs)
	}
	got := secs[0]
	if got.Title != "关系" || got.Summary != "熟悉" || got.Domain != "" {
		t.Errorf("title=%q summary=%q domain=%q", got.Title, got.Summary, got.Domain)
	}
	if len(got.Rows) < 4 {
		t.Fatalf("rows = %+v", got.Rows)
	}
	// 顶部条不该收到关系块
	if pinned := p.State(plugin.StateQuery{Lang: "zh", PinnedOnly: true}); len(pinned) != 0 {
		t.Fatalf("PinnedOnly = %+v，期望空", pinned)
	}
}

func TestStateEmpty(t *testing.T) {
	p := stateTestPlugin(t)
	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 || secs[0].Empty == "" {
		t.Fatalf("空关系的 State = %+v", secs)
	}
}
