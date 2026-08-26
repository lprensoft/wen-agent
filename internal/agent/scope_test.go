package agent

import (
	"context"
	"strings"
	"testing"

	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/session"
)

// scopePlugin 是按固定可见域裁决的测试插件，可选地注入本轮提示词。
type scopePlugin struct {
	name  string
	scope plugin.Scope
	note  string
	seen  plugin.TurnEvent // 最近一次 TurnPrompt 收到的事件
}

func (p *scopePlugin) Name() string                                  { return p.name }
func (p *scopePlugin) Description() string                           { return "测试可见域插件" }
func (p *scopePlugin) Init(plugin.InitContext, map[string]any) error { return nil }
func (p *scopePlugin) SystemPrompt() string                          { return "" }
func (p *scopePlugin) Tools() []plugin.Tool                          { return nil }

func (p *scopePlugin) DecideScope(context.Context, plugin.TurnEvent) (plugin.Scope, error) {
	return p.scope, nil
}

func (p *scopePlugin) TurnPrompt(_ context.Context, ev plugin.TurnEvent) (string, error) {
	p.seen = ev
	return p.note, nil
}

func msg(role, content string) session.StoredMessage {
	return session.StoredMessage{Message: llm.Message{Role: role, Content: content}}
}

func tagged(m session.StoredMessage, tag string) session.StoredMessage {
	m.Tag = tag
	return m
}

func TestVisibleHistoryFiltersByTag(t *testing.T) {
	history := []session.StoredMessage{
		msg(llm.RoleUser, "共享问"), // 无标签
		msg(llm.RoleAssistant, "共享答"),
		tagged(msg(llm.RoleUser, "甲问"), "a"),
		tagged(msg(llm.RoleAssistant, "甲答"), "a"),
		tagged(msg(llm.RoleUser, "乙问"), "b"),
		tagged(msg(llm.RoleAssistant, "乙答"), "b"),
	}

	// 只读 a：无标签内容始终可读，b 的内容不可见
	got := visibleHistory(history, plugin.Scope{Write: "a", Read: []string{"a"}})
	var texts []string
	for _, m := range got {
		texts = append(texts, m.Content)
	}
	want := []string{"共享问", "共享答", "甲问", "甲答"}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Errorf("可见历史 = %v, want %v", texts, want)
	}

	// 零值 Scope 不做任何限制，行为与未启用裁决插件时一致
	if got := visibleHistory(history, plugin.Scope{}); len(got) != len(history) {
		t.Errorf("零值 Scope 应不过滤，得到 %d/%d", len(got), len(history))
	}

	// 读多个标签
	got = visibleHistory(history, plugin.Scope{Write: "b", Read: []string{"a", "b"}})
	if len(got) != 6 {
		t.Errorf("读 a+b 应看到全部 6 条，得到 %d", len(got))
	}
}

func TestVisibleHistoryKeepsToolPairsIntact(t *testing.T) {
	call := llm.ToolCall{ID: "c1", Name: "echo"}
	history := []session.StoredMessage{
		tagged(msg(llm.RoleUser, "甲问"), "a"),
		tagged(session.StoredMessage{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call}}}, "a"),
		tagged(session.StoredMessage{Message: llm.Message{Role: llm.RoleTool, Content: "结果", ToolCallID: "c1"}}, "a"),
		tagged(msg(llm.RoleUser, "乙问"), "b"),
	}

	// 整轮同去同留：a 可见时工具调用与结果都在
	got := visibleHistory(history, plugin.Scope{Write: "a", Read: []string{"a"}})
	if len(got) != 3 || len(got[1].ToolCalls) != 1 || got[2].ToolCallID != "c1" {
		t.Errorf("工具调用与结果应成对保留: %+v", got)
	}

	// a 不可见时整轮消失，不会留下孤立的工具结果
	got = visibleHistory(history, plugin.Scope{Write: "b", Read: []string{"b"}})
	if len(got) != 1 || got[0].Content != "乙问" {
		t.Errorf("不可读的整轮应完全消失: %+v", got)
	}
}

func TestSanitizeSequenceRepairsDanglingToolUse(t *testing.T) {
	// 进程在 assistant 落盘之后、tool 结果落盘之前被杀掉留下的残缺历史
	got := sanitizeSequence([]session.StoredMessage{
		msg(llm.RoleUser, "问"),
		{Message: llm.Message{Role: llm.RoleAssistant, Content: "我查一下",
			ToolCalls: []llm.ToolCall{{ID: "c1"}, {ID: "c2"}}}},
		{Message: llm.Message{Role: llm.RoleTool, Content: "c1 的结果", ToolCallID: "c1"}},
	})
	if len(got) != 3 {
		t.Fatalf("期望 3 条，得到 %d: %+v", len(got), got)
	}
	if len(got[1].ToolCalls) != 1 || got[1].ToolCalls[0].ID != "c1" {
		t.Errorf("没有结果的 tool_call 应被丢弃: %+v", got[1].ToolCalls)
	}
}

func TestSanitizeSequenceDropsEmptyAssistant(t *testing.T) {
	// 只有悬挂调用、又没有正文的 assistant 整条无意义
	got := sanitizeSequence([]session.StoredMessage{
		msg(llm.RoleUser, "问"),
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1"}}}},
	})
	if len(got) != 1 || got[0].Content != "问" {
		t.Errorf("得到 %+v", got)
	}
}

func TestSanitizeSequenceDropsOrphanToolResult(t *testing.T) {
	got := sanitizeSequence([]session.StoredMessage{
		msg(llm.RoleUser, "问"),
		{Message: llm.Message{Role: llm.RoleTool, Content: "无主结果", ToolCallID: "zzz"}},
		msg(llm.RoleAssistant, "答"),
	})
	if len(got) != 2 || got[1].Content != "答" {
		t.Errorf("孤立的工具结果应被丢弃: %+v", got)
	}
}

func TestSanitizeSequenceDropsLeadingNonUser(t *testing.T) {
	got := sanitizeSequence([]session.StoredMessage{
		msg(llm.RoleAssistant, "凭空出现的回答"),
		msg(llm.RoleUser, "问"),
		msg(llm.RoleAssistant, "答"),
	})
	if len(got) != 2 || got[0].Content != "问" {
		t.Errorf("首条必须是 user: %+v", got)
	}
}

func TestRunAppliesScopeToStorageAndRequest(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	// 预置一段属于别的可见域的历史
	store.Append(meta.ID, tagged(msg(llm.RoleUser, "秘密问"), "secret"))
	store.Append(meta.ID, tagged(msg(llm.RoleAssistant, "秘密答"), "secret"))

	sp := &scopePlugin{name: "scoper", scope: plugin.Scope{Write: "open", Read: []string{"open"}}, note: "本轮提示词"}
	provider := &mockProvider{turns: []mockTurn{{content: "答"}}}
	ag := New(provider, newTestManager(t, sp), store, Options{Model: "test"})

	ag.Run(context.Background(), meta.ID, "公开问", func(Event) {})

	// 请求里不该出现不可读域的内容
	body := ""
	for _, m := range provider.reqs[0].Messages {
		body += m.Content + "\n"
	}
	if strings.Contains(body, "秘密") {
		t.Errorf("不可读域的历史泄漏进了请求:\n%s", body)
	}
	// TurnPrompt 的片段随本轮状态块走在最后一条消息里，不进 system——
	// system 必须逐轮字节一致，否则提示词缓存前缀立刻作废
	last := provider.reqs[0].Messages[len(provider.reqs[0].Messages)-1]
	if !strings.Contains(last.Content, "本轮提示词") {
		t.Errorf("TurnPrompt 的片段未随本轮状态块注入: %q", last.Content)
	}
	if strings.Contains(provider.reqs[0].Messages[0].Content, "本轮提示词") {
		t.Error("TurnPrompt 的片段不该进 system（会作废缓存前缀）")
	}

	// 本轮落盘的消息都带上了 Write 标签
	gotMeta, msgs, _ := store.Get(meta.ID)
	for _, m := range msgs[2:] {
		if m.Tag != "open" {
			t.Errorf("本轮消息应带 open 标签: %+v", m)
		}
	}
	// 标题连同它所属的可见域一起记下
	if gotMeta.Title != "公开问" || gotMeta.Tag != "open" {
		t.Errorf("标题归属不对: title=%q tag=%q", gotMeta.Title, gotMeta.Tag)
	}
	// 钩子拿到的是会话 id、本轮输入与已裁决的可见域——不含会话历史，
	// 未过滤的全量历史广播出去正好与可见域机制的目的相抵
	if sp.seen.SessionID != meta.ID || sp.seen.UserInput != "公开问" || sp.seen.Scope.Write != "open" {
		t.Errorf("TurnEvent 不对: %+v", sp.seen)
	}
}

func TestDecideScopeSingleOwner(t *testing.T) {
	first := &scopePlugin{name: "first", scope: plugin.Scope{Write: "a"}}
	second := &scopePlugin{name: "second", scope: plugin.Scope{Write: "b"}}
	m := newTestManager(t, first, second)

	// 先注册者胜出，后者被忽略
	if got := m.DecideScope(context.Background(), plugin.TurnEvent{}); got.Write != "a" {
		t.Errorf("DecideScope = %+v, want Write=a", got)
	}
}

func TestDecideScopeRejectsInvalidTag(t *testing.T) {
	// 标签会被插件用来拼持久化目录，非法值必须整条作废而不是流进文件路径
	bad := &scopePlugin{name: "bad", scope: plugin.Scope{Write: "../evil", Read: []string{"x"}}}
	m := newTestManager(t, bad)
	if got := m.DecideScope(context.Background(), plugin.TurnEvent{}); !got.IsZero() {
		t.Errorf("非法标签应被忽略，得到 %+v", got)
	}
}

func TestDecideScopeSkipsDisabledPlugin(t *testing.T) {
	sp := &scopePlugin{name: "scoper", scope: plugin.Scope{Write: "a"}}
	m := plugin.NewManager(plugin.InitContext{}, "")
	if err := m.Register(sp, plugin.PluginConfig{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if got := m.DecideScope(context.Background(), plugin.TurnEvent{}); !got.IsZero() {
		t.Errorf("禁用的插件不应参与裁决，得到 %+v", got)
	}
}

func TestScopeCanRead(t *testing.T) {
	open := plugin.Scope{Write: "a", Read: []string{"a"}}
	if !open.CanRead("") {
		t.Error("无标签内容应始终可读")
	}
	if !open.CanRead("a") || open.CanRead("b") {
		t.Error("按白名单判定可读性")
	}
	if !(plugin.Scope{}).CanRead("anything") {
		t.Error("零值 Scope 应不做限制")
	}
}

func TestTrimToBudgetPinsSummary(t *testing.T) {
	big := strings.Repeat("字", 3000)
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "env"},
		{Role: llm.RoleUser, Content: "摘要：" + big}, // 压缩摘要，被钉住
		{Role: llm.RoleUser, Content: big},
		{Role: llm.RoleAssistant, Content: big},
		{Role: llm.RoleUser, Content: "最新问题"},
	}
	pinned := []bool{true, true, false, false, false}

	got := trimToBudget(Options{ContextLength: 8000, MaxTokens: 1000}, msgs, pinned)
	if len(got) >= len(msgs) {
		t.Fatalf("应有裁剪，得到 %d 条", len(got))
	}
	// 摘要代表一大段已被物理删除的历史，丢掉它等于永久失忆
	if !strings.HasPrefix(got[1].Content, "摘要：") {
		t.Errorf("被钉住的摘要不应被裁掉: %q", truncateRunes("", got[1].Content, 12))
	}
	if got[len(got)-1].Content != "最新问题" {
		t.Error("最近一轮必须保留")
	}
}
