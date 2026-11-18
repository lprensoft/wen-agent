package scene

import (
	"testing"
	"time"

	"wen/internal/plugin"
)

// 每条场景一行，标题按语言取，描述进 Detail。
func TestStateListsScenes(t *testing.T) {
	p := newTestPlugin(t, nil)
	if _, err := p.storeFor("").Save(Scene{Name: "城南书店", Detail: "两层，靠窗有一排旧书"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := p.storeFor("").Save(Scene{Name: "家", Detail: "阳台朝南"}, true); err != nil {
		t.Fatal(err)
	}

	secs := p.State(plugin.NewStateQuery("en", false, time.Now()))
	if len(secs) != 1 {
		t.Fatalf("块数 = %d，期望 1", len(secs))
	}
	s := secs[0]
	if s.Title != "Scenes" || s.Summary != "2 places" || len(s.Rows) != 2 {
		t.Fatalf("块 = %+v", s)
	}
	if s.Rows[0].Value != "城南书店" || s.Rows[0].Detail == "" {
		t.Errorf("行 = %+v", s.Rows[0])
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
