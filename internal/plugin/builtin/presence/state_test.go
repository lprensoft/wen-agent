package presence

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"wen/internal/plugin"
)

func stateTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	p := New()
	p.lang = "zh"
	p.base = filepath.Join(t.TempDir(), "presence")
	return p
}

func TestStateEmpty(t *testing.T) {
	p := stateTestPlugin(t)
	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 || secs[0].Empty == "" || len(secs[0].Rows) != 0 {
		t.Fatalf("空快照的 State = %+v，期望一块带 Empty 的空块", secs)
	}
}

func TestStateRowsFollowFieldOrder(t *testing.T) {
	p := stateTestPlugin(t)
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.Local)
	if _, _, err := p.storeFor("").Apply(map[string]string{
		"activity": "改方案",
		"location": "书房",
		"attire":   "家居服",
	}, now); err != nil {
		t.Fatal(err)
	}

	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 {
		t.Fatalf("State = %+v，期望一块", secs)
	}
	got := secs[0]
	if got.Title != "现场" || got.Summary != "书房 · 家居服 · 改方案" {
		t.Errorf("title=%q summary=%q", got.Title, got.Summary)
	}
	// fieldDefs 的顺序是所在、穿着、姿态、在做；缺的姿态不应占位
	if len(got.Rows) != 3 || got.Rows[0].Value != "书房" || got.Rows[1].Value != "家居服" || got.Rows[2].Value != "改方案" {
		t.Errorf("rows = %+v", got.Rows)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt 不该为零")
	}
}

// 读失败不能显示成「还没有记录现场」——那是在肯定地告诉用户没有数据。
func TestStateReadFailureIsNotAnEmptyState(t *testing.T) {
	p := stateTestPlugin(t)
	if err := os.MkdirAll(p.base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.base, "presence.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if secs := p.State(plugin.StateQuery{Lang: "zh"}); len(secs) != 0 {
		t.Fatalf("读失败时 State = %+v，期望不报空状态", secs)
	}
}
