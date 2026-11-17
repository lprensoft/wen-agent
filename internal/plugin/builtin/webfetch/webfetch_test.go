package webfetch

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

func newTool(t *testing.T, cfg map[string]any) plugin.Tool {
	t.Helper()
	p := New()
	// httptest 的地址就是 127.0.0.1，而生产路径一律不许抓本机；地址检查本身由
	// TestRejectPrivateAddress 单独盯。
	p.allowLocal = true
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

func TestRejectPrivateAddress(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	// 不置 allowLocal：这就是生产路径上的那份配置
	p := New()
	if err := p.Init(plugin.InitContext{}, nil); err != nil {
		t.Fatal(err)
	}
	tool := p.Tools()[0]

	for _, u := range []string{
		srv.URL,                  // 127.0.0.1
		"http://localhost:8080/", // 名字指向本机
		"http://169.254.169.254/latest/meta-data/", // 云厂商元数据
		"http://[::1]:8080/",                       // IPv6 回环
		"http://10.0.0.1/admin",                    // 私网
	} {
		if _, err := fetch(t, tool, u); err == nil {
			t.Errorf("%s should have been refused", u)
		} else if !strings.Contains(err.Error(), "公网") {
			t.Errorf("%s: expected the public-only message, got %v", u, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("a refused address must not reach the server, got %d requests", n)
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
