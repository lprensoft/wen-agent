package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// sseServer 返回一个记录请求体并回放固定 SSE 帧的测试服务。
func sseServer(t *testing.T, frames []string, gotBody *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, gotBody); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func collect(t *testing.T, ch <-chan StreamEvent) (content, reasoning string, calls []ToolCall) {
	t.Helper()
	for ev := range ch {
		switch ev.Type {
		case EventContentDelta:
			content += ev.Content
		case EventReasoningDelta:
			reasoning += ev.Content
		case EventToolCalls:
			calls = ev.ToolCalls
		case EventError:
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	return
}

func TestThinkingEnabledRequest(t *testing.T) {
	var got map[string]any
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"reasoning_content":"思考中"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"…"}}]}`,
		`{"choices":[{"delta":{"content":"你好"}}]}`,
	}, &got)
	defer srv.Close()

	p := NewOpenAICompat("", srv.URL, "sk-test", "")
	ch, err := p.ChatStream(context.Background(), ChatRequest{
		Model:       "deepseek-v4-flash",
		Messages:    testMessages("hi"),
		Temperature: 0.7,
		Thinking:    "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	content, reasoning, _ := collect(t, ch)

	if content != "你好" || reasoning != "思考中…" {
		t.Errorf("content=%q reasoning=%q", content, reasoning)
	}
	thinking, _ := got["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Errorf("thinking = %v, want enabled", got["thinking"])
	}
	if got["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v", got["reasoning_effort"])
	}
	if _, has := got["temperature"]; has {
		t.Error("temperature should be omitted in thinking mode")
	}
}

func TestThinkingOffRequest(t *testing.T) {
	var got map[string]any
	srv := sseServer(t, []string{`{"choices":[{"delta":{"content":"ok"}}]}`}, &got)
	defer srv.Close()

	p := NewOpenAICompat("", srv.URL, "sk-test", "")
	ch, err := p.ChatStream(context.Background(), ChatRequest{
		Model: "deepseek-chat", Messages: testMessages("hi"), Temperature: 0.7, Thinking: "off",
	})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, ch)

	thinking, _ := got["thinking"].(map[string]any)
	if thinking["type"] != "disabled" {
		t.Errorf("thinking = %v, want disabled", got["thinking"])
	}
	if got["temperature"] != 0.7 {
		t.Errorf("temperature = %v, want 0.7", got["temperature"])
	}
	if _, has := got["reasoning_effort"]; has {
		t.Error("reasoning_effort should be omitted when thinking off")
	}
}

func TestToolCallChunkAssembly(t *testing.T) {
	var got map[string]any
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"pa"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a.txt\"}"}}]}}]}`,
	}, &got)
	defer srv.Close()

	p := NewOpenAICompat("", srv.URL, "sk-test", "")
	ch, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Messages: testMessages("hi")})
	if err != nil {
		t.Fatal(err)
	}
	_, _, calls := collect(t, ch)

	if len(calls) != 1 || calls[0].ID != "call_1" || calls[0].Name != "read_file" {
		t.Fatalf("calls = %+v", calls)
	}
	if string(calls[0].Arguments) != `{"path":"a.txt"}` {
		t.Errorf("arguments = %s", calls[0].Arguments)
	}
}

func TestReasoningContentSentBack(t *testing.T) {
	var got map[string]any
	srv := sseServer(t, []string{`{"choices":[{"delta":{"content":"ok"}}]}`}, &got)
	defer srv.Close()

	p := NewOpenAICompat("", srv.URL, "sk-test", "")
	msgs := []Message{
		{Role: RoleUser, Content: "hi"},
		{Role: RoleAssistant, Content: "", Reasoning: "之前的思考", ToolCalls: []ToolCall{
			{ID: "call_1", Name: "read_file", Arguments: json.RawMessage(`{}`)},
		}},
		{Role: RoleTool, Content: "result", ToolCallID: "call_1"},
	}
	ch, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Messages: msgs, Thinking: "high"})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, ch)

	sent, _ := got["messages"].([]any)
	assistant, _ := sent[1].(map[string]any)
	if assistant["reasoning_content"] != "之前的思考" {
		t.Errorf("assistant reasoning_content not sent back: %v", assistant)
	}
}

// TestReasoningContentSentBackForEveryTurn：DeepSeek 要求请求带工具时「所有之前轮次」的
// reasoning_content 都回传，漏了就是 400——不只是本轮工具循环里的那几条。
// 我们每轮都带工具，所以已经结束的轮次（没有工具调用的那种）也要带上。
func TestReasoningContentSentBackForEveryTurn(t *testing.T) {
	var got map[string]any
	srv := sseServer(t, []string{`{"choices":[{"delta":{"content":"ok"}}]}`}, &got)
	defer srv.Close()

	p := NewOpenAICompat("", srv.URL, "sk-test", DialectDeepSeek)
	msgs := []Message{
		{Role: RoleUser, Content: "第一问"},
		{Role: RoleAssistant, Content: "第一答", Reasoning: "第一轮的思考"},
		{Role: RoleUser, Content: "第二问"},
	}
	tools := []ToolSpec{{Name: "read_file", Schema: json.RawMessage(`{"type":"object"}`)}}
	ch, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Messages: msgs, Tools: tools, Thinking: "high"})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, ch)

	sent, _ := got["messages"].([]any)
	assistant, _ := sent[1].(map[string]any)
	if assistant["reasoning_content"] != "第一轮的思考" {
		t.Errorf("上一轮的 reasoning_content 没有回传: %v", assistant)
	}
}

func TestUsageParsedAndStreamOptionsSent(t *testing.T) {
	var got map[string]any
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":123,"completion_tokens":45,"total_tokens":168}}`,
	}, &got)
	defer srv.Close()

	p := NewOpenAICompat("", srv.URL, "sk-test", "")
	ch, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Messages: testMessages("hi")})
	if err != nil {
		t.Fatal(err)
	}
	var usage *Usage
	for ev := range ch {
		if ev.Type == EventUsage {
			usage = ev.Usage
		}
		if ev.Type == EventError {
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	if usage == nil || usage.PromptTokens != 123 || usage.CompletionTokens != 45 {
		t.Errorf("usage = %+v", usage)
	}
	so, _ := got["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Errorf("stream_options = %v", got["stream_options"])
	}
}

func TestAPIErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
	}))
	defer srv.Close()

	p := NewOpenAICompat("", srv.URL, "sk-bad", "")
	if _, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Messages: testMessages("hi")}); err == nil {
		t.Fatal("expected error on 401")
	}
}

func testMessages(text string) []Message {
	return []Message{{Role: RoleUser, Content: text}}
}
