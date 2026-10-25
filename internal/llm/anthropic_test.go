package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// antFrame 是一条 Anthropic SSE 帧（事件名 + data 负载）。
type antFrame struct{ event, data string }

// antServer 返回一个记录请求体并回放固定 SSE 帧的测试服务。
func antServer(t *testing.T, frames []antFrame, gotBody *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("X-Api-Key") == "" || r.Header.Get("Anthropic-Version") != anthropicVersion {
			t.Errorf("bad headers: %v", r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, gotBody); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f.event, f.data)
		}
	}))
}

type antResult struct {
	content   string
	reasoning string
	calls     []ToolCall
	blocks    []ReasoningBlock
	usage     *Usage
	usageHits int
	err       error
}

func antCollect(ch <-chan StreamEvent) antResult {
	var r antResult
	for ev := range ch {
		switch ev.Type {
		case EventContentDelta:
			r.content += ev.Content
		case EventReasoningDelta:
			r.reasoning += ev.Content
		case EventToolCalls:
			r.calls = ev.ToolCalls
		case EventReasoning:
			r.blocks = ev.Reasoning
		case EventUsage:
			r.usage = ev.Usage
			r.usageHits++
		case EventError:
			r.err = ev.Err
		}
	}
	return r
}

// antRun 跑一次完整请求，返回请求体与流式产出。
func antRun(t *testing.T, frames []antFrame, req ChatRequest) (map[string]any, antResult) {
	t.Helper()
	var got map[string]any
	srv := antServer(t, frames, &got)
	defer srv.Close()

	ch, err := NewAnthropic("", srv.URL, "sk-ant-test", false).ChatStream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return got, antCollect(ch)
}

// antRunCached 同上，但开启提示词缓存。
func antRunCached(t *testing.T, frames []antFrame, req ChatRequest) map[string]any {
	t.Helper()
	var got map[string]any
	srv := antServer(t, frames, &got)
	defer srv.Close()

	ch, err := NewAnthropic("", srv.URL, "sk-ant-test", true).ChatStream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	antCollect(ch)
	return got
}

func textFrames(text string) []antFrame {
	return []antFrame{
		{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + text + `"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}
}

func TestAnthropicThinkingAdaptiveRequest(t *testing.T) {
	got, r := antRun(t, textFrames("你好"), ChatRequest{
		Model:       "claude-opus-4-5",
		Messages:    testMessages("hi"),
		Temperature: 0.7,
		MaxTokens:   4096,
		Thinking:    "xhigh",
	})

	if r.content != "你好" {
		t.Errorf("content = %q", r.content)
	}
	thinking, _ := got["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" {
		t.Errorf("thinking = %v, want adaptive", got["thinking"])
	}
	out, _ := got["output_config"].(map[string]any)
	if out["effort"] != "xhigh" {
		t.Errorf("effort = %v", got["output_config"])
	}
	// 当前世代 Claude 模型不接受采样参数，一律不发送
	if _, has := got["temperature"]; has {
		t.Error("temperature should not be sent to anthropic")
	}
	if got["max_tokens"] != float64(4096) || got["stream"] != true {
		t.Errorf("max_tokens=%v stream=%v", got["max_tokens"], got["stream"])
	}
}

func TestAnthropicThinkingOffRequest(t *testing.T) {
	got, _ := antRun(t, textFrames("ok"), ChatRequest{
		Model:    "claude-opus-4-5",
		Messages: testMessages("hi"),
		Thinking: "off",
	})

	thinking, _ := got["thinking"].(map[string]any)
	if thinking["type"] != "disabled" {
		t.Errorf("thinking = %v, want disabled", got["thinking"])
	}
	if _, has := got["output_config"]; has {
		t.Error("output_config should be absent when thinking is off")
	}
	if got["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens = %v, want default 4096", got["max_tokens"])
	}
}

func TestAnthropicToolUseAssembly(t *testing.T) {
	_, r := antRun(t, []antFrame{
		{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":5}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"a.txt\"}"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_stop", `{"type":"message_stop"}`},
	}, ChatRequest{Model: "claude-opus-4-5", Messages: testMessages("读文件")})

	if len(r.calls) != 1 {
		t.Fatalf("calls = %+v", r.calls)
	}
	if r.calls[0].ID != "toolu_1" || r.calls[0].Name != "read_file" {
		t.Errorf("call = %+v", r.calls[0])
	}
	if string(r.calls[0].Arguments) != `{"path":"a.txt"}` {
		t.Errorf("arguments = %s", r.calls[0].Arguments)
	}
}

func TestAnthropicThinkingBlockWithSignature(t *testing.T) {
	_, r := antRun(t, []antFrame{
		{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":5}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先想"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"一下"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"abc"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"答案"}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}, ChatRequest{Model: "claude-opus-4-5", Messages: testMessages("hi"), Thinking: "high"})

	if r.reasoning != "先想一下" || r.content != "答案" {
		t.Errorf("reasoning=%q content=%q", r.reasoning, r.content)
	}
	if len(r.blocks) != 1 {
		t.Fatalf("blocks = %+v", r.blocks)
	}
	if r.blocks[0].Type != "thinking" || r.blocks[0].Text != "先想一下" || r.blocks[0].Signature != "sig-abc" {
		t.Errorf("block = %+v", r.blocks[0])
	}
}

// TestAnthropicSignatureSentBack：本轮之内（工具循环）的思考块要带签名原样回传，
// 工具调用离不开它。
func TestAnthropicSignatureSentBack(t *testing.T) {
	got, _ := antRun(t, textFrames("继续"), ChatRequest{
		Model: "claude-opus-4-5",
		Messages: []Message{
			{Role: RoleSystem, Content: "环境"},
			{Role: RoleUser, Content: "问题"},
			{Role: RoleAssistant, Content: "答案", Reasoning: "先想一下",
				ReasoningBlocks: []ReasoningBlock{{Type: "thinking", Text: "先想一下", Signature: "sig-abc"}},
				ToolCalls:       []ToolCall{{ID: "t1", Name: "read_file", Arguments: json.RawMessage(`{}`)}}},
			{Role: RoleTool, Content: "结果", ToolCallID: "t1"},
		},
		Thinking: "high",
	})

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v", msgs)
	}
	assistant, _ := msgs[1].(map[string]any)
	blocks, _ := assistant["content"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("assistant blocks = %v", blocks)
	}
	first, _ := blocks[0].(map[string]any)
	if first["type"] != "thinking" || first["thinking"] != "先想一下" || first["signature"] != "sig-abc" {
		t.Errorf("thinking block = %v", first)
	}
}

// TestAnthropicPriorTurnThinkingStripped：已经结束的轮次不回传思考块。新一代模型只
// 接受前缀没变的思考块，而我们的历史在轮次之间会变（本轮状态块、一次性输入、日界线），
// 回传必然 400。其余块原样保留。
func TestAnthropicPriorTurnThinkingStripped(t *testing.T) {
	got, _ := antRun(t, textFrames("继续"), ChatRequest{
		Model: "claude-opus-5-5",
		Messages: []Message{
			{Role: RoleUser, Content: "问题"},
			{Role: RoleAssistant, Content: "答案", Reasoning: "先想一下",
				ReasoningBlocks: []ReasoningBlock{{Type: "thinking", Text: "先想一下", Signature: "sig-abc"}}},
			{Role: RoleUser, Content: "再问"},
		},
		Thinking: "high",
	})
	msgs, _ := got["messages"].([]any)
	assistant, _ := msgs[1].(map[string]any)
	blocks, _ := assistant["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("上一轮的思考块应当剥掉，只剩正文: %v", blocks)
	}
	if b, _ := blocks[0].(map[string]any); b["type"] != "text" || b["text"] != "答案" {
		t.Errorf("正文应当原样保留: %v", b)
	}
}

// TestAnthropicAsksForThinkingSummary：新一代模型默认不返回思考文本（display 默认
// omitted），不显式要 summarized，界面上的思考过程就是空的。
func TestAnthropicAsksForThinkingSummary(t *testing.T) {
	got, _ := antRun(t, textFrames("ok"), ChatRequest{Model: "claude-opus-5-5", Messages: testMessages("hi"), Thinking: "high"})
	th, _ := got["thinking"].(map[string]any)
	if th["type"] != "adaptive" || th["display"] != "summarized" {
		t.Errorf("thinking = %v", th)
	}
}

func strPtr(s string) *string { return &s }

// offServer 先按 rejects 的顺序回 400，之后正常回放；记下每次请求的完整请求体。
func offServer(t *testing.T, rejects []string, seen *[]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		*seen = append(*seen, body)
		if n := len(*seen); n <= len(rejects) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, rejects[n-1])
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range textFrames("ok") {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f.event, f.data)
		}
	}))
}

// TestAnthropicOffFallsBackToBetweenTools：Sonnet 5.5 拒绝 disabled、点名要 between_tools。
// 换写法重发一次，并记住，下一次直接用对的写法。
func TestAnthropicOffFallsBackToBetweenTools(t *testing.T) {
	var seen []map[string]any
	srv := offServer(t, []string{`{"type":"error","error":{"type":"invalid_request_error","message":"To turn thinking off on this model, send \"thinking\": {\"type\": \"between_tools\"} instead of {\"type\": \"disabled\"}."}}`}, &seen)
	defer srv.Close()
	p := NewAnthropic("", srv.URL, "sk-ant-test", false)
	req := ChatRequest{Model: "claude-sonnet-5-5", Messages: testMessages("hi"), Thinking: "off"}

	for i := 0; i < 2; i++ {
		ch, err := p.ChatStream(context.Background(), req)
		if err != nil {
			t.Fatalf("第 %d 次: %v", i+1, err)
		}
		antCollect(ch)
	}
	if len(seen) != 3 {
		t.Fatalf("应当是 400 一次、重发一次、之后直接成功，共 3 次请求，实际 %d 次: %v", len(seen), seen)
	}
	for i, want := range []string{"disabled", "between_tools", "between_tools"} {
		th, _ := seen[i]["thinking"].(map[string]any)
		if th["type"] != want {
			t.Errorf("第 %d 次请求 thinking = %v，应为 %s", i+1, seen[i], want)
		}
		if _, has := th["display"]; has {
			t.Errorf("关闭思考时不能带 display（会 400）: %v", th)
		}
	}
}

// TestAnthropicOffAdaptiveLowWhenAlwaysOn：思考常开的模型（Fable 5.1、Opus 5.5）任何关闭
// 写法都是 400，退到「adaptive + 最低档 low」。只是不发 thinking 的话，模型会按默认档位
// 去想，而压缩、记忆提炼这些以 off 发起的内部调用图的正是快和省。
func TestAnthropicOffAdaptiveLowWhenAlwaysOn(t *testing.T) {
	var seen []map[string]any
	srv := offServer(t, []string{`{"type":"error","error":{"type":"invalid_request_error","message":"\"thinking.type.disabled\" is not supported for this model. Use \"thinking.type.adaptive\" and \"output_config.effort\" to control thinking behavior."}}`}, &seen)
	defer srv.Close()
	ch, err := NewAnthropic("", srv.URL, "sk-ant-test", false).ChatStream(context.Background(),
		ChatRequest{Model: "claude-opus-5-5", Messages: testMessages("hi"), Thinking: "off"})
	if err != nil {
		t.Fatal(err)
	}
	antCollect(ch)
	if len(seen) != 2 {
		t.Fatalf("应当 400 一次后重发，实际 %d 次", len(seen))
	}
	th, _ := seen[1]["thinking"].(map[string]any)
	oc, _ := seen[1]["output_config"].(map[string]any)
	if th["type"] != "adaptive" || oc["effort"] != "low" {
		t.Errorf("重发应为 adaptive + effort low: thinking=%v output_config=%v", th, oc)
	}
}

// TestAnthropicEmptyThinkingBlockKept：本轮里文本为空的签名思考块（display 为 omitted、
// 或 Sonnet 5.5 的工具间进度块）也要回传，且带着空的 thinking 字段——少回一个就是
// 「部分丢弃」，API 会拒绝。
func TestAnthropicEmptyThinkingBlockKept(t *testing.T) {
	got, _ := antRun(t, textFrames("继续"), ChatRequest{
		Model: "claude-sonnet-5-5",
		Messages: []Message{
			{Role: RoleUser, Content: "问题"},
			{Role: RoleAssistant,
				ReasoningBlocks: []ReasoningBlock{{Type: "thinking", Text: "", Signature: "sig-empty"}},
				ToolCalls:       []ToolCall{{ID: "t1", Name: "read_file", Arguments: json.RawMessage(`{}`)}}},
			{Role: RoleTool, Content: "结果", ToolCallID: "t1"},
		},
		Thinking: "high",
	})
	msgs, _ := got["messages"].([]any)
	assistant, _ := msgs[1].(map[string]any)
	blocks, _ := assistant["content"].([]any)
	first, _ := blocks[0].(map[string]any)
	text, has := first["thinking"]
	if first["type"] != "thinking" || first["signature"] != "sig-empty" || !has || text != "" {
		t.Errorf("空文本的签名思考块应当原样回传（含空的 thinking 字段）: %v", first)
	}
}

// TestAnthropicSignatureMismatchRetriedStripped：本轮里的思考块与前缀对不上（预算裁剪或
// 工具列表在轮中变了）时，按官方做法剥掉全部思考块重发一次，其余块原样保留。
func TestAnthropicSignatureMismatchRetriedStripped(t *testing.T) {
	var seen []map[string]any
	srv := offServer(t, []string{"{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"messages.1.content.0: Invalid `signature` in `thinking` block. The block is bound to a different conversation.\"}}"}, &seen)
	defer srv.Close()
	ch, err := NewAnthropic("", srv.URL, "sk-ant-test", false).ChatStream(context.Background(), ChatRequest{
		Model: "claude-opus-5-5",
		Messages: []Message{
			{Role: RoleUser, Content: "问题"},
			{Role: RoleAssistant,
				ReasoningBlocks: []ReasoningBlock{{Type: "thinking", Text: "想", Signature: "sig"}},
				ToolCalls:       []ToolCall{{ID: "t1", Name: "read_file", Arguments: json.RawMessage(`{}`)}}},
			{Role: RoleTool, Content: "结果", ToolCallID: "t1"},
		},
		Thinking: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	antCollect(ch)
	if len(seen) != 2 {
		t.Fatalf("应当 400 一次后重发一次，实际 %d 次", len(seen))
	}
	msgs, _ := seen[1]["messages"].([]any)
	assistant, _ := msgs[1].(map[string]any)
	blocks, _ := assistant["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("重发时思考块应当全部剥掉、只留工具调用: %v", blocks)
	}
	if b, _ := blocks[0].(map[string]any); b["type"] != "tool_use" {
		t.Errorf("工具调用应当原样保留: %v", b)
	}
}

// TestAnthropicUnrelated400NotRetried：与关闭思考无关的 400 原样报出，不重发。
func TestAnthropicUnrelated400NotRetried(t *testing.T) {
	var seen []map[string]any
	srv := offServer(t, []string{`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: too large"}}`}, &seen)
	defer srv.Close()
	_, err := NewAnthropic("", srv.URL, "sk-ant-test", false).ChatStream(context.Background(),
		ChatRequest{Model: "claude-opus-5-5", Messages: testMessages("hi"), Thinking: "off"})
	if err == nil || len(seen) != 1 {
		t.Errorf("应当一次请求后报错，实际 %d 次，err=%v", len(seen), err)
	}
}

func TestAnthropicUnsignedReasoningDropped(t *testing.T) {
	got, _ := antRun(t, textFrames("ok"), ChatRequest{
		Model: "claude-opus-4-5",
		Messages: []Message{
			{Role: RoleUser, Content: "问题"},
			// 来自其他 provider 的历史：有思考文本但没有签名
			{Role: RoleAssistant, Content: "答案", Reasoning: "之前的思考"},
			{Role: RoleUser, Content: "再问"},
		},
		Thinking: "high",
	})

	msgs, _ := got["messages"].([]any)
	assistant, _ := msgs[1].(map[string]any)
	blocks, _ := assistant["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("assistant blocks = %v", blocks)
	}
	if b, _ := blocks[0].(map[string]any); b["type"] != "text" {
		t.Errorf("unsigned thinking should be dropped, got %v", b)
	}
}

func TestAnthropicSystemHoistedAndToolResultsMerged(t *testing.T) {
	got, _ := antRun(t, textFrames("ok"), ChatRequest{
		Model: "claude-opus-4-5",
		Messages: []Message{
			{Role: RoleSystem, Content: "环境块"},
			{Role: RoleUser, Content: "读两个文件"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: "t1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a"}`)},
				{ID: "t2", Name: "read_file", Arguments: json.RawMessage(`{"path":"b"}`)},
			}},
			{Role: RoleTool, Content: "内容 A", ToolCallID: "t1"},
			{Role: RoleTool, Content: "内容 B", ToolCallID: "t2"},
		},
	})

	sys, _ := got["system"].([]any)
	if len(sys) != 1 {
		t.Fatalf("system = %v", got["system"])
	}
	if b, _ := sys[0].(map[string]any); b["text"] != "环境块" {
		t.Errorf("system = %v", got["system"])
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 3 { // user / assistant(tool_use x2) / user(tool_result x2)
		t.Fatalf("messages = %v", msgs)
	}
	last, _ := msgs[2].(map[string]any)
	if last["role"] != "user" {
		t.Errorf("tool results must ride a user message, got %v", last["role"])
	}
	blocks, _ := last["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("tool_result blocks = %v", blocks)
	}
	b0, _ := blocks[0].(map[string]any)
	if b0["type"] != "tool_result" || b0["tool_use_id"] != "t1" || b0["content"] != "内容 A" {
		t.Errorf("tool_result = %v", b0)
	}
}

func TestAnthropicConsecutiveUsersMerged(t *testing.T) {
	got, _ := antRun(t, textFrames("ok"), ChatRequest{
		Model: "claude-opus-4-5",
		Messages: []Message{
			// 压缩后的形状：摘要（user）+ 本轮输入（user）
			{Role: RoleUser, Content: "摘要"},
			{Role: RoleUser, Content: "新问题"},
		},
	})

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("consecutive users must merge, got %v", msgs)
	}
	m, _ := msgs[0].(map[string]any)
	blocks, _ := m["content"].([]any)
	if len(blocks) != 2 {
		t.Errorf("merged blocks = %v", blocks)
	}
}

func TestAnthropicUsageMerged(t *testing.T) {
	_, r := antRun(t, []antFrame{
		{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":50,"output_tokens":1}}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`},
		{"message_delta", `{"type":"message_delta","usage":{"output_tokens":30}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}, ChatRequest{Model: "claude-opus-4-5", Messages: testMessages("hi")})

	if r.usageHits != 1 {
		t.Fatalf("usage emitted %d times, want 1", r.usageHits)
	}
	if r.usage.PromptTokens != 150 || r.usage.CompletionTokens != 30 || r.usage.TotalTokens != 180 {
		t.Errorf("usage = %+v", r.usage)
	}
}

func TestAnthropicStreamErrorEvent(t *testing.T) {
	_, r := antRun(t, []antFrame{
		{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":5}}}`},
		{"error", `{"type":"error","error":{"type":"overloaded_error","message":"服务繁忙"}}`},
	}, ChatRequest{Model: "claude-opus-4-5", Messages: testMessages("hi")})

	if r.err == nil {
		t.Fatal("expected stream error")
	}
}

func TestAnthropicAPIErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	}))
	defer srv.Close()

	ch, err := NewAnthropic("", srv.URL, "bad", false).ChatStream(context.Background(), ChatRequest{
		Model: "claude-opus-4-5", Messages: testMessages("hi"),
	})
	if err == nil {
		t.Fatal("expected error before stream opens")
	}
	if ch != nil {
		t.Error("channel must not be opened on error")
	}
}

func TestAnthropicEndpointNotDuplicated(t *testing.T) {
	if got := NewAnthropic("", "https://api.anthropic.com/v1", "k", false).endpoint(); got != "https://api.anthropic.com/v1/messages" {
		t.Errorf("endpoint = %s", got)
	}
	if got := NewAnthropic("", "https://api.anthropic.com/", "k", false).endpoint(); got != "https://api.anthropic.com/v1/messages" {
		t.Errorf("endpoint = %s", got)
	}
}

// TestAnthropicWorkspaceErrorIsExplained：没绑定工作区的密钥每个请求都 400。前面补一句
// 说清该去哪儿改，并归为配置错误原样示人，不交给角色演成走神。
func TestAnthropicWorkspaceErrorIsExplained(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"This API key is not scoped to a workspace. Set the anthropic-workspace-id header."}}`))
	}))
	defer srv.Close()
	p := NewAnthropic("", srv.URL, "sk-ant-test", false)
	_, err := p.ChatStream(context.Background(), ChatRequest{Model: "claude-sonnet-5-5", Messages: testMessages("hi"), Thinking: "high"})
	if err == nil || !strings.Contains(err.Error(), "工作区") || !strings.Contains(err.Error(), "anthropic-workspace-id") {
		t.Fatalf("应补一句说明并保留原文: %v", err)
	}
	if !IsConfigError(err) {
		t.Error("应归为配置错误")
	}
}
