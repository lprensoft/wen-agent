package agent

import (
	"context"
	"strings"
	"testing"

	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/session"
)

// newTestStore 建一个临时会话目录。
func newTestStore(t *testing.T) *session.Store {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestAppendNoticeStaysOutOfModelContext(t *testing.T) {
	store := newTestStore(t)
	a := New(nil, plugin.NewManager(plugin.InitContext{}, ""), store, Options{})
	meta, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(meta.ID, session.StoredMessage{
		Message: llm.Message{Role: llm.RoleUser, Content: "你好"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.AppendNotice(context.Background(), meta.ID, "🧠 记忆提炼：新增「事实/构建命令」"); err != nil {
		t.Fatal(err)
	}

	_, stored, err := store.Get(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 落盘了，界面才看得到
	var notice *session.StoredMessage
	for i := range stored {
		if stored[i].Kind == session.KindNotice {
			notice = &stored[i]
		}
	}
	if notice == nil {
		t.Fatal("注记应落进会话")
	}
	// 但组装上下文时一定要被滤掉——模型看到它就等于凭空多了一条它没做过的记录
	for _, m := range visibleHistory(stored, plugin.Scope{}) {
		if strings.Contains(m.Content, "记忆提炼") {
			t.Error("注记不该进入模型上下文")
		}
	}
	// 也不该计进上下文用量
	if got := EstimateStoredTokens(stored); got != EstimateHistoryTokens([]llm.Message{{Role: llm.RoleUser, Content: "你好"}}) {
		t.Errorf("注记不该计进 token 估算：%d", got)
	}
}

func TestAppendNoticeCarriesScopeAndOrigin(t *testing.T) {
	store := newTestStore(t)
	a := New(nil, plugin.NewManager(plugin.InitContext{}, ""), store, Options{})
	meta, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}

	ctx := plugin.WithScope(context.Background(), plugin.Scope{Write: "inner"})
	ctx = plugin.WithTurnOrigin(ctx, "memory")
	if err := a.AppendNotice(ctx, meta.ID, "里侧的注记"); err != nil {
		t.Fatal(err)
	}

	_, stored, _ := store.Get(meta.ID)
	if len(stored) != 1 {
		t.Fatalf("应有一条注记，实际 %d 条", len(stored))
	}
	// 「在人格 A 的库里记了什么」不该被人格 B 看见
	if stored[0].Tag != "inner" {
		t.Errorf("注记应带上本轮可见域标签，实际 %q", stored[0].Tag)
	}
	if stored[0].Origin != "memory" {
		t.Errorf("注记应记下发起方，实际 %q", stored[0].Origin)
	}
	if got := visibleHistory(stored, plugin.Scope{Write: "outer", Read: []string{"outer"}}); len(got) != 0 {
		t.Error("不可读域的注记不该出现")
	}
}

func TestAppendNoticeReachesSink(t *testing.T) {
	store := newTestStore(t)
	a := New(nil, plugin.NewManager(plugin.InitContext{}, ""), store, Options{})
	meta, _ := store.Create()

	var got []session.StoredMessage
	var gotSession string
	a.SetNoticeSink(func(sid string, m session.StoredMessage) {
		gotSession = sid
		got = append(got, m)
	})
	if err := a.AppendNotice(context.Background(), meta.ID, "实时推给界面"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "实时推给界面" || gotSession != meta.ID {
		t.Errorf("注记未送达实时出口：%v %q", got, gotSession)
	}
}

func TestAppendNoticeIgnoresEmptyText(t *testing.T) {
	store := newTestStore(t)
	a := New(nil, plugin.NewManager(plugin.InitContext{}, ""), store, Options{})
	meta, _ := store.Create()

	called := false
	a.SetNoticeSink(func(string, session.StoredMessage) { called = true })
	if err := a.AppendNotice(context.Background(), meta.ID, "   "); err != nil {
		t.Errorf("空注记应静默跳过而不是报错：%v", err)
	}
	if called {
		t.Error("空注记不该推给界面")
	}
	if _, stored, _ := store.Get(meta.ID); len(stored) != 0 {
		t.Error("空注记不该落盘")
	}
}

func TestAppendNoticeTruncatesOverlongText(t *testing.T) {
	store := newTestStore(t)
	a := New(nil, plugin.NewManager(plugin.InitContext{}, ""), store, Options{})
	meta, _ := store.Create()

	if err := a.AppendNotice(context.Background(), meta.ID, strings.Repeat("很长的注记", 500)); err != nil {
		t.Fatal(err)
	}
	_, stored, _ := store.Get(meta.ID)
	// 写注记的是插件，核心不该指望对方自觉；截断标记要留着，免得看起来像内容本身就那样
	if n := len([]rune(stored[0].Content)); n > noticeMaxRunes+8 {
		t.Errorf("注记应有长度上界，实际 %d 字", n)
	}
	if !strings.Contains(stored[0].Content, "截断") {
		t.Error("截断后应有标记")
	}
}

func TestNoticeExcludedFromCompactionSummary(t *testing.T) {
	msgs := []session.StoredMessage{
		{Message: llm.Message{Role: llm.RoleUser, Content: "问题"}},
		{Message: llm.Message{Role: llm.RoleSystem, Content: "🧠 记忆提炼：新增「事实/某事」"}, Kind: session.KindNotice},
		{Message: llm.Message{Role: llm.RoleAssistant, Content: "回答"}},
	}
	got := serializeHistory("", msgs)
	if strings.Contains(got, "记忆提炼") {
		t.Errorf("注记不该进摘要——它不是对话内容：\n%s", got)
	}
	if !strings.Contains(got, "问题") || !strings.Contains(got, "回答") {
		t.Errorf("其余消息应照常进摘要：\n%s", got)
	}
}

// noticeSpy 是最小的 NoticeObserver 插件，记下收到的注记事件。
type noticeSpy struct{ events []plugin.NoticeEvent }

func (s *noticeSpy) Name() string                                  { return "notice_spy" }
func (s *noticeSpy) Description() string                           { return "" }
func (s *noticeSpy) Init(plugin.InitContext, map[string]any) error { return nil }
func (s *noticeSpy) SystemPrompt() string                          { return "" }
func (s *noticeSpy) Tools() []plugin.Tool                          { return nil }
func (s *noticeSpy) OnNotice(_ context.Context, ev plugin.NoticeEvent) {
	s.events = append(s.events, ev)
}

// 注记落盘后要广播给 NoticeObserver（IM 通道据此把后台说明推给远端用户），
// 事件带发起方与可见域标签，正文与落盘内容一字不差。
func TestAppendNoticeBroadcastsToObservers(t *testing.T) {
	store := newTestStore(t)
	m := plugin.NewManager(plugin.InitContext{}, "")
	spy := &noticeSpy{}
	if err := m.Register(spy, plugin.PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	a := New(nil, m, store, Options{})
	meta, _ := store.Create()

	ctx := plugin.WithScope(context.Background(), plugin.Scope{Write: "inner"})
	ctx = plugin.WithTurnOrigin(ctx, "memory")
	if err := a.AppendNotice(ctx, meta.ID, "🧠 记忆提炼：新增「事实/A」"); err != nil {
		t.Fatal(err)
	}

	if len(spy.events) != 1 {
		t.Fatalf("应广播一次，实际 %d 次", len(spy.events))
	}
	ev := spy.events[0]
	if ev.SessionID != meta.ID || ev.Origin != "memory" || ev.Tag != "inner" ||
		ev.Text != "🧠 记忆提炼：新增「事实/A」" {
		t.Fatalf("事件内容不对：%+v", ev)
	}
}
