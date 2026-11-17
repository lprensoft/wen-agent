package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"wen/internal/prompt"
)

// maxRespBytes 是单次响应的读取上限。对方是可信接口，但读取量仍要有上界——
// 一条被污染的响应不该把进程的内存吃穿。
const maxRespBytes = 1 << 20

// hit 是一条搜索结果。字段名与 Tavily 的 JSON 不同名，是为了让渲染那侧不必知道
// 是哪一家返回的。
type hit struct {
	Title     string
	URL       string
	Content   string
	Published string
}

// search 发一次查询。key 为空时走 keyless 模式：返回与带 key 的逐字段相同，只是限流
// 更紧，所以这里的两种鉴权不是两条代码路径。
func (p *Plugin) search(ctx context.Context, s settings, q, freshness string) ([]hit, error) {
	body := map[string]any{
		"query":       q,
		"max_results": s.results,
		// basic 而不是 advanced：basic 是一个 credit、延迟低，advanced 是两个 credit 且
		// 更慢。这是一轮对话里的顺手一查，不是写调研报告。
		"search_depth":           "basic",
		"include_published_date": true,
		// 摘要要，全文与 LLM 生成的答案都不要：全文会挤爆上下文，而 answer 是另一个模型
		// 的书面语，塞给角色会让它突然换一种口吻说话。
		"include_raw_content": false,
		"include_answer":      false,
	}
	if freshness != "" {
		body["time_range"] = freshness
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if s.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
	} else {
		req.Header.Set("X-Tavily-Access-Mode", "keyless")
	}

	client := s.client
	if client == nil {
		client = &http.Client{Timeout: time.Duration(defaultTimeout) * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf(prompt.T(s.lang, "prompt.web_search.err.network", "搜索失败：%w"), err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return nil, fmt.Errorf(prompt.T(s.lang, "prompt.web_search.err.network", "搜索失败：%w"), err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(s.lang, resp.StatusCode, data)
	}

	var out struct {
		Results []struct {
			Title         string `json:"title"`
			URL           string `json:"url"`
			Content       string `json:"content"`
			PublishedDate string `json:"published_date"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%s", prompt.Tf(s.lang, "prompt.web_search.err.bad_response",
			"搜索服务的返回读不懂（HTTP %d）：%s", resp.StatusCode, excerpt(string(data), 120)))
	}

	hits := make([]hit, 0, len(out.Results))
	for _, r := range out.Results {
		hits = append(hits, hit{
			Title:     r.Title,
			URL:       r.URL,
			Content:   r.Content,
			Published: r.PublishedDate,
		})
	}
	return hits, nil
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
