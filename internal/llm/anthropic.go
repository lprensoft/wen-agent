package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"wen/internal/i18n"
)

const anthropicVersion = "2023-06-01"

// Anthropic 是 Anthropic Messages API 的 Provider 实现。
type Anthropic struct {
	lang    string // 报错说话用的语言，见 Config.Lang
	baseURL string
	apiKey  string
	// cache 为真时在请求里显式打提示词缓存断点（见 markCacheBreakpoints）。
	// Anthropic 的缓存必须由调用方声明，且写入按约 1.25 倍输入价单独计费，
	// 因此它是提供商上一个看得见的开关，不由这里替用户决定。
	cache  bool
	client *http.Client
}

func NewAnthropic(lang, baseURL, apiKey string, promptCache bool) *Anthropic {
	return &Anthropic{
		lang:    lang,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		cache:   promptCache,
		// 与 openai_compat 一致：总超时不设上限（长思考+长输出），连接阶段单独限时
		client: &http.Client{Transport: &http.Transport{
			ResponseHeaderTimeout: 60 * time.Second,
		}},
	}
}

// endpoint 拼接 /v1/messages，兼容 base_url 已以 /v1 结尾的写法。
func (p *Anthropic) endpoint() string {
	if strings.HasSuffix(p.baseURL, "/v1") {
		return p.baseURL + "/messages"
	}
	return p.baseURL + "/v1/messages"
}

// ---------- 线上格式 ----------

type antRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	// System 用块数组而不是字符串：缓存断点是打在块上的（cache_control）。
	System       []antBlock   `json:"system,omitempty"`
	Messages     []antMessage `json:"messages"`
	Tools        []antTool    `json:"tools,omitempty"`
	Stream       bool         `json:"stream"`
	Thinking     *antThinking `json:"thinking,omitempty"`
	OutputConfig *antOutput   `json:"output_config,omitempty"`
}

type antThinking struct {
	Type string `json:"type"` // adaptive / disabled
}

type antOutput struct {
	Effort string `json:"effort,omitempty"` // low / medium / high / xhigh / max
}

type antMessage struct {
	Role    string     `json:"role"` // 只能是 user / assistant
	Content []antBlock `json:"content"`
}

// antBlock 是各类 content block 的并集，靠 Type 区分。
type antBlock struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"` // text

	Thinking  string `json:"thinking,omitempty"`  // thinking
	Signature string `json:"signature,omitempty"` // thinking 的签名，必须原样回传
	Data      string `json:"data,omitempty"`      // redacted_thinking 的密文

	ID    string          `json:"id,omitempty"`    // tool_use
	Name  string          `json:"name,omitempty"`  // tool_use
	Input json.RawMessage `json:"input,omitempty"` // tool_use

	ToolUseID string `json:"tool_use_id,omitempty"` // tool_result
	Content   string `json:"content,omitempty"`     // tool_result

	// CacheControl 标记「到本块为止的内容作为一段提示词缓存」，见 markCacheBreakpoints。
	CacheControl *antCacheControl `json:"cache_control,omitempty"`
}

// antCacheControl 目前只有 ephemeral 一种类型（默认 5 分钟有效期，每次命中会续期）。
type antCacheControl struct {
	Type string `json:"type"`
}

type antTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type antUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// antStreamEvent 是 SSE data 帧；每帧自带 type，因此只需解析 data 行。
type antStreamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage *antUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *antBlock `json:"content_block"`
	Delta        *antDelta `json:"delta"`
	Usage        *antUsage `json:"usage"`
	Error        *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type antDelta struct {
	Type        string `json:"type"` // text_delta / thinking_delta / signature_delta / input_json_delta
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	Signature   string `json:"signature"`
	PartialJSON string `json:"partial_json"`
}

// ---------- 请求装配 ----------

func buildAnthropicRequest(req ChatRequest, cache bool) antRequest {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	wr := antRequest{
		Model:     req.Model,
		MaxTokens: maxTokens,
		Stream:    true,
	}

	// 思考模式：off 关闭；其余档位走 adaptive + effort（档位取值与 effort 一一对应）。
	// 当前世代 Claude 模型不接受 temperature 等采样参数，一律不发送。
	switch req.Thinking {
	case "", "off":
		wr.Thinking = &antThinking{Type: "disabled"}
	default:
		wr.Thinking = &antThinking{Type: "adaptive"}
		wr.OutputConfig = &antOutput{Effort: req.Thinking}
	}

	var system []string
	appendBlocks := func(role string, blocks []antBlock) {
		if len(blocks) == 0 {
			return
		}
		// Anthropic 要求 user/assistant 严格交替，连续同角色需合并成一条
		if n := len(wr.Messages); n > 0 && wr.Messages[n-1].Role == role {
			wr.Messages[n-1].Content = append(wr.Messages[n-1].Content, blocks...)
			return
		}
		wr.Messages = append(wr.Messages, antMessage{Role: role, Content: blocks})
	}

	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			if m.Content != "" {
				system = append(system, m.Content) // system 提到顶层，不进 messages
			}
		case RoleUser:
			if m.Content != "" {
				appendBlocks("user", []antBlock{{Type: "text", Text: m.Content}})
			}
		case RoleTool:
			// Anthropic 没有 tool 角色：工具结果作为 user 消息里的 tool_result block，
			// 同一轮的多个结果会因上面的合并逻辑落进同一条 user 消息
			content := m.Content
			if content == "" {
				content = "(empty output)"
			}
			appendBlocks("user", []antBlock{{Type: "tool_result", ToolUseID: m.ToolCallID, Content: content}})
		case RoleAssistant:
			var blocks []antBlock
			for _, b := range m.ReasoningBlocks {
				switch {
				case b.Type == "redacted_thinking" && b.Data != "":
					blocks = append(blocks, antBlock{Type: "redacted_thinking", Data: b.Data})
				case b.Signature != "" && b.Text != "":
					blocks = append(blocks, antBlock{Type: "thinking", Thinking: b.Text, Signature: b.Signature})
				}
				// 无签名的思考块一律丢弃：跨 provider 的历史会被 API 拒绝
			}
			if m.Content != "" {
				blocks = append(blocks, antBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input := tc.Arguments
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, antBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: input})
			}
			appendBlocks("assistant", blocks)
		}
	}
	if len(system) > 0 {
		wr.System = []antBlock{{Type: "text", Text: strings.Join(system, "\n\n")}}
	}

	for _, t := range req.Tools {
		schema := t.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		wr.Tools = append(wr.Tools, antTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	if cache {
		markCacheBreakpoints(&wr)
	}
	return wr
}

// markCacheBreakpoints 打提示词缓存断点。
//
// Anthropic 的缓存是前缀精确匹配：断点之前（按 tools → system → messages 的规范
// 顺序）的全部内容作为一段缓存写入，下次请求前缀逐字节相同即可按约十分之一的价格
// 读回。两个断点就够，也正对应上下文的两个稳定层：
//  1. system 块末尾——覆盖 tools 与 system，它们整轮之间不变；
//  2. 倒数第二条消息末尾——覆盖除本轮新增内容以外的全部历史。
//
// 第二个断点刻意不打在最后一条消息上：那条带着每轮都变的本轮状态块，
// 把它写进缓存只会付写入的钱、永远读不回来。
func markCacheBreakpoints(wr *antRequest) {
	mark := func(blocks []antBlock) {
		for i := len(blocks) - 1; i >= 0; i-- {
			// 思考块不接受 cache_control，跳到它前面一个可标记的块
			if blocks[i].Type == "thinking" || blocks[i].Type == "redacted_thinking" {
				continue
			}
			blocks[i].CacheControl = &antCacheControl{Type: "ephemeral"}
			return
		}
	}
	mark(wr.System)
	if n := len(wr.Messages); n >= 2 {
		mark(wr.Messages[n-2].Content)
	}
}

// ---------- 流式调用 ----------

// partialBlock 累积流式下发的思考块分片。
type partialBlock struct {
	typ       string
	text      strings.Builder
	signature strings.Builder
	data      string
}

func (p *Anthropic) ChatStream(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	body, err := json.Marshal(buildAnthropicRequest(req, p.cache))
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Api-Key", p.apiKey)
	httpReq.Header.Set("Anthropic-Version", anthropicVersion)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(errBody))}
	}

	ch := make(chan StreamEvent)
	go func() {
		defer close(ch)
		defer resp.Body.Close()

		emit := func(ev StreamEvent) bool {
			select {
			case ch <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}

		calls := map[int]*partialCall{}
		thinks := map[int]*partialBlock{}
		var usage Usage

		finish := func() {
			if len(thinks) > 0 {
				emit(StreamEvent{Type: EventReasoning, Reasoning: assembleBlocks(thinks)})
			}
			if len(calls) > 0 {
				emit(StreamEvent{Type: EventToolCalls, ToolCalls: assembleCalls(calls)})
			}
			if usage.PromptTokens > 0 || usage.CompletionTokens > 0 {
				u := usage
				u.TotalTokens = u.PromptTokens + u.CompletionTokens
				emit(StreamEvent{Type: EventUsage, Usage: &u})
			}
			emit(StreamEvent{Type: EventDone})
		}

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue // event: 行的信息在 data 的 type 字段里，无需单独处理
			}
			var ev antStreamEvent
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev); err != nil {
				continue
			}

			switch ev.Type {
			case "message_start":
				// usage 分两处下发：这里给输入侧（含缓存命中），message_delta 给输出侧
				if ev.Message != nil && ev.Message.Usage != nil {
					u := ev.Message.Usage
					usage.PromptTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
					usage.CompletionTokens = u.OutputTokens
					usage.CachedTokens = u.CacheReadInputTokens
					usage.CacheWriteTokens = u.CacheCreationInputTokens
				}
			case "content_block_start":
				if ev.ContentBlock == nil {
					continue
				}
				switch ev.ContentBlock.Type {
				case "tool_use":
					calls[ev.Index] = &partialCall{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
				case "thinking":
					thinks[ev.Index] = &partialBlock{typ: "thinking"}
				case "redacted_thinking":
					thinks[ev.Index] = &partialBlock{typ: "redacted_thinking", data: ev.ContentBlock.Data}
				}
			case "content_block_delta":
				if ev.Delta == nil {
					continue
				}
				switch ev.Delta.Type {
				case "text_delta":
					if ev.Delta.Text != "" && !emit(StreamEvent{Type: EventContentDelta, Content: ev.Delta.Text}) {
						return
					}
				case "thinking_delta":
					if b := thinks[ev.Index]; b != nil {
						b.text.WriteString(ev.Delta.Thinking)
					}
					if ev.Delta.Thinking != "" && !emit(StreamEvent{Type: EventReasoningDelta, Content: ev.Delta.Thinking}) {
						return
					}
				case "signature_delta":
					if b := thinks[ev.Index]; b != nil {
						b.signature.WriteString(ev.Delta.Signature)
					}
				case "input_json_delta":
					if c := calls[ev.Index]; c != nil {
						c.args.WriteString(ev.Delta.PartialJSON)
					}
				}
			case "message_delta":
				// output_tokens 是累计值，覆盖而非累加
				if ev.Usage != nil && ev.Usage.OutputTokens > 0 {
					usage.CompletionTokens = ev.Usage.OutputTokens
				}
			case "message_stop":
				finish()
				return
			case "error":
				msg := i18n.T(p.lang, "llm.err.unknown", "未知错误")
				if ev.Error != nil {
					msg = ev.Error.Type + ": " + ev.Error.Message
				}
				emit(StreamEvent{Type: EventError, Err: &APIError{Body: msg}})
				return
			}
		}
		if err := sc.Err(); err != nil {
			emit(StreamEvent{Type: EventError, Err: fmt.Errorf("read stream: %w", err)})
			return
		}
		// 流未见 message_stop 即结束：按已收内容收尾
		finish()
	}()
	return ch, nil
}

// assembleBlocks 按 content block 下标顺序还原思考块。
func assembleBlocks(acc map[int]*partialBlock) []ReasoningBlock {
	idxs := make([]int, 0, len(acc))
	for i := range acc {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)

	blocks := make([]ReasoningBlock, 0, len(acc))
	for _, i := range idxs {
		b := acc[i]
		blocks = append(blocks, ReasoningBlock{
			Type:      b.typ,
			Text:      b.text.String(),
			Signature: b.signature.String(),
			Data:      b.data,
		})
	}
	return blocks
}
