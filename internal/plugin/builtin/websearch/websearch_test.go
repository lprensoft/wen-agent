package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"wen/internal/plugin"
)

// newPlugin 起一个桩服务并把插件指向它。桩而不是真打 api.tavily.com：真打既慢、不可
// 复现，还会把开发机的 IP 算进限流。
func newPlugin(t *testing.T, cfg map[string]any, handler http.HandlerFunc) *Plugin {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p := New()
	if err := p.Init(plugin.InitContext{Lang: "zh"}, cfg); err != nil {
		t.Fatal(err)
	}
	p.endpoint = srv.URL
	return p
}

func run(t *testing.T, p *Plugin, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return p.Tools()[0].Execute(context.Background(), raw)
}

const twoResults = `{"results":[
	{"title":"杭州马拉松","url":"https://www.zhihu.com/question/1","content":"报名 9 月 1 日开始，<b>名额有限</b>","published_date":"Tue, 11 Mar 2025 17:00:00 GMT"},
	{"title":"","url":"https://example.com/x","content":"没有日期的页面","published_date":""}
]}`

func TestSearchRendersSnippets(t *testing.T) {
	var gotHeader http.Header
	var gotBody map[string]any
	p := newPlugin(t, nil, func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, twoResults)
	})

	out, err := run(t, p, map[string]any{"query": "  杭州  马拉松  "})
	if err != nil {
		t.Fatal(err)
	}

	// 无 key 时走 keyless：这条鉴权头发错了，请求会在真机上 401，而桩服务看不出来
	if got := gotHeader.Get("X-Tavily-Access-Mode"); got != "keyless" {
		t.Errorf("X-Tavily-Access-Mode = %q, want keyless", got)
	}
	if got := gotHeader.Get("Authorization"); got != "" {
		t.Errorf("keyless must not send Authorization, got %q", got)
	}
	if got := gotBody["query"]; got != "杭州 马拉松" {
		t.Errorf("query = %v, want the whitespace collapsed", got)
	}
	if got := gotBody["max_results"]; got != float64(defaultResults) {
		t.Errorf("max_results = %v, want %d", got, defaultResults)
	}
	if got := gotBody["include_answer"]; got != false {
		t.Errorf("include_answer = %v, want false", got)
	}
	if _, ok := gotBody["time_range"]; ok {
		t.Errorf("no freshness given, time_range should be absent, got %v", gotBody["time_range"])
	}

	for _, want := range []string{
		"2 条", "杭州马拉松", "zhihu.com", "2025-03-11", "名额有限",
		"（无标题）", "日期未明", "链接：https://www.zhihu.com/question/1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// 摘要里的标签要剥掉，否则模型的上下文里会混进 <b> 这种东西
	if strings.Contains(out, "<b>") {
		t.Errorf("snippet HTML not stripped:\n%s", out)
	}
}

func TestSearchKeyAndFreshness(t *testing.T) {
	var gotHeader http.Header
	var gotBody map[string]any
	p := newPlugin(t, map[string]any{"api_key": "tvly-test"}, func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprint(w, twoResults)
	})

	if _, err := run(t, p, map[string]any{"query": "新闻", "freshness": "week"}); err != nil {
		t.Fatal(err)
	}
	if got := gotHeader.Get("Authorization"); got != "Bearer tvly-test" {
		t.Errorf("Authorization = %q, want Bearer tvly-test", got)
	}
	if got := gotHeader.Get("X-Tavily-Access-Mode"); got != "" {
		t.Errorf("a key was given, keyless header should be absent, got %q", got)
	}
	if got := gotBody["time_range"]; got != "week" {
		t.Errorf("time_range = %v, want week", got)
	}
}

func TestSearchEmptyResults(t *testing.T) {
	p := newPlugin(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"results":[]}`)
	})
	out, err := run(t, p, map[string]any{"query": "查不到的东西"})
	if err != nil {
		t.Fatalf("an empty result is not a failure: %v", err)
	}
	if !strings.Contains(out, "没有搜到结果") {
		t.Errorf("expected an explicit empty note, got:\n%s", out)
	}
}

func TestSearchCache(t *testing.T) {
	var calls atomic.Int32
	p := newPlugin(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, twoResults)
	})
	args := map[string]any{"query": "同一件事"}
	if _, err := run(t, p, args); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, p, args)
	if err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the same query was sent %d times, want 1", n)
	}
	if !strings.Contains(out, "刚才") {
		t.Errorf("a cached answer should say how old it is, got:\n%s", out)
	}
}

func TestSearchHourlyCap(t *testing.T) {
	var calls atomic.Int32
	p := newPlugin(t, map[string]any{"max_per_hour": 1}, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, twoResults)
	})
	if _, err := run(t, p, map[string]any{"query": "第一件"}); err != nil {
		t.Fatal(err)
	}
	_, err := run(t, p, map[string]any{"query": "第二件"})
	if err == nil {
		t.Fatal("the second distinct query should have been refused")
	}
	if !strings.Contains(err.Error(), "已经查了 1 次") {
		t.Errorf("unexpected error: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("a refused call should not reach the network, got %d calls", n)
	}
}

func TestSearchStatusErrors(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "密钥不对"},
		{http.StatusTooManyRequests, "限流"},
		{http.StatusBadGateway, "搜索失败（HTTP 502）"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.status), func(t *testing.T) {
			p := newPlugin(t, nil, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				fmt.Fprint(w, `{"detail":"nope"}`)
			})
			_, err := run(t, p, map[string]any{"query": "随便"})
			if err == nil {
				t.Fatalf("HTTP %d should be an error", c.status)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestSearchBadResponse(t *testing.T) {
	p := newPlugin(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html>maintenance</html>`)
	})
	_, err := run(t, p, map[string]any{"query": "随便"})
	if err == nil || !strings.Contains(err.Error(), "读不懂") {
		t.Fatalf("expected a readable error, got %v", err)
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	p := newPlugin(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an empty query must not reach the network")
	})
	if _, err := run(t, p, map[string]any{"query": "   "}); err == nil {
		t.Fatal("an empty query should be an error")
	}
}

func TestNormalizeFreshness(t *testing.T) {
	for in, want := range map[string]string{
		"day": "day", " WEEK ": "week", "today": "", "": "",
	} {
		if got := normalizeFreshness(in); got != want {
			t.Errorf("normalizeFreshness(%q) = %q, want %q", in, got, want)
		}
	}
}
