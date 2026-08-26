package plugin

import (
	"context"
	"testing"
	"time"
)

// reentrantReporter 在 StatusLines 里反过来调用 Manager，模拟插件「问一句状态」。
type reentrantReporter struct {
	fakePlugin
	m       *Manager
	entered chan struct{}
	release chan struct{}
}

func (p *reentrantReporter) StatusLines(string) []string {
	close(p.entered)
	<-p.release
	return p.m.SystemPrompts() // 反向调用：回调若在读锁内进行，这里会永久卡住
}

// 插件回调期间反向调用 Manager 必须能返回。
//
// Go 的 RWMutex 在有写者排队时会挡住后续所有 RLock，所以只要回调是在读锁内进行的，
// 插件里一次反向调用就足以让整个服务停住——而 OnCompact 里还有一次真实的模型往返，
// 那把锁会一直握到提炼结束。
func TestPluginCallbacksRunOutsideLock(t *testing.T) {
	m := NewManager(InitContext{}, "")
	if err := m.Register(&fakePlugin{name: "other", prompt: "提示"}, PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	rp := &reentrantReporter{
		fakePlugin: fakePlugin{name: "reporter"},
		m:          m,
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	if err := m.Register(rp, PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}

	done := make(chan []string, 1)
	go func() { done <- m.StatusLines("") }()

	<-rp.entered // 已经进到插件回调里

	queued := make(chan struct{})
	go func() {
		close(queued)
		_ = m.SetEnabled("other", false) // 写者排队
	}()
	<-queued
	time.Sleep(50 * time.Millisecond) // 给写者真正排上队的时间
	close(rp.release)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("插件回调里反向调用 Manager 卡住了：回调必须在锁外进行")
	}
}

// panicPlugin 的每个回调都 panic。armed 之前老实返回，好让它先注册进去——
// 注册期的 Tools() 在写锁内调用，不在本测试关心的广播路径上。
type panicPlugin struct {
	fakePlugin
	armed bool
}

func (p *panicPlugin) SystemPrompt() string {
	if !p.armed {
		return ""
	}
	panic("提示词炸了")
}

func (p *panicPlugin) Tools() []Tool {
	if !p.armed {
		return nil
	}
	panic("工具列表炸了")
}

func (p *panicPlugin) StatusLines(string) []string { panic("状态行炸了") }
func (p *panicPlugin) OnCompact(context.Context, CompactEvent) (string, error) {
	panic("压缩钩子炸了")
}
func (p *panicPlugin) TurnPrompt(context.Context, TurnEvent) (string, error) {
	panic("本轮提示词炸了")
}
func (p *panicPlugin) DecideScope(context.Context, TurnEvent) (Scope, error) {
	panic("可见域裁决炸了")
}
func (p *panicPlugin) OnTurnEnd(context.Context, TurnEndEvent) { panic("轮次结束钩子炸了") }

// 任何一处回调 panic 都只记日志，不连累调用方，也不影响其余插件的结果。
func TestPluginPanicDoesNotPropagate(t *testing.T) {
	m := NewManager(InitContext{}, "")
	bad := &panicPlugin{fakePlugin: fakePlugin{name: "bad"}}
	good := &fakePlugin{name: "good", prompt: "好提示", tools: []Tool{fakeTool{"tool_good"}}}
	for _, p := range []Plugin{bad, good} {
		if err := m.Register(p, PluginConfig{Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	bad.armed = true

	ctx := context.Background()
	if got := m.SystemPrompts(); len(got) != 1 || got[0] != "好提示" {
		t.Errorf("SystemPrompts = %v，坏插件不该带走好插件的片段", got)
	}
	if got := m.EnabledTools(); len(got) != 1 || got[0].Name() != "tool_good" {
		t.Errorf("EnabledTools = %v", got)
	}
	if _, ok := m.FindTool("tool_good"); !ok {
		t.Error("FindTool 没找到好插件的工具")
	}
	if got := m.StatusLines(""); len(got) != 0 {
		t.Errorf("StatusLines = %v", got)
	}
	if got := m.NotifyCompact(ctx, CompactEvent{}); len(got) != 0 {
		t.Errorf("NotifyCompact = %v", got)
	}
	if got := m.TurnPrompts(ctx, TurnEvent{}); len(got) != 0 {
		t.Errorf("TurnPrompts = %v", got)
	}
	if got := m.DecideScope(ctx, TurnEvent{}); !got.IsZero() {
		t.Errorf("DecideScope = %+v", got)
	}
	m.NotifyTurnEnd(ctx, TurnEndEvent{})
	if got := m.List(); len(got) != 2 || got[1].Description != "测试插件 good" {
		t.Errorf("List 不该被坏插件带崩: %+v", got)
	}
}
