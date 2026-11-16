package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"wen/internal/clock"
	"wen/internal/plugin"
	"wen/internal/plugin/builtin/agenda"
	"wen/internal/plugin/builtin/growth"
	"wen/internal/plugin/builtin/heartbeat"
	"wen/internal/plugin/builtin/memory"
	"wen/internal/plugin/builtin/people"
	"wen/internal/plugin/builtin/roleplay"
	"wen/internal/plugin/builtin/sessionsearch"
	"wen/internal/session"
)

// 跨日心跳结束后，真实的 agenda 与 growth 应各自登记规划/反思，并在时钟推进前跑完。
func TestCrossDayHeartbeatTriggersPlanningAndReflection(t *testing.T) {
	clk := clock.NewManual(time.Date(2026, 3, 1, 9, 0, 0, 0, time.Local))
	ctx := clock.With(context.Background(), clk)

	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu     sync.Mutex
		inputs []string
	)
	m := plugin.NewManager(plugin.InitContext{
		Workdir:    t.TempDir(),
		Lang:       "zh",
		Clock:      clk,
		ManualTime: true,
		Sessions:   store,
		SessionDir: t.TempDir(),
		Complete:   func(context.Context, string) (string, error) { return "", nil },
		RunTurn: func(_ context.Context, _, input string) (string, error) {
			mu.Lock()
			inputs = append(inputs, input)
			mu.Unlock()
			return "", nil
		},
		Notice: func(context.Context, string, string) error { return nil },
		NewSession: func() (string, error) {
			meta, err := store.CreateAt(clk.Now())
			return meta.ID, err
		},
	}, "", plugin.WithStateDir(t.TempDir()))

	for _, p := range []plugin.Plugin{
		memory.New(), sessionsearch.New(), roleplay.New(), people.New(), growth.New(), agenda.New(nil), heartbeat.New(),
	} {
		if err := m.Register(p, plugin.PluginConfig{Enabled: true}); err != nil {
			t.Fatalf("注册 %s 失败: %v", p.Name(), err)
		}
	}
	t.Cleanup(m.StopAll)

	meta, err := store.CreateAt(clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	sid := meta.ID

	// 第一天：真人轮次让 growth 记下 Dirty，并触发当天的规划。
	m.NotifyTurnEnd(ctx, plugin.TurnEndEvent{SessionID: sid, Interactive: true, EndedAt: clk.Now()})
	if err := m.Settle(ctx); err != nil {
		t.Fatal(err)
	}

	// 第二天：先跑一次心跳，再通知轮次结束；规划与反思都应登记并在 Settle 里跑完。
	clk.Set(time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local))
	if err := m.RunBackground(ctx, "heartbeat", clk.Now()); err != nil {
		t.Fatal(err)
	}
	m.NotifyTurnEnd(ctx, plugin.TurnEndEvent{SessionID: sid, Origin: "heartbeat", EndedAt: clk.Now()})
	if err := m.Settle(ctx); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	all := strings.Join(inputs, "\n---\n")
	mu.Unlock()
	if !strings.Contains(all, "【规划今天】") {
		t.Fatalf("跨日心跳后没有触发日程规划：\n%s", all)
	}
	if !strings.Contains(all, "【成长·反思】") {
		t.Fatalf("跨日心跳后没有触发成长反思：\n%s", all)
	}
}

// 预算中止时可能还有未结算的待办；它们不占 WaitGroup，StopAll 必须正常返回。
func TestStopAllReturnsWithPendingSettlement(t *testing.T) {
	clk := clock.NewManual(time.Date(2026, 3, 1, 9, 0, 0, 0, time.Local))
	ctx := clock.With(context.Background(), clk)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := plugin.NewManager(plugin.InitContext{
		Lang:       "zh",
		Clock:      clk,
		ManualTime: true,
		Sessions:   store,
		SessionDir: t.TempDir(),
		Complete:   func(context.Context, string) (string, error) { return "", nil },
	}, "", plugin.WithStateDir(t.TempDir()))
	if err := m.Register(memory.New(), plugin.PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	meta, err := store.CreateAt(clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	m.NotifyTurnEnd(ctx, plugin.TurnEndEvent{SessionID: meta.ID, Interactive: true, EndedAt: clk.Now()})

	done := make(chan struct{})
	go func() {
		m.StopAll()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("有未结算待办时 StopAll 不应挂住")
	}
}
