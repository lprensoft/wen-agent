package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// responsesServer 记录请求体并按顺序回放 SSE 帧。
func responsesServer(t *testing.T, frames []string) (*httptest.Server, *map[string]any) {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", f)
		}
	}))
	return srv, &got
}

func collectResponses(t *testing.T, ch <-chan StreamEvent) (content, reasoning string, calls []ToolCall, usage *Usage, raw json.RawMessage) {
	t.Helper()
	for ev := range ch {
		switch ev.Type {
		case EventContentDelta:
			content += ev.Content
		case EventReasoningDelta:
			reasoning += ev.Content
		case EventToolCalls:
			if calls != nil {
				t.Fatalf("tool calls emitted more than once")
			}
			calls = ev.ToolCalls
		case EventUsage:
			usage = ev.Usage
		case EventDone:
			raw = ev.RawItems
		case EventError:
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	return
}

func TestResponsesRequestShapeAndReplay(t *testing.T) {
	srv, got := responsesServer(t, []string{
		`{"type":"response.output_text.delta","delta":"ok"}`,
		`{"type":"response.completed","response":{"output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
	})
	defer srv.Close()

	replay := json.RawMessage(`[{"type":"reasoning","id":"rs_1","encrypted_content":"cipher"},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a\"}"}]`)
	p := NewResponses("", srv.URL, "sk-test")
	ch, err := p.ChatStream(context.Background(), ChatRequest{
		Model: "gpt-test",
		Messages: []Message{
			{Role: RoleSystem, Content: "你是助手"},
			{Role: RoleUser, Content: "看看 a"},
			{Role: RoleAssistant, ResponsesItems: replay, ResponsesOrigin: p.origin("gpt-test"), Content: "旧回复", ToolCalls: []ToolCall{{ID: "call_1", Name: "read_file"}}},
			{Role: RoleTool, Content: "文件内容", ToolCallID: "call_1"},
		},
		Tools:     []ToolSpec{{Name: "read_file", Description: "读文件", Schema: json.RawMessage(`{"type":"object"}`)}},
		MaxTokens: 256,
		Thinking:  "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	collectResponses(t, ch)

	if (*got)["store"] != false || (*got)["stream"] != true {
		t.Fatalf("store/stream = %v/%v", (*got)["store"], (*got)["stream"])
	}
	if (*got)["instructions"] != "你是助手" {
		t.Fatalf("instructions = %v", (*got)["instructions"])
	}
	if (*got)["max_output_tokens"] != float64(256) {
		t.Fatalf("max_output_tokens = %v", (*got)["max_output_tokens"])
	}
	if _, has := (*got)["temperature"]; has {
		t.Fatal("temperature must be omitted")
	}
	reasoning, _ := (*got)["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning = %v", (*got)["reasoning"])
	}
	include, _ := (*got)["include"].([]any)
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %v", (*got)["include"])
	}
	tools, _ := (*got)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", (*got)["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "read_file" {
		t.Fatalf("tool = %v", tool)
	}

	input, _ := (*got)["input"].([]any)
	if len(input) != 4 {
		t.Fatalf("input length = %d, want 4: %v", len(input), input)
	}
	// 第一条 user，接着是原样回放的两个 Item，最后是 function_call_output。
	if input[0].(map[string]any)["role"] != "user" {
		t.Fatalf("input[0] = %v", input[0])
	}
	if input[1].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("replayed reasoning missing: %v", input[1])
	}
	if input[2].(map[string]any)["call_id"] != "call_1" {
		t.Fatalf("replayed function_call missing: %v", input[2])
	}
	toolOut := input[3].(map[string]any)
	if toolOut["type"] != "function_call_output" || toolOut["call_id"] != "call_1" || toolOut["output"] != "文件内容" {
		t.Fatalf("tool output = %v", toolOut)
	}
}

func TestResponsesStreamingAccumulatesToolCalls(t *testing.T) {
	var calls1 = `{"type":"output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a\"}"}}`
	var calls2 = `{"type":"output_item.done","item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"list_dir","arguments":"{}"}}`
	completed := `{"type":"response.completed","response":{"output":[` +
		`{"type":"reasoning","id":"rs_1","encrypted_content":"cipher"},` +
		`{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"好的"}]},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a\"}"},` +
		`{"type":"function_call","id":"fc_2","call_id":"call_2","name":"list_dir","arguments":"{}"}` +
		`],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":2}}}}`
	srv, _ := responsesServer(t, []string{
		`{"type":"response.output_text.delta","delta":"你"}`,
		`{"type":"response.output_text.delta","delta":"好"}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"想"}`,
		calls1, calls2,
		completed,
	})
	defer srv.Close()

	p := NewResponses("", srv.URL, "sk-test")
	ch, err := p.ChatStream(context.Background(), ChatRequest{
		Model: "gpt-test", Messages: []Message{{Role: RoleUser, Content: "hi"}}, Thinking: "medium",
	})
	if err != nil {
		t.Fatal(err)
	}
	content, reasoning, calls, usage, raw := collectResponses(t, ch)
	if content != "你好" || reasoning != "想" {
		t.Fatalf("content=%q reasoning=%q", content, reasoning)
	}
	if len(calls) != 2 || calls[0].ID != "call_1" || calls[1].ID != "call_2" {
		t.Fatalf("calls = %+v", calls)
	}
	if string(calls[0].Arguments) != `{"path":"a"}` {
		t.Fatalf("arguments = %s", calls[0].Arguments)
	}
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 5 || usage.CachedTokens != 3 || usage.CacheWriteTokens != 2 {
		t.Fatalf("usage = %+v", usage)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) != 4 {
		t.Fatalf("raw items = %s (err %v)", raw, err)
	}
}

func TestResponsesTransientStreamError(t *testing.T) {
	srv, _ := responsesServer(t, []string{
		`{"type":"error","code":"server_error","message":"temporarily unavailable"}`,
	})
	defer srv.Close()

	p := NewResponses("", srv.URL, "sk-test")
	ch, err := p.ChatStream(context.Background(), ChatRequest{
		Model: "gpt-test", Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got error
	for ev := range ch {
		if ev.Type == EventError {
			got = ev.Err
		}
	}
	if got == nil || !IsTransientError(got) {
		t.Fatalf("error = %v, want a transient APIError", got)
	}
	var ae *APIError
	if !errors.As(got, &ae) || ae.Code != "server_error" {
		t.Fatalf("APIError = %#v", got)
	}
}

func TestResponsesContentFilterIncomplete(t *testing.T) {
	srv, _ := responsesServer(t, []string{
		`{"type":"response.incomplete","response":{"output":[],"incomplete_details":{"reason":"content_filter"}}}`,
	})
	defer srv.Close()

	p := NewResponses("", srv.URL, "sk-test")
	ch, err := p.ChatStream(context.Background(), ChatRequest{
		Model: "gpt-test", Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got error
	for ev := range ch {
		if ev.Type == EventError {
			got = ev.Err
		}
	}
	var ae *APIError
	if !errors.As(got, &ae) || ae.Kind != KindContentFilter {
		t.Fatalf("error = %#v, want content filter", got)
	}
	if IsTransientError(got) {
		t.Fatal("content filter must not be treated as transient")
	}
}

func TestNewSelectsResponsesProtocol(t *testing.T) {
	p, err := New(Config{Type: TypeOpenAICompat, BaseURL: "https://x.test", Protocol: ProtocolResponses})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*Responses); !ok {
		t.Fatalf("provider = %T, want *Responses", p)
	}
	p, err = New(Config{Type: TypeOpenAICompat, BaseURL: "https://x.test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*OpenAICompat); !ok {
		t.Fatalf("default provider = %T, want *OpenAICompat", p)
	}
	if _, err := New(Config{Type: TypeAnthropic, BaseURL: "https://x.test", Protocol: ProtocolResponses}); err == nil {
		t.Fatal("responses protocol should be rejected for anthropic")
	}
}

func TestResponsesEndpointKeepsV1(t *testing.T) {
	cases := map[string]string{
		"https://api.openai.com":    "https://api.openai.com/v1/responses",
		"https://api.openai.com/v1": "https://api.openai.com/v1/responses",
		"https://x.test/compat/v1/": "https://x.test/compat/v1/responses",
	}
	for base, want := range cases {
		if got := NewResponses("", base, "").endpoint(); got != want {
			t.Errorf("endpoint(%q) = %q, want %q", base, got, want)
		}
	}
}

func TestResponsesToolCallArgumentsObject(t *testing.T) {
	items := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","call_id":"c1","name":"f","arguments":{"x":1}}`),
	}
	calls, incomplete, err := toolCallsFromItems(items)
	if err != nil || incomplete || len(calls) != 1 || string(calls[0].Arguments) != `{"x":1}` {
		t.Fatalf("calls=%+v incomplete=%v err=%v", calls, incomplete, err)
	}
}

func TestResponsesIncompleteWithToolCallsIsRejected(t *testing.T) {
	frames := []string{
		`{"type":"response.output_text.delta","delta":"partial"}`,
		`{"type":"response.output_item.done","item":{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"x\":1}","status":"completed"}}`,
		`{"type":"response.incomplete","response":{"output":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"x\":1}","status":"completed"}],"incomplete_details":{"reason":"max_output_tokens"}}}`,
	}
	srv, _ := responsesServer(t, frames)
	defer srv.Close()

	p := NewResponses("", srv.URL, "")
	ch, err := p.ChatStream(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}, Thinking: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	var gotErr error
	sawCalls, sawDone := false, false
	for ev := range ch {
		switch ev.Type {
		case EventToolCalls:
			sawCalls = true
		case EventDone:
			sawDone = true
		case EventError:
			gotErr = ev.Err
		}
	}
	var ae *APIError
	if !errors.As(gotErr, &ae) || ae.Code != "max_output_tokens" {
		t.Fatalf("error = %#v", gotErr)
	}
	if sawCalls || sawDone {
		t.Fatalf("truncated tool call must not be executed: calls=%v done=%v", sawCalls, sawDone)
	}
}

func TestResponsesIncompleteToolItemStatus(t *testing.T) {
	frames := []string{
		`{"type":"response.incomplete","response":{"output":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{}","status":"incomplete"}],"incomplete_details":{"reason":"max_output_tokens"}}}`,
	}
	srv, _ := responsesServer(t, frames)
	defer srv.Close()

	p := NewResponses("", srv.URL, "")
	ch, err := p.ChatStream(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var gotErr error
	for ev := range ch {
		if ev.Type == EventError {
			gotErr = ev.Err
		}
	}
	var ae *APIError
	if !errors.As(gotErr, &ae) || ae.Code != "max_output_tokens" {
		t.Fatalf("error = %#v", gotErr)
	}
}

func TestResponsesIncompleteTextStillFinishes(t *testing.T) {
	frames := []string{
		`{"type":"response.output_text.delta","delta":"半截"}`,
		`{"type":"response.incomplete","response":{"output":[{"type":"message","status":"incomplete","role":"assistant","content":[{"type":"output_text","text":"半截"}]}],"incomplete_details":{"reason":"max_output_tokens"}}}`,
	}
	srv, _ := responsesServer(t, frames)
	defer srv.Close()

	p := NewResponses("", srv.URL, "")
	ch, err := p.ChatStream(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	content, _, calls, _, raw := collectResponses(t, ch)
	if content != "半截" || len(calls) != 0 || len(raw) == 0 {
		t.Fatalf("content=%q calls=%v raw=%s", content, calls, raw)
	}
}

func TestResponsesInvalidToolArgumentsRejected(t *testing.T) {
	frames := []string{
		`{"type":"response.completed","response":{"output":[{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"x\":","status":"completed"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
	}
	srv, _ := responsesServer(t, frames)
	defer srv.Close()

	p := NewResponses("", srv.URL, "")
	ch, err := p.ChatStream(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var gotErr error
	for ev := range ch {
		if ev.Type == EventError {
			gotErr = ev.Err
		}
	}
	var ae *APIError
	if !errors.As(gotErr, &ae) || ae.Code != "invalid_tool_arguments" {
		t.Fatalf("error = %#v", gotErr)
	}
}

func TestResponsesPrematureEOFIsError(t *testing.T) {
	srv, _ := responsesServer(t, []string{
		`{"type":"response.output_text.delta","delta":"半截"}`,
	})
	defer srv.Close()

	p := NewResponses("", srv.URL, "")
	ch, err := p.ChatStream(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var gotErr error
	sawDone := false
	for ev := range ch {
		switch ev.Type {
		case EventDone:
			sawDone = true
		case EventError:
			gotErr = ev.Err
		}
	}
	if gotErr == nil || !IsTransientError(gotErr) {
		t.Fatalf("error = %v, want IO unexpected EOF", gotErr)
	}
	if sawDone {
		t.Fatal("premature EOF must not produce EventDone")
	}
}

func TestResponsesForeignItemsReconstructed(t *testing.T) {
	srv, got := responsesServer(t, []string{
		`{"type":"response.completed","response":{"output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
	})
	defer srv.Close()

	raw := json.RawMessage(`[{"type":"reasoning","id":"rs_1","encrypted_content":"foreign"},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{}"}]`)
	p := NewResponses("", srv.URL, "")
	ch, err := p.ChatStream(context.Background(), ChatRequest{
		Model: "new-model",
		Messages: []Message{
			{Role: RoleAssistant, Content: "旧回复", ResponsesItems: raw, ResponsesOrigin: "other-model",
				ToolCalls: []ToolCall{{ID: "call_1", Name: "read_file", Arguments: json.RawMessage(`{}`)}}},
			{Role: RoleTool, Content: "ok", ToolCallID: "call_1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	collectResponses(t, ch)

	input, _ := (*got)["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("foreign items should be reconstructed into message + function_call + output: %v", input)
	}
	if input[0].(map[string]any)["role"] != "assistant" || input[1].(map[string]any)["type"] != "function_call" {
		t.Fatalf("reconstructed input = %v", input)
	}
}

func TestResponsesDoesNotReplayFunctionCallsTwice(t *testing.T) {
	srv, got := responsesServer(t, []string{
		`{"type":"response.completed","response":{"output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
	})
	defer srv.Close()

	raw := json.RawMessage(`[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{}"}]`)
	p := NewResponses("", srv.URL, "")
	ch, err := p.ChatStream(context.Background(), ChatRequest{
		Model: "m",
		Messages: []Message{
			{Role: RoleAssistant, ResponsesItems: raw, ResponsesOrigin: p.origin("m"), ToolCalls: []ToolCall{{ID: "call_1", Name: "read_file"}}},
			{Role: RoleTool, Content: "ok", ToolCallID: "call_1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	collectResponses(t, ch)

	input, _ := (*got)["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("input should contain one replayed call + one tool output, got %v", input)
	}
	if input[0].(map[string]any)["type"] != "function_call" || input[1].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("function call duplicated: %v", input)
	}
}
