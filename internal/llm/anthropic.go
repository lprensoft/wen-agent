package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
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

	// offModes 记着各模型「关闭思考」实际该怎么写，见 offMode。
	mu       sync.Mutex
	offModes map[string]string
}

// 「关闭思考」的三种写法。各代模型对它的接受程度不一：较早的接受 disabled；
// Sonnet 5.5 拒绝 disabled，要写 between_tools；Fable 5.1、Opus 5.5 等思考常开，
// 任何关闭写法都是 400，只能退到「adaptive + 最低档 low」——那是这类模型上离
// 「关闭」最近的写法。压缩、记忆提炼这些内部调用都以 off 发起，图的就是快和省，
// 若只是不发 thinking 字段，模型会按默认档位（medium 甚至 high）去想。
const (
	offDisabled     = "disabled"
	offBetweenTools = "between_tools"
	offAdaptiveLow  = "adaptive_low"
)

func NewAnthropic(lang, baseURL, apiKey string, promptCache bool) *Anthropic {
	return &Anthropic{
		lang:     lang,
		baseURL:  strings.TrimRight(baseURL, "/"),
		apiKey:   apiKey,
		cache:    promptCache,
		offModes: map[string]string{},
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
	Type string `json:"type"` // adaptive / disabled / between_tools
	// Display 控制思考文本返不返回。新一代模型默认 omitted（只给签名、文本为空），
	// 界面上的思考过程因此是空的；显式要 summarized 才拿得到摘要。
	// 它只能与 adaptive 同发：disabled 与 between_tools 带上它都是 400。
	Display string `json:"display,omitempty"`
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

	// Thinking 用指针：思考块的 thinking 字段可能是空串（display 为 omitted 时、
	// Sonnet 5.5 的工具间进度块），回传时也得照样带上这个空字段；而它与别的块类型
	// 共用这个结构，普通字符串加 omitempty 会把它吞掉。
	Thinking  *string `json:"thinking,omitempty"`  // thinking
	Signature string  `json:"signature,omitempty"` // thinking 的签名，必须原样回传
	Data      string  `json:"data,omitempty"`      // redacted_thinking 的密文

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

// antBuild 是装配请求时 ChatRequest 之外的几样选择。
type antBuild struct {
	cache bool
	// off 是「关闭思考」在这个模型上的写法（offDisabled / offBetweenTools /
	// offAdaptiveLow），只在 req.Thinking 为关闭时用到。
	off string
	// stripAll 为真时连本轮的思考块也一并剥掉，见 ChatStream 里那次签名失败的重发。
	stripAll bool
}

func buildAnthropicRequest(req ChatRequest, o antBuild) antRequest {
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
		switch o.off {
		case offAdaptiveLow:
			wr.Thinking = &antThinking{Type: "adaptive", Display: "summarized"}
			wr.OutputConfig = &antOutput{Effort: "low"}
		case offBetweenTools:
			wr.Thinking = &antThinking{Type: offBetweenTools}
		default:
			wr.Thinking = &antThinking{Type: offDisabled}
		}
	default:
		wr.Thinking = &antThinking{Type: "adaptive", Display: "summarized"}
		wr.OutputConfig = &antOutput{Effort: req.Thinking}
	}

	// 已经结束的轮次不回传思考块，只保留本轮（最后一条用户消息之后）的。
	//
	// 新一代模型（Fable 5.1、Opus 5.5、Sonnet 5.5）只接受「前缀没变」的思考块：
	// system、tools 与它之前的每一条消息都要与生成它时逐字节一致，否则整个请求
	// 400。我们的历史不是只追加的——本轮状态块只拼在当轮输入上、下一轮就去掉，
	// 一次性输入（心跳提示）事后剔除，日界线与间隔标注事后补上，还有压缩——
	// 所以上一轮的思考块到了这一轮必然对不上。官方给的两条出路之一正是把它们剥掉，
	// 其余块原样保留；较早的模型本来就只保留最后一轮，服务端会自动剥。
	// 代价是 Opus 4.5 起、Sonnet 4.6 起的模型默认本会保留之前各轮的思考，现在看不到了；
	// Haiku 4.5 与更早的模型本来就只保留最后一轮。
	//
	// 本轮之内（工具循环的多次请求）消息通常只追加不改，思考块照常回传，这也是工具
	// 调用必需的。「通常」是因为预算裁剪或工具列表在轮中变化时前缀也会变，那种情况
	// 由 ChatStream 按官方做法剥掉全部思考块重发一次（stripAll）。
	lastUser := -1
	for i, m := range req.Messages {
		if m.Role == RoleUser {
			lastUser = i
		}
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

	for i, m := range req.Messages {
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
			reasoning := m.ReasoningBlocks
			if i < lastUser || o.stripAll {
				reasoning = nil
			}
			for _, b := range reasoning {
				switch {
				case b.Type == "redacted_thinking" && b.Data != "":
					blocks = append(blocks, antBlock{Type: "redacted_thinking", Data: b.Data})
				case b.Signature != "":
					// 文本为空也要回传（omitted 或工具间进度块）：签名里才是完整的思考，
					// 本轮的思考块少回一个就是「部分丢弃」，API 会拒绝
					text := b.Text
					blocks = append(blocks, antBlock{Type: "thinking", Thinking: &text, Signature: b.Signature})
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
	if o.cache {
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

// offMode 返回这个模型上「关闭思考」该用的写法，没学到过就先试 disabled。
func (p *Anthropic) offMode(model string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.offModes[model]; ok {
		return m
	}
	return offDisabled
}

// nextOffMode 根据一次 400 的报错决定换哪种写法再试；返回空串表示这次失败与此无关。
//
// 靠报错学而不是维护一张「哪个模型认哪种写法」的表：那张表每出一代模型就要跟着改，
// 漏一次就是关掉思考的用户整轮报错。报错原文由官方文档给出：Sonnet 5.5 的那句里
// 点名要 between_tools，思考常开的模型说 "thinking.type.disabled" is not supported。
// 只看这两个关键词、不比对整句——官方提醒过措辞会变。
func nextOffMode(current, body string) string {
	switch {
	case current == offDisabled && strings.Contains(body, "between_tools"):
		return offBetweenTools
	case current != offAdaptiveLow && strings.Contains(body, "thinking.type."+current):
		return offAdaptiveLow
	}
	return ""
}

// isSignatureMismatch 认出「回传的思考块与前缀对不上」的 400。只看报错开头那半句：
// 官方说结尾那句点名改了什么的话措辞会变，不要拿来匹配。
func isSignatureMismatch(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest &&
		strings.Contains(apiErr.Body, "Invalid `signature` in `thinking` block")
}

func (p *Anthropic) ChatStream(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	b := antBuild{cache: p.cache, off: p.offMode(req.Model)}
	resp, err := p.send(ctx, buildAnthropicRequest(req, b))
	// 关闭思考被这个模型拒绝：换成它接受的写法重发，并记住，以后不再白挨这一次 400。
	// 最多换两次（disabled → between_tools → 不发），不会无限重试。
	for tries := 0; tries < 2 && (req.Thinking == "" || req.Thinking == "off"); tries++ {
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
			break
		}
		next := nextOffMode(b.off, apiErr.Body)
		if next == "" {
			break
		}
		b.off = next
		p.mu.Lock()
		p.offModes[req.Model] = b.off
		p.mu.Unlock()
		resp, err = p.send(ctx, buildAnthropicRequest(req, b))
	}
	// 回传的思考块与前缀对不上：本轮之内本该只追加，但预算裁剪或工具列表在轮中变化
	// 时前缀也会变。官方给的做法是剥掉全部思考块（其余块原样留着）重发一次。
	// 原样重发没用，官方也说了同一请求体重试不会好。
	if isSignatureMismatch(err) {
		b.stripAll = true
		resp, err = p.send(ctx, buildAnthropicRequest(req, b))
	}
	if err != nil {
		return nil, err
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

// send 发出一次请求；非 200 时读出报错正文，作为 *APIError 返回。
func (p *Anthropic) send(ctx context.Context, wr antRequest) (*http.Response, error) {
	body, err := json.Marshal(wr)
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
		return nil, p.apiError(resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	return resp, nil
}

// apiError 包装非 200 响应。Console 新建的个人密钥若没绑定工作区，每个请求都会
// 400 并要求带 anthropic-workspace-id 头——原文是英文的协议细节，用户多半看不出
// 该去哪儿改，所以在前面补一句说人话的，并归为配置错误原样示人（不交给角色演成走神）。
func (p *Anthropic) apiError(status int, body string) *APIError {
	e := &APIError{Status: status, Body: body}
	if status == http.StatusBadRequest && strings.Contains(strings.ToLower(body), "anthropic-workspace-id") {
		e.Kind = KindConfig
		e.Body = i18n.T(p.lang, "llm.err.anthropic_workspace",
			"这把 API 密钥没有绑定工作区：请在 Claude Console 新建密钥时选定一个工作区，再换上新密钥。") + " " + body
	}
	return e
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
