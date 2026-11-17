// Package webfetch 提供抓取网页文本内容的系统插件。
package webfetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

const defaultMaxBytes = 64 * 1024

// Plugin 是 web_fetch 系统插件（注入一句提示词，演示注入链路）。
type Plugin struct {
	timeout  time.Duration
	maxBytes int
	client   *http.Client
	lang     string

	// allowLocal 只在测试里置真：httptest 的地址就是 127.0.0.1，而正常的抓取一律
	// 不许碰本机与内网（见 plugin.BlockedHost）。不可导出，外部没有打开它的办法。
	allowLocal bool
}

func New() *Plugin {
	return &Plugin{timeout: 20 * time.Second, maxBytes: defaultMaxBytes}
}

func (p *Plugin) Name() string { return "web_fetch" }

func (p *Plugin) Category() string    { return plugin.CategoryTools }
func (p *Plugin) Description() string { return "抓取网页并返回其文本内容" }

func (p *Plugin) SystemPrompt() string {
	return prompt.T(p.lang, "prompt.web_fetch.system", "你可以使用 fetch_url 工具抓取网页的文本内容。")
}

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key:         "timeout_seconds",
			Label:       "请求超时（秒）",
			Type:        plugin.FieldInt,
			Description: "抓取单个网页的最长等待时间。",
			Default:     20,
			Min:         plugin.IntPtr(1),
			Max:         plugin.IntPtr(300),
		},
		{
			Key:         "max_bytes",
			Label:       "最大抓取字节数",
			Type:        plugin.FieldInt,
			Description: "单个网页读取的最大字节数，超出部分截断。",
			Default:     defaultMaxBytes,
			Min:         plugin.IntPtr(1024),
			Max:         plugin.IntPtr(4 * 1024 * 1024),
		},
	}
}

func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	p.lang = ictx.Lang
	p.timeout = time.Duration(plugin.CfgInt(cfg, "timeout_seconds", 20)) * time.Second
	p.maxBytes = plugin.CfgInt(cfg, "max_bytes", defaultMaxBytes)
	p.client = p.newClient()
	return nil
}

// newClient 建一个带出站检查的客户端。
//
// 重定向要单独看：Transit 的拨号检查管得住「跳到内网」，但那是在代理之外的路径上
// 才算数，而写死的地址（http://127.0.0.1/）在任何路径上都该当场回绝。设了
// CheckRedirect 之后标准库的十跳上限就失效了，所以这里自己收尾。
func (p *Plugin) newClient() *http.Client {
	lang := p.lang
	c := &http.Client{Timeout: p.timeout}
	if !p.allowLocal {
		c.Transport = plugin.GuardedTransport()
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errTooManyRedirects(lang)
		}
		if !p.allowLocal && plugin.BlockedHost(req.URL.Hostname()) {
			return plugin.ErrBlockedAddr
		}
		return nil
	}
	return c
}

func (p *Plugin) Tools() []plugin.Tool { return []plugin.Tool{&tool{p: p}} }

type tool struct {
	p *Plugin
}

func (t *tool) Name() string { return "fetch_url" }

func (t *tool) Description() string {
	return prompt.T(t.p.lang, "prompt.web_fetch.tool.fetch_url.desc",
		"抓取指定 URL 的网页并返回其文本内容（HTML 会剥离标签）。仅支持 http/https 的公网地址（本机与内网不行），内容过长会被截断。")
}

// schemaZH 是参数 Schema。整段一个键：它是手写的 JSON，拆成一个字段一个键的话，
// 译者要照着结构去拼，而拼错一个引号就是一份非法的 Schema。
const schemaZH = `{
		"type": "object",
		"properties": {
			"url": {"type": "string", "description": "要抓取的网页地址（http/https）"}
		},
		"required": ["url"]
	}`

func (t *tool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.lang, "prompt.web_fetch.tool.fetch_url.schema", schemaZH))
}

func (t *tool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	u, err := url.Parse(a.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("%s", prompt.T(t.p.lang, "prompt.web_fetch.result.scheme", "仅支持 http/https 地址"))
	}
	// 写死的本机与内网地址在这里就回绝，不等拨号：这一层与代理无关，也给出了一句
	// 模型能懂的话，而不是一个 connection refused。
	if !t.p.allowLocal && plugin.BlockedHost(u.Hostname()) {
		return "", blockedErr(t.p.lang)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "wen-agent/1.0")
	req.Header.Set("Accept", "text/html, text/plain;q=0.9, */*;q=0.5")

	client := t.p.client
	if client == nil {
		client = t.p.newClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, plugin.ErrBlockedAddr) {
			return "", blockedErr(t.p.lang)
		}
		return "", err
	}
	defer resp.Body.Close()

	// 多读 1 字节以判断是否超限
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(t.p.maxBytes)+1))
	if err != nil {
		return "", err
	}
	raw = plugin.NormalizeEncoding(raw)
	raw, truncated := plugin.TruncateUTF8(raw, t.p.maxBytes)

	text := string(raw)
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "html") || looksLikeHTML(text) {
		text = plugin.StripHTML(text)
	}

	out := fmt.Sprintf("[HTTP %d] %s\n\n%s", resp.StatusCode, a.URL, strings.TrimSpace(text))
	if truncated {
		out += prompt.Tf(t.p.lang, "prompt.web_fetch.result.truncated",
			"\n...[内容超过 %d 字节，已截断]", t.p.maxBytes)
	}
	return out, nil
}

// blockedErr 是「地址不该抓」那句话。它给人也看、也给模型看，所以走提示词语言包：
// 模型读到「本机或内网」才不会再换个写法重试。
func blockedErr(lang string) error {
	return fmt.Errorf("%s", prompt.T(lang, "prompt.web_fetch.result.blocked",
		"这个地址指向本机或内网，抓不了；只能抓公网地址。"))
}

func errTooManyRedirects(lang string) error {
	return fmt.Errorf("%s", prompt.T(lang, "prompt.web_fetch.result.redirects", "重定向太多次，抓不到这个页面。"))
}

func looksLikeHTML(s string) bool {
	head := strings.ToLower(s[:min(512, len(s))])
	return strings.Contains(head, "<html") || strings.Contains(head, "<!doctype html")
}
