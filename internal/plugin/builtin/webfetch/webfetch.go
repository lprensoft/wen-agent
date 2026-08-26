// Package webfetch 提供抓取网页文本内容的系统插件。
package webfetch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
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
	p.client = &http.Client{Timeout: p.timeout}
	return nil
}

func (p *Plugin) Tools() []plugin.Tool { return []plugin.Tool{&tool{p: p}} }

type tool struct {
	p *Plugin
}

func (t *tool) Name() string { return "fetch_url" }

func (t *tool) Description() string {
	return prompt.T(t.p.lang, "prompt.web_fetch.tool.fetch_url.desc",
		"抓取指定 URL 的网页并返回其文本内容（HTML 会剥离标签）。仅支持 http/https，内容过长会被截断。")
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "wen-agent/1.0")
	req.Header.Set("Accept", "text/html, text/plain;q=0.9, */*;q=0.5")

	client := t.p.client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
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
		text = stripHTML(text)
	}

	out := fmt.Sprintf("[HTTP %d] %s\n\n%s", resp.StatusCode, a.URL, strings.TrimSpace(text))
	if truncated {
		out += prompt.Tf(t.p.lang, "prompt.web_fetch.result.truncated",
			"\n...[内容超过 %d 字节，已截断]", t.p.maxBytes)
	}
	return out, nil
}

func looksLikeHTML(s string) bool {
	head := strings.ToLower(s[:min(512, len(s))])
	return strings.Contains(head, "<html") || strings.Contains(head, "<!doctype html")
}

var (
	reScript = regexp.MustCompile(`(?is)<(script|style|noscript)\b.*?</\s*(script|style|noscript)\s*>`)
	reTag    = regexp.MustCompile(`(?s)<[^>]*>`)
	reBlank  = regexp.MustCompile(`\n{3,}`)
	reSpaces = regexp.MustCompile(`[ \t]{2,}`)
)

// stripHTML 粗剥 HTML：去掉 script/style/标签，压缩空白，保留可读文本。
func stripHTML(s string) string {
	s = reScript.ReplaceAllString(s, " ")
	// 常见块级标签换成换行，避免文本黏连
	s = regexp.MustCompile(`(?i)</\s*(p|div|li|h[1-6]|tr|br)\s*>|<br\s*/?>`).ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, " ")
	s = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'").Replace(s)
	s = reSpaces.ReplaceAllString(s, " ")
	// 去掉每行首尾空白后压缩连续空行
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	s = strings.Join(lines, "\n")
	return reBlank.ReplaceAllString(s, "\n\n")
}
