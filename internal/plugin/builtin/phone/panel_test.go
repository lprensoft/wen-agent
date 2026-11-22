package phone

import (
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

func stateTestPlugin(t *testing.T, now time.Time) *Plugin {
	t.Helper()
	p := New()
	p.lang = "zh"
	p.feeds = []string{"https://example.com/feed"}
	p.st = State{LastRefresh: now.Add(-time.Hour)}
	return p
}

func TestStateListsRecentItems(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	last := p.st.LastRefresh
	p.st.Items = []Item{
		{Source: "数码频道", Title: "旧消息", Summary: "第一行\n第二行", Seen: now.Add(-2 * time.Hour)},
		{Source: "本地新闻", Title: "新消息", Summary: "一件新鲜事", Seen: now.Add(-10 * time.Minute)},
	}

	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 {
		t.Fatalf("State = %+v", secs)
	}
	got := secs[0]
	if got.Title != "手机" || !got.Global || got.Summary != "共 2 条" {
		t.Errorf("section = %+v", got)
	}
	if !got.UpdatedAt.Equal(last) {
		t.Errorf("UpdatedAt = %v，期望 %v", got.UpdatedAt, last)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("rows = %+v", got.Rows)
	}
	if got.Rows[0].Value != "新消息" || got.Rows[0].Meta != "本地新闻" || got.Rows[0].Detail != "一件新鲜事" {
		t.Errorf("第一条 = %+v", got.Rows[0])
	}
	if got.Rows[1].Detail != "第一行 第二行" {
		t.Errorf("摘要里的换行应折成空格：%q", got.Rows[1].Detail)
	}
}

func TestStateLimitsRowsAndReportsMore(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	for i := 0; i < 12; i++ {
		p.st.Items = append(p.st.Items, Item{
			Source: "源", Title: "标题", Seen: now.Add(-time.Duration(i) * time.Minute),
		})
	}
	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 {
		t.Fatalf("State = %+v", secs)
	}
	if len(secs[0].Rows) != stateMaxRows || secs[0].More != 2 {
		t.Errorf("rows=%d more=%d，期望 %d 条与 2 条省略", len(secs[0].Rows), secs[0].More, stateMaxRows)
	}
	if secs[0].Summary != "共 12 条" {
		t.Errorf("summary = %q", secs[0].Summary)
	}
}

func TestStateListsFailedFeeds(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	p.st.Errors = map[string]string{"https://bad.example/feed": "超时"}

	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 || len(secs[0].Rows) != 1 {
		t.Fatalf("State = %+v", secs)
	}
	row := secs[0].Rows[0]
	if row.Label != "未能取到" || row.Value != "bad.example" || row.Detail != "超时" || row.Tone != plugin.StateToneWarn {
		t.Errorf("row = %+v", row)
	}
	if secs[0].Summary != "1 个源取不到" {
		t.Errorf("summary = %q", secs[0].Summary)
	}
}

func TestStateEmptyAndNoFeeds(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.Local)

	p := stateTestPlugin(t, now)
	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 || !strings.Contains(secs[0].Empty, "还没刷到什么") {
		t.Fatalf("没有条目时 State = %+v", secs)
	}

	p2 := New()
	p2.lang = "zh"
	secs = p2.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 || !strings.Contains(secs[0].Empty, "没有配置订阅源") {
		t.Fatalf("没有订阅源时 State = %+v", secs)
	}
}

func TestStatePinnedOnlySkipsEverything(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	if secs := p.State(plugin.NewStateQuery("zh", true, now)); len(secs) != 0 {
		t.Fatalf("PinnedOnly = %+v", secs)
	}
}
