package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"wen/internal/runlock"
)

// client 是运行中实例的 HTTP 客户端。
//
// 配置工具在服务运行时必须走这条路而不是直接改状态文件：状态文件由服务端从内存
// 全量重写，外部改动既不会生效，还会在服务端下一次写入时被整个抹掉。
// 走接口则复用服务端已有的校验，改动当场生效（SetConfig 会重新 Init）。
type client struct {
	base string
	http *http.Client
}

// dialAddr 把监听地址转成可连接的地址。
// 0.0.0.0 / :: 是「所有网卡」的写法，不能拿去连接；连回环还能吃到回环免认证，
// 配置工具因此完全不必处理凭据。
func dialAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// reachable 报告该地址上是否有 wen 实例在应答。
func reachable(addr string) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + dialAddr(addr) + "/api/auth/status")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// dial 按配置目录里的登记找到运行中的实例。
// 第二个返回值为 false 表示没有实例在跑（或登记已陈旧），调用方应转入离线模式。
func dial(dir string) (*client, bool) {
	info, ok := runlock.Read(dir)
	if !ok || !reachable(info.Addr) {
		return nil, false
	}
	return &client{
		base: "http://" + dialAddr(info.Addr),
		http: &http.Client{Timeout: 30 * time.Second},
	}, true
}

func (c *client) get(path string, out any) error {
	return c.do(http.MethodGet, path, nil, out)
}

func (c *client) do(method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// 同源标记：服务端会拒绝 Origin 与 Host 不一致的写请求
	req.Header.Set("Origin", c.base)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf(tr("cli.err.http", "服务返回 %s"), resp.Status)
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}
