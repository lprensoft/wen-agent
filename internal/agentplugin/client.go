package agentplugin

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"wen/internal/version"
)

// 连接与调用的默认时限。两者分开：握手慢多半是解释器在冷启动（npx 第一次要下包），
// 而调用慢是那件事本身在做——一个抓网页的工具跑十几秒是正常的。
const (
	defaultConnectTimeout = 30 * time.Second
	defaultCallTimeout    = 120 * time.Second
)

// client 是与一台 MCP server 的连接，按需拉起、断了自愈。
//
// 懒启动是硬要求而不是优化：插件在注册时就要交出工具清单（插件系统要查全局重名），
// 那一步握着写锁，在里面拉起子进程等于让整个服务停在启动那一秒。清单因此来自安装时
// 落盘的缓存，子进程留到第一次真的要调用时再起。
type client struct {
	cfg            ResolvedServer
	connectTimeout time.Duration
	callTimeout    time.Duration

	mu      sync.Mutex
	session *mcp.ClientSession
	closed  bool
}

func newClient(cfg ResolvedServer) *client {
	return &client{cfg: cfg, connectTimeout: defaultConnectTimeout, callTimeout: defaultCallTimeout}
}

// connect 建立一次新连接。
//
// 握手用的是 context.Background 而不是调用方的 ctx：SDK 把 ctx 交给会话，而调用方的
// ctx 在这一轮对话结束时就会被取消——拿它连出来的 server 会跟着一起死，下一轮又要
// 重连一次。超时因此自己看着办：连接在旁边的 goroutine 里做，这边等一个计时器，
// 迟到的连接直接关掉。
func (c *client) connect() (*mcp.ClientSession, error) {
	transport, err := c.transport()
	if err != nil {
		return nil, err
	}

	impl := &mcp.Implementation{Name: "wen", Version: version.Version}
	mc := mcp.NewClient(impl, nil)

	type result struct {
		s   *mcp.ClientSession
		err error
	}
	ch := make(chan result, 1)
	go func() {
		s, err := mc.Connect(context.Background(), transport, nil)
		ch <- result{s, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("连接 %s 失败：%w", c.cfg.Name, r.err)
		}
		return r.s, nil
	case <-time.After(c.connectTimeout):
		// 迟到的连接没人接手，等它到了就地关掉，别把子进程留在那儿
		go func() {
			if r := <-ch; r.s != nil {
				_ = r.s.Close()
			}
		}()
		return nil, fmt.Errorf("连接 %s 超时（%s）", c.cfg.Name, c.connectTimeout)
	}
}

// transport 按声明的传输方式造一个连接器。
//
// 三种都由 SDK 提供，本地只负责把 mcp.json 里的声明翻译过去：stdio 拉子进程，
// 两种 HTTP 各自建连接。sse 是 2024-11-05 那版协议留下的，已废弃，接着只是为了
// 让还在用它的 server 能用——不必为「识别得出、就是连不上」这个中间状态再留一版。
func (c *client) transport() (mcp.Transport, error) {
	switch c.cfg.Type {
	case TransportStdio:
		cmd := exec.Command(c.cfg.Command, c.cfg.Args...)
		cmd.Dir = c.cfg.Dir
		cmd.Env = c.cfg.EnvList()
		return &mcp.CommandTransport{Command: cmd}, nil
	case TransportHTTP:
		return &mcp.StreamableClientTransport{
			Endpoint:   c.cfg.URL,
			HTTPClient: httpClient(c.cfg.Headers),
		}, nil
	case TransportSSE:
		return &mcp.SSEClientTransport{
			Endpoint:   c.cfg.URL,
			HTTPClient: httpClient(c.cfg.Headers),
		}, nil
	}
	return nil, fmt.Errorf("不认识的传输方式 %q", c.cfg.Type)
}

// httpClient 造一个会带上声明头的 HTTP 客户端。
//
// 不设 Timeout：MCP 的 Streamable HTTP 会挂一条长连接收服务端主动发来的消息，
// 给客户端设总超时会把它按时掐断。每次调用的时限由 ctx 管（见 callOnce）。
func httpClient(headers map[string]string) *http.Client {
	if len(headers) == 0 {
		return nil // 交给 SDK 用默认的
	}
	return &http.Client{Transport: headerRoundTripper{headers: headers}}
}

// headerRoundTripper 给每个请求加上插件包声明的头。
type headerRoundTripper struct {
	headers map[string]string
}

func (h headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// 复制一份再改：RoundTripper 按约定不得改动传进来的请求
	clone := req.Clone(req.Context())
	for k, v := range h.headers {
		clone.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(clone)
}

// ensure 返回可用的会话，没有就建一个。
func (c *client) ensure() (*mcp.ClientSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("插件已停止")
	}
	if c.session != nil {
		return c.session, nil
	}
	s, err := c.connect()
	if err != nil {
		return nil, err
	}
	c.session = s
	return s, nil
}

// drop 丢掉当前会话，下次调用会重连。
func (c *client) drop(s *mcp.ClientSession) {
	c.mu.Lock()
	if c.session == s {
		c.session = nil
	}
	c.mu.Unlock()
	if s != nil {
		_ = s.Close()
	}
}

// listTools 握一次手并取回工具清单。安装与刷新时用，不走缓存。
func (c *client) listTools(ctx context.Context) ([]*mcp.Tool, error) {
	s, err := c.ensure()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.connectTimeout)
	defer cancel()

	var out []*mcp.Tool
	cursor := ""
	for {
		res, err := s.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			c.drop(s)
			return nil, fmt.Errorf("取 %s 的工具清单失败：%w", c.cfg.Name, err)
		}
		out = append(out, res.Tools...)
		if res.NextCursor == "" || len(out) > maxToolsPerServer {
			return out, nil
		}
		cursor = res.NextCursor
	}
}

// maxToolsPerServer 是单台 server 收下的工具数上限。
//
// 工具清单每一轮都要随请求发出去，一台 server 报出两百件工具就能把上下文塞满，
// 而这是外部数据、我们说了不算——所以在这一侧封顶。
const maxToolsPerServer = 60

// call 调用一件远端工具，返回已经转成文本的结果。
//
// 连接断了会重来一次：MCP server 是子进程，可能自己崩了、被系统清理了，或者机器
// 睡过一觉。为一次可预期的重连让用户看到一条报错不划算——但只重一次，真的起不来
// 时要如实报出来，而不是在这里反复拉起一个起不来的进程。
func (c *client) call(ctx context.Context, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	res, err := c.callOnce(ctx, tool, args)
	if err == nil {
		return res, nil
	}
	if c.closed {
		return nil, err
	}
	res2, err2 := c.callOnce(ctx, tool, args)
	if err2 != nil {
		return nil, err // 报第一次的错：它才是真正的原因，重试的失败多半是同一件事
	}
	return res2, nil
}

func (c *client) callOnce(ctx context.Context, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	s, err := c.ensure()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()

	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		c.drop(s)
		return nil, err
	}
	return res, nil
}

// Close 关掉连接并结束子进程。
func (c *client) Close() {
	c.mu.Lock()
	s := c.session
	c.session, c.closed = nil, true
	c.mu.Unlock()
	if s != nil {
		_ = s.Close()
	}
}

// renderResult 把 MCP 的结果转成给模型看的文本。
//
// 非文本的内容（图片、音频、资源链接）只留一行说明：本项目的工具结果是纯文本通道，
// 把几百 KB 的 base64 塞进上下文，既没人看得懂也会把这一轮的预算烧光。
func renderResult(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, ct := range res.Content {
		switch v := ct.(type) {
		case *mcp.TextContent:
			b.WriteString(v.Text)
			b.WriteString("\n")
		case *mcp.ImageContent:
			fmt.Fprintf(&b, "（这里有一张图片，%s，本程序的工具结果只走文本，未取回）\n", v.MIMEType)
		case *mcp.AudioContent:
			fmt.Fprintf(&b, "（这里有一段音频，%s，本程序的工具结果只走文本，未取回）\n", v.MIMEType)
		case *mcp.ResourceLink:
			fmt.Fprintf(&b, "（资源链接：%s）\n", v.URI)
		default:
			b.WriteString("（一段本程序不认识的内容，已略过）\n")
		}
	}
	out := strings.TrimRight(b.String(), "\n")
	if out == "" && res.StructuredContent != nil {
		out = fmt.Sprintf("%v", res.StructuredContent)
	}
	return out
}
