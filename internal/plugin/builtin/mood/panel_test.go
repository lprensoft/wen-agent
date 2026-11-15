package mood

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
	p.base = filepath.Join(t.TempDir(), "mood")
	return p
}

func TestStateSummaryAndPinned(t *testing.T) {
	p := stateTestPlugin(t)
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.Local)
	if _, _, err := p.storeFor("").Apply(20, 30, 0, "有点累", now); err != nil {
		t.Fatal(err)
	}

	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 {
		t.Fatalf("State = %+v", secs)
	}
	got := secs[0]
	if got.Title != "心情" || got.Summary != "心情：有点累" || !got.Pinned || got.PinnedOrder != 2 {
		t.Errorf("section = %+v", got)
	}
	if len(got.Rows) != 1 || got.Rows[0].Value != "愉快" || got.Rows[0].Detail != "有点累" {
		t.Errorf("rows = %+v", got.Rows)
	}
	// PinnedOnly 仍要拿到这一块
	if pinned := p.State(plugin.StateQuery{Lang: "zh", PinnedOnly: true}); len(pinned) != 1 || !pinned[0].Pinned {
		t.Fatalf("PinnedOnly = %+v", pinned)
	}
}

func TestStateEmpty(t *testing.T) {
	p := stateTestPlugin(t)
	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 || secs[0].Empty == "" {
		t.Fatalf("空心情的 State = %+v", secs)
	}
}
