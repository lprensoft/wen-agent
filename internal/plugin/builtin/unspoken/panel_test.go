package unspoken

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
	p.base = filepath.Join(t.TempDir(), "unspoken")
	return p
}

func TestStateCollapsedRows(t *testing.T) {
	p := stateTestPlugin(t)
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.Local)
	if _, err := p.storeFor("").Keep("其实有点在意", now, p.maxEntries); err != nil {
		t.Fatal(err)
	}

	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 {
		t.Fatalf("State = %+v", secs)
	}
	got := secs[0]
	if got.Title != "心里话" || got.Summary != "1 条" || !got.Collapsed {
		t.Errorf("section = %+v", got)
	}
	if len(got.Rows) != 1 || got.Rows[0].Value != "其实有点在意" {
		t.Errorf("rows = %+v", got.Rows)
	}
	if pinned := p.State(plugin.StateQuery{Lang: "zh", PinnedOnly: true}); len(pinned) != 0 {
		t.Fatalf("PinnedOnly = %+v，期望空", pinned)
	}
}

func TestStateEmpty(t *testing.T) {
	p := stateTestPlugin(t)
	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 || secs[0].Empty == "" {
		t.Fatalf("空心里话的 State = %+v", secs)
	}
}
