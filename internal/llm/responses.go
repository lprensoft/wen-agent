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

// Responses 是 OpenAI Responses API（/v1/responses）的 Provider 实现。
//
// 与 Chat Completions 的关键差别是输入/输出都是 Item 数组：助手消息、函数调用、
// 推理项各自独立。这里固定 store=false，把每轮输出的原始 Item 经 StreamEvent.RawItems
// 交回 Agent 落盘；下一轮原样回放，不把会话状态放到 OpenAI 一侧。
type Responses struct {
	lang    string
	baseURL string
	apiKey  string
	client  *http.Client
}

func NewResponses(lang, baseURL, apiKey string) *Responses {
	return &Responses{
		lang:    lang,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		// 与其它 provider 一致：总超时不设上限（长思考+长输出），连接阶段单独限时
		client: &http.Client{Transport: &http.Transport{
			ResponseHeaderTimeout: 60 * time.Second,
		}},
	}
}

// endpoint 拼接 /v1/responses，兼容 base_url 已以 /v1 结尾的写法。
func (p *Responses) endpoint() string {
	if strings.HasSuffix(p.baseURL, "/v1") {
		return p.baseURL + "/responses"
	}
	return p.baseURL + "/v1/responses"
}

// origin 标识 Item 的产出来源。推理项的 encrypted_content 与具体模型/端点绑定，
// 换一家或换一个模型后不能再原样回放。
func (p *Responses) origin(model string) string {
	return p.baseURL + "|" + model
}

// ---------- 请求 ----------

type responsesRequest struct {
	Model           string              `json:"model"`
	Input           []json.RawMessage   `json:"input"`
	Instructions    string              `json:"instructions,omitempty"`
	Tools           []responsesTool     `json:"tools,omitempty"`
	MaxOutputTokens int                 `json:"max_output_tokens,omitempty"`
	Reasoning       *responsesReasoning `json:"reasoning,omitempty"`
	Stream          bool                `json:"stream"`
	Store           bool                `json:"store"`
	Include         []string            `json:"include,omitempty"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type responsesReasoning struct {
	Effort string `json:"effort"`
	// Summary 必须显式请求，否则同步返回的只有加密推理项，Web UI 没有可展示的
	// 思考摘要（见官方 reasoning summaries）。
	Summary string `json:"summary,omitempty"`
}

func buildResponsesRequest(lang, origin string, req ChatRequest) responsesRequest {
	wr := responsesRequest{
		Model:           req.Model,
		MaxOutputTokens: req.MaxTokens,
		Stream:          true,
		Store:           false,
	}
	// temperature 在 Responses 的推理模型上普遍被拒绝，这里一律不发；
	// 需要精确控制采样时仍可把该提供商切回 Chat Completions。
	if effort := responsesEffort(req.Thinking); effort != "" {
		wr.Reasoning = &responsesReasoning{Effort: effort}
		if effort != "none" {
			wr.Reasoning.Summary = "auto"
			wr.Include = []string{"reasoning.encrypted_content"}
		}
	}

	var instructions []string
	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			if s := strings.TrimSpace(m.Content); s != "" {
				instructions = append(instructions, s)
			}
		case RoleTool:
			wr.Input = append(wr.Input, mustRaw(map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  m.Content,
			}))
		case RoleAssistant:
			// 上一轮由 Responses 生成的原始 Item 优先原样回放：其中包含推理项的
			// encrypted_content 与 function_call 的 call_id，重新拼装会丢。
			if len(m.ResponsesItems) > 0 && m.ResponsesOrigin == origin {
				wr.Input = append(wr.Input, splitRawItems(lang, m.ResponsesItems)...)
				continue
			}
			if m.Content != "" {
				wr.Input = append(wr.Input, mustRaw(map[string]any{
					"role": "assistant", "content": m.Content,
				}))
			}
			for _, tc := range m.ToolCalls {
				wr.Input = append(wr.Input, mustRaw(map[string]any{
					"type":      "function_call",
					"call_id":   tc.ID,
					"name":      tc.Name,
					"arguments": string(tc.Arguments),
				}))
			}
		default:
			wr.Input = append(wr.Input, mustRaw(map[string]any{
				"role": m.Role, "content": responsesContent(m),
			}))
		}
	}
	wr.Instructions = strings.Join(instructions, "\n\n")
	for _, t := range req.Tools {
		wr.Tools = append(wr.Tools, responsesTool{
			Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Schema,
		})
	}
	return wr
}

// responsesContent 在带图片时返回 input content part 数组；纯文本仍用字符串，
// 兼容只认字符串 content 的实现。
func responsesContent(m Message) any {
	if len(m.Images) == 0 {
		return m.Content
	}
	parts := make([]map[string]any, 0, len(m.Images)+1)
	if m.Content != "" {
		parts = append(parts, map[string]any{"type": "input_text", "text": m.Content})
	}
	for _, img := range m.Images {
		if len(img.Data) == 0 {
			continue
		}
		parts = append(parts, map[string]any{
			"type":      "input_image",
			"image_url": imageDataURL(img),
		})
	}
	if len(parts) == 0 {
		return m.Content
	}
	return parts
}

// responsesEffort 把思考档位映射到 Responses 的 reasoning.effort。
// off 作为 "none" 发，与 Chat 侧的 OpenAI 方言保持一致。
func responsesEffort(thinking string) string {
	switch thinking {
	case "", "off":
		return "none"
	case "low", "medium", "high", "xhigh", "max":
		return thinking
	}
	return ""
}

func mustRaw(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		// 这里的值全部来自已经过校验的结构体，序列化不会失败；真失败也只会
		// 丢掉一个输入项，不能把整轮对话拖死。
		return json.RawMessage(`null`)
	}
	return raw
}

func splitRawItems(lang string, raw json.RawMessage) []json.RawMessage {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		i18n.Logf(lang, "log.llm.responses_items_bad", "llm: 丢弃无法解析的 Responses 历史项: %v", err)
		return nil
	}
	return items
}

// ---------- 流式调用 ----------

func (p *Responses) ChatStream(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	origin := p.origin(req.Model)
	body, err := json.Marshal(buildResponsesRequest(p.lang, origin, req))
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
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

		var doneItems []json.RawMessage
		finished := false
		terminal := false
		finish := func(output []json.RawMessage, incompleteReason string) {
			if finished {
				return
			}
			finished = true
			items := output
			if len(items) == 0 {
				items = doneItems
			}
			calls, sawIncomplete, err := toolCallsFromItems(items)
			if err != nil {
				emit(StreamEvent{Type: EventError, Err: &APIError{
					Code: "invalid_tool_arguments",
					Body: i18n.Tf(p.lang, "llm.err.responses_bad_tool_args",
						"Responses 返回了不完整的工具调用参数（%s），本次调用未执行", err.Error()),
				}})
				return
			}
			// 输出被截断时，任何函数调用（包括参数已完整但 status 尚未完成的）
			// 都不能执行：参数可能只是恰好能解析，模型并没有写完。
			if sawIncomplete || (incompleteReason != "" && len(calls) > 0) {
				code := incompleteReason
				if code == "" {
					code = "incomplete_tool_call"
				}
				emit(StreamEvent{Type: EventError, Err: &APIError{
					Code: code,
					Body: i18n.Tf(p.lang, "llm.err.responses_incomplete_tools",
						"Responses 输出在工具调用完成前被截断（%s），本次调用未执行", code),
				}})
				return
			}
			if len(calls) > 0 {
				emit(StreamEvent{Type: EventToolCalls, ToolCalls: calls})
			}
			ev := StreamEvent{Type: EventDone}
			if len(items) > 0 {
				if raw, err := json.Marshal(items); err == nil {
					ev.RawItems = raw
					ev.RawOrigin = origin
				}
			}
			emit(ev)
		}

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				terminal = true
				finish(nil, "")
				return
			}
			var ev responsesEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				continue
			}
			switch ev.Type {
			case "response.output_text.delta", "response.refusal.delta":
				if ev.Delta != "" {
					emit(StreamEvent{Type: EventContentDelta, Content: ev.Delta})
				}
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				if ev.Delta != "" {
					emit(StreamEvent{Type: EventReasoningDelta, Content: ev.Delta})
				}
			case "response.output_item.done":
				if len(ev.Item) > 0 {
					doneItems = append(doneItems, ev.Item)
				}
			case "response.completed", "response.incomplete":
				terminal = true
				reason := ""
				if ev.Response != nil {
					if ev.Response.Usage != nil {
						emit(StreamEvent{Type: EventUsage, Usage: ev.Response.Usage.usage()})
					}
					if ev.Response.IncompleteDetails != nil {
						reason = ev.Response.IncompleteDetails.Reason
					}
					if ev.Type == "response.incomplete" && reason == "content_filter" {
						emit(StreamEvent{Type: EventError, Err: &APIError{
							Kind: KindContentFilter,
							Body: "response incomplete: content_filter",
						}})
						return
					}
					finish(ev.Response.Output, reason)
					return
				}
				if ev.Type == "response.incomplete" {
					emit(StreamEvent{Type: EventError, Err: &APIError{
						Code: "incomplete",
						Body: "response incomplete without details",
					}})
					return
				}
				finish(nil, "")
				return
			case "response.failed", "error":
				terminal = true
				emit(StreamEvent{Type: EventError, Err: ev.apiError()})
				return
			}
		}
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			emit(StreamEvent{Type: EventError, Err: err})
			return
		}
		if ctx.Err() != nil {
			return
		}
		if !terminal {
			// 没有 response.completed / incomplete / [DONE] 就断流：不能把半截
			// 回复当作成功完成，否则它会被落盘并继续参与后续轮次。
			emit(StreamEvent{Type: EventError, Err: fmt.Errorf("%s: %w",
				i18n.T(p.lang, "llm.err.responses_eof", "Responses 流在终止事件前中断"),
				io.ErrUnexpectedEOF)})
		}
	}()
	return ch, nil
}

type responsesEvent struct {
	Type     string          `json:"type"`
	Delta    string          `json:"delta"`
	Item     json.RawMessage `json:"item"`
	Code     string          `json:"code"`
	Param    string          `json:"param"`
	Error    *responsesError `json:"error"`
	Response *struct {
		Output            []json.RawMessage `json:"output"`
		Usage             *responsesUsage   `json:"usage"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Error *responsesError `json:"error"`
	} `json:"response"`
	Message string `json:"message"`
}

type responsesError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *responsesEvent) apiError() *APIError {
	err := e.Error
	if e.Response != nil && e.Response.Error != nil {
		err = e.Response.Error
	}
	if err == nil {
		msg := e.Message
		if msg == "" {
			msg = "response failed"
		}
		return &APIError{Code: e.Code, Body: msg}
	}
	code := err.Code
	if code == "" {
		code = e.Code
	}
	msg := err.Message
	if msg == "" {
		msg = e.Message
	}
	if msg == "" {
		msg = "response failed"
	}
	return &APIError{Code: code, Body: msg}
}

type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
}

func (u *responsesUsage) usage() *Usage {
	return &Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens,
		CachedTokens:     u.InputTokensDetails.CachedTokens,
		CacheWriteTokens: u.InputTokensDetails.CacheWriteTokens,
	}
}

type responsesItem struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	CallID string          `json:"call_id"`
	Name   string          `json:"name"`
	Status string          `json:"status"`
	Args   json.RawMessage `json:"arguments"`
}

// toolCallsFromItems 按输出顺序取出 function_call。arguments 是 JSON 字符串，
// 但个别兼容实现可能给对象，两种都收。参数不完整或无法解析时返回错误，绝不用
// 空对象顶替——那会把一次没写完的调用伪装成合法调用交给 Agent 执行。
//
// sawIncomplete 表示存在 status 不是 completed 的函数调用；调用方在响应被截断
// 时必须据此拒绝执行。
func toolCallsFromItems(items []json.RawMessage) (calls []ToolCall, sawIncomplete bool, err error) {
	type indexed struct {
		index int
		call  ToolCall
	}
	var out []indexed
	for i, raw := range items {
		var item responsesItem
		if err := json.Unmarshal(raw, &item); err != nil || item.Type != "function_call" {
			continue
		}
		if item.Status != "" && item.Status != "completed" {
			sawIncomplete = true
			continue
		}
		args := strings.TrimSpace(string(item.Args))
		if args == "" || args == "null" {
			return nil, sawIncomplete, fmt.Errorf("function_call %q has no arguments", item.Name)
		}
		if strings.HasPrefix(args, `"`) {
			var s string
			if err := json.Unmarshal(item.Args, &s); err != nil {
				return nil, sawIncomplete, fmt.Errorf("function_call %q arguments: %w", item.Name, err)
			}
			args = s
		}
		if !json.Valid([]byte(args)) {
			return nil, sawIncomplete, fmt.Errorf("function_call %q arguments are not valid JSON", item.Name)
		}
		id := item.CallID
		if id == "" {
			// call_id 是回传工具结果时要用的 ID；个别兼容实现只给 id，兜底。
			id = item.ID
		}
		out = append(out, indexed{index: i, call: ToolCall{
			ID: id, Name: item.Name, Arguments: json.RawMessage(args),
		}})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].index < out[j].index })
	calls = make([]ToolCall, 0, len(out))
	for _, o := range out {
		calls = append(calls, o.call)
	}
	return calls, sawIncomplete, nil
}
