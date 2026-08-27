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

// OpenAICompat 通过 OpenAI 兼容协议对接后端（DeepSeek、MiniMax、Qwen 等）。
// 自实现请求与 SSE 解析，以支持各家思考扩展字段的完整往返；
// 思考参数按 dialect（思考参数方言）分派，见 dialect.go。
type OpenAICompat struct {
	lang    string // 报错说话用的语言，见 Config.Lang
	baseURL string
	apiKey  string
	dialect string
	client  *http.Client
}

func NewOpenAICompat(lang, baseURL, apiKey, dialect string) *OpenAICompat {
	return &OpenAICompat{
		lang:    lang,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		dialect: dialect,
		// 总超时不设上限（长思考+长输出），连接阶段单独限时
		client: &http.Client{Transport: &http.Transport{
			ResponseHeaderTimeout: 60 * time.Second,
		}},
	}
}

// ---------- 请求/响应的线上格式 ----------

type wireThinking struct {
	Type string `json:"type"` // enabled | disabled
}

type wireRequest struct {
	Model       string        `json:"model"`
	Messages    []wireMessage `json:"messages"`
	Tools       []wireTool    `json:"tools,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	// MaxCompletionTokens 是 max_tokens 的替代写法。GPT-5 起的 OpenAI 模型在
	// /chat/completions 上**拒绝** max_tokens，报 "Unsupported parameter"，
	// 只接受这一个；两者互斥，按方言二选一（见 dialectSpec.maxCompletionTokens）。
	MaxCompletionTokens int                `json:"max_completion_tokens,omitempty"`
	Stream              bool               `json:"stream"`
	StreamOptions       *wireStreamOptions `json:"stream_options,omitempty"`
	Thinking            *wireThinking      `json:"thinking,omitempty"`
	ReasoningEffort     string             `json:"reasoning_effort,omitempty"`
	ReasoningSplit      bool               `json:"reasoning_split,omitempty"` // MiniMax：思考走独立字段而非 <think> 内联
	EnableThinking      *bool              `json:"enable_thinking,omitempty"` // Qwen：思考开关
}

type wireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function wireToolCallFunc `json:"function"`
}

type wireToolCallFunc struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type wireTool struct {
	Type     string      `json:"type"`
	Function wireToolDef `json:"function"`
}

type wireToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireChunk struct {
	Choices []struct {
		Delta struct {
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content"`
			ToolCalls        []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"` // 最后一个 chunk 携带（需 stream_options.include_usage）
	// Error 是流中途下发的错误帧（限流、内容拦截等都可能走这条路）。
	// 不解析它的话错误会被当成无法识别的帧静默跳过，表现成「空回复且正常结束」。
	Error *wireError `json:"error"`
}

type wireError struct {
	Type string `json:"type"`
	// Code 各家类型不一（字符串或数字），收原文避免解析失败连累整帧。
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
}

func (e *wireError) text() string {
	head := e.Type
	if head == "" {
		head = strings.Trim(string(e.Code), `"`)
	}
	if head == "" || head == "null" {
		return e.Message
	}
	return head + ": " + e.Message
}

// wireUsage 单独一个结构而不是直接解进 llm.Usage：缓存命中数各家字段名不同，
// DeepSeek 放在顶层的 prompt_cache_hit_tokens，OpenAI 放在 prompt_tokens_details
// 里面，两种都收下再归一。
type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// DeepSeek：命中磁盘缓存的输入 token（服务端自动缓存，无需任何请求参数）
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
	// OpenAI：同一信息在这里
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// usage 归一成 llm.Usage。写入侧恒为 0：这两家的缓存写入不单独计费。
func (u *wireUsage) usage() *Usage {
	out := &Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		CachedTokens:     u.PromptCacheHitTokens,
	}
	if out.CachedTokens == 0 && u.PromptTokensDetails != nil {
		out.CachedTokens = u.PromptTokensDetails.CachedTokens
	}
	return out
}

func buildRequest(req ChatRequest, dialect string) wireRequest {
	wr := wireRequest{
		Model:         req.Model,
		Stream:        true,
		StreamOptions: &wireStreamOptions{IncludeUsage: true},
	}
	if specOf(dialect).maxCompletionTokens {
		wr.MaxCompletionTokens = req.MaxTokens
	} else {
		wr.MaxTokens = req.MaxTokens
	}
	applyThinking(&wr, req, dialect)
	for _, m := range req.Messages {
		wm := wireMessage{
			Role:             m.Role,
			Content:          m.Content,
			ReasoningContent: m.Reasoning,
			ToolCallID:       m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: wireToolCallFunc{
					Name:      tc.Name,
					Arguments: string(tc.Arguments),
				},
			})
		}
		wr.Messages = append(wr.Messages, wm)
	}
	for _, t := range req.Tools {
		wr.Tools = append(wr.Tools, wireTool{
			Type: "function",
			Function: wireToolDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Schema,
			},
		})
	}
	return wr
}

// applyThinking 按方言拼思考参数。
// DeepSeek 在思考模式下拒绝 temperature 等采样参数，这是它特有的限制；
// MiniMax 反而推荐思考时照常发 temperature，不能全局套用。
// 档位一律经 snapEffort 收敛到对方接受的取值，不原样透传。
func applyThinking(wr *wireRequest, req ChatRequest, dialect string) {
	spec := specOf(dialect)
	off := req.Thinking == "" || req.Thinking == "off"
	temp := req.Temperature
	// sample 记下这门方言在这一轮该不该发 temperature，最后统一落笔——
	// noSampling 的方言（OpenAI 系）任何情况下都不发，漏一处就是一个 400。
	sample := false

	switch spec.shape {
	case shapeThinkingEffort:
		if off {
			wr.Thinking = &wireThinking{Type: "disabled"}
			sample = true
		} else {
			wr.Thinking = &wireThinking{Type: "enabled"}
			wr.ReasoningEffort = snapEffort(spec.levels, req.Thinking)
		}
	case shapeAdaptive:
		if off {
			// 注意 M2.x 系列并不真正关闭思考（平台接受 disabled 但无效）
			wr.Thinking = &wireThinking{Type: "disabled"}
		} else {
			wr.Thinking = &wireThinking{Type: "adaptive"}
			wr.ReasoningSplit = true // 不带的话思考会以 <think> 内联进正文
		}
		sample = true
	case shapeEnableThinking:
		et := !off
		wr.EnableThinking = &et
		sample = true
	case shapeEffortOnly:
		if off {
			// offLevel 非空表示对方有一个「不思考」的档位（OpenAI 的 none）；
			// 没有的话就什么都不发，让它按自己的默认来。
			wr.ReasoningEffort = spec.offLevel
			sample = true
		} else {
			wr.ReasoningEffort = snapEffort(spec.levels, req.Thinking)
		}
	case shapeNothing:
		sample = true
	}

	if sample && !spec.noSampling {
		wr.Temperature = &temp
	}
}

// ---------- 流式调用 ----------

func (p *OpenAICompat) ChatStream(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	body, err := json.Marshal(buildRequest(req, p.dialect))
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// 本地推理服务（Ollama 等）不做鉴权，密钥为空时干脆不发这个头，
	// 免得对面把 "Bearer " 当成一个真的空密钥去校验。
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

		acc := map[int]*partialCall{}
		// MiniMax 兜底：即便 reasoning_split 没生效（历史模型/网关差异），
		// 也把 <think>…</think> 从正文剥出来按思考下发
		var tf *thinkFilter
		if p.dialect == DialectMiniMax {
			tf = &thinkFilter{}
		}
		finish := func() {
			if tf != nil {
				if c, r := tf.flush(); c != "" || r != "" {
					if r != "" {
						emit(StreamEvent{Type: EventReasoningDelta, Content: r})
					}
					if c != "" {
						emit(StreamEvent{Type: EventContentDelta, Content: c})
					}
				}
			}
			if len(acc) > 0 {
				emit(StreamEvent{Type: EventToolCalls, ToolCalls: assembleCalls(acc)})
			}
			emit(StreamEvent{Type: EventDone})
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
				finish()
				return
			}
			var chunk wireChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}
			if chunk.Error != nil {
				emit(StreamEvent{Type: EventError, Err: &APIError{Body: chunk.Error.text()}})
				return
			}
			if chunk.Usage != nil {
				if !emit(StreamEvent{Type: EventUsage, Usage: chunk.Usage.usage()}) {
					return
				}
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			switch chunk.Choices[0].FinishReason {
			case "content_filter":
				// 内容被后端安全策略拦截。当成错误上报而不是静默收尾，
				// 否则表现为「说到一半没了」且没有任何解释。
				emit(StreamEvent{Type: EventError, Err: &APIError{Kind: KindContentFilter, Body: i18n.T(p.lang, "llm.err.content_filter", "内容被提供商安全策略拦截（finish_reason=content_filter）")}})
				return
			case "length":
				i18n.Logf(p.lang, "log.llm.truncated", "llm: 输出被 max_tokens 截断（finish_reason=length）")
			}
			delta := chunk.Choices[0].Delta
			if delta.ReasoningContent != "" {
				if !emit(StreamEvent{Type: EventReasoningDelta, Content: delta.ReasoningContent}) {
					return
				}
			}
			if delta.Content != "" {
				out, think := delta.Content, ""
				if tf != nil {
					out, think = tf.feed(delta.Content)
				}
				if think != "" {
					if !emit(StreamEvent{Type: EventReasoningDelta, Content: think}) {
						return
					}
				}
				if out != "" {
					if !emit(StreamEvent{Type: EventContentDelta, Content: out}) {
						return
					}
				}
			}
			for _, tc := range delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				pt, ok := acc[idx]
				if !ok {
					pt = &partialCall{}
					acc[idx] = pt
				}
				if tc.ID != "" {
					pt.id = tc.ID
				}
				if tc.Function.Name != "" {
					pt.name = tc.Function.Name
				}
				pt.args.WriteString(tc.Function.Arguments)
			}
		}
		if err := sc.Err(); err != nil {
			emit(StreamEvent{Type: EventError, Err: fmt.Errorf("read stream: %w", err)})
			return
		}
		// 流未见 [DONE] 即结束：按已收内容收尾
		finish()
	}()
	return ch, nil
}

// partialCall 累积流式下发的 tool_call 分片。
type partialCall struct {
	id   string
	name string
	args strings.Builder
}

func assembleCalls(acc map[int]*partialCall) []ToolCall {
	idxs := make([]int, 0, len(acc))
	for i := range acc {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)

	calls := make([]ToolCall, 0, len(acc))
	for _, i := range idxs {
		pt := acc[i]
		args := pt.args.String()
		if args == "" {
			args = "{}"
		}
		calls = append(calls, ToolCall{
			ID:        pt.id,
			Name:      pt.name,
			Arguments: json.RawMessage(args),
		})
	}
	return calls
}
