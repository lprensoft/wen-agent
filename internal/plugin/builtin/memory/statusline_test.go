package memory

import (
	"path/filepath"
	"strings"
	"testing"

	"wen/internal/plugin"
)

func statusLineOf(t *testing.T, cfg map[string]any) string {
	t.Helper()
	p := New()
	if err := p.Init(plugin.InitContext{StateDir: filepath.Join(t.TempDir(), "memory")}, cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}
	lines := p.StatusLines("")
	if len(lines) != 1 {
		t.Fatalf("状态行数 = %d, want 1: %v", len(lines), lines)
	}
	return lines[0]
}

// 状态行一行讲完当前的记忆行为，不铺陈成一句长叙述。
func TestStatusLineIsCompact(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{
			"定期与压缩前都提炼、开启淡忘",
			map[string]any{"turn_extract": true, "turn_extract_every": 10, "auto_extract": true,
				"decay": true, "decay_blur_days": 30, "decay_forget_days": 90},
			"🧠 记忆：每 10 轮及压缩前提炼 · 30 天淡忘、90 天移出",
		},
		{
			"只定期提炼",
			map[string]any{"turn_extract": true, "turn_extract_every": 5, "auto_extract": false, "decay": false},
			"🧠 记忆：每 5 轮提炼",
		},
		{
			"只在压缩前提炼",
			map[string]any{"turn_extract": false, "auto_extract": true, "decay": false},
			"🧠 记忆：压缩前提炼",
		},
		{
			"都不提炼",
			map[string]any{"turn_extract": false, "auto_extract": false, "decay": false},
			"🧠 记忆：仅模型主动保存",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := statusLineOf(t, c.cfg)
			if got != c.want {
				t.Errorf("状态行 =\n  %s\nwant\n  %s", got, c.want)
			}
			// 原先那行 52 字，改完 33 字。留一点余量，但别让它再长回去
			if n := len([]rune(got)); n > 40 {
				t.Errorf("状态行 %d 字，过长: %s", n, got)
			}
		})
	}
}

// 条数刻意不报：它会泄漏「存在什么」，而 StatusLines 拿不到本轮的可见域。
func TestStatusLineOmitsCount(t *testing.T) {
	got := statusLineOf(t, map[string]any{"turn_extract": true, "turn_extract_every": 10})
	if strings.Contains(got, "条") {
		t.Errorf("状态行不该报条数: %s", got)
	}
}
