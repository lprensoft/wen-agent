package people

import (
	"testing"
	"time"

	"wen/internal/plugin"
)

func stateNow() time.Time {
	return time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local)
}

func strPtr(s string) *string { return &s }

// 两个可见域各有一份名册时，应分成两块，并各自带上域标签。
func TestStateListsPeopleByDomain(t *testing.T) {
	p := newTestPlugin(t)
	now := stateNow()
	for _, tag := range []string{"", "inner"} {
		if _, err := p.storeFor(tag).Upsert(Update{
			Name: "林舟", Relation: strPtr("大学同学"), Profile: strPtr("爱做菜"),
		}, now, Limits{MaxPeople: 5}); err != nil {
			t.Fatal(err)
		}
	}

	secs := p.State(plugin.NewStateQuery("en", false, now))
	if len(secs) != 2 {
		t.Fatalf("块数 = %d，期望 2（共享域与里域各一块）", len(secs))
	}
	for _, s := range secs {
		if s.Title != "People" || s.Summary != "1 people" {
			t.Errorf("块 = %+v", s)
		}
		if len(s.Rows) != 1 || s.Rows[0].Value != "林舟" || s.Rows[0].Meta != "Known" {
			t.Errorf("行 = %+v", s.Rows)
		}
		if s.Rows[0].Detail == "" {
			t.Error("关系与设定应出现在 Detail 里")
		}
	}
	if secs[0].Domain != "" || secs[1].Domain != "inner" {
		t.Errorf("域标签 = %q / %q", secs[0].Domain, secs[1].Domain)
	}
}

// 一个域都没有时给一块空态；pinned 查询不产出人物块。
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
