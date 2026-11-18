package growth

import (
	"testing"
	"time"

	"wen/internal/plugin"
)

// 待批准排在最前并标成需要注意，核心与枝叶各带编号。
func TestStateListsGrowth(t *testing.T) {
	p, _ := newTestPlugin(t, nil, nil, nil)
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local)
	if _, err := p.storeFor("").Update(func(st *State) (bool, []LogEntry, error) {
		st.Details = []Entry{{ID: "d1", Text: "开始喝无糖可乐", Basis: "上周说想少糖", Why: "还是那个爱尝新的人"}}
		st.Core = []Entry{{ID: "c2", Text: "遇事先听完再开口", Basis: "几次争执之后", Why: "本来就不喜欢抢话"}}
		st.Pending = &Entry{ID: "c3", Nonce: "abc", Text: "说话不再绕弯", Basis: "反复被提醒", Why: "骨子里其实直", Since: now}
		return true, nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	secs := p.State(plugin.NewStateQuery("en", false, now))
	if len(secs) != 1 {
		t.Fatalf("块数 = %d，期望 1", len(secs))
	}
	s := secs[0]
	if s.Title != "Growth" || len(s.Rows) != 3 {
		t.Fatalf("块 = %+v", s)
	}
	if s.Summary != "1 small change(s) · 1 bigger change(s) · awaiting approval" {
		t.Errorf("摘要 = %q", s.Summary)
	}
	if s.Rows[0].Label != "Awaiting approval" || s.Rows[0].Tone != plugin.StateToneWarn {
		t.Errorf("待批准行 = %+v", s.Rows[0])
	}
	if s.Rows[0].Value != "说话不再绕弯" || s.Rows[0].Meta != "2026-11-18" {
		t.Errorf("待批准行 = %+v", s.Rows[0])
	}
	if s.Rows[1].Label != "Core" || s.Rows[1].Meta != "c2" {
		t.Errorf("核心行 = %+v", s.Rows[1])
	}
	if s.Rows[2].Label != "Small change" || s.Rows[2].Value != "开始喝无糖可乐" {
		t.Errorf("枝叶行 = %+v", s.Rows[2])
	}
}

func TestStateEmptyAndPinned(t *testing.T) {
	p, _ := newTestPlugin(t, nil, nil, nil)
	now := time.Now()
	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 || secs[0].Empty == "" {
		t.Fatalf("空态 = %+v", secs)
	}
	if got := p.State(plugin.NewStateQuery("zh", true, now)); got != nil {
		t.Errorf("pinned 查询应返回 nil，得到 %+v", got)
	}
}
