package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"wen/internal/prompt"
)

const braveEndpoint = "https://api.search.brave.com/res/v1/web/search"

// brave 是 Brave Search 的 Web Search 接口：GET + 一个订阅 token 头。
//
// 与 Tavily 的两处实质差别：条数参数叫 count；时间过滤是 pd / pw / pm / py 这种缩写。
// 结果里的 description 带 HTML（<b> 高亮），渲染那一侧本来就会剥，这里不必处理。
//
// 日期用 page_age 而不是 age：后者是「2 days ago」这种人话，拿它当日期渲染出来
// 会是「2 days ago」挂在标题后面，而我们要的是一个能判断新旧的日子。
type brave struct {
	key      string
	client   *http.Client
	lang     string
	endpoint string
}

func (b *brave) Search(ctx context.Context, query, freshness string, n int) ([]hit, error) {
	if b.key == "" {
		// 不静默回落到 Tavily：那等于把人的查询发到一家他没选的服务上。
		return nil, errors.New(prompt.T(b.lang, "prompt.web_search.err.brave_key",
			"选了 Brave 但没填 API key（设置页的 api_key），查不了。"))
	}

	q := url.Values{}
	q.Set("q", query)
	q.Set("count", strconv.Itoa(n))
	if f := braveFreshness(freshness); f != "" {
		q.Set("freshness", f)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", b.key)

	data, status, err := send(b.lang, b.client, req)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, statusError(b.lang, status, data)
	}

	var out struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
				PageAge     string `json:"page_age"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, badResponse(b.lang, status, data)
	}

	hits := make([]hit, 0, len(out.Web.Results))
	for _, r := range out.Web.Results {
		hits = append(hits, hit{Title: r.Title, URL: r.URL, Content: r.Description, Published: r.PageAge})
	}
	return hits, nil
}

// braveFreshness 把工具那一侧的四个取值翻成 Brave 的缩写。没有对应值时返回空串，
// 表示这次不限时间。
func braveFreshness(v string) string {
	switch v {
	case "day":
		return "pd"
	case "week":
		return "pw"
	case "month":
		return "pm"
	case "year":
		return "py"
	}
	return ""
}
