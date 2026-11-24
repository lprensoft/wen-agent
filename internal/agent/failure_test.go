package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/session"
	"wen/internal/usage"
)

// erroringProvider 先流出一段半截文本（可选先报一笔用量），然后以错误事件收场。
type erroringProvider struct {
	partial string
	// usage 非 nil 时先发一条用量事件：失败的那次请求同样花了钱，账本要记下来。
	usage *llm.Usage
	err   error
}

func (p *erroringProvider) ChatStream(_ context.Context, _ llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	go func() {
		defer close(ch)
		for _, r := range p.partial {
			ch <- llm.StreamEvent{Type: llm.EventContentDelta, Content: string(r)}
		}
		if p.usage != nil {
			ch <- llm.StreamEvent{Type: llm.EventUsage, Usage: p.usage}
		}
		ch <- llm.StreamEvent{Type: llm.EventError, Err: p.err}
	}()
	return ch, nil
}

// translatorPlugin 是实现了失败转译的测试插件。
type translatorPlugin struct {
	text   string
	ok     bool
	gotErr error
	// complete 非 nil 时在转译里发起一次辅助调用：真实插件的转译本身就是一次模型
	// 调用，用它验证那次调用在账本里记成哪个来源。
	complete func(context.Context) error
}

func (p *translatorPlugin) Name() string                                  { return "fake_translator" }
func (p *translatorPlugin) Description() string                           { return "测试转译" }
func (p *translatorPlugin) Init(plugin.InitContext, map[string]any) error { return nil }
func (p *translatorPlugin) SystemPrompt() string                          { return "" }
func (p *translatorPlugin) Tools() []plugin.Tool                          { return nil }
func (p *translatorPlugin) TranslateFailure(ctx context.Context, ev plugin.TurnFailure) (string, bool) {
	p.gotErr = ev.Err
	if p.complete != nil {
		_ = p.complete(ctx)
	}
	return p.text, p.ok
}

func TestFailureTranslatedOnInteractiveTurn(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	cause := errors.New("boom")
	provider := &erroringProvider{partial: "半截", err: cause}
	tr := &translatorPlugin{text: "（愣了一下）……刚才说到哪了？", ok: true}

	ag := New(provider, newTestManager(t, tr), store, Options{Model: "test", MaxTurns: 3})

	var deltas string
	var sawError, sawDone bool
	ag.Run(plugin.WithInteractive(context.Background()), meta.ID, "hi", func(ev Event) {
		switch ev.Type {
		case EventDelta:
			deltas += ev.Content
		case EventError:
			sawError = true
		case EventDone:
			sawDone = true
		}
	})

	if sawError || !sawDone {
		t.Fatalf("sawError=%v sawDone=%v，转译成功应按正常轮次收尾", sawError, sawDone)
	}
	if tr.gotErr != cause {
		t.Errorf("translator got err %v, want %v", tr.gotErr, cause)
	}
	// 半截文本已经上屏，台词另起一段接在后面
	if !strings.HasSuffix(deltas, "\n\n（愣了一下）……刚才说到哪了？") {
		t.Errorf("deltas = %q", deltas)
	}

	_, msgs, _ := store.Get(meta.ID)
	// user + assistant（台词）+ notice 共 3 条；半截文本不落盘
	if len(msgs) != 3 {
		t.Fatalf("stored %d messages, want 3: %+v", len(msgs), msgs)
	}
	if msgs[1].Role != llm.RoleAssistant || msgs[1].Content != "（愣了一下）……刚才说到哪了？" {
		t.Errorf("assistant message = %+v", msgs[1])
	}
	if msgs[2].Kind != session.KindNotice || !strings.Contains(msgs[2].Content, "boom") {
		t.Errorf("notice message = %+v，应含原始错误", msgs[2])
	}
}

// 失败前已经流出正文时，转译文本必须另起一段（\n\n 开头）：两段话粘成一句，
// 界面上看起来就成了模型自己改口。原始错误则进会话注记，真相不丢。
func TestFailureTranslatedAfterPartialContent(t *testing.T) {
	cause := &llm.APIError{Status: 500, Body: "boom"}
	provider := &chainProvider{events: append(contentEvents("半截话"),
		llm.StreamEvent{Type: llm.EventError, Err: cause})}
	tr := &translatorPlugin{text: "（愣了一下）刚才说到哪了？", ok: true}
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	ag := New(provider, newTestManager(t, tr), store, Options{Model: "test", MaxTurns: 3})

	var translated string
	var sawError bool
	ag.Run(plugin.WithInteractive(context.Background()), meta.ID, "hi", func(ev Event) {
		switch ev.Type {
		case EventDelta:
			if strings.Contains(ev.Content, "愣了一下") {
				translated = ev.Content
			}
		case EventError:
			sawError = true
		}
	})
	if sawError {
		t.Fatal("转译成功应按正常轮次收尾")
	}
	if !strings.HasPrefix(translated, "\n\n") {
		t.Errorf("转译文本应另起一段（\\n\\n 开头），实际 %q", translated)
	}
	if tr.gotErr != cause {
		t.Errorf("translator got err %v, want %v", tr.gotErr, cause)
	}

	_, msgs, _ := store.Get(meta.ID)
	var notice string
	for _, m := range msgs {
		if m.Kind == session.KindNotice {
			notice = m.Content
		}
	}
	if !strings.Contains(notice, "boom") {
		t.Errorf("会话注记应记下原始错误，实际 %q", notice)
	}
}

// 交互轮次里由转译发起的辅助调用属于前台：它不带 TurnOrigin，但有人在等那句台词。
// 只按 Kind 归来源会把它记成 internal，账本上「后台花了多少」因此虚高。
func TestFailureTranslationHelperNotInternal(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	provider := &erroringProvider{usage: &llm.Usage{PromptTokens: 12}, err: errors.New("boom")}
	tr := &translatorPlugin{text: "……刚才说到哪了？", ok: true}
	ag := New(provider, newTestManager(t, tr), store, Options{Model: "test", MaxTurns: 3})
	dir := t.TempDir()
	ledger, err := usage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ag.SetUsageLedger(ledger)
	// 转译与真实插件一样经 Agent.Complete 发起那次模型调用，ctx 带着交互标记。
	tr.complete = func(ctx context.Context) error {
		_, err := ag.Complete(ctx, "把失败说成一句话")
		return err
	}

	ag.Run(plugin.WithInteractive(context.Background()), meta.ID, "hi", func(Event) {})

	got := ledgerEntries(t, dir)
	var helper *usage.Entry
	for i := range got {
		if got[i].Kind == usage.KindHelper {
			helper = &got[i]
		}
	}
	if helper == nil {
		t.Fatalf("转译发起的辅助调用应记一条 helper: %+v", got)
	}
	if helper.Origin == "internal" {
		t.Errorf("交互轮次里的辅助调用被记成了 internal: %+v", *helper)
	}
	if helper.Origin != "" {
		t.Errorf("前台辅助调用的来源应为空，得到 %q", helper.Origin)
	}
}

func TestFailureNotTranslatedWithoutInteractive(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	provider := &erroringProvider{err: errors.New("boom")}
	tr := &translatorPlugin{text: "不该出现", ok: true}

	ag := New(provider, newTestManager(t, tr), store, Options{Model: "test", MaxTurns: 3})

	var sawError bool
	ag.Run(context.Background(), meta.ID, "hi", func(ev Event) {
		if ev.Type == EventError {
			sawError = true
		}
	})
	if !sawError {
		t.Fatal("后台轮次不该转译，应照常报错")
	}
	_, msgs, _ := store.Get(meta.ID)
	// 只有 user 一条：没有台词、没有注记
	if len(msgs) != 1 {
		t.Fatalf("stored %d messages, want 1: %+v", len(msgs), msgs)
	}
}

func TestFailureFallsThroughWhenTranslatorDeclines(t *testing.T) {
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	provider := &erroringProvider{err: errors.New("boom")}
	tr := &translatorPlugin{ok: false}

	ag := New(provider, newTestManager(t, tr), store, Options{Model: "test", MaxTurns: 3})

	var errText string
	ag.Run(plugin.WithInteractive(context.Background()), meta.ID, "hi", func(ev Event) {
		if ev.Type == EventError {
			errText = ev.Error
		}
	})
	if !strings.Contains(errText, "boom") {
		t.Errorf("error = %q，不接手时应报原始错误", errText)
	}
}
