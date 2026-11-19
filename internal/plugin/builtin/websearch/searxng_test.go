package websearch

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wen/internal/plugin"
)

const searxResults = `{"results":[
	{"url":"https://example.com/a","title":"结果一","content":"摘要一","publishedDate":"2026-08-14T03:00:00+00:00"},
	{"url":"https://example.com/b","title":"结果二","content":"摘要二","publishedDate":null},
	{"url":"https://example.com/c","title":"结果三","content":"摘要三"}
]}`

// newStub 起一个桩服务并给出它的地址。SearXNG 的地址来自 base_url 而不是 endpoint，
// 所以这条路要单独起一个服务，不套 newPlugin。
func newStub(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func newLocal(t *testing.T, cfg map[string]any) *Plugin {
	t.Helper()
	p := New()
	if err := p.Init(plugin.InitContext{Lang: "zh"}, cfg); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSearxngRequestAndParse(t *testing.T) {
	ch := make(chan capReq, 4)
	base := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		ch <- capReq{header: r.Header.Clone(), body: map[string]any{
			"path": r.URL.Path, "q": q.Get("q"), "format": q.Get("format"), "time_range": q.Get("time_range"),
		}}
		fmt.Fprint(w, searxResults)
	})
	p := newLocal(t, map[string]any{"provider": "searxng", "base_url": base})

	out, err := run(t, p, map[string]any{"query": "杭州 马拉松", "freshness": "week"})
	if err != nil {
		t.Fatal(err)
	}
	got := <-ch
	if got.body["path"] != "/search" {
		t.Errorf("path = %v, want /search", got.body["path"])
	}
	if got.body["format"] != "json" {
		t.Errorf("format = %v, want json", got.body["format"])
	}
	if got.body["q"] != "杭州 马拉松" {
		t.Errorf("q = %v", got.body["q"])
	}
	// 实例不认 week，退一档成 month——宽一档是诚实的折衷，退成「不限时间」就是另一件事
	if got.body["time_range"] != "month" {
		t.Errorf("time_range = %v, want month", got.body["time_range"])
	}
	for _, want := range []string{"结果一", "2026-08-14", "结果三", "日期未明"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestSearxngTruncatesResults(t *testing.T) {
	base := newStub(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, searxResults) })
	p := newLocal(t, map[string]any{"provider": "searxng", "base_url": base, "results": 2})

	out, err := run(t, p, map[string]any{"query": "随便"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2 条") || strings.Contains(out, "结果三") {
		t.Errorf("the instance ignores a count, so we truncate:\n%s", out)
	}
}

func TestSearxngJSONDisabled(t *testing.T) {
	base := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `<html>Forbidden</html>`)
	})
	p := newLocal(t, map[string]any{"provider": "searxng", "base_url": base})
	_, err := run(t, p, map[string]any{"query": "随便"})
	if err == nil || !strings.Contains(err.Error(), "search.formats") {
		t.Fatalf("403 should point at the JSON setting, got %v", err)
	}
}

func TestSearxngNeedsBaseURL(t *testing.T) {
	p := newLocal(t, map[string]any{"provider": "searxng"})
	_, err := run(t, p, map[string]any{"query": "随便"})
	if err == nil || !strings.Contains(err.Error(), "实例地址") {
		t.Fatalf("expected a base-url error, got %v", err)
	}
}

func TestSearxngRejectsBadBaseURL(t *testing.T) {
	p := newLocal(t, map[string]any{"provider": "searxng", "base_url": "searx.local"})
	_, err := run(t, p, map[string]any{"query": "随便"})
	if err == nil || !strings.Contains(err.Error(), "http") {
		t.Fatalf("expected a scheme error, got %v", err)
	}
}
