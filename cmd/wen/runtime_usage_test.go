package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wen/internal/agent"
	"wen/internal/config"
	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/session"
	"wen/internal/usage"
)

// usageProvider 报一次带 usage 的响应，用来确认账本真的接到模型调用上。
type usageProvider struct{}

func (usageProvider) ChatStream(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, 3)
	ch <- llm.StreamEvent{Type: llm.EventContentDelta, Content: "好"}
	ch <- llm.StreamEvent{Type: llm.EventUsage, Usage: &llm.Usage{PromptTokens: 7, CompletionTokens: 2}}
	ch <- llm.StreamEvent{Type: llm.EventDone}
	close(ch)
	return ch, nil
}

// 评测把整场落盘搬进临时目录，用量账本也必须跟着走：否则一次回放就把
// 真实账本染上了机器造出来的数字。
func TestEvalUsageDirOverride(t *testing.T) {
	base := t.TempDir()
	cfg := &config.Config{BaseDir: base}
	if got := usageDirFor(cfg, runtimeOverrides{}); got != filepath.Join(base, "usage") {
		t.Errorf("默认账本目录 = %q，want <配置目录>/usage", got)
	}

	tmp := t.TempDir()
	dir := usageDirFor(cfg, runtimeOverrides{UsageDir: filepath.Join(tmp, "usage")})
	if dir != filepath.Join(tmp, "usage") || strings.HasPrefix(dir, base) {
		t.Fatalf("覆盖后的账本目录 = %q，不该落在配置目录里", dir)
	}

	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(usageProvider{}, plugin.NewManager(plugin.InitContext{}, ""), store, agent.Options{Model: "m"})
	ledger, writable := attachUsageLedger("zh", dir)
	if ledger == nil || !writable {
		t.Fatalf("正常目录上账本应接上且可写: ledger=%v writable=%v", ledger, writable)
	}
	ag.SetUsageLedger(ledger)
	ag.Run(context.Background(), meta.ID, "hi", func(agent.Event) {})

	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("账本目录里应有记录: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("轮次结束后账本目录仍是空的")
	}
	if entries, err := os.ReadDir(filepath.Join(base, "usage")); err == nil && len(entries) > 0 {
		t.Fatalf("账本不该写进配置目录: %v", entries)
	}
}

// 预算设置跟着账本落点走：评测把账本搬进临时目录时，usage.json 也读那边的
// （通常不存在，于是得到不设限的默认值），开发机上的真实限额不会拦住回放。
func TestEvalUsageSettingsPath(t *testing.T) {
	base := t.TempDir()
	cfg := &config.Config{BaseDir: base}
	if got := usageSettingsFor(cfg, runtimeOverrides{}); got != filepath.Join(base, "usage.json") {
		t.Errorf("默认设置路径 = %q，want <配置目录>/usage.json", got)
	}
	tmp := t.TempDir()
	got := usageSettingsFor(cfg, runtimeOverrides{UsageDir: filepath.Join(tmp, "usage")})
	if got != filepath.Join(tmp, "usage.json") {
		t.Fatalf("覆盖后的设置路径 = %q，want 账本目录旁边的 usage.json", got)
	}
}

// 设置文件不存在：按默认值（不设限）起，状态不标降级。
func TestNewUsageBudgetDefaultsToNoLimit(t *testing.T) {
	dir := t.TempDir()
	ledger, err := usage.Open(filepath.Join(dir, "usage"))
	if err != nil {
		t.Fatal(err)
	}
	b := newUsageBudget(ledger, true, filepath.Join(dir, "usage.json"), "zh", time.Now, nil)
	st := b.Status(time.Now())
	if st.Enabled || st.LimitTokens != 0 || st.Degraded {
		t.Fatalf("没有设置文件时应不设限且不标降级: %+v", st)
	}
}

// 设置读不出来：仍按默认值（不设限）跑，但状态标降级——否则用户以为自己设的
// 限额在生效，实际一分没限。
func TestNewUsageBudgetDegradesOnBrokenSettings(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "usage.json")
	if err := os.WriteFile(settingsPath, []byte("{ 不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger, err := usage.Open(filepath.Join(dir, "usage"))
	if err != nil {
		t.Fatal(err)
	}
	b := newUsageBudget(ledger, true, settingsPath, "zh", time.Now, nil)
	st := b.Status(time.Now())
	if !st.Degraded || st.Enabled {
		t.Fatalf("坏设置应标降级且不设限: %+v", st)
	}
}

// 账本不可用不等于预算消失：限额读得出来就照实报出，标记降级，并且不拦。
// 从前这里整场不挂预算，状态里那个字段直接不见了，用户看不出上限为什么没生效。
func TestNewUsageBudgetDegradesWithoutUsableLedger(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "usage.json")
	if err := usage.SaveSettings(settingsPath, usage.Settings{
		MonthlyTokens: 1000,
		Currency:      "CNY",
		WarnPercent:   80,
		Over:          usage.OverSkipBackground,
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name     string
		ledger   *usage.Ledger
		writable bool
	}{
		{"账本没打开", nil, false},
		{"探活失败", mustUsageLedger(t, dir), false},
	} {
		b := newUsageBudget(c.ledger, c.writable, settingsPath, "zh", time.Now, nil)
		st := b.Status(time.Now())
		if !st.Enabled || st.LimitTokens != 1000 || !st.Degraded {
			t.Fatalf("%s: 应报出限额并标降级: %+v", c.name, st)
		}
		if _, blocked := b.Blocked(time.Now(), false); blocked {
			t.Fatalf("%s: 降级时不该拦", c.name)
		}
	}
}

// 打开账本失败只记日志、继续启动：用量是旁路，不该阻断服务。
func TestAttachUsageLedgerToleratesFailure(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := store.Create()
	ag := agent.New(usageProvider{}, plugin.NewManager(plugin.InitContext{}, ""), store, agent.Options{Model: "m"})
	// 空目录路径是 Open 唯一会拒绝的情况
	if ledger, writable := attachUsageLedger("zh", ""); ledger != nil || writable {
		t.Fatalf("打不开账本时应返回 nil + 不可写，得到 ledger=%v writable=%v", ledger, writable)
	}

	done := false
	ag.Run(context.Background(), meta.ID, "hi", func(ev agent.Event) {
		if ev.Type == agent.EventDone {
			done = true
		}
	})
	if !done {
		t.Fatal("账本没接上时轮次仍应正常收尾")
	}

	// 顺带确认 usage 包这里的契约没变：空串是唯一会被 Open 拒绝的输入。
	if _, err := usage.Open(""); err == nil {
		t.Fatal("usage.Open(\"\") 应当报错")
	}
}

// 账本目录不可写（被普通文件占住）：Open 看不出来（它是惰性的），Probe 看得出来。
// 探活失败仍把账本交出去——Append 的失败只提示一次，目录修好就能恢复；但预算要
// 按降级起步，否则状态里看不出限额为什么没生效。
func TestAttachUsageLedgerProbeFailureKeepsLedgerAndDegrades(t *testing.T) {
	// 把「目录」换成一个普通文件：MkdirAll 必然失败。
	blocked := filepath.Join(t.TempDir(), "usage")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger, err := usage.Open(blocked)
	if err != nil {
		t.Fatalf("Open 应保持惰性，不因路径不可写而失败: %v", err)
	}
	if err := ledger.Probe(); err == nil {
		t.Fatal("路径被普通文件占住时 Probe 应当报错")
	}

	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(usageProvider{}, plugin.NewManager(plugin.InitContext{}, ""), store, agent.Options{Model: "m"})
	got, writable := attachUsageLedger("zh", blocked)
	if got == nil || writable {
		t.Fatalf("探活失败应返回账本 + 不可写，得到 ledger=%v writable=%v", got, writable)
	}
	ag.SetUsageLedger(got)

	// 预算照建：限额读得出来就报出来，同时标降级、不拦。
	settingsPath := filepath.Join(t.TempDir(), "usage.json")
	if err := usage.SaveSettings(settingsPath, usage.Settings{
		MonthlyTokens: 1000,
		Currency:      "CNY",
		WarnPercent:   80,
		Over:          usage.OverSkipBackground,
	}); err != nil {
		t.Fatal(err)
	}
	b := newUsageBudget(got, writable, settingsPath, "zh", time.Now, nil)
	st := b.Status(time.Now())
	if !st.Enabled || st.LimitTokens != 1000 || !st.Degraded {
		t.Fatalf("账本不可用时预算应报降级: %+v", st)
	}

	var done bool
	ag.Run(context.Background(), meta.ID, "hi", func(ev agent.Event) {
		if ev.Type == agent.EventDone {
			done = true
		}
	})
	if !done {
		t.Fatal("账本不可写时轮次仍应正常收尾")
	}

	// 一个分片都不该写出来：那个占位路径仍是原来那个普通文件。
	if info, err := os.Stat(blocked); err != nil || info.IsDir() || info.Size() != 1 {
		t.Fatalf("占位路径应原样保持为一个文件: info=%v err=%v", info, err)
	}
}

// 预算快照到插件状态的搬运必须逐字段对上：漏一个字段不会编译失败，只会让某一处
// 输出里那个量悄悄消失（第 5 步加金额字段时最容易踩）。
func TestBudgetInfoMapping(t *testing.T) {
	st := usage.BudgetStatus{
		Month:       "2026-11",
		Enabled:     true,
		Warn:        true,
		Exceeded:    true,
		LimitTokens: 2_000_000,
		SpentTokens: 1_900_000,
		LimitCost:   50,
		SpentCost:   47.5,
		Currency:    "CNY",
		CostReady:   true,
		CostInexact: true,
		// 未定价的 token 与金额同来同去：漏搬它只会静默少报一部分用量
		UnpricedTokens: 1_000,
		// 「上限其实没生效」的两档与坏分片数同样逐字段搬
		CostUnpriced: true,
		ForeignCost:  true,
		SkippedFiles: 3,
		// 记账失败也逐字段搬：漏搬只会让「本月统计不完整」这句话在某一处输出里消失
		WriteFailed: true,
		WarnPercent: 80,
		Over:        usage.OverBlockAll,
		Degraded:    true,
	}
	want := plugin.BudgetInfo{
		Enabled:        true,
		Warn:           true,
		Exceeded:       true,
		Degraded:       true,
		LimitTokens:    2_000_000,
		SpentTokens:    1_900_000,
		LimitCost:      50,
		SpentCost:      47.5,
		Currency:       "CNY",
		CostReady:      true,
		CostInexact:    true,
		UnpricedTokens: 1_000,
		CostUnpriced:   true,
		ForeignCost:    true,
		SkippedFiles:   3,
		WriteFailed:    true,
		WarnPercent:    80,
		Over:           usage.OverBlockAll,
		Month:          "2026-11",
	}
	if got := budgetInfo(st); got != want {
		t.Errorf("budgetInfo = %+v\nwant %+v", got, want)
	}
}

// 装配窗口里 Agent 尚未发布（ag 为 nil）：插件要的每一项能力都必须返回可识别的
// 「轮次未开始」，而不是普通错误。普通错误会被心跳/调度器当成这一拍失败，把一次性
// 任务记成失败；ErrTurnNotStarted 才要求它们回滚待办、稍后重试。它同时保证不会再
// 往下走——一次绕过预算的模型请求就是这样被挡住的。
func TestPluginAgentNotStartedBeforePublish(t *testing.T) {
	a, err := pluginAgent(nil, "模型尚未就绪")
	if a != nil {
		t.Fatalf("未发布时不该给出 Agent: %v", a)
	}
	if !errors.Is(err, plugin.ErrTurnNotStarted) {
		t.Fatalf("未就绪应返回 ErrTurnNotStarted，得到: %v", err)
	}
	if !strings.Contains(err.Error(), "模型尚未就绪") {
		t.Fatalf("错误应带上说明，日志里才看得出是启动窗口: %v", err)
	}

	// 发布之后原样交回同一个 Agent，调用方接着发请求。
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ready := agent.New(usageProvider{}, plugin.NewManager(plugin.InitContext{}, ""), store, agent.Options{Model: "m"})
	got, err := pluginAgent(ready, "模型尚未就绪")
	if err != nil || got != ready {
		t.Fatalf("就绪后应原样交回 Agent: got=%v err=%v", got, err)
	}
}

// mustUsageLedger 造一个能打开但无法探活的账本（路径已被一个普通文件占住）。
func mustUsageLedger(t *testing.T, dir string) *usage.Ledger {
	t.Helper()
	blocked := filepath.Join(dir, "usage-blocked")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := usage.Open(blocked)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Probe(); err == nil {
		t.Fatal("占位路径上 Probe 应当报错")
	}
	return l
}
