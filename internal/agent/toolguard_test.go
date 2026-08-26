package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/session"
)

// countingTool 数自己被真正执行了多少次——护栏拦下的那些不该走到这里。
type countingTool struct{ runs *int }

func (countingTool) Name() string        { return "echo" }
func (countingTool) Description() string { return "echo text back" }
func (countingTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`)
}
func (t countingTool) Execute(context.Context, json.RawMessage) (string, error) {
	*t.runs++
	return "ok", nil
}

func TestGuardBlocksIdenticalCall(t *testing.T) {
	var g toolGuard
	call := llm.ToolCall{ID: "1", Name: "echo", Arguments: json.RawMessage(`{"a":1,"b":2}`)}
	if msg := g.check("", call); msg != "" {
		t.Fatalf("第一次调用不该被拦: %s", msg)
	}
	// 同一次调用换个键序、加些空白，仍是同一次调用
	same := llm.ToolCall{ID: "2", Name: "echo", Arguments: json.RawMessage(`{ "b": 2, "a": 1 }`)}
	msg := g.check("", same)
	if msg == "" {
		t.Fatal("同名同参的重复调用该被拦下")
	}
	if !strings.Contains(msg, "结束本轮") {
		t.Fatalf("拦截说明没告诉模型该怎么办: %s", msg)
	}
	// 参数不同就该放行：绕圈的那种每次都会变一点，靠下面的次数上限管
	other := llm.ToolCall{ID: "3", Name: "echo", Arguments: json.RawMessage(`{"a":1,"b":3}`)}
	if msg := g.check("", other); msg != "" {
		t.Fatalf("参数不同不该被拦: %s", msg)
	}
}

func TestGuardBlocksTooManySameTool(t *testing.T) {
	var g toolGuard
	for i := range maxSameTool {
		call := llm.ToolCall{Name: "echo", Arguments: json.RawMessage(`{"n":` + string(rune('0'+i)) + `}`)}
		if msg := g.check("", call); msg != "" {
			t.Fatalf("第 %d 次就被拦了: %s", i+1, msg)
		}
	}
	over := llm.ToolCall{Name: "echo", Arguments: json.RawMessage(`{"n":"x"}`)}
	if msg := g.check("", over); msg == "" {
		t.Fatalf("同一工具第 %d 次调用该被拦下", maxSameTool+1)
	}
	// 另一个工具不受影响：上限是按工具名各算各的
	if msg := g.check("", llm.ToolCall{Name: "other", Arguments: json.RawMessage(`{}`)}); msg != "" {
		t.Fatalf("别的工具不该受牵连: %s", msg)
	}
}

func TestGuardCanonicalArgsFallsBackOnBadJSON(t *testing.T) {
	if got := canonicalArgs(json.RawMessage(`不是 JSON`)); got != "不是 JSON" {
		t.Fatalf("解析不了时该用原文: %q", got)
	}
}

// 端到端：模型反复提交同一个调用，工具只被执行一次，轮次被护栏中止。
func TestRunAbortsOnRepeatedToolCalls(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := store.Create()

	same := []llm.ToolCall{{ID: "c", Name: "echo", Arguments: json.RawMessage(`{"text":"hi"}`)}}
	provider := &mockProvider{turns: []mockTurn{
		{toolCalls: same}, {toolCalls: same}, {toolCalls: same},
		{toolCalls: same}, {toolCalls: same}, {toolCalls: same},
	}}
	runs := 0
	ag := New(provider, newTestManager(t, toolOnlyPlugin{countingTool{runs: &runs}}), store,
		Options{Model: "test", MaxTurns: 20})

	_, err = ag.RunTurn(context.Background(), meta.ID, "你好")
	if !errors.Is(err, errToolLoop) {
		t.Fatalf("该被护栏中止，实际 err=%v", err)
	}
	if runs != 1 {
		t.Fatalf("工具只该真正执行一次，实际 %d 次", runs)
	}
	// 拦了 maxGuardHits 次就收手，不该跑满 MaxTurns
	if provider.calls > maxGuardHits+1 {
		t.Fatalf("模型调用了 %d 次，护栏收手太晚", provider.calls)
	}

	// 每个 tool_call 都要有对应的结果，拦下也不例外——否则历史里留下悬挂的调用
	_, msgs, err := store.Get(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	calls, results := 0, 0
	for _, m := range msgs {
		calls += len(m.Message.ToolCalls)
		if m.Message.Role == llm.RoleTool {
			results++
		}
	}
	if calls != results {
		t.Fatalf("调用 %d 次、结果 %d 条，对不上", calls, results)
	}
}

// toolOnlyPlugin 是只提供一个工具的测试插件。
type toolOnlyPlugin struct{ tool countingTool }

func (toolOnlyPlugin) Name() string                                  { return "counting_plugin" }
func (toolOnlyPlugin) Description() string                           { return "测试插件" }
func (toolOnlyPlugin) Init(plugin.InitContext, map[string]any) error { return nil }
func (toolOnlyPlugin) SystemPrompt() string                          { return "" }
func (p toolOnlyPlugin) Tools() []plugin.Tool                        { return []plugin.Tool{p.tool} }
