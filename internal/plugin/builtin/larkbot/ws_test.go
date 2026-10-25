package larkbot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestWSStopsAfterConnected 盯住「长连接建立之后还停得下来」：SDK v3.9.10 的 Start 连上
// 之后执行一个空的 select{}，不看 ctx，于是 Stop 永远卡在 wg.Wait() 上——停用插件、改
// 配置、退出进程都会挂住。这里用假的开放平台让它真的连上，再取消 ctx。
func TestWSStopsAfterConnected(t *testing.T) {
	connected := make(chan struct{}, 1)
	up := websocket.Upgrader{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/callback/ws/endpoint"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"URL": "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?device_id=1&service_id=1"},
			})
		case r.URL.Path == "/ws":
			c, err := up.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer c.Close()
			select {
			case connected <- struct{}{}:
			default:
			}
			for {
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := NewFeishu()
	ctx, cancel := context.WithCancel(context.Background())
	p.runWS(ctx, "cli_test", "123", srv.URL)
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("没有连上假的长连接服务")
	}
	cancel()

	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("长连接建立后取消 ctx，Start 没有返回：Stop 会永远卡住")
	}
}
