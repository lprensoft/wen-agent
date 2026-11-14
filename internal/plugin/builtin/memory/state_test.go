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
