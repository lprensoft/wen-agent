package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
)

// tavilyEndpoint 只在测试里被替换（见 Plugin.endpoint）：真去打 api.tavily.com 的
// 测试既慢又不可复现，还会把开发机的 IP 算进限流。
const tavilyEndpoint = "https://api.tavily.com/search"

// tavily 是 Tavily 的 /search：一次 POST，返回按相关度排好序的标题、摘要与发布日期。
// 选它做默认不是因为「它最好」，是因为它的返回形状已经给模型看过了——不用再剥一层
// HTML，也不用猜摘要落在哪个字段。
//
// 不填 key 也能用：keyless 模式与带 key 的返回逐字段相同，只是限流更紧。填 key 换来
// 的是额度，不是可用性。
type tavily struct {
	key      string
	client   *http.Client
	lang     string
	endpoint string
}

func (t *tavily) Search(ctx context.Context, query, freshness string, n int) ([]hit, error) {
	body := map[string]any{
		"query":       query,
		"max_results": n,
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if t.key != "" {
		req.Header.Set("Authorization", "Bearer "+t.key)
	} else {
		req.Header.Set("X-Tavily-Access-Mode", "keyless")
	}

	data, status, err := send(t.lang, t.client, req)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, statusError(t.lang, status, data)
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
		return nil, badResponse(t.lang, status, data)
	}

	hits := make([]hit, 0, len(out.Results))
	for _, r := range out.Results {
		hits = append(hits, hit{Title: r.Title, URL: r.URL, Content: r.Content, Published: r.PublishedDate})
	}
	return hits, nil
}
