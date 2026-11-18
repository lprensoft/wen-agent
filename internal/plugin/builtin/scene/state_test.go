package scene

import (
	"testing"
	"time"

	"wen/internal/plugin"
)

// 每条场景一行，最近更新的排在前面；描述进 Detail。
func TestStateListsScenes(t *testing.T) {
	p := newTestPlugin(t, nil)
	base := time.Date(2026, 11, 18, 10, 0, 0, 0, time.Local)
	if _, err := p.storeFor("").SaveAt(Scene{Name: "城南书店", Detail: "两层，靠窗有一排旧书"}, true, base); err != nil {
		t.Fatal(err)
	}
	if _, err := p.storeFor("").SaveAt(Scene{Name: "家", Detail: "阳台朝南"}, true, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	secs := p.State(plugin.NewStateQuery("en", false, base.Add(2*time.Hour)))
	if len(secs) != 1 {
		t.Fatalf("块数 = %d，期望 1", len(secs))
	}
	s := secs[0]
	if s.Title != "Scenes" || s.Summary != "2 places" || len(s.Rows) != 2 {
		t.Fatalf("块 = %+v", s)
	}
	if s.Rows[0].Value != "家" || s.Rows[0].Detail == "" {
		t.Errorf("最近更新的一行 = %+v", s.Rows[0])
	}
	if s.Rows[1].Value != "城南书店" {
		t.Errorf("较早的一行 = %+v", s.Rows[1])
	}
}

// 场景只列最近 15 处：编辑描述要带现值，三十条长描述会顶穿每插件字节上限。
func TestStateCapsScenes(t *testing.T) {
	p := newTestPlugin(t, nil)
	base := time.Date(2026, 11, 18, 10, 0, 0, 0, time.Local)
	for i := 0; i < 20; i++ {
		name := "场景" + string(rune('A'+i))
		if _, err := p.storeFor("").SaveAt(Scene{Name: name, Detail: "描述"}, true, base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	secs := p.State(plugin.NewStateQuery("zh", false, base.Add(time.Hour)))
	if len(secs) != 1 {
		t.Fatalf("块数 = %d", len(secs))
	}
	if len(secs[0].Rows) != stateMaxRows || secs[0].More != 20-stateMaxRows {
		t.Fatalf("rows=%d more=%d，期望 %d / %d", len(secs[0].Rows), secs[0].More, stateMaxRows, 20-stateMaxRows)
	}
}

func TestStateEmptyAndPinned(t *testing.T) {
	p := newTestPlugin(t, nil)
	secs := p.State(plugin.NewStateQuery("zh", false, time.Now()))
	if len(secs) != 1 || secs[0].Empty == "" {
		t.Fatalf("空态 = %+v", secs)
	}
	if got := p.State(plugin.NewStateQuery("zh", true, time.Now())); got != nil {
		t.Errorf("pinned 查询应返回 nil，得到 %+v", got)
	}
}
