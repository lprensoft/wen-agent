package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"wen/internal/agent"
	"wen/internal/clock"
	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/plugin/builtin/growth"
	"wen/internal/plugin/builtin/memory"
	"wen/internal/plugin/builtin/roleplay"
	"wen/internal/plugin/builtin/sessionsearch"
	"wen/internal/session"
)

type recordProvider struct {
	mu    sync.Mutex
	reqs  []llm.ChatRequest
	reply string
}

func (p *recordProvider) ChatStream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	ch := make(chan llm.StreamEvent, 2)
	ch <- llm.StreamEvent{Type: llm.EventContentDelta, Content: p.reply}
	ch <- llm.StreamEvent{Type: llm.EventDone}
	close(ch)
	return ch, nil
}

func (p *recordProvider) snapshot() []llm.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.ChatRequest(nil), p.reqs...)
}

// 反思必须看到 memory 刚落盘的昨日摘要：只有走真实 Agent 组装才能验证这一点，
// 假 RunTurn 只能看到反思指令本身，看不到 TurnPrompts 拼进去的上下文。
func TestReflectionSeesMemoryTimeline(t *testing.T) {
	summary := "昨天：一起讨论了搬家的事。"
	provider := &recordProvider{reply: summary}
	clk := clock.NewManual(time.Date(2026, 3, 1, 20, 0, 0, 0, time.Local))
	ctx := clock.With(context.Background(), clk)

	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var ag *agent.Agent
	m := plugin.NewManager(plugin.InitContext{
		Workdir:    t.TempDir(),
		Lang:       "zh",
		Clock:      clk,
		ManualTime: true,
		Sessions:   store,
		SessionDir: t.TempDir(),
		Complete: func(ctx context.Context, prompt string) (string, error) {
			if ag == nil {
				return summary, nil
			}
			return ag.Complete(ctx, prompt)
		},
		RunTurn: func(ctx context.Context, sid, input string) (string, error) {
			return ag.RunTurn(ctx, sid, input)
		},
		Notice: func(ctx context.Context, sid, text string) error {
			return ag.AppendNotice(ctx, sid, text)
		},
		NewSession: func() (string, error) {
			meta, err := store.CreateAt(clk.Now())
			return meta.ID, err
		},
	}, "", plugin.WithStateDir(t.TempDir()))

	for _, p := range []plugin.Plugin{
		memory.New(), sessionsearch.New(), roleplay.New(), growth.New(),
	} {
		if err := m.Register(p, plugin.PluginConfig{Enabled: true}); err != nil {
			t.Fatalf("注册 %s 失败: %v", p.Name(), err)
		}
	}
	t.Cleanup(m.StopAll)

	ag = agent.New(provider, m, store, agent.Options{
		Model: "fake", Lang: "zh", MaxTurns: 4, ContextLength: 200000,
		Workdir: t.TempDir(),
	})
	meta, err := store.CreateAt(clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	sid := meta.ID

	// 第一天：真人轮次把当天的缓冲攒起来。
	ag.Run(plugin.WithInteractive(ctx), sid, "今天一起讨论搬家的事", func(agent.Event) {})
	if err := m.Settle(ctx); err != nil {
		t.Fatal(err)
	}

	// 第二天：日切把第一天收束成时间线，成长反思随后读上下文。
	clk.Set(time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local))
	ag.Run(plugin.WithInteractive(ctx), sid, "早", func(agent.Event) {})
	if err := m.Settle(ctx); err != nil {
		t.Fatal(err)
	}

	found := false
	for _, req := range provider.snapshot() {
		var b strings.Builder
		for _, msg := range req.Messages {
			b.WriteString(msg.Content)
			b.WriteString("\n")
		}
		text := b.String()
		if strings.Contains(text, "【成长·反思】") && strings.Contains(text, summary) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("反思轮次的上下文里没有看到 memory 刚落盘的昨日摘要")
	}
}
