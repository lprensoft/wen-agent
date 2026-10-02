package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// cacheFrames 是一段带缓存用量的流：输入侧在 message_start 下发。
func cacheFrames() []antFrame {
	return []antFrame{
		{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":120,"output_tokens":1,"cache_read_input_tokens":9000,"cache_creation_input_tokens":300}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"好"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}
}

func convo() []Message {
	return []Message{
		{Role: RoleSystem, Content: "环境块"},
		{Role: RoleUser, Content: "第一问"},
		{Role: RoleAssistant, Content: "第一答"},
		{Role: RoleUser, Content: "本轮状态 + 第二问"},
	}
}

// 关掉开关时请求里不能出现任何 cache_control：缓存写入是要单独付钱的，
// 不能因为「默认开着更划算」就替用户决定。
func TestAnthropicNoCacheControlWhenDisabled(t *testing.T) {
	got, _ := antRun(t, textFrames("好"), ChatRequest{Model: "claude-opus-4-5", Messages: convo()})
	raw, _ := json.Marshal(got)
	if s := string(raw); strings.Contains(s, "cache_control") {
		t.Errorf("关闭时不该出现 cache_control:\n%s", s)
	}
}

// 断点该落在两个稳定层的末尾：system 与倒数第二条消息。
func TestAnthropicCacheBreakpoints(t *testing.T) {
	got := antRunCached(t, textFrames("好"), ChatRequest{
		Model:    "claude-opus-4-5",
		Messages: convo(),
		Tools:    []ToolSpec{{Name: "read_file", Description: "读文件"}},
	})

	sys, _ := got["system"].([]any)
	if len(sys) != 1 {
		t.Fatalf("system = %v", got["system"])
	}
	if b, _ := sys[0].(map[string]any); b["cache_control"] == nil {
		t.Errorf("system 块上应当有断点（它覆盖 tools 与 system）: %v", b)
	}

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v", msgs)
	}
	if cc := blockCacheControl(t, msgs[1]); cc == nil {
		t.Error("倒数第二条消息末尾应当有断点（它覆盖已有历史）")
	}
	// 最后一条带着每轮都变的本轮状态块，写进缓存只会付写入的钱、永远读不回来
	if cc := blockCacheControl(t, msgs[2]); cc != nil {
		t.Error("最后一条消息上不该有断点")
	}
}

// 思考块不接受 cache_control，断点要跳到它前面一个可标记的块上。
//
// 直接测 markCacheBreakpoints：上一轮的思考块不再回传（见 buildAnthropicRequest），
// 经完整请求已经构造不出「倒数第二条消息以思考块结尾」的形状，但规则本身仍要守住。
func TestAnthropicCacheSkipsThinkingBlock(t *testing.T) {
	wr := antRequest{Messages: []antMessage{
		{Role: "user", Content: []antBlock{{Type: "text", Text: "问"}}},
		{Role: "assistant", Content: []antBlock{
			{Type: "text", Text: "答"},
			{Type: "thinking", Thinking: strPtr("想"), Signature: "sig"},
		}},
		{Role: "user", Content: []antBlock{{Type: "text", Text: "再问"}}},
	}}
	markCacheBreakpoints(&wr)
	blocks := wr.Messages[1].Content
	if blocks[1].CacheControl != nil {
		t.Errorf("思考块上不该打断点: %+v", blocks[1])
	}
	if blocks[0].CacheControl == nil {
		t.Errorf("断点应当落在思考块前面那个可标记的块上: %+v", blocks[0])
	}
}

// 只有一条消息时（首轮）没有「倒数第二条」，不能越界。
func TestAnthropicCacheSingleMessage(t *testing.T) {
	got := antRunCached(t, textFrames("好"), ChatRequest{
		Model:    "claude-opus-4-5",
		Messages: []Message{{Role: RoleSystem, Content: "环境块"}, {Role: RoleUser, Content: "问"}},
	})
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	if cc := blockCacheControl(t, msgs[0]); cc != nil {
		t.Error("唯一那条消息就是本轮内容，不该打断点")
	}
}

// 缓存读写要透出来：稳定前缀是靠约定维持的，看不见命中数就没法确认它还成立。
func TestAnthropicUsageCarriesCacheTokens(t *testing.T) {
	_, res := antRun(t, cacheFrames(), ChatRequest{Model: "claude-opus-4-5", Messages: convo()})
	if res.usage == nil {
		t.Fatal("没有收到用量事件")
	}
	if res.usage.CachedTokens != 9000 || res.usage.CacheWriteTokens != 300 {
		t.Errorf("缓存用量 = 命中 %d / 写入 %d", res.usage.CachedTokens, res.usage.CacheWriteTokens)
	}
	// 输入总量仍是三部分之和，占用比例的口径不变
	if res.usage.PromptTokens != 120+9000+300 {
		t.Errorf("PromptTokens = %d", res.usage.PromptTokens)
	}
}

// blockCacheControl 取一条消息最后一个块上的 cache_control。
func blockCacheControl(t *testing.T, msg any) any {
	t.Helper()
	m, _ := msg.(map[string]any)
	blocks, _ := m["content"].([]any)
	if len(blocks) == 0 {
		t.Fatalf("消息没有内容块: %v", msg)
	}
	last, _ := blocks[len(blocks)-1].(map[string]any)
	return last["cache_control"]
}
