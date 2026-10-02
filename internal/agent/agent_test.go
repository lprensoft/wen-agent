package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/session"
)

// echoPlugin 是仅提供 echo 工具的测试插件。
type echoPlugin struct{}

func (echoPlugin) Name() string                                  { return "echo_plugin" }
func (echoPlugin) Description() string                           { return "测试插件" }
func (echoPlugin) Init(plugin.InitContext, map[string]any) error { return nil }
func (echoPlugin) SystemPrompt() string                          { return "" }
func (echoPlugin) Tools() []plugin.Tool                          { return []plugin.Tool{echoTool{}} }

// promptPlugin 是仅注入提示词的测试插件。
type promptPlugin struct{}

func (promptPlugin) Name() string                                  { return "prompt_plugin" }
func (promptPlugin) Description() string                           { return "测试提示词注入" }
func (promptPlugin) Init(plugin.InitContext, map[string]any) error { return nil }
func (promptPlugin) SystemPrompt() string                          { return "插件注入的提示词片段" }
func (promptPlugin) Tools() []plugin.Tool                          { return nil }

func newTestManager(t *testing.T, ps ...plugin.Plugin) *plugin.Manager {
	t.Helper()
	m := plugin.NewManager(plugin.InitContext{}, "")
	for _, p := range ps {
		if err := m.Register(p, plugin.PluginConfig{Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// mockProvider 按预设脚本依次响应每轮调用，并记录收到的请求。
type mockProvider struct {
	turns []mockTurn
	calls int
	reqs  []llm.ChatRequest
}

type mockTurn struct {
	content   string
	reasoning string
	toolCalls []llm.ToolCall
	usage     *llm.Usage
}

func (m *mockProvider) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	turn := m.turns[m.calls]
	m.calls++
	m.reqs = append(m.reqs, req)
	ch := make(chan llm.StreamEvent)
	go func() {
		defer close(ch)
		for _, r := range turn.reasoning {
			ch <- llm.StreamEvent{Type: llm.EventReasoningDelta, Content: string(r)}
		}
		// 文本按字符流式下发，模拟真实增量
		for _, r := range turn.content {
			ch <- llm.StreamEvent{Type: llm.EventContentDelta, Content: string(r)}
		}
		if len(turn.toolCalls) > 0 {
			ch <- llm.StreamEvent{Type: llm.EventToolCalls, ToolCalls: turn.toolCalls}
		}
		if turn.usage != nil {
			ch <- llm.StreamEvent{Type: llm.EventUsage, Usage: turn.usage}
		}
		ch <- llm.StreamEvent{Type: llm.EventDone}
	}()
	return ch, nil
}

// echoTool 原样返回参数中的 text 字段。
type echoTool struct{}

func (echoTool) Name() string        { return "echo" }
func (echoTool) Description() string { return "echo text back" }
func (echoTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)
}
func (echoTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var p struct{ Text string }
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	return "echo: " + p.Text, nil
}

func TestRunToolLoop(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := store.Create()

	provider := &mockProvider{turns: []mockTurn{
		// 第一轮：模型请求调用工具
		{toolCalls: []llm.ToolCall{{ID: "call_1", Name: "echo", Arguments: json.RawMessage(`{"text":"hi"}`)}}},
		// 第二轮：模型给出最终回答
		{content: "工具返回了 echo: hi"},
	}}

	ag := New(provider, newTestManager(t, echoPlugin{}), store, Options{Model: "test", MaxTurns: 5})

	var events []Event
	ag.Run(context.Background(), meta.ID, "调用一下 echo", func(ev Event) { events = append(events, ev) })

	// 事件序列检查
	var types []EventType
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	if types[len(types)-1] != EventDone {
		t.Fatalf("last event = %v, want done; all events: %v", types[len(types)-1], types)
	}
	hasType := func(want EventType) bool {
		for _, tp := range types {
			if tp == want {
				return true
			}
		}
		return false
	}
	for _, want := range []EventType{EventToolStart, EventToolResult, EventDelta} {
		if !hasType(want) {
			t.Errorf("missing event %v in %v", want, types)
		}
	}
	for _, ev := range events {
		if ev.Type == EventToolResult && ev.ToolResult != "echo: hi" {
			t.Errorf("tool result = %q", ev.ToolResult)
		}
		if ev.Type == EventError {
			t.Errorf("unexpected error event: %s", ev.Error)
		}
	}

	// 持久化检查：user / assistant(tool_calls) / tool / assistant 共 4 条
	gotMeta, msgs, err := store.Get(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("got %d stored messages, want 4", len(msgs))
	}
	roles := []string{msgs[0].Role, msgs[1].Role, msgs[2].Role, msgs[3].Role}
	want := []string{"user", "assistant", "tool", "assistant"}
	for i := range want {
		if roles[i] != want[i] {
			t.Errorf("message[%d].role = %q, want %q", i, roles[i], want[i])
		}
	}
	if len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].Name != "echo" {
		t.Errorf("assistant tool_calls not persisted: %+v", msgs[1].ToolCalls)
	}
	if msgs[3].Content != "工具返回了 echo: hi" {
		t.Errorf("final content = %q", msgs[3].Content)
	}
	// 首条消息生成标题
	if gotMeta.Title != "调用一下 echo" {
		t.Errorf("title = %q", gotMeta.Title)
	}
}

func TestRunPlainChat(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	provider := &mockProvider{turns: []mockTurn{{content: "你好"}}}

	ag := New(provider, newTestManager(t), store, Options{Model: "test"})

	var final string
	var done bool
	ag.Run(context.Background(), meta.ID, "hi", func(ev Event) {
		if ev.Type == EventDelta {
			final += ev.Content
		}
		if ev.Type == EventDone {
			done = true
		}
	})
	if final != "你好" || !done {
		t.Errorf("final = %q, done = %v", final, done)
	}
	if provider.calls != 1 {
		t.Errorf("provider called %d times, want 1", provider.calls)
	}
}

func TestThinkingEventsAndPersistence(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	provider := &mockProvider{turns: []mockTurn{{reasoning: "推理一下", content: "答案"}}}

	ag := New(provider, newTestManager(t), store, Options{Model: "test", Thinking: "high"})

	var thinking string
	ag.Run(context.Background(), meta.ID, "问题", func(ev Event) {
		if ev.Type == EventThinking {
			thinking += ev.Content
		}
	})
	if thinking != "推理一下" {
		t.Errorf("thinking events = %q", thinking)
	}
	// Thinking 配置透传到请求
	if provider.reqs[0].Thinking != "high" {
		t.Errorf("request thinking = %q", provider.reqs[0].Thinking)
	}
	// reasoning 持久化到 session
	_, msgs, _ := store.Get(meta.ID)
	if msgs[1].Reasoning != "推理一下" {
		t.Errorf("stored reasoning = %q", msgs[1].Reasoning)
	}
}

// TestReasoningCarriedIntoNextTurn：上一轮的思考内容要随历史进入下一轮的请求。
// DeepSeek 在请求带工具时要求回传所有之前轮次的 reasoning_content，漏了就是 400；
// 剥不剥由各 provider 自己决定（Anthropic 只回传本轮的），核心不替它们丢。
func TestReasoningCarriedIntoNextTurn(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	provider := &mockProvider{turns: []mockTurn{
		{reasoning: "第一轮的思考", content: "第一答"},
		{content: "第二答"},
	}}
	ag := New(provider, newTestManager(t), store, Options{Model: "test", Thinking: "high", MaxTurns: 5})
	ag.Run(context.Background(), meta.ID, "第一问", func(Event) {})
	ag.Run(context.Background(), meta.ID, "第二问", func(Event) {})

	for _, m := range provider.reqs[1].Messages {
		if m.Role == llm.RoleAssistant && m.Content == "第一答" {
			if m.Reasoning != "第一轮的思考" {
				t.Errorf("上一轮的思考没有带进下一轮: %+v", m)
			}
			return
		}
	}
	t.Fatal("第二轮的请求里找不到上一轮的助手消息")
}

func TestTrimToBudget(t *testing.T) {
	big := strings.Repeat("字", 3000) // ≈3000 token
	msgs := []llm.Message{
		{Role: "system", Content: "env"},
		{Role: "user", Content: big},
		{Role: "assistant", Content: big},
		{Role: "user", Content: big},
		{Role: "assistant", Content: big},
		{Role: "user", Content: "最新问题"},
	}
	// 预算只够最近内容：ContextLength - MaxTokens - 2048 ≈ 4952 token
	got := trimToBudget(Options{ContextLength: 8000, MaxTokens: 1000}, msgs, nil)

	if got[0].Role != "system" {
		t.Fatal("system message must be kept")
	}
	if len(got) >= len(msgs) {
		t.Fatalf("expected trimming, got %d messages", len(got))
	}
	// 裁剪必须从整轮丢弃：剩余的第一条非 system 消息必须是 user
	if got[1].Role != "user" {
		t.Errorf("first non-system message role = %q, want user", got[1].Role)
	}
	if got[len(got)-1].Content != "最新问题" {
		t.Error("latest user message must be kept")
	}
	// 预算充足时不裁剪
	if got2 := trimToBudget(Options{ContextLength: 1000000, MaxTokens: 4096}, msgs, nil); len(got2) != len(msgs) {
		t.Errorf("should not trim under budget, got %d/%d", len(got2), len(msgs))
	}
}

func TestUsagePersistedAndNoCompactUnderThreshold(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	provider := &mockProvider{turns: []mockTurn{
		{content: "回答", usage: &llm.Usage{PromptTokens: 500, CompletionTokens: 100}},
	}}
	ag := New(provider, newTestManager(t), store, Options{Model: "test", ContextLength: 10000})

	var types []EventType
	ag.Run(context.Background(), meta.ID, "hi", func(ev Event) { types = append(types, ev.Type) })

	gotMeta, _, _ := store.Get(meta.ID)
	if gotMeta.LastUsage == nil || gotMeta.LastUsage.PromptTokens != 500 || gotMeta.LastUsage.CompletionTokens != 100 {
		t.Errorf("usage not persisted: %+v", gotMeta.LastUsage)
	}
	for _, tp := range types {
		if tp == EventCompactStart {
			t.Error("should not auto-compact under threshold")
		}
	}
}

func TestAutoCompactOverThreshold(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	// 窗口 1000，实测 920+30=950 ≥ 90% → 触发自动压缩；第二轮是压缩调用
	provider := &mockProvider{turns: []mockTurn{
		{content: "很长的回答", usage: &llm.Usage{PromptTokens: 920, CompletionTokens: 30}},
		{content: "自动摘要内容"},
	}}
	ag := New(provider, newTestManager(t), store, Options{Model: "test", ContextLength: 1000})

	var types []EventType
	var compactText string
	ag.Run(context.Background(), meta.ID, "hi", func(ev Event) {
		types = append(types, ev.Type)
		if ev.Type == EventCompactDelta {
			compactText += ev.Content
		}
		if ev.Type == EventCompactDone && ev.Error != "" {
			t.Fatalf("auto compact failed: %s", ev.Error)
		}
	})

	has := func(want EventType) bool {
		for _, tp := range types {
			if tp == want {
				return true
			}
		}
		return false
	}
	if !has(EventCompactStart) || !has(EventCompactDone) {
		t.Fatalf("missing compact events: %v", types)
	}
	if compactText != "自动摘要内容" {
		t.Errorf("compact delta = %q", compactText)
	}
	// done 必须在 compact_done 之后
	if types[len(types)-1] != EventDone {
		t.Errorf("last event = %v", types[len(types)-1])
	}
	// 历史被替换为摘要，实测用量被清除
	gotMeta, msgs, _ := store.Get(meta.ID)
	if len(msgs) != 1 || msgs[0].Kind != "summary" {
		t.Errorf("session not compacted: %d msgs", len(msgs))
	}
	if gotMeta.LastUsage != nil {
		t.Errorf("usage should be cleared after compact, got %+v", gotMeta.LastUsage)
	}
}

func TestCompact(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()

	// 先跑一轮带工具的对话，制造可压缩的历史
	provider := &mockProvider{turns: []mockTurn{
		{toolCalls: []llm.ToolCall{{ID: "call_1", Name: "echo", Arguments: json.RawMessage(`{"text":"hi"}`)}}},
		{content: "第一轮回答"},
		{content: "这是压缩摘要内容"}, // Compact 使用的第三次调用
	}}
	ag := New(provider, newTestManager(t, echoPlugin{}), store, Options{Model: "test", MaxTurns: 5, Thinking: "high"})
	ag.Run(context.Background(), meta.ID, "做点事", func(Event) {})

	var streamed string
	var done bool
	ag.Compact(context.Background(), meta.ID, func(ev Event) {
		if ev.Type == EventDelta {
			streamed += ev.Content
		}
		if ev.Type == EventDone {
			done = true
		}
		if ev.Type == EventError {
			t.Fatalf("compact error: %s", ev.Error)
		}
	})
	if !done || streamed != "这是压缩摘要内容" {
		t.Errorf("done=%v streamed=%q", done, streamed)
	}

	// 压缩请求：思考关闭、序列化历史包含工具信息
	compactReq := provider.reqs[len(provider.reqs)-1]
	if compactReq.Thinking != "off" {
		t.Errorf("compact thinking = %q, want off", compactReq.Thinking)
	}
	if !strings.Contains(compactReq.Messages[0].Content, "echo") {
		t.Error("serialized history should mention tool call")
	}

	// 历史被替换为单条 summary 消息
	_, msgs, _ := store.Get(meta.ID)
	if len(msgs) != 1 {
		t.Fatalf("got %d messages after compact, want 1", len(msgs))
	}
	if msgs[0].Kind != "summary" || msgs[0].Role != "user" {
		t.Errorf("summary message = kind %q role %q", msgs[0].Kind, msgs[0].Role)
	}
	if !strings.Contains(msgs[0].Content, "这是压缩摘要内容") {
		t.Errorf("summary content = %q", msgs[0].Content)
	}
}

// archiverPlugin 实现 CompactObserver，记录压缩事件并返回一条注记。
type archiverPlugin struct {
	note string
	got  plugin.CompactEvent
}

func (*archiverPlugin) Name() string                                  { return "archiver" }
func (*archiverPlugin) Description() string                           { return "测试压缩钩子" }
func (*archiverPlugin) Init(plugin.InitContext, map[string]any) error { return nil }
func (*archiverPlugin) SystemPrompt() string                          { return "" }
func (*archiverPlugin) Tools() []plugin.Tool                          { return nil }
func (p *archiverPlugin) OnCompact(_ context.Context, ev plugin.CompactEvent) (string, error) {
	p.got = ev
	return p.note, nil
}

func TestCompactNotifiesPluginsBeforeReplace(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	provider := &mockProvider{turns: []mockTurn{
		{content: "第一轮回答"},
		{content: "这是压缩摘要内容"},
	}}
	arch := &archiverPlugin{note: "（本次压缩前的完整历史已归档：x.md）"}
	ag := New(provider, newTestManager(t, arch), store, Options{Model: "test", MaxTurns: 5})
	ag.Run(context.Background(), meta.ID, "做点事", func(Event) {})
	ag.Compact(context.Background(), meta.ID, func(Event) {})

	// 钩子拿到的是尚未被删除的完整历史
	if arch.got.SessionID != meta.ID {
		t.Errorf("session id = %q, want %q", arch.got.SessionID, meta.ID)
	}
	if len(arch.got.History) != 2 {
		t.Fatalf("history 应为 user+assistant 两条，得到 %d 条", len(arch.got.History))
	}
	if arch.got.History[0].Content != "做点事" || arch.got.History[1].Content != "第一轮回答" {
		t.Errorf("history 内容不对: %+v", arch.got.History)
	}
	if arch.got.Summary != "这是压缩摘要内容" {
		t.Errorf("summary = %q（应为不含前缀的摘要正文）", arch.got.Summary)
	}

	// 注记随摘要一起落进该会话的历史
	_, msgs, _ := store.Get(meta.ID)
	if len(msgs) != 1 {
		t.Fatalf("got %d messages after compact, want 1", len(msgs))
	}
	if !strings.Contains(msgs[0].Content, "这是压缩摘要内容") ||
		!strings.Contains(msgs[0].Content, "已归档：x.md") {
		t.Errorf("摘要消息应同时包含摘要与注记: %q", msgs[0].Content)
	}
}

func TestComplete(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	provider := &mockProvider{turns: []mockTurn{{content: "提炼结果"}}}
	ag := New(provider, newTestManager(t, echoPlugin{}), store,
		Options{Model: "test", Thinking: "high", MaxTokens: 4096})

	got, err := ag.Complete(context.Background(), "请提炼")
	if err != nil {
		t.Fatal(err)
	}
	if got != "提炼结果" {
		t.Errorf("Complete = %q", got)
	}

	// 辅助调用不带工具、不启用思考，也不写入任何会话
	req := provider.reqs[0]
	if len(req.Tools) != 0 {
		t.Errorf("辅助调用不应带工具: %v", req.Tools)
	}
	if req.Thinking != "off" {
		t.Errorf("thinking = %q, want off", req.Thinking)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != llm.RoleUser {
		t.Errorf("应只发一条 user 消息: %+v", req.Messages)
	}
	if _, msgs, _ := store.Get(meta.ID); len(msgs) != 0 {
		t.Errorf("辅助调用不应写入会话，实际 %d 条", len(msgs))
	}
}

func TestPluginPromptInjectionOrder(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	provider := &mockProvider{turns: []mockTurn{{content: "ok"}}}

	ag := New(provider, newTestManager(t, promptPlugin{}), store, Options{
		Model:        "test",
		SystemPrompt: "用户配置提示词",
	})
	ag.Run(context.Background(), meta.ID, "hi", func(Event) {})

	sys := provider.reqs[0].Messages[0].Content
	iEnv := strings.Index(sys, "[系统环境]")
	iPlugin := strings.Index(sys, "插件注入的提示词片段")
	iUser := strings.Index(sys, "用户配置提示词")
	if iEnv < 0 || iPlugin < 0 || iUser < 0 {
		t.Fatalf("system message missing parts:\n%s", sys)
	}
	if !(iEnv < iPlugin && iPlugin < iUser) {
		t.Errorf("order wrong: env=%d plugin=%d user=%d", iEnv, iPlugin, iUser)
	}
}

func TestSystemMessageContainsEnvContext(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	provider := &mockProvider{turns: []mockTurn{{content: "ok"}}}

	ag := New(provider, newTestManager(t), store, Options{
		Model:        "test",
		SystemPrompt: "自定义提示词",
		Workdir:      "D:\\some\\dir",
	})
	ag.Run(context.Background(), meta.ID, "hi", func(Event) {})

	msgs := provider.reqs[0].Messages
	if msgs[0].Role != "system" {
		t.Fatalf("first message role = %q, want system", msgs[0].Role)
	}
	sys := msgs[0].Content
	for _, want := range []string{"[系统环境]", "操作系统:", "工作目录: D:\\some\\dir", "自定义提示词"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system message missing %q:\n%s", want, sys)
		}
	}
	// 环境块在前，配置提示词拼接在后
	if strings.Index(sys, "[系统环境]") > strings.Index(sys, "自定义提示词") {
		t.Error("env context should come before the configured system prompt")
	}
}

// blockingProvider 在收到请求后阻塞，直到 release 被关闭，用于验证进行中的请求使用旧快照。
type blockingProvider struct {
	name    string
	started chan struct{}
	release chan struct{}
	reqs    []llm.ChatRequest
}

func (p *blockingProvider) ChatStream(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.reqs = append(p.reqs, req)
	ch := make(chan llm.StreamEvent)
	go func() {
		defer close(ch)
		close(p.started)
		<-p.release
		ch <- llm.StreamEvent{Type: llm.EventContentDelta, Content: p.name}
		ch <- llm.StreamEvent{Type: llm.EventDone}
	}()
	return ch, nil
}

func TestSetModelHotSwap(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()

	p1 := &mockProvider{turns: []mockTurn{{content: "旧模型"}}}
	ag := New(p1, newTestManager(t), store, Options{
		Model: "old", MaxTokens: 1000, ContextLength: 8000,
		SystemPrompt: "自定义", MaxTurns: 7, Workdir: "/tmp", Thinking: "high",
	})
	ag.Run(context.Background(), meta.ID, "问题一", func(Event) {})

	p2 := &mockProvider{turns: []mockTurn{{content: "新模型"}}}
	ag.SetModel(p2, ModelOptions{Model: "new", MaxTokens: 2000, Thinking: "low", ContextLength: 200000})
	ag.Run(context.Background(), meta.ID, "问题二", func(Event) {})

	if p1.calls != 1 || p2.calls != 1 {
		t.Fatalf("calls: p1=%d p2=%d", p1.calls, p2.calls)
	}
	if p2.reqs[0].Model != "new" || p2.reqs[0].Thinking != "low" || p2.reqs[0].MaxTokens != 2000 {
		t.Errorf("new request = %+v", p2.reqs[0])
	}
	// 切换模型不应影响进程级配置
	opts := ag.Options()
	if opts.SystemPrompt != "自定义" || opts.MaxTurns != 7 || opts.Workdir != "/tmp" {
		t.Errorf("process options changed: %+v", opts)
	}
}

func TestSetModelDuringRunUsesSnapshot(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()

	old := &blockingProvider{name: "旧", started: make(chan struct{}), release: make(chan struct{})}
	ag := New(old, newTestManager(t), store, Options{Model: "old", MaxTokens: 1000, ContextLength: 8000})

	done := make(chan struct{})
	go func() {
		defer close(done)
		ag.Run(context.Background(), meta.ID, "问题", func(Event) {})
	}()

	<-old.started // 请求已发出，此时切换模型
	ag.SetModel(&mockProvider{turns: []mockTurn{{content: "新"}}}, ModelOptions{Model: "new", MaxTokens: 2000})
	close(old.release)
	<-done

	if len(old.reqs) != 1 || old.reqs[0].Model != "old" {
		t.Errorf("in-flight request must keep the old snapshot: %+v", old.reqs)
	}
}
