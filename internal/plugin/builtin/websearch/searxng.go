package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"wen/internal/prompt"
)

// searxng 连一个自建实例：GET /search?format=json，没有 key、没有厂商。
//
// 它便宜在不用付钱，贵在要自己有一台：地址由用户填，所以这里的地址检查是「必须是
// http/https 的完整地址」，不套 plugin.BlockedHost——自家内网里的实例本来就该能连。
//
// 两个坑写在这里，免得下一个人重新踩：
//
//   - 默认配置不给 JSON。settings.yml 的 search.formats 里没有 json 时，实例回 403，
//     而那与「密钥不对」长得一模一样。403 因此单独翻一句能照做的话。
//   - time_range 只有 day / month / year，没有 week。week 退成 month：宽一档是诚实的
//     折衷，退成「不限时间」就成了另一件事。
type searxng struct {
	base   string
	client *http.Client
	lang   string
}

func (s *searxng) Search(ctx context.Context, query, freshness string, n int) ([]hit, error) {
	base := strings.TrimSpace(s.base)
	if base == "" {
		return nil, errors.New(prompt.T(s.lang, "prompt.web_search.err.searxng_url",
			"选了 SearXNG 但没填实例地址（设置页的 base_url），查不了。"))
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New(prompt.T(s.lang, "prompt.web_search.err.base_url",
			"实例地址要填成 http:// 或 https:// 开头的完整地址。"))
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/search"
	q := u.Query()
	q.Set("q", query)
	q.Set("format", "json")
	if f := searxngFreshness(freshness); f != "" {
		q.Set("time_range", f)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	data, status, err := send(s.lang, s.client, req)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized {
		// 套了反向代理密码的实例会回 401。这一句不能沿用「密钥不对」——SearXNG 根本
		// 没有 key 可填，照着那句去设置页翻会一无所获。
		return nil, errors.New(prompt.T(s.lang, "prompt.web_search.err.searxng_auth",
			"实例要求认证（HTTP 401）：这个插件不带认证信息，看看是不是套了反向代理的密码。"))
	}
	if status == http.StatusForbidden {
		return nil, errors.New(prompt.T(s.lang, "prompt.web_search.err.searxng_json",
			"实例拒绝了这次请求（HTTP 403）：多半是没开 JSON 输出，去 settings.yml 的 search.formats 里加上 json。"))
	}
	if status != http.StatusOK {
		return nil, statusError(s.lang, status, data)
	}

	var out struct {
		Results []struct {
			URL           string `json:"url"`
			Title         string `json:"title"`
			Content       string `json:"content"`
			PublishedDate string `json:"publishedDate"`
		} `json:"results"`
		// 上游故障也走 HTTP 200：哪一个引擎超时、被验证码拦了、解析失败了，都记在
		// 这里，而 results 是空的。只认「有没有」不认内容——它的元素形状随版本与
		// 语言变（[引擎, 译文]），这里不需要读。
		Unresponsive []json.RawMessage `json:"unresponsive_engines"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, badResponse(s.lang, status, data)
	}

	hits := make([]hit, 0, len(out.Results))
	for _, r := range out.Results {
		if len(hits) == n {
			break // 实例不认条数，自己截
		}
		hits = append(hits, hit{Title: r.Title, URL: r.URL, Content: r.Content, Published: r.PublishedDate})
	}
	if len(hits) == 0 && len(out.Unresponsive) > 0 {
		// 全挂的消息不能当成「没搜到」：模型会照着这个说法编一句「网上没有」，
		// 而且调用方会按成功记账——额度就白烧在一次没人答的请求上。
		return nil, errors.New(prompt.Tf(s.lang, "prompt.web_search.err.engines_down",
			"上游搜索引擎都没响应（%d 个超时或被验证码拦了），这不是没搜到。过一会儿再试。", len(out.Unresponsive)))
	}
	return hits, nil
}

// searxngFreshness 只映射实例认的三个取值；week 退一档成 month，理由见类型注释。
func searxngFreshness(v string) string {
	switch v {
	case "day":
		return "day"
	case "week", "month":
		return "month"
	case "year":
		return "year"
	}
	return ""
}
