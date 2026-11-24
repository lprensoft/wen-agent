package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/session"
	"wen/internal/usage"
)

// ledgerEntries 读回账本目录里的全部记录。账本只承诺「一行一条、按天分片」，
// 测试按这个形状读原件，不去凑 Summary 的聚合口径。
func ledgerEntries(t *testing.T, dir string) []usage.Entry {
	t.Helper()
	files, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		t.Fatal(err)
	}
	var out []usage.Entry
	for _, de := range files {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".jsonl") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var e usage.Entry
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Fatalf("账本行不是合法 JSON: %v (%q)", err, line)
			}
			out = append(out, e)
		}
	}
	return out
}

// usageAgent 造一个带账本的 Agent：模型链只含 provider 一项，名字取 provider/model。
func usageAgent(t *testing.T, p llm.Provider) (*Agent, string, string) {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	ag := New(p, newTestManager(t), store, Options{Model: "m1", MaxTurns: 3})
	ag.SetModelTargets([]ModelTarget{{
		Provider:     p,
		Options:      ModelOptions{Model: "m1", MaxTokens: 4096},
		Label:        "p1 / m1",
		ProviderName: "p1",
	}})
	dir := t.TempDir()
	ledger, err := usage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ag.SetUsageLedger(ledger)
	return ag, meta.ID, dir
}

// usageChainAgent 造一个带账本的 Agent，模型链按传入顺序排列（第一项是主模型）。
func usageChainAgent(t *testing.T, targets ...ModelTarget) (*Agent, string, string) {
	t.Helper()
	if len(targets) == 0 {
		t.Fatal("模型链不能为空")
	}
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	ag := New(targets[0].Provider, newTestManager(t), store, Options{Model: targets[0].Options.Model, MaxTurns: 3})
	ag.SetModelTargets(targets)
	dir := t.TempDir()
	ledger, err := usage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ag.SetUsageLedger(ledger)
	return ag, meta.ID, dir
}

func TestUsageRecordsOneTurnPerRequest(t *testing.T) {
	p := &mockProvider{turns: []mockTurn{{
		content: "你好",
		usage:   &llm.Usage{PromptTokens: 11, CompletionTokens: 4, CachedTokens: 2, CacheWriteTokens: 1},
	}}}
	ag, id, dir := usageAgent(t, p)

	ag.Run(context.Background(), id, "在吗", func(Event) {})

	got := ledgerEntries(t, dir)
	if len(got) != 1 {
		t.Fatalf("应记一条，得到 %d 条: %+v", len(got), got)
	}
	e := got[0]
	if e.Kind != usage.KindTurn || e.Provider != "p1" || e.Model != "m1" {
		t.Errorf("Kind/provider/model = %q/%q/%q，want turn/p1/m1", e.Kind, e.Provider, e.Model)
	}
	if e.Origin != "" {
		t.Errorf("前台轮次的来源应为空，得到 %q", e.Origin)
	}
	if e.Prompt != 11 || e.Completion != 4 || e.Cached != 2 || e.CacheWrite != 1 {
		t.Errorf("四个计数不符: %+v", e)
	}
	if e.TS.IsZero() {
		t.Error("应带时间戳")
	}
}

// 一轮里调两次模型（先工具、后收尾）就该有两条记录：账本记的是每次请求，
// 不是每轮只留最后一次。
func TestUsageRecordsEveryRequestInATurn(t *testing.T) {
	p := &mockProvider{turns: []mockTurn{
		{
			toolCalls: []llm.ToolCall{{ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"text":"hi"}`)}},
			usage:     &llm.Usage{PromptTokens: 100, CompletionTokens: 10},
		},
		{
			content: "好了",
			usage:   &llm.Usage{PromptTokens: 200, CompletionTokens: 20},
		},
	}}
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := store.Create()
	ag := New(p, newTestManager(t, echoPlugin{}), store, Options{Model: "m1", MaxTurns: 5})
	ag.SetModelTargets([]ModelTarget{{
		Provider: p, Options: ModelOptions{Model: "m1", MaxTokens: 4096}, ProviderName: "p1",
	}})
	dir := t.TempDir()
	ledger, _ := usage.Open(dir)
	ag.SetUsageLedger(ledger)

	ag.Run(context.Background(), meta.ID, "调用一下 echo", func(Event) {})

	got := ledgerEntries(t, dir)
	if len(got) != 2 {
		t.Fatalf("两次请求应记两条，得到 %d 条: %+v", len(got), got)
	}
	if got[0].Prompt != 100 || got[1].Prompt != 200 {
		t.Errorf("两条的 prompt 计数应各自对应: %+v", got)
	}
	for _, e := range got {
		if e.Kind != usage.KindTurn || e.Provider != "p1" || e.Model != "m1" {
			t.Errorf("每条都应是 turn / p1 / m1: %+v", e)
		}
	}
}

func TestUsageCompleteOrigin(t *testing.T) {
	p := &mockProvider{turns: []mockTurn{
		{content: "甲", usage: &llm.Usage{PromptTokens: 5, CompletionTokens: 1}},
		{content: "乙", usage: &llm.Usage{PromptTokens: 6, CompletionTokens: 2}},
	}}
	ag, _, dir := usageAgent(t, p)

	if _, err := ag.Complete(plugin.WithTurnOrigin(context.Background(), "memory"), "提炼"); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Complete(context.Background(), "内部整理"); err != nil {
		t.Fatal(err)
	}

	got := ledgerEntries(t, dir)
	if len(got) != 2 {
		t.Fatalf("两次辅助调用应记两条，得到 %d 条: %+v", len(got), got)
	}
	if got[0].Kind != usage.KindHelper || got[0].Origin != "memory" {
		t.Errorf("带来源的辅助调用 = %q/%q，want helper/memory", got[0].Kind, got[0].Origin)
	}
	if got[0].Provider != "p1" || got[0].Model != "m1" {
		t.Errorf("辅助调用的 provider/model 不符: %+v", got[0])
	}
	// 核心自己的辅助调用不能记成网页前台，否则账本永远查不出后台开销。
	if got[1].Kind != usage.KindHelper || got[1].Origin != "internal" {
		t.Errorf("无来源的辅助调用 = %q/%q，want helper/internal", got[1].Kind, got[1].Origin)
	}
}

func TestUsageCompactRecorded(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := store.Create()
	for _, m := range []session.StoredMessage{bulk(llm.RoleUser, "a"), bulk(llm.RoleAssistant, "a")} {
		if err := store.Append(meta.ID, m); err != nil {
			t.Fatal(err)
		}
	}
	p := &mockProvider{turns: []mockTurn{{
		content: "摘要",
		usage:   &llm.Usage{PromptTokens: 900, CompletionTokens: 90, CachedTokens: 800},
	}}}
	ag := New(p, newTestManager(t), store, Options{Model: "m1"})
	ag.SetModelTargets([]ModelTarget{{
		Provider: p, Options: ModelOptions{Model: "m1", MaxTokens: 4096}, ProviderName: "p1",
	}})
	dir := t.TempDir()
	ledger, _ := usage.Open(dir)
	ag.SetUsageLedger(ledger)

	ag.Compact(context.Background(), meta.ID, func(Event) {})

	got := ledgerEntries(t, dir)
	if len(got) != 1 {
		t.Fatalf("压缩应记一条，得到 %d 条: %+v", len(got), got)
	}
	e := got[0]
	if e.Kind != usage.KindCompact || e.Provider != "p1" || e.Model != "m1" {
		t.Errorf("Kind/provider/model = %q/%q/%q，want compact/p1/m1", e.Kind, e.Provider, e.Model)
	}
	if e.Prompt != 900 || e.Completion != 90 || e.Cached != 800 {
		t.Errorf("数值应取自摘要调用的 usage: %+v", e)
	}
}

func TestUsageRecordsFallbackTarget(t *testing.T) {
	primary := &chainProvider{syncErr: &llm.APIError{Status: 500, Body: "down"}}
	backup := &chainProvider{events: append(contentEvents("备用回复"),
		llm.StreamEvent{Type: llm.EventUsage, Usage: &llm.Usage{PromptTokens: 30, CompletionTokens: 3}},
		llm.StreamEvent{Type: llm.EventDone},
	)}
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := store.Create()
	ag := New(primary, newTestManager(t), store, Options{Model: "primary-model", MaxTurns: 3})
	ag.SetModelTargets([]ModelTarget{
		{Provider: primary, Options: ModelOptions{Model: "primary-model", MaxTokens: 4096}, ProviderName: "primary"},
		{Provider: backup, Options: ModelOptions{Model: "backup-model", MaxTokens: 4096}, ProviderName: "backup"},
	})
	dir := t.TempDir()
	ledger, _ := usage.Open(dir)
	ag.SetUsageLedger(ledger)

	ag.Run(context.Background(), meta.ID, "hi", func(Event) {})

	got := ledgerEntries(t, dir)
	if len(got) != 1 {
		t.Fatalf("应记一条（备用那次），得到 %d 条: %+v", len(got), got)
	}
	// 钱是备用模型花的，账不能记到主模型头上。
	if got[0].Provider != "backup" || got[0].Model != "backup-model" {
		t.Errorf("记录到了错误的模型: %+v", got[0])
	}
}

// 失败前已经拿到 usage 的那笔钱同样要入账。
func TestUsageRecordsFailedRequest(t *testing.T) {
	p := &chainProvider{events: []llm.StreamEvent{
		{Type: llm.EventUsage, Usage: &llm.Usage{PromptTokens: 40, CompletionTokens: 0}},
		{Type: llm.EventError, Err: &llm.APIError{Status: 500, Body: "down"}},
	}}
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := store.Create()
	ag := New(p, newTestManager(t), store, Options{Model: "m1", MaxTurns: 3})
	ag.SetModelTargets([]ModelTarget{{
		Provider: p, Options: ModelOptions{Model: "m1", MaxTokens: 4096}, ProviderName: "p1",
	}})
	dir := t.TempDir()
	ledger, _ := usage.Open(dir)
	ag.SetUsageLedger(ledger)

	ag.Run(context.Background(), meta.ID, "hi", func(Event) {})

	got := ledgerEntries(t, dir)
	if len(got) != 1 {
		t.Fatalf("失败请求的用量也应记一条，得到 %d 条: %+v", len(got), got)
	}
	if got[0].Prompt != 40 || got[0].Provider != "p1" || got[0].Model != "m1" {
		t.Errorf("失败记录的数值或模型不符: %+v", got[0])
	}
}

func TestUsageAbsentProviderWritesNothing(t *testing.T) {
	p := &mockProvider{turns: []mockTurn{{content: "没有用量"}}}
	ag, id, dir := usageAgent(t, p)

	ag.Run(context.Background(), id, "hi", func(Event) {})

	if got := ledgerEntries(t, dir); len(got) != 0 {
		t.Fatalf("提供商不报 usage 时不该产生记录: %+v", got)
	}
}

func TestUsageNilLedgerIsNoop(t *testing.T) {
	p := &mockProvider{turns: []mockTurn{{
		content: "ok",
		usage:   &llm.Usage{PromptTokens: 1, CompletionTokens: 1},
	}}}
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	ag := New(p, newTestManager(t), store, Options{Model: "m1", MaxTurns: 3})

	var done, failed bool
	ag.Run(context.Background(), meta.ID, "hi", func(ev Event) {
		switch ev.Type {
		case EventDone:
			done = true
		case EventError:
			failed = true
		}
	})
	if !done || failed {
		t.Fatalf("没有账本时轮次行为应保持不变: done=%v failed=%v", done, failed)
	}
}

// 账本写不进去只是少一行账，绝不能让轮次失败。
func TestUsageWriteFailureDoesNotFailTurn(t *testing.T) {
	p := &mockProvider{turns: []mockTurn{{
		content: "ok",
		usage:   &llm.Usage{PromptTokens: 1, CompletionTokens: 1},
	}}}
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	ag := New(p, newTestManager(t), store, Options{Model: "m1", MaxTurns: 3})

	// 把「目录」换成一个普通文件：MkdirAll 必然失败。
	blocked := filepath.Join(t.TempDir(), "usage")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger, err := usage.Open(blocked)
	if err != nil {
		t.Fatal(err)
	}
	ag.SetUsageLedger(ledger)

	var done, failed bool
	ag.Run(context.Background(), meta.ID, "hi", func(ev Event) {
		switch ev.Type {
		case EventDone:
			done = true
		case EventError:
			failed = true
		}
	})
	if !done || failed {
		t.Fatalf("账本写入失败不该让轮次失败: done=%v failed=%v", done, failed)
	}
}

// 备用链上已经报过 usage 的那次失败尝试也要入账：它真的花了钱，只是没拿到结果。
// 成功那笔仍由 run 记，两笔不能互相顶掉，也不能重复。
func TestUsageRecordsFailedAttemptBeforeFallback(t *testing.T) {
	primary := &chainProvider{events: []llm.StreamEvent{
		{Type: llm.EventUsage, Usage: &llm.Usage{PromptTokens: 40}},
		{Type: llm.EventError, Err: &llm.APIError{Status: 500, Body: "down"}},
	}}
	backup := &chainProvider{events: append(contentEvents("备用回复"),
		llm.StreamEvent{Type: llm.EventUsage, Usage: &llm.Usage{PromptTokens: 30, CompletionTokens: 3}},
		llm.StreamEvent{Type: llm.EventDone},
	)}
	ag, id, dir := usageChainAgent(t,
		ModelTarget{Provider: primary, Options: ModelOptions{Model: "primary-model", MaxTokens: 4096}, ProviderName: "primary"},
		ModelTarget{Provider: backup, Options: ModelOptions{Model: "backup-model", MaxTokens: 4096}, ProviderName: "backup"},
	)

	ag.Run(context.Background(), id, "hi", func(Event) {})

	got := ledgerEntries(t, dir)
	if len(got) != 2 {
		t.Fatalf("失败一次 + 成功一次应记两条，得到 %d 条: %+v", len(got), got)
	}
	if got[0].Provider != "primary" || got[0].Model != "primary-model" || got[0].Prompt != 40 {
		t.Errorf("失败的那笔应记在主模型上: %+v", got[0])
	}
	if got[1].Provider != "backup" || got[1].Model != "backup-model" || got[1].Prompt != 30 {
		t.Errorf("成功的那笔应记在备用模型上: %+v", got[1])
	}
}

// 辅助调用先报 usage、后报错时同样入账：钱已经花掉了，只是没拿到结果。
func TestUsageRecordsFailedComplete(t *testing.T) {
	p := &chainProvider{events: []llm.StreamEvent{
		{Type: llm.EventUsage, Usage: &llm.Usage{PromptTokens: 12, CompletionTokens: 0}},
		{Type: llm.EventError, Err: &llm.APIError{Status: 500, Body: "down"}},
	}}
	ag, _, dir := usageAgent(t, p)

	if _, err := ag.Complete(context.Background(), "提炼"); err == nil {
		t.Fatal("提供商报错时应把错误交回调用方")
	}

	got := ledgerEntries(t, dir)
	if len(got) != 1 {
		t.Fatalf("失败的辅助调用应记一条，得到 %d 条: %+v", len(got), got)
	}
	if got[0].Kind != usage.KindHelper || got[0].Provider != "p1" || got[0].Model != "m1" || got[0].Prompt != 12 {
		t.Errorf("记录内容不符: %+v", got[0])
	}
}

// 压缩失败但已经报过 usage：同样入账，类型是 compact。
func TestUsageRecordsFailedCompact(t *testing.T) {
	p := &chainProvider{events: []llm.StreamEvent{
		{Type: llm.EventUsage, Usage: &llm.Usage{PromptTokens: 900, CompletionTokens: 0, CachedTokens: 800}},
		{Type: llm.EventError, Err: &llm.APIError{Status: 500, Body: "down"}},
	}}
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := store.Create()
	for _, m := range []session.StoredMessage{bulk(llm.RoleUser, "a"), bulk(llm.RoleAssistant, "a")} {
		if err := store.Append(meta.ID, m); err != nil {
			t.Fatal(err)
		}
	}
	ag := New(p, newTestManager(t), store, Options{Model: "m1"})
	ag.SetModelTargets([]ModelTarget{{
		Provider: p, Options: ModelOptions{Model: "m1", MaxTokens: 4096}, ProviderName: "p1",
	}})
	dir := t.TempDir()
	ledger, _ := usage.Open(dir)
	ag.SetUsageLedger(ledger)

	ag.Compact(context.Background(), meta.ID, func(Event) {})

	got := ledgerEntries(t, dir)
	if len(got) != 1 {
		t.Fatalf("失败的压缩应记一条，得到 %d 条: %+v", len(got), got)
	}
	if got[0].Kind != usage.KindCompact || got[0].Provider != "p1" || got[0].Model != "m1" {
		t.Errorf("Kind/provider/model 不符: %+v", got[0])
	}
	if got[0].Prompt != 900 || got[0].Cached != 800 {
		t.Errorf("计数应取自失败前收到的 usage: %+v", got[0])
	}
}

// cancelProvider 先发一条 usage，然后阻塞到 ctx 取消才关流：模拟用户中途取消。
type cancelProvider struct {
	partial string
	cancel  context.CancelFunc
}

func (p *cancelProvider) ChatStream(ctx context.Context, _ llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	go func() {
		defer close(ch)
		for _, r := range p.partial {
			ch <- llm.StreamEvent{Type: llm.EventContentDelta, Content: string(r)}
		}
		ch <- llm.StreamEvent{Type: llm.EventUsage, Usage: &llm.Usage{PromptTokens: 40, CompletionTokens: 0}}
		// 用量已经发出，用户恰好在这一刻取消：流不再有后续事件，直接关掉。
		p.cancel()
		<-ctx.Done()
	}()
	return ch, nil
}

// 用户取消时已经收到的 usage 不能丢：那次请求的钱同样花掉了。
func TestUsageRecordsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &cancelProvider{partial: "半截", cancel: cancel}
	ag, id, dir := usageAgent(t, p)

	var failed bool
	ag.Run(ctx, id, "hi", func(ev Event) {
		if ev.Type == EventError {
			failed = true
		}
	})
	if !failed {
		t.Fatal("取消后的轮次应照常报错收尾")
	}

	got := ledgerEntries(t, dir)
	if len(got) != 1 {
		t.Fatalf("取消前收到的用量应记一条，得到 %d 条: %+v", len(got), got)
	}
	if got[0].Prompt != 40 || got[0].Provider != "p1" {
		t.Errorf("记录内容不符: %+v", got[0])
	}
}
