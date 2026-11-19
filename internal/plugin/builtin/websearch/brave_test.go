package websearch

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestBraveRequestAndParse(t *testing.T) {
	ch := make(chan capReq, 4)
	p := newPlugin(t, map[string]any{"provider": "brave", "api_key": "test-key"},
		func(w http.ResponseWriter, r *http.Request) {
			// Brave 是 GET：参数在 URL 里，借 capReq 的 body 把它们交回测试
			q := r.URL.Query()
			ch <- capReq{header: r.Header.Clone(), body: map[string]any{
				"q": q.Get("q"), "freshness": q.Get("freshness"), "count": q.Get("count"),
			}}
			fmt.Fprint(w, `{"web":{"results":[
				{"title":"结果一","url":"https://example.com/a","description":"带 <b>高亮</b> 的摘要","page_age":"2026-08-14T03:00:00"},
				{"title":"结果二","url":"https://example.com/b","description":"没有日期的","page_age":""}
			]}}`)
		})

	out, err := run(t, p, map[string]any{"query": "杭州 马拉松", "freshness": "week"})
	if err != nil {
		t.Fatal(err)
	}

	got := <-ch
	if v := got.header.Get("X-Subscription-Token"); v != "test-key" {
		t.Errorf("X-Subscription-Token = %q, want test-key", v)
	}
	if v := got.header.Get("Authorization"); v != "" {
		t.Errorf("Brave must not get a Bearer header, got %q", v)
	}
	if v := got.body["q"]; v != "杭州 马拉松" {
		t.Errorf("q = %v", v)
	}
	if v := got.body["freshness"]; v != "pw" {
		t.Errorf("freshness = %v, want pw", v)
	}
	if v := got.body["count"]; v != "5" {
		t.Errorf("count = %v, want 5", v)
	}

	for _, want := range []string{"结果一", "高亮", "2026-08-14", "结果二", "日期未明"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "<b>") {
		t.Errorf("description HTML not stripped:\n%s", out)
	}
}

func TestBraveNeedsKey(t *testing.T) {
	p := newPlugin(t, map[string]any{"provider": "brave"}, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no key: the request must not go out")
	})
	_, err := run(t, p, map[string]any{"query": "随便"})
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("expected a key-sized error, got %v", err)
	}
}

func TestBraveAuthError(t *testing.T) {
	p := newPlugin(t, map[string]any{"provider": "brave", "api_key": "bad"}, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"SUBSCRIPTION_TOKEN_INVALID"}`)
	})
	_, err := run(t, p, map[string]any{"query": "随便"})
	if err == nil || !strings.Contains(err.Error(), "密钥不对") {
		t.Fatalf("expected an auth error, got %v", err)
	}
}

func TestNormalizeProvider(t *testing.T) {
	cases := map[string]string{
		"brave": "brave", " Brave ": "brave", "searxng": "searxng",
		"tavily": "tavily", "": "tavily", "whatever": "tavily",
	}
	for in, want := range cases {
		if got := normalizeProvider(in); got != want {
			t.Errorf("normalizeProvider(%q) = %q, want %q", in, got, want)
		}
	}
}
