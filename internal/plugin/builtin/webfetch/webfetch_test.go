package webfetch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wen/internal/plugin"
)

func newTool(t *testing.T, cfg map[string]any) plugin.Tool {
	t.Helper()
	p := New()
	if err := p.Init(plugin.InitContext{}, cfg); err != nil {
		t.Fatal(err)
	}
	return p.Tools()[0]
}

func fetch(t *testing.T, tool plugin.Tool, url string) (string, error) {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"url": url})
	return tool.Execute(context.Background(), args)
}

func TestFetchStripsHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><body><h1>你好</h1><p>第一段内容</p>
<script>var x = "不应出现";</script></body></html>`)
	}))
	defer srv.Close()

	out, err := fetch(t, newTool(t, nil), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// 剥标签本身的细节由 internal/plugin 的 TestStripHTML 盯；这里只确认它确实被接上了
	for _, want := range []string{"[HTTP 200]", "你好", "第一段内容"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, ban := range []string{"<script", "不应出现", "<p>"} {
		if strings.Contains(out, ban) {
			t.Errorf("should not contain %q in:\n%s", ban, out)
		}
	}
}

func TestTruncate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Repeat("a", 5000))
	}))
	defer srv.Close()

	out, err := fetch(t, newTool(t, map[string]any{"max_bytes": 1000}), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "已截断") {
		t.Errorf("expected truncation note, got %d bytes", len(out))
	}
}

func TestRejectNonHTTP(t *testing.T) {
	tool := newTool(t, nil)
	for _, u := range []string{"file:///etc/passwd", "ftp://x", "not-a-url"} {
		if _, err := fetch(t, tool, u); err == nil {
			t.Errorf("url %q should be rejected", u)
		}
	}
}
