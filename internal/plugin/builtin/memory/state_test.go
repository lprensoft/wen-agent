package memory

import (
	"path/filepath"
	"testing"
	"time"

	"wen/internal/plugin"
)

func stateTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	p := New()
	p.lang = "zh"
	p.library = libraryGlobal
	p.libBase = filepath.Join(t.TempDir(), "memories")
	p.store = NewStore("zh", p.libBase)
	return p
}

func TestStateEmpty(t *testing.T) {
	p := stateTestPlugin(t)
	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 || secs[0].Empty == "" {
		t.Fatalf("空记忆库的 State = %+v，期望一块带 Empty 的空块", secs)
	}
}

func TestStateIndexRows(t *testing.T) {
	p := stateTestPlugin(t)
	if _, err := p.storeFor("").Save(Entry{Name: "喜欢乌龙茶", Description: "不加糖", Type: "偏好"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := p.storeFor("").Save(Entry{Name: "在上海做市场策划", Description: "最近在赶提案", Type: "事实"}, false); err != nil {
		t.Fatal(err)
	}

	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 {
		t.Fatalf("State = %+v，期望一块", secs)
	}
	got := secs[0]
	if got.Title != "记忆索引" || got.Summary != "2 条" || len(got.Rows) != 2 {
		t.Fatalf("title=%q summary=%q rows=%+v", got.Title, got.Summary, got.Rows)
	}
	types := map[string]string{}
	for _, r := range got.Rows {
		types[r.Value] = r.Meta
	}
	if types["喜欢乌龙茶"] != "偏好" || types["在上海做市场策划"] != "事实" {
		t.Errorf("分类标签不对: %+v", types)
	}
}

func TestStateTypeLabelFollowsLanguage(t *testing.T) {
	p := stateTestPlugin(t)
	if _, err := p.storeFor("").Save(Entry{Name: "喜欢乌龙茶", Type: "偏好"}, false); err != nil {
		t.Fatal(err)
	}
	secs := p.State(plugin.StateQuery{Lang: "en"})
	if len(secs) != 1 || len(secs[0].Rows) != 1 {
		t.Fatalf("State = %+v", secs)
	}
	if got := secs[0].Rows[0].Meta; got != "Preference" {
		t.Errorf("英文分类 = %q，期望 Preference", got)
	}
}

// 日记读已收束的时间线：新的在前，当日事实放进 Detail。
func TestStateDiaryRows(t *testing.T) {
	p := stateTestPlugin(t)
	dir := plugin.DomainDir(p.libBase, "")
	p.appendTimeline(dir, timelineEntry{Date: "2026-11-16", Summary: "陪她去医院"})
	p.appendTimeline(dir, timelineEntry{Date: "2026-11-17", Summary: "一起炖了排骨汤", Facts: []string{"天气：小雨"}})

	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 {
		t.Fatalf("State = %+v，期望只有日记一块", secs)
	}
	got := secs[0]
	if got.Title != "日记" || got.Summary != "最近 2 天" || len(got.Rows) != 2 {
		t.Fatalf("日记块 = %+v", got)
	}
	if got.Rows[0].Meta != "2026-11-17" || got.Rows[0].Value != "一起炖了排骨汤" {
		t.Errorf("最新一行 = %+v", got.Rows[0])
	}
	if got.Rows[0].Detail != "天气：小雨" {
		t.Errorf("当日事实 = %q", got.Rows[0].Detail)
	}
	if got.Rows[1].Meta != "2026-11-16" {
		t.Errorf("较早一行 = %+v", got.Rows[1])
	}
}

// 时间线空着时不出日记块：没有记录就不该给每个用户多摆一块空版面。
func TestStateHasNoDiaryWhenEmpty(t *testing.T) {
	p := stateTestPlugin(t)
	p.timeline = true
	secs := p.State(plugin.StateQuery{Lang: "zh"})
	if len(secs) != 1 || secs[0].Key != "index" {
		t.Fatalf("State = %+v，期望只有索引一块", secs)
	}
}

// State 按更新时间重排只是为了展示，不能把 Store 的缓存顺序改掉——
// 记忆索引与召回共用那份缓存，顺序一乱，别处的行为也会跟着变。
func TestStateDoesNotReorderStoreCache(t *testing.T) {
	p := stateTestPlugin(t)
	for _, name := range []string{"A", "B", "C"} {
		if _, err := p.storeFor("").Save(Entry{Name: name, Type: "事实"}, false); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}

	before, err := p.storeFor("").List()
	if err != nil {
		t.Fatal(err)
	}
	_ = p.State(plugin.StateQuery{Lang: "zh"})
	after, err := p.storeFor("").List()
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i].Slug != after[i].Slug {
			t.Fatalf("缓存顺序被 State 改动了：before=%v after=%v", before, after)
		}
	}
}
