package health

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

func stateTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	p := New()
	p.lang = "zh"
	p.base = filepath.Join(t.TempDir(), "health")
	return p
}

func TestStateActiveCondition(t *testing.T) {
	p := stateTestPlugin(t)
	now := time.Now()
	if _, err := p.storeFor("").Add("感冒", "有点不舒服", now, 3, "硬扛", now, p.snapshot().limits()); err != nil {
		t.Fatal(err)
	}

	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 {
		t.Fatalf("State = %+v", secs)
	}
	got := secs[0]
	if got.Title != "健康状况" || got.Summary != "共 1 个状况" || len(got.Rows) != 1 {
		t.Errorf("section = %+v", got)
	}
	if got.Rows[0].Value != "感冒" {
		t.Errorf("rows = %+v", got.Rows)
	}
	// 行已经有状况名，Detail 不该再重复一遍。
	if strings.Contains(got.Rows[0].Detail, "感冒") {
		t.Errorf("Detail 重复了状况名: %q", got.Rows[0].Detail)
	}
	if pinned := p.State(plugin.StateQuery{Lang: "zh", PinnedOnly: true}); len(pinned) != 0 {
		t.Fatalf("PinnedOnly = %+v，期望空", pinned)
	}
}

func TestStateEmpty(t *testing.T) {
	p := stateTestPlugin(t)
	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 0 {
		t.Fatalf("空身体的 State = %+v，期望不出现空块", secs)
	}
}
