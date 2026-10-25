package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/session"
)

// gateProvider 的流会停在开头直到 release 关闭，用于制造「会话忙」窗口。
// （与 agent_test.go 的 blockingProvider 不同，它可被多次调用。）
type gateProvider struct {
	started chan struct{}
	release chan struct{}
}

func (p *gateProvider) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	go func() {
		defer close(ch)
		p.started <- struct{}{}
		<-p.release
		ch <- llm.StreamEvent{Type: llm.EventContentDelta, Content: "好"}
		ch <- llm.StreamEvent{Type: llm.EventDone}
	}()
	return ch, nil
}

func newTestSession(t *testing.T) (*session.Store, string) {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	return store, meta.ID
}

// 会话被前台轮次占用时，插件轮次立即得到 ErrSessionBusy 而不是排队。
func TestRunTurnBusy(t *testing.T) {
	store, id := newTestSession(t)
	bp := &gateProvider{started: make(chan struct{}, 4), release: make(chan struct{})}
	ag := New(bp, newTestManager(t), store, Options{Model: "m"})

	done := make(chan struct{})
	go func() {
		ag.Run(context.Background(), id, "你好", func(Event) {})
		close(done)
	}()
	<-bp.started // 前台轮次已持锁并停在流上

	if _, err := ag.RunTurn(context.Background(), id, "后台输入"); !errors.Is(err, plugin.ErrSessionBusy) {
		t.Fatalf("期望 ErrSessionBusy，得到 %v", err)
	}

	close(bp.release)
	<-done

	// 锁释放后插件轮次可以正常进行，并返回最终文本
	text, err := ag.RunTurn(context.Background(), id, "再来一轮")
	if err != nil {
		t.Fatalf("解锁后 RunTurn 应成功: %v", err)
	}
	if text != "好" {
		t.Fatalf("最终文本 = %q，期望 %q", text, "好")
	}
}

// 只有标记了真人交互的轮次才更新 LastActiveAt；发起方标记随整轮消息落盘。
func TestInteractiveAndOrigin(t *testing.T) {
	store, id := newTestSession(t)
	mp := &mockProvider{turns: []mockTurn{{content: "一"}, {content: "二"}}}
	ag := New(mp, newTestManager(t), store, Options{Model: "m"})

	// 无交互标记的后台轮次：LastActiveAt 保持为空
	ctx := plugin.WithTurnOrigin(context.Background(), "hb_test")
	if _, err := ag.RunTurn(ctx, id, "心跳输入"); err != nil {
		t.Fatal(err)
	}
	meta, msgs, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if meta.LastActiveAt != nil {
		t.Fatalf("后台轮次不应更新 LastActiveAt，得到 %v", meta.LastActiveAt)
	}
	for _, m := range msgs {
		if m.Origin != "hb_test" {
			t.Fatalf("消息 Origin = %q，期望 hb_test", m.Origin)
		}
	}

	// 带交互标记的前台轮次：LastActiveAt 被更新，Origin 为空
	before := time.Now()
	ag.Run(plugin.WithInteractive(context.Background()), id, "前台输入", func(Event) {})
	meta, msgs, err = store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if meta.LastActiveAt == nil || meta.LastActiveAt.Before(before.Add(-time.Second)) {
		t.Fatalf("前台轮次应更新 LastActiveAt，得到 %v", meta.LastActiveAt)
	}
	last := msgs[len(msgs)-1]
	if last.Origin != "" {
		t.Fatalf("前台消息 Origin 应为空，得到 %q", last.Origin)
	}
}

// 一次性输入：只在自己那一轮发给模型，落盘带标记、不参与标题、不进后续上下文；
// 它触发的助手回复照常保留。
func TestEphemeralInput(t *testing.T) {
	store, id := newTestSession(t)
	mp := &mockProvider{turns: []mockTurn{{content: "回答一"}, {content: "心跳回复"}, {content: "回答三"}}}
	ag := New(mp, newTestManager(t), store, Options{Model: "m"})

	ag.Run(plugin.WithInteractive(context.Background()), id, "你好", func(Event) {})

	hbCtx := plugin.WithEphemeralInput(plugin.WithTurnOrigin(context.Background(), "hb_test"))
	if _, err := ag.RunTurn(hbCtx, id, "【心跳】模板提示词"); err != nil {
		t.Fatal(err)
	}

	meta, msgs, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "你好" {
		t.Fatalf("标题应来自真实用户消息，得到 %q", meta.Title)
	}
	var found bool
	for _, m := range msgs {
		if m.Content == "【心跳】模板提示词" {
			found = true
			if m.Kind != session.KindEphemeral {
				t.Fatalf("一次性输入落盘应带标记，得到 kind=%q", m.Kind)
			}
		}
	}
	if !found {
		t.Fatal("一次性输入应留在磁盘上（审计与界面提示用）")
	}

	// 第三轮：上下文包含两轮助手回复，但不包含心跳提示词
	ag.Run(plugin.WithInteractive(context.Background()), id, "再见", func(Event) {})
	req := mp.reqs[len(mp.reqs)-1]
	var all string
	for _, m := range req.Messages {
		all += m.Content + "\n"
	}
	if strings.Contains(all, "【心跳】模板提示词") {
		t.Fatal("心跳提示词泄漏进了后续上下文")
	}
	for _, want := range []string{"你好", "回答一", "心跳回复", "再见"} {
		if !strings.Contains(all, want) {
			t.Fatalf("后续上下文缺少 %q", want)
		}
	}
}

// observerPlugin 记录收到的轮次结束事件。
type observerPlugin struct{ evs []plugin.TurnEndEvent }

func (o *observerPlugin) Name() string                                  { return "observer_plugin" }
func (o *observerPlugin) Description() string                           { return "测试轮次观察" }
func (o *observerPlugin) Init(plugin.InitContext, map[string]any) error { return nil }
func (o *observerPlugin) SystemPrompt() string                          { return "" }
func (o *observerPlugin) Tools() []plugin.Tool                          { return nil }
func (o *observerPlugin) OnTurnEnd(_ context.Context, ev plugin.TurnEndEvent) {
	o.evs = append(o.evs, ev)
}

// 轮次成功结束后广播 TurnEndEvent，字段与本轮一致。
func TestTurnEndBroadcast(t *testing.T) {
	store, id := newTestSession(t)
	ob := &observerPlugin{}
	mp := &mockProvider{turns: []mockTurn{{content: "回答"}}}
	ag := New(mp, newTestManager(t, ob), store, Options{Model: "m"})

	ctx := plugin.WithInteractive(plugin.WithTurnOrigin(context.Background(), "qq_test"))
	if _, err := ag.RunTurn(ctx, id, "提问"); err != nil {
		t.Fatal(err)
	}
	if len(ob.evs) != 1 {
		t.Fatalf("期望 1 个轮次结束事件，得到 %d", len(ob.evs))
	}
	ev := ob.evs[0]
	if ev.SessionID != id || ev.Origin != "qq_test" || !ev.Interactive ||
		ev.UserInput != "提问" || ev.FinalText != "回答" {
		t.Fatalf("事件字段不符: %+v", ev)
	}
	if ev.EndedAt.Before(ev.StartedAt) {
		t.Fatalf("EndedAt 早于 StartedAt: %+v", ev)
	}
}

// 安装过程通知回调后：每轮的完整思考链、每批工具名按序送达；未安装则零开销。
func TestTurnNotes(t *testing.T) {
	store, id := newTestSession(t)
	mp := &mockProvider{turns: []mockTurn{
		{reasoning: "先查一下", toolCalls: []llm.ToolCall{
			{ID: "c1", Name: "echo", Arguments: []byte(`{"text":"敏感参数"}`)},
		}},
		{reasoning: "查完了", content: "最终回答"},
	}}
	ag := New(mp, newTestManager(t, echoPlugin{}), store, Options{Model: "m", MaxTurns: 5})

	var notes []plugin.TurnNote
	ctx := plugin.WithTurnNotes(context.Background(), func(n plugin.TurnNote) {
		notes = append(notes, n)
	})
	final, err := ag.RunTurn(ctx, id, "帮我查")
	if err != nil {
		t.Fatal(err)
	}
	if final != "最终回答" {
		t.Fatalf("最终文本 = %q", final)
	}

	want := []plugin.TurnNote{
		{Kind: plugin.NoteThinking, Text: "先查一下"},
		{Kind: plugin.NoteToolCalls, Tools: []string{"echo"}},
		{Kind: plugin.NoteThinking, Text: "查完了"},
	}
	if len(notes) != len(want) {
		t.Fatalf("通知数 = %d，期望 %d: %+v", len(notes), len(want), notes)
	}
	for i, n := range notes {
		if n.Kind != want[i].Kind || n.Text != want[i].Text || !slices.Equal(n.Tools, want[i].Tools) {
			t.Fatalf("通知[%d] = %+v，期望 %+v", i, n, want[i])
		}
	}
	// 工具通知只有名字，不含参数
	for _, n := range notes {
		if strings.Contains(n.Text, "敏感参数") {
			t.Fatal("通知不该携带工具参数")
		}
	}
}

// 回复预览：每次送的是当前这次模型响应到目前为止的全文；调了工具之后下一次响应从空开始
func TestReplyPreview(t *testing.T) {
	store, id := newTestSession(t)
	mp := &mockProvider{turns: []mockTurn{
		{content: "我查一下", toolCalls: []llm.ToolCall{{ID: "c1", Name: "echo", Arguments: []byte(`{"text":"x"}`)}}},
		{content: "最终回答"},
	}}
	ag := New(mp, newTestManager(t, echoPlugin{}), store, Options{Model: "m", MaxTurns: 5})
	var got []string
	ctx := plugin.WithReplyPreview(context.Background(), func(text string) { got = append(got, text) })
	if _, err := ag.RunTurn(ctx, id, "帮我查"); err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[len(got)-1] != "最终回答" {
		t.Fatalf("最后一次预览应是最终回复的全文: %q", got)
	}
	if !slices.Contains(got, "我查一下") {
		t.Errorf("工具调用之前的那段也该预览过: %q", got)
	}
}
