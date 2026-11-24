package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wen/internal/i18n"
	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/session"
	"wen/internal/usage"
)

// budgetSettings 造一份只限 token 的预算设置。
func budgetSettings(tokens int64, over string) usage.Settings {
	s := usage.DefaultSettings()
	s.MonthlyTokens = tokens
	s.Over = over
	return s
}

// budgetAgent 造一个带账本与预算的 Agent，账本里先记下 spent 个 token 的用量。
func budgetAgent(t *testing.T, p llm.Provider, s usage.Settings, spent int, ps ...plugin.Plugin) (*Agent, string) {
	t.Helper()
	return budgetAgentLookup(t, p, s, spent, nil, ps...)
}

// budgetAgentLookup 与 budgetAgent 相同，但可以带价格表（金额上限要用它折算）。
func budgetAgentLookup(t *testing.T, p llm.Provider, s usage.Settings, spent int, lookup usage.PriceLookup, ps ...plugin.Plugin) (*Agent, string) {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	ag := New(p, newTestManager(t, ps...), store, Options{Model: "m1", MaxTurns: 4})
	ag.SetModelTargets([]ModelTarget{{
		Provider: p, Options: ModelOptions{Model: "m1", MaxTokens: 4096}, ProviderName: "p1",
	}})
	ledger, err := usage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ag.SetUsageLedger(ledger)
	if spent > 0 {
		if err := ledger.Append(usage.Entry{TS: time.Now(), Kind: usage.KindTurn, Prompt: spent}); err != nil {
			t.Fatal(err)
		}
	}
	ag.SetUsageBudget(usage.NewBudget(s, ledger, lookup, time.Now()))
	return ag, meta.ID
}

// 后台轮次超限：直接报错、一次模型调用都不发，用户消息也不落盘。
func TestBudgetBlocksBackgroundRunTurn(t *testing.T) {
	p := &mockProvider{} // 没有脚本：真被调用会越界 panic
	ag, id := budgetAgent(t, p, budgetSettings(10, usage.OverBlockAll), 10)

	_, err := ag.RunTurn(context.Background(), id, "在吗")
	if !errors.Is(err, plugin.ErrBudgetExceeded) {
		t.Fatalf("应报 plugin.ErrBudgetExceeded，得到 %v", err)
	}
	if !errors.Is(err, usage.ErrBudgetExceeded) {
		t.Fatalf("同一个错误也应认 usage.ErrBudgetExceeded，得到 %v", err)
	}
	// 入口就被拦下：这一轮什么都没做，发起方可以安全回滚待办。
	if plugin.BudgetStoppedMidTurn(err) {
		t.Fatalf("轮次入口被拦不应报「中途停下」: %v", err)
	}
	if p.calls != 0 {
		t.Fatalf("被拦下的轮次不该调用模型，实际 %d 次", p.calls)
	}
	if _, hist, err := ag.store.Get(id); err != nil || len(hist) != 0 {
		t.Fatalf("被拦下的轮次不该写历史: 条数=%d err=%v", len(hist), err)
	}
}

// skip_background：后台停、前台照常走完。
func TestBudgetSkipBackgroundLetsForegroundRun(t *testing.T) {
	p := &mockProvider{turns: []mockTurn{{
		content: "在的",
		usage:   &llm.Usage{PromptTokens: 5, CompletionTokens: 1},
	}}}
	ag, id := budgetAgent(t, p, budgetSettings(10, usage.OverSkipBackground), 100)

	var done, failed bool
	ag.Run(plugin.WithInteractive(context.Background()), id, "在吗", func(ev Event) {
		switch ev.Type {
		case EventDone:
			done = true
		case EventError:
			failed = true
		}
	})
	if !done || failed {
		t.Fatalf("skip_background 下前台轮次应正常完成: done=%v failed=%v", done, failed)
	}
	if p.calls != 1 {
		t.Fatalf("前台轮次应真的调用一次模型，实际 %d 次", p.calls)
	}
}

// block_all：前台也拦，走 error 事件，一次模型调用都不发。
func TestBudgetBlockAllStopsForeground(t *testing.T) {
	p := &mockProvider{}
	ag, id := budgetAgent(t, p, budgetSettings(10, usage.OverBlockAll), 10)

	var errText string
	var done bool
	ag.Run(plugin.WithInteractive(context.Background()), id, "在吗", func(ev Event) {
		switch ev.Type {
		case EventError:
			errText = ev.Error
		case EventDone:
			done = true
		}
	})
	if done || errText == "" {
		t.Fatalf("block_all 下前台轮次应以 error 收尾: done=%v err=%q", done, errText)
	}
	// 文案要说清「已用 / 上限」，光一个「超限了」用户不知道离多远
	if !strings.Contains(errText, "10") || !strings.Contains(errText, "上限") {
		t.Fatalf("错误文案应带上已用与上限: %q", errText)
	}
	if p.calls != 0 {
		t.Fatalf("被拦下的轮次不该调用模型，实际 %d 次", p.calls)
	}
}

// countPlugin 提供一个会计数的 echo 工具：用来确认预算中途停下时工具只执行了一次。
type countPlugin struct{ hits *int32 }

func (countPlugin) Name() string                                  { return "count_plugin" }
func (countPlugin) Description() string                           { return "计数插件" }
func (countPlugin) Init(plugin.InitContext, map[string]any) error { return nil }
func (countPlugin) SystemPrompt() string                          { return "" }
func (p countPlugin) Tools() []plugin.Tool                        { return []plugin.Tool{countTool{p.hits}} }

type countTool struct{ hits *int32 }

func (countTool) Name() string        { return "echo" }
func (countTool) Description() string { return "echo text back" }
func (countTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)
}
func (t countTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	atomic.AddInt32(t.hits, 1)
	var p struct{ Text string }
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	return "echo: " + p.Text, nil
}

// 后台工具循环：第一轮请求把额度用光，第二轮请求前就该停下——不是把这轮走完。
// 这一轮已经执行过工具，错误必须带上「中途停下」的标记：调度器据此不能把任务
// 回滚成待处理，否则额度恢复后整轮从头再跑，工具与通知都做两遍。
func TestBudgetStopsBackgroundToolLoop(t *testing.T) {
	p := &mockProvider{turns: []mockTurn{
		{
			toolCalls: []llm.ToolCall{{ID: "c1", Name: "echo", Arguments: []byte(`{"text":"hi"}`)}},
			usage:     &llm.Usage{PromptTokens: 1, CompletionTokens: 0},
		},
		{content: "不该走到这里"},
	}}
	// 已用 9 / 上限 10：第一笔记完就到顶，第二次请求前恰好被拦。
	var hits int32
	ag, id := budgetAgent(t, p, budgetSettings(10, usage.OverBlockAll), 9, countPlugin{hits: &hits})

	_, err := ag.RunTurn(context.Background(), id, "调用一下 echo")
	if !errors.Is(err, plugin.ErrBudgetExceeded) {
		t.Fatalf("第二轮请求前应被拦下，得到 %v", err)
	}
	if !plugin.BudgetStoppedMidTurn(err) {
		t.Fatalf("轮次已经开始过，应报「中途停下」: %v", err)
	}
	if p.calls != 1 {
		t.Fatalf("只该发出第一轮请求，实际 %d 次", p.calls)
	}
	if hits != 1 {
		t.Fatalf("工具只该执行一次，实际 %d 次", hits)
	}
	// 历史里留下 user + 带工具调用的 assistant + 一条工具结果：工具确实跑过一遍。
	_, hist, err := ag.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	var tools, users int
	for _, m := range hist {
		switch m.Role {
		case llm.RoleTool:
			tools++
		case llm.RoleUser:
			users++
		}
	}
	if tools != 1 {
		t.Fatalf("历史里应有一条工具结果，实际 %d 条（共 %d 条消息）", tools, len(hist))
	}
	// 用户消息只落一次：中途停下的一轮由发起方当作「已发生」，不会被重跑，
	// 会话里因此不会出现同一句话问两遍。
	if users != 1 {
		t.Fatalf("历史里应只有一条用户消息，实际 %d 条（共 %d 条消息）", users, len(hist))
	}
}

// 主模型先上报用量、再返回可重试错误：那一笔钱已经入账，换备用模型前必须重查预算，
// 否则上限形同虚设。这次被拦时也必须报「轮次已开始」——请求已经发出（钱已经花了），
// 调度器若把任务回滚，额度恢复后会把同一件事再做一遍。
func TestBudgetChecksBeforeFallback(t *testing.T) {
	primary := &chainProvider{events: []llm.StreamEvent{
		{Type: llm.EventUsage, Usage: &llm.Usage{PromptTokens: 10}},
		{Type: llm.EventError, Err: &llm.APIError{Status: 500, Body: "down"}},
	}}
	backup := &chainProvider{events: contentEvents("不该出现")}

	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	ag := New(primary, newTestManager(t), store, Options{Model: "primary-model", MaxTurns: 3})
	ag.SetModelTargets([]ModelTarget{
		{Provider: primary, Options: ModelOptions{Model: "primary-model", MaxTokens: 4096}, ProviderName: "p1"},
		{Provider: backup, Options: ModelOptions{Model: "backup-model", MaxTokens: 4096}, ProviderName: "p2"},
	})
	ledger, err := usage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ag.SetUsageLedger(ledger)
	ag.SetUsageBudget(usage.NewBudget(budgetSettings(10, usage.OverBlockAll), ledger, nil, time.Now()))

	_, err = ag.RunTurn(context.Background(), meta.ID, "跑一下")
	if !errors.Is(err, plugin.ErrBudgetExceeded) {
		t.Fatalf("主模型用尽额度后应拦住备用模型，得到 %v", err)
	}
	if !plugin.BudgetStoppedMidTurn(err) {
		t.Fatalf("请求已经发出，应报「轮次已开始」: %v", err)
	}
	if primary.calls != 1 {
		t.Fatalf("主模型应被调用一次，实际 %d 次", primary.calls)
	}
	if backup.calls != 0 {
		t.Fatalf("额度已用尽，不该调用备用模型，实际 %d 次", backup.calls)
	}
	if st := ag.BudgetStatus(time.Now()); st == nil || st.SpentTokens != 10 {
		t.Fatalf("主模型上报的用量应已入账: %+v", st)
	}
}

// spendScopePlugin 在裁决可见域时把额度花光：模拟「入口检查通过、用户消息落盘前
// 另一个会话刚好用尽额度」这个并发窗口。
type spendScopePlugin struct{ budget *usage.Budget }

func (*spendScopePlugin) Name() string                                  { return "spend_scope" }
func (*spendScopePlugin) Description() string                           { return "测试可见域裁决" }
func (*spendScopePlugin) Init(plugin.InitContext, map[string]any) error { return nil }
func (*spendScopePlugin) SystemPrompt() string                          { return "" }
func (*spendScopePlugin) Tools() []plugin.Tool                          { return nil }

func (p *spendScopePlugin) DecideScope(context.Context, plugin.TurnEvent) (plugin.Scope, error) {
	if p.budget != nil {
		_ = p.budget.Record(usage.Entry{TS: time.Now(), Kind: usage.KindTurn, Prompt: 100})
	}
	return plugin.Scope{}, nil
}

// 入口检查通过、用户消息落盘前额度被别的会话用光：这一轮仍然什么都没写。
// 报的是入口被拦（发起方可以安全回滚待办后重试），会话里没有用户消息、没有标题，
// 重试不会把同一句话写第二遍。
func TestBudgetBlocksBeforePersistingUserMessage(t *testing.T) {
	p := &mockProvider{} // 没有脚本：真被调用会越界 panic
	sp := &spendScopePlugin{}
	ag, id := budgetAgent(t, p, budgetSettings(10, usage.OverBlockAll), 0, sp)
	sp.budget = ag.usageBudget.Load()

	_, err := ag.RunTurn(context.Background(), id, "在吗")
	if !errors.Is(err, plugin.ErrBudgetExceeded) {
		t.Fatalf("落盘前应被预算拦下，得到 %v", err)
	}
	if plugin.BudgetStoppedMidTurn(err) {
		t.Fatalf("本轮还没拿到过响应，不应报「中途停下」: %v", err)
	}
	if p.calls != 0 {
		t.Fatalf("被拦下的轮次不该调用模型，实际 %d 次", p.calls)
	}
	meta, hist, err := ag.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 0 {
		t.Fatalf("被拦下的轮次不该留下任何消息: 条数=%d", len(hist))
	}
	if meta.Title != "" {
		t.Fatalf("被拦下的轮次不该写标题: %q", meta.Title)
	}
}

// 辅助调用同样受硬上限约束：它是最没人盯着的那类开销。
func TestBudgetBlocksComplete(t *testing.T) {
	p := &mockProvider{}
	ag, _ := budgetAgent(t, p, budgetSettings(1, usage.OverBlockAll), 1)

	_, err := ag.Complete(context.Background(), "提炼")
	if !errors.Is(err, plugin.ErrBudgetExceeded) {
		t.Fatalf("辅助调用应被预算拦下，得到 %v", err)
	}
	if p.calls != 0 {
		t.Fatalf("被拦下的辅助调用不该调用模型，实际 %d 次", p.calls)
	}
}

// 手动压缩是前台操作：block_all 下也拦，且不落盘任何替换。
func TestBudgetBlocksInteractiveCompact(t *testing.T) {
	p := &mockProvider{}
	ag, id := budgetAgent(t, p, budgetSettings(1, usage.OverBlockAll), 1)
	for _, m := range []session.StoredMessage{bulk(llm.RoleUser, "a"), bulk(llm.RoleAssistant, "a")} {
		if err := ag.store.Append(id, m); err != nil {
			t.Fatal(err)
		}
	}

	var errText string
	ag.Compact(plugin.WithInteractive(context.Background()), id, func(ev Event) {
		if ev.Type == EventError {
			errText = ev.Error
		}
	})
	if !strings.Contains(errText, "上限") {
		t.Fatalf("压缩应被拦并报出预算文案，得到 %q", errText)
	}
	if p.calls != 0 {
		t.Fatalf("被拦下的压缩不该调用模型，实际 %d 次", p.calls)
	}
	if _, hist, err := ag.store.Get(id); err != nil || len(hist) != 2 {
		t.Fatalf("被拦下的压缩不该改写历史: 条数=%d err=%v", len(hist), err)
	}
}

// 辅助调用的闸门按 ctx 的交互标记判定：失败转译带着交互标记进来，skip_background
// 下要放行；插件无人值守的辅助调用仍按后台拦——那是这条策略要停的那一半。
func TestBudgetSkipBackgroundCompleteByInteractive(t *testing.T) {
	// 交互：放行，真的发出一次请求
	p := &mockProvider{turns: []mockTurn{{content: "走神了一下"}}}
	ag, _ := budgetAgent(t, p, budgetSettings(10, usage.OverSkipBackground), 100)
	out, err := ag.Complete(plugin.WithInteractive(context.Background()), "把这次失败演成一句台词")
	if err != nil || out != "走神了一下" {
		t.Fatalf("交互 ctx 的辅助调用应放行: out=%q err=%v", out, err)
	}
	if p.calls != 1 {
		t.Fatalf("应真的调用一次模型，实际 %d 次", p.calls)
	}

	// 后台：拦下，一次请求都不发
	pb := &mockProvider{}
	agb, _ := budgetAgent(t, pb, budgetSettings(10, usage.OverSkipBackground), 100)
	if _, err := agb.Complete(context.Background(), "提炼"); !errors.Is(err, plugin.ErrBudgetExceeded) {
		t.Fatalf("后台辅助调用应被拦下，得到 %v", err)
	}
	if pb.calls != 0 {
		t.Fatalf("被拦下的辅助调用不该调用模型，实际 %d 次", pb.calls)
	}
}

// block_all 不看交互：两种 ctx 都拦。两个判据因此真正可区分（只在 block_all 下测，
// 「按交互判定」与「一律按后台拦」得到的结果一模一样）。
func TestBudgetBlockAllCompleteByInteractive(t *testing.T) {
	for _, interactive := range []bool{true, false} {
		p := &mockProvider{}
		ag, _ := budgetAgent(t, p, budgetSettings(1, usage.OverBlockAll), 1)
		ctx := context.Background()
		if interactive {
			ctx = plugin.WithInteractive(ctx)
		}
		if _, err := ag.Complete(ctx, "x"); !errors.Is(err, plugin.ErrBudgetExceeded) {
			t.Fatalf("block_all 下 interactive=%v 也应被拦，得到 %v", interactive, err)
		}
		if p.calls != 0 {
			t.Fatalf("block_all 下 interactive=%v 不该调用模型，实际 %d 次", interactive, p.calls)
		}
	}
}

// 手动压缩同理：skip_background 只停后台，`/compact` 是用户在等的一次操作。
// 后台压缩（插件发起、没有交互标记）才该被拦。
func TestBudgetSkipBackgroundCompactByInteractive(t *testing.T) {
	for _, interactive := range []bool{true, false} {
		p := &mockProvider{turns: []mockTurn{{content: "摘要"}}}
		ag, id := budgetAgent(t, p, budgetSettings(10, usage.OverSkipBackground), 100)
		for _, m := range []session.StoredMessage{bulk(llm.RoleUser, "a"), bulk(llm.RoleAssistant, "a")} {
			if err := ag.store.Append(id, m); err != nil {
				t.Fatal(err)
			}
		}
		ctx := context.Background()
		if interactive {
			ctx = plugin.WithInteractive(ctx)
		}
		var errText string
		ag.Compact(ctx, id, func(ev Event) {
			if ev.Type == EventError {
				errText = ev.Error
			}
		})
		if interactive {
			if errText != "" || p.calls != 1 {
				t.Fatalf("前台压缩应放行: err=%q calls=%d", errText, p.calls)
			}
			continue
		}
		if !strings.Contains(errText, "上限") {
			t.Fatalf("后台压缩应被拦并报出预算文案，得到 %q", errText)
		}
		if p.calls != 0 {
			t.Fatalf("被拦下的后台压缩不该调用模型，实际 %d 次", p.calls)
		}
		if _, hist, err := ag.store.Get(id); err != nil || len(hist) != 2 {
			t.Fatalf("被拦下的压缩不该改写历史: 条数=%d err=%v", len(hist), err)
		}
	}
}

// 一轮成功后预算立即看到这笔开销，不必等下一次扫盘。
func TestBudgetSeesSpendAfterTurn(t *testing.T) {
	p := &mockProvider{turns: []mockTurn{{
		content: "好",
		usage:   &llm.Usage{PromptTokens: 7, CompletionTokens: 2},
	}}}
	ag, id := budgetAgent(t, p, budgetSettings(1000, usage.OverSkipBackground), 0)

	if st := ag.BudgetStatus(time.Now()); st == nil || st.SpentTokens != 0 {
		t.Fatalf("开跑前不该有用量: %+v", st)
	}
	ag.Run(plugin.WithInteractive(context.Background()), id, "hi", func(Event) {})
	st := ag.BudgetStatus(time.Now())
	if st == nil || st.SpentTokens != 9 {
		t.Fatalf("一轮之后应看到 9 个 token: %+v", st)
	}
	if !st.Enabled || st.LimitTokens != 1000 || st.Exceeded {
		t.Fatalf("快照的其余字段不符: %+v", st)
	}
}

// 没有预算时不判定，状态也返回 nil（装不上账本的运行照常工作）。
func TestBudgetAbsentNeverBlocks(t *testing.T) {
	p := &mockProvider{turns: []mockTurn{{content: "好"}}}
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := store.Create()
	ag := New(p, newTestManager(t), store, Options{Model: "m1", MaxTurns: 3})

	var done bool
	ag.Run(context.Background(), meta.ID, "hi", func(ev Event) {
		if ev.Type == EventDone {
			done = true
		}
	})
	if !done {
		t.Fatal("没有预算时轮次应正常完成")
	}
	if st := ag.BudgetStatus(time.Now()); st != nil {
		t.Fatalf("没有预算时状态应为 nil，得到 %+v", st)
	}
}

// costPrice 是只测金额上限时的价格表：一百万输入 token 恰好 1.00 元。
func costPrice(_, _ string) (usage.Price, bool) {
	return usage.Price{Currency: "CNY", Input: 1, Output: 2}, true
}

func costOnlySettings(cost float64) usage.Settings {
	s := usage.DefaultSettings()
	s.MonthlyCost = cost
	s.Over = usage.OverBlockAll
	return s
}

// 只设金额上限时，被拦的文案必须报金额，不能报「已用 0 / 上限 0 tokens」——
// 那句话既不是原因，也看不出该去调哪一项。中英文各走一遍。
func TestBudgetExceededMessageCostOnly(t *testing.T) {
	ag, id := budgetAgentLookup(t, &mockProvider{}, costOnlySettings(0.5), 1_000_000, costPrice)
	_, err := ag.RunTurn(context.Background(), id, "在吗")
	if !errors.Is(err, usage.ErrBudgetExceeded) {
		t.Fatalf("应被金额上限拦下，得到 %v", err)
	}
	msg := err.Error()
	if strings.Contains(msg, "0 / 0 tokens") || !strings.Contains(msg, "CNY 1.00 / 0.50") {
		t.Fatalf("文案应报金额而不是 token: %q", msg)
	}

	en, enID := budgetAgentLookup(t, &mockProvider{}, costOnlySettings(0.5), 1_000_000, costPrice)
	en.opts.Lang = i18n.LangEN
	_, err = en.RunTurn(context.Background(), enID, "在吗")
	if err == nil {
		t.Fatal("英文环境同样应被拦下")
	}
	if !strings.Contains(err.Error(), "cost budget is reached") || !strings.Contains(err.Error(), "CNY 1.00 of 0.50") {
		t.Fatalf("英文文案应报金额: %q", err.Error())
	}
}

// 两个维度都设了：两个数字都报出来，用户才知道离哪个上限更近。
func TestBudgetExceededMessageBothDimensions(t *testing.T) {
	s := costOnlySettings(5)
	s.MonthlyTokens = 1_000_000
	ag, id := budgetAgentLookup(t, &mockProvider{}, s, 1_000_000, costPrice)
	_, err := ag.RunTurn(context.Background(), id, "在吗")
	if err == nil {
		t.Fatal("token 到限应被拦下")
	}
	msg := err.Error()
	for _, want := range []string{"1000000", "5.00"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("文案应同时报出 token 与金额，缺少 %q: %q", want, msg)
		}
	}
}
