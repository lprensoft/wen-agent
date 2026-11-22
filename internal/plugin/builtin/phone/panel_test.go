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
	p.st.Errors = map[string]string{"https://example.com/feed": "超时"}

	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 || len(secs[0].Rows) != 1 {
		t.Fatalf("State = %+v", secs)
	}
	row := secs[0].Rows[0]
	if row.Meta != "#1" || row.Label != "未能取到" || row.Value != "example.com" || row.Detail != "超时" || row.Tone != plugin.StateToneWarn {
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

// 页面问的是「最近刷到了什么」：今天刚刷到、上周发布的文章不能被昨天刷到的
// 十条挤掉，因此按 Seen 而不是发布时间排序。
func TestStateSortsBySeenNotPublished(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	for i := 0; i < 10; i++ {
		p.st.Items = append(p.st.Items, Item{
			Source:    "旧批次",
			Title:     "昨天刷到的消息",
			Published: now.Add(-time.Hour),
			Seen:      now.Add(-24 * time.Hour),
		})
	}
	p.st.Items = append(p.st.Items, Item{
		Source:    "新批次",
		Title:     "刚刷到的旧文章",
		Published: now.Add(-7 * 24 * time.Hour),
		Seen:      now.Add(-time.Minute),
	})

	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 || len(secs[0].Rows) != stateMaxRows || secs[0].More != 1 {
		t.Fatalf("State = %+v", secs)
	}
	if secs[0].Rows[0].Value != "刚刷到的旧文章" {
		t.Errorf("第一条 = %q，期望刚刷到的那条排在最前", secs[0].Rows[0].Value)
	}
}

// 清空订阅源只该停掉后续刷新，不该把手机上已经刷到的消息藏掉。
func TestStateKeepsItemsWhenFeedsRemoved(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	p.feeds = nil
	p.st.Items = []Item{{Source: "旧源", Title: "留着的消息", Seen: now.Add(-time.Hour)}}
	p.st.Errors = map[string]string{"https://example.com/feed": "超时"}

	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 || len(secs[0].Rows) != 1 || secs[0].Empty != "" {
		t.Fatalf("State = %+v", secs)
	}
	if secs[0].Rows[0].Value != "留着的消息" || secs[0].Summary != "共 1 条" {
		t.Errorf("section = %+v", secs[0])
	}
}

// 换源后磁盘上还留着旧源的错误；当前配置里没有的地址不该再算作失败。
func TestStateIgnoresRemovedFeedErrors(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	p.st.Errors = map[string]string{
		"https://example.com/feed":     "超时",
		"https://removed.example/feed": "404",
	}

	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 || len(secs[0].Rows) != 1 {
		t.Fatalf("State = %+v", secs)
	}
	if secs[0].Rows[0].Detail != "超时" || secs[0].Summary != "1 个源取不到" {
		t.Errorf("section = %+v", secs[0])
	}
}

// 同一主机下的两个源都坏了时，主机名与原因可能完全相同，靠配置里的序号区分。
func TestStateDistinguishesSameHostErrors(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	p.feeds = []string{"https://news.example/a", "https://news.example/b"}
	p.st.Errors = map[string]string{
		"https://news.example/a": "404",
		"https://news.example/b": "500",
	}

	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 || len(secs[0].Rows) != 2 {
		t.Fatalf("State = %+v", secs)
	}
	if secs[0].Rows[0].Meta != "#1" || secs[0].Rows[1].Meta != "#2" {
		t.Errorf("rows = %+v，期望用 #1/#2 区分", secs[0].Rows)
	}
	if secs[0].Rows[0].Value != "news.example" || secs[0].Rows[1].Value != "news.example" {
		t.Errorf("rows = %+v", secs[0].Rows)
	}
	if secs[0].Rows[0].Detail != "404" || secs[0].Rows[1].Detail != "500" {
		t.Errorf("rows = %+v", secs[0].Rows)
	}
	if secs[0].Summary != "2 个源取不到" {
		t.Errorf("summary = %q", secs[0].Summary)
	}
}
