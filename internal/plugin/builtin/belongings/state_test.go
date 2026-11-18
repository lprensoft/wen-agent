package belongings

import (
	"testing"
	"time"

	"wen/internal/plugin"
)

func stateNow() time.Time {
	return time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local)
}

// 一件物品一行、空容器也留一行，容器名放在 Meta 里。
func TestStateListsContainersAndItems(t *testing.T) {
	p := newTestPlugin(t)
	now := stateNow()
	lim := Limits{MaxContainers: 10, MaxItemsPerContainer: 50}
	if _, err := p.storeFor("").Apply("冰箱", []Change{{Name: "鸡蛋", Qty: 6, Note: "今天买的"}}, nil, now, lim); err != nil {
		t.Fatal(err)
	}
	if _, err := p.storeFor("").Apply("衣柜", []Change{{Name: "外套"}}, nil, now, lim); err != nil {
		t.Fatal(err)
	}

	secs := p.State(plugin.NewStateQuery("en", false, now))
	if len(secs) != 1 {
		t.Fatalf("块数 = %d，期望 1", len(secs))
	}
	s := secs[0]
	if s.Title != "Belongings" || len(s.Rows) != 2 {
		t.Fatalf("块 = %+v", s)
	}
	if s.Summary != "2 container(s) · 2 item(s)" {
		t.Errorf("摘要 = %q", s.Summary)
	}
	byContainer := map[string]plugin.StateRow{}
	for _, r := range s.Rows {
		byContainer[r.Meta] = r
	}
	if got := byContainer["冰箱"].Value; got != "鸡蛋×6" {
		t.Errorf("冰箱行 = %q", got)
	}
	if got := byContainer["衣柜"].Detail; got == "" {
		t.Error("外套应带放入日期")
	}
}

func TestStateEmptyAndPinned(t *testing.T) {
	p := newTestPlugin(t)
	now := stateNow()
	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 || secs[0].Empty == "" {
		t.Fatalf("空态 = %+v", secs)
	}
	if got := p.State(plugin.NewStateQuery("zh", true, now)); got != nil {
		t.Errorf("pinned 查询应返回 nil，得到 %+v", got)
	}
}
