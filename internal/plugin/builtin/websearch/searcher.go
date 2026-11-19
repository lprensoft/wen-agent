package websearch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"wen/internal/prompt"
)

// maxRespBytes 是单次响应的读取上限。对方是可信接口，但读取量仍要有上界——一条被
// 污染的响应不该把进程的内存吃穿。
const maxRespBytes = 1 << 20

// Searcher 是一家搜索服务。三家的线格式各不相同，但拿到这儿的东西必须一样：标题、
// 链接、摘要与发布日期。工具、缓存、限流与渲染都只认这一层，加第四家不必再动它们。
type Searcher interface {
	// Search 查一次。freshness 取 day / week / month / year 或空串，由各家自己映射成
	// 它认的取值；n 是想要的条数。
	Search(ctx context.Context, query, freshness string, n int) ([]hit, error)
}

// hit 是一条搜索结果。字段名与任何一家的 JSON 都不同名，是为了让渲染那侧不必知道
// 是哪一家返回的。
type hit struct {
	Title     string
	URL       string
	Content   string
	Published string
}

// newSearcher 按 provider 造一家。endpoint 只在测试里非空——真去打那三个地址的测试
// 既慢又不可复现，还会把开发机的 IP 算进限流。
func newSearcher(provider, apiKey, baseURL string, client *http.Client, lang, endpoint string) Searcher {
	switch provider {
	case "brave":
		if endpoint == "" {
			endpoint = braveEndpoint
		}
		return &brave{key: apiKey, client: client, lang: lang, endpoint: endpoint}
	case "searxng":
		return &searxng{base: baseURL, client: client, lang: lang}
	}
	if endpoint == "" {
		endpoint = tavilyEndpoint
	}
	return &tavily{key: apiKey, client: client, lang: lang, endpoint: endpoint}
}

// normalizeProvider 把配置里的取值收成三个之一。认不出的回 tavily：老的状态文件里
// 根本没有这个键，而那时插件只可能是 tavily。
func normalizeProvider(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "brave":
		return "brave"
	case "searxng":
		return "searxng"
	}
	return "tavily"
}

// send 发出去、读回一段有界的响应体。非 200 不算错误，交给调用方判——SearXNG 的 403
// 有专门的一句解释，那一条只有它自己知道该怎么说。
func send(lang string, client *http.Client, req *http.Request) ([]byte, int, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf(prompt.T(lang, "prompt.web_search.err.network", "搜索失败：%w"), err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return nil, 0, fmt.Errorf(prompt.T(lang, "prompt.web_search.err.network", "搜索失败：%w"), err)
	}
	return data, resp.StatusCode, nil
}

// statusError 把状态码翻成模型能照做的一句话。正文只在兜底那一档带出来一点：404 与
// 500 的区别对模型没有意义，但「密钥不对」「被限流」有——前者要去设置页改，后者要停。
func statusError(lang string, status int, body []byte) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return errors.New(prompt.Tf(lang, "prompt.web_search.err.auth",
			"搜索服务的密钥不对（HTTP %d），去设置页检查 api_key。", status))
	case http.StatusTooManyRequests:
		return errors.New(prompt.Tf(lang, "prompt.web_search.err.provider_limit",
			"搜索服务那边限流了（HTTP %d），过一会儿再试。", status))
	}
	return errors.New(prompt.Tf(lang, "prompt.web_search.err.http",
		"搜索失败（HTTP %d）：%s", status, excerpt(string(body), 200)))
}

func badResponse(lang string, status int, body []byte) error {
	return errors.New(prompt.Tf(lang, "prompt.web_search.err.bad_response",
		"搜索服务的返回读不懂（HTTP %d）：%s", status, excerpt(string(body), 120)))
}
