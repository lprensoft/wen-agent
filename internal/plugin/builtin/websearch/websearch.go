// Package websearch 提供联网搜索的系统插件。
//
// 搜索与 web_fetch 是两件事：这一边是「不知道网址，先找几个来源」，那一边是「网址已经
// 在手上」。两边都开着的时候，模型自己会挑。
//
// 走 Tavily 的 /search：一次 POST，返回按相关度排好序的标题、摘要与发布日期。选它
// 不是因为它最好，是因为返回形状已经给模型看过了——不用再剥一层 HTML，也不用猜
// 摘要落在哪个字段。
//
// 不填 key 也能用：Tavily 的 keyless 模式与带 key 的返回逐字段相同，只是限流更紧。
// 填 key 换来的是额度，不是可用性。
//
// 默认关闭只为一个理由：它会把从对话里推出来的查询发给第三方。web_fetch 发出去的是
// 明摆着的地址，搜索发出去的是你们刚说的话里改写出来的词。
package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

const (
	// tavilyEndpoint 只在测试里被替换（见 Plugin.endpoint）：真去打 api.tavily.com 的
	// 测试既慢又不可复现，还会把开发机的 IP 算进限流。
	tavilyEndpoint = "https://api.tavily.com/search"

	defaultResults = 5
	defaultPerHour = 20
	defaultTimeout = 20 // 秒

	// maxSnippetRunes 是单条摘要的上限。五条加起来仍在一千多字，够模型判断「值不值得
	// 再查一次」，又不至于把一轮对话的上下文吃掉。
	maxSnippetRunes = 300

	cacheTTL  = 10 * time.Minute
	cacheSize = 64
)

// Plugin 是 web_search 系统插件。
//
// Init 可重入（设置页改一次配置就重新 Init 一次），此时可能有 in-flight 的 Execute
// 正在读配置，所以配置一律由 mu 保护、在 Execute 开头取一份快照。
type Plugin struct {
	mu sync.RWMutex

	lang     string
	apiKey   string
	results  int
	perHour  int
	client   *http.Client
	endpoint string

	book *book
}

func New() *Plugin {
	return &Plugin{results: defaultResults, perHour: defaultPerHour, endpoint: tavilyEndpoint}
}

func (p *Plugin) Name() string { return "web_search" }

func (p *Plugin) Category() string { return plugin.CategoryTools }

func (p *Plugin) Description() string { return "上网搜索，拿回几条网页摘要" }

func (p *Plugin) SystemPrompt() string {
	return prompt.T(p.langOf(), "prompt.web_search.system",
		"你可以用 search_web 上网查自己不确定的事，就像人拿起手机查一下。查到的是别人的页面摘要：可能过时，也可能是错的，不是你本来就知道的事。用自己的话讲，别念链接，也别把搜来的一整段当成自己说的话。没查到就说没查到，别编。")
}

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key:         "api_key",
			Label:       "Tavily API Key",
			Type:        plugin.FieldString,
			Description: "留空则用免 key 的免费模式，限额更紧；填上换的是额度，不是可用性。",
			Default:     "",
		},
		{
			Key:         "results",
			Label:       "每次结果条数",
			Type:        plugin.FieldInt,
			Description: "一次搜索返回几条网页摘要。",
			Default:     defaultResults,
			Min:         plugin.IntPtr(1),
			Max:         plugin.IntPtr(10),
		},
		{
			Key:         "max_per_hour",
			Label:       "每小时上限",
			Type:        plugin.FieldInt,
			Description: "一小时内最多真的发出去几次查询，超出的调用直接回绝，防止聊得兴起把额度烧光。",
			Default:     defaultPerHour,
			Min:         plugin.IntPtr(1),
			Max:         plugin.IntPtr(200),
		},
		{
			Key:         "timeout_seconds",
			Label:       "请求超时（秒）",
			Type:        plugin.FieldInt,
			Description: "单次搜索的最长等待时间。",
			Default:     defaultTimeout,
			Min:         plugin.IntPtr(1),
			Max:         plugin.IntPtr(120),
		},
	}
}

func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	timeout := time.Duration(plugin.CfgInt(cfg, "timeout_seconds", defaultTimeout)) * time.Second

	p.mu.Lock()
	defer p.mu.Unlock()
	p.lang = ictx.Lang
	p.apiKey = strings.TrimSpace(plugin.CfgString(cfg, "api_key", ""))
	p.results = clamp(plugin.CfgInt(cfg, "results", defaultResults), 1, 10)
	p.perHour = clamp(plugin.CfgInt(cfg, "max_per_hour", defaultPerHour), 1, 200)
	p.client = &http.Client{Timeout: timeout}
	if p.book == nil {
		// 缓存跨 Init 保留：改个超时不该让刚查过的东西再查一遍
		p.book = newBook(cacheTTL, cacheSize)
	}
	return nil
}

func (p *Plugin) Tools() []plugin.Tool { return []plugin.Tool{&tool{p: p}} }

func (p *Plugin) langOf() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lang
}

// settings 是一次调用用到的全部配置。取快照而不是逐项读，是为了让一轮调用里看到的
// 是同一份配置——否则设置页正好在中间改了超时，前后两段用的就是两个客户端。
type settings struct {
	lang     string
	apiKey   string
	results  int
	perHour  int
	client   *http.Client
	endpoint string
}

func (p *Plugin) snapshot() settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return settings{
		lang:     p.lang,
		apiKey:   p.apiKey,
		results:  p.results,
		perHour:  p.perHour,
		client:   p.client,
		endpoint: p.endpoint,
	}
}

type tool struct {
	p *Plugin
}

func (t *tool) Name() string { return "search_web" }

func (t *tool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.web_search.tool.search_web.desc",
		"用搜索引擎上网查资料。query 写关键词而不是整句话（「2026 杭州马拉松 报名时间」），要最新的消息就把年份或月份写进去。返回的是网页摘要，不是你自己知道的事。")
}

// schemaZH 整段一个键：它是手写的 JSON，拆成一个字段一个键的话，译者要照着结构去拼，
// 拼错一个引号就是一份非法的 Schema。
const schemaZH = `{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "搜索关键词"},
			"freshness": {
				"type": "string",
				"enum": ["day", "week", "month", "year"],
				"description": "只要最近这段时间的内容，可选；问「最近」「现在」的事时用"
			}
		},
		"required": ["query"]
	}`

func (t *tool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.web_search.tool.search_web.schema", schemaZH))
}

func (t *tool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Query     string `json:"query"`
		Freshness string `json:"freshness"`
	}
	s := t.p.snapshot()
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(s.lang, err)
	}
	// 连续空白压成一个空格：模型有时会把一句话拆成几行，那不该变成两次不同的查询
	q := strings.Join(strings.Fields(a.Query), " ")
	if q == "" {
		return "", fmt.Errorf("%s", prompt.T(s.lang, "prompt.web_search.err.empty_query", "查询是空的"))
	}
	fresh := normalizeFreshness(a.Freshness)

	now := time.Now()
	key := q + "\x00" + fresh
	if out, at, ok := t.p.book.lookup(key, now); ok {
		return out + "\n" + cachedNote(s.lang, now.Sub(at)), nil
	}
	if !t.p.book.reserve(s.perHour, now) {
		return "", fmt.Errorf("%s", prompt.Tf(s.lang, "prompt.web_search.err.quota",
			"这个小时已经查了 %d 次，先不查了。等一会儿再说，或者不查也行。", s.perHour))
	}

	hits, err := t.p.search(ctx, s, q, fresh)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return prompt.T(s.lang, "prompt.web_search.result.empty",
			"没有搜到结果。别编，如实说没查到，或者换个说法再查一次。"), nil
	}
	out := render(s.lang, q, hits)
	t.p.book.store(key, out, now)
	return out, nil
}

// normalizeFreshness 只认 schema 里那四个值。别的写法（"today"、"recent"）直接丢掉：
// 模型偶尔自造取值，为这个回绝整次搜索不值得，而丢掉只意味着「这次不限时间」。
func normalizeFreshness(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "day", "week", "month", "year":
		return v
	}
	return ""
}

func render(lang, q string, hits []hit) string {
	var b strings.Builder
	b.WriteString(prompt.Tf(lang, "prompt.web_search.result.header",
		"「%s」的搜索结果，%d 条。这些是网页摘要，不是你本来就知道的事：可能过时，也可能是错的。用自己的话说，别念链接。",
		q, len(hits)))
	for i, h := range hits {
		src := hostOf(h.URL)
		if src == "" {
			src = prompt.T(lang, "prompt.web_search.result.unknown_source", "来源未明")
		}
		b.WriteString("\n\n")
		b.WriteString(prompt.Tf(lang, "prompt.web_search.result.item", "%d. %s（%s · %s）\n%s",
			i+1, titleOf(lang, plugin.StripHTML(h.Title)), src, dateOf(lang, h.Published),
			excerpt(plugin.StripHTML(h.Content), maxSnippetRunes)))
		if h.URL != "" {
			b.WriteString(prompt.Tf(lang, "prompt.web_search.result.link", "\n链接：%s", h.URL))
		}
	}
	return b.String()
}

func titleOf(lang, s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return prompt.T(lang, "prompt.web_search.result.unknown_title", "（无标题）")
	}
	return s
}

// dateOf 把发布日期收成 YYYY-MM-DD。认不出来的保留原文前 40 字而不是抹平：一个反常的
// 日期字符串对模型仍是「这条有日期」的信号，改成「日期未明」反而是丢信息。
func dateOf(lang, s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return prompt.T(lang, "prompt.web_search.result.unknown_date", "日期未明")
	}
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return excerpt(s, 40)
}

// hostOf 取来源站名。完整 URL 也给了模型（要细读时得靠它），但标题行里只放站点，
// 免得整段结果看起来像一张书签列表。
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Host, "www.")
}

// excerpt 截到 n 个 rune 并加省略号。按 rune 而不是字节：中文摘要按字节截会切出半个字。
func excerpt(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func cachedNote(lang string, age time.Duration) string {
	mins := int(age.Minutes())
	if mins < 1 {
		return prompt.T(lang, "prompt.web_search.result.cached_just_now", "（这些是刚才查到的）")
	}
	return prompt.Tf(lang, "prompt.web_search.result.cached", "（这些是 %d 分钟前查到的，可能已经不是最新的）", mins)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
