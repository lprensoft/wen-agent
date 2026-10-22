package heartbeat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

func TestParseContexts(t *testing.T) {
	got := parseContexts(`说明文字，段头之前的内容忽略
[睡前]
轻声一点。
只说一句。

[闲聊]
随便聊聊。
[空的]

[默认]
不许占用保留名
[  ]
无名段丢弃`)
	if len(got) != 2 {
		t.Fatalf("got %d contexts: %v", len(got), got)
	}
	if got["睡前"] != "轻声一点。\n只说一句。" {
		t.Errorf("睡前 = %q", got["睡前"])
	}
	if got["闲聊"] != "随便聊聊。" {
		t.Errorf("闲聊 = %q", got["闲聊"])
	}
}

// 没配置过情境时用内置的「生活」范例；用户显式清空后不长回来。
func TestDefaultContextsShipLifeSample(t *testing.T) {
	p, _ := newInited(t, noTurn, nil)
	p.mu.Lock()
	life, ok := p.contexts["生活"]
	p.mu.Unlock()
	if !ok {
		t.Fatal("默认配置应内置「生活」情境")
	}
	for _, want := range []string{"过自己的日子", "不编造进展", "set_heartbeat_interval"} {
		if !strings.Contains(life, want) {
			t.Errorf("「生活」情境缺少 %q", want)
		}
	}

	// FieldText 的空串是合法取值：清空保存后就没有情境
	q, _ := newInited(t, noTurn, map[string]any{"context_prompts": ""})
	q.mu.Lock()
	n := len(q.contexts)
	q.mu.Unlock()
	if n != 0 {
		t.Errorf("显式清空后不该有情境: %d 个", n)
	}
}

func TestBeatPromptFollowsContext(t *testing.T) {
	p, _ := newInited(t, noTurn, map[string]any{
		"prompt":          "默认提示词",
		"context_prompts": "[睡前]\n轻声。",
	})
	p.mu.Lock()
	if got := p.beatPromptLocked(); got != "默认提示词" {
		t.Errorf("未切情境应用默认：%q", got)
	}
	p.curContext = "睡前"
	if got := p.beatPromptLocked(); got != "轻声。" {
		t.Errorf("切到睡前应用情境提示词：%q", got)
	}
	p.curContext = "不存在的"
	if got := p.beatPromptLocked(); got != "默认提示词" {
		t.Errorf("情境不存在应回落默认：%q", got)
	}
	p.mu.Unlock()
}

func TestSetIntervalSwitchesContext(t *testing.T) {
	p, _ := newInited(t, noTurn, map[string]any{"context_prompts": "[睡前]\n轻声。"})
	tool := p.Tools()[0]

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"minutes":60,"reason":"要睡了","context":"睡前"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "睡前") {
		t.Errorf("回显应提到情境切换：%q", out)
	}
	p.mu.Lock()
	cur := p.curContext
	p.mu.Unlock()
	if cur != "睡前" {
		t.Errorf("curContext = %q", cur)
	}

	// 传保留名切回默认
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"minutes":30,"reason":"醒了","context":"默认"}`)); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	cur = p.curContext
	p.mu.Unlock()
	if cur != "" {
		t.Errorf("切回默认后 curContext = %q", cur)
	}

	// 不存在的情境报错并列出可选
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"minutes":30,"reason":"x","context":"打游戏"}`)); err == nil || !strings.Contains(err.Error(), "睡前") {
		t.Errorf("err = %v，应列出可选情境", err)
	}
}

// pause_heartbeat 可顺带指定醒来后的情境：「睡前暂停到早上、醒来用早安开场」
// 本是一个动作，拆成两次调用模型容易只做一半。
func TestPauseSwitchesContext(t *testing.T) {
	p := newTestPluginWithContexts(t)
	out, err := p.Tools()[1].Execute(context.Background(),
		json.RawMessage(`{"minutes":480,"reason":"她说要睡了","context":"早安"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "早安") {
		t.Errorf("回显应提到情境切换：%q", out)
	}
	p.mu.Lock()
	cur, paused := p.curContext, p.pausedUntil
	p.mu.Unlock()
	if cur != "早安" {
		t.Errorf("curContext = %q", cur)
	}
	if paused.IsZero() {
		t.Error("暂停应同时生效")
	}

	// 情境名不对时整个调用失败，暂停不能只生效一半
	q := newTestPluginWithContexts(t)
	if _, err := q.Tools()[1].Execute(context.Background(),
		json.RawMessage(`{"minutes":480,"reason":"睡","context":"不存在的"}`)); err == nil || !strings.Contains(err.Error(), "早安") {
		t.Fatalf("err = %v，应报错并列出可选情境", err)
	}
	q.mu.Lock()
	paused = q.pausedUntil
	q.mu.Unlock()
	if !paused.IsZero() {
		t.Error("情境出错时不该暂停成功")
	}
}

func newTestPluginWithContexts(t *testing.T) *Plugin {
	t.Helper()
	p, _ := newInited(t, noTurn, map[string]any{"context_prompts": "[早安]\n说早安。\n[睡前]\n轻声。"})
	return p
}

// 两个工具的 schema 都只在配置了情境时声明 context 参数。
func TestContextParamDeclaredOnlyWithContexts(t *testing.T) {
	p := newTestPluginWithContexts(t)
	for i, name := range []string{"set_heartbeat_interval", "pause_heartbeat"} {
		if got := string(p.Tools()[i].Schema()); !strings.Contains(got, `"context"`) {
			t.Errorf("%s 配置了情境时应声明 context 参数:\n%s", name, got)
		}
	}
	q, _ := newInited(t, noTurn, map[string]any{"context_prompts": ""})
	for i, name := range []string{"set_heartbeat_interval", "pause_heartbeat"} {
		if got := string(q.Tools()[i].Schema()); strings.Contains(got, `"context"`) {
			t.Errorf("%s 未配置情境时不该声明 context 参数:\n%s", name, got)
		}
	}
}

func TestContextSurvivesRestart(t *testing.T) {
	stateDir := t.TempDir()
	cfg := map[string]any{"context_prompts": "[睡前]\n轻声。"}
	p, store := newInited(t, noTurn, nil)
	_ = p // 只借 helper 建店
	ictx := plugin.InitContext{
		StateDir: stateDir, Sessions: store, RunTurn: noTurn,
		NewSession: func() (string, error) { m, err := store.Create(); return m.ID, err },
	}

	p1 := New()
	if err := p1.Init(ictx, cfg); err != nil {
		t.Fatal(err)
	}
	p1.Stop()
	if _, err := p1.Tools()[0].Execute(context.Background(), json.RawMessage(`{"minutes":60,"reason":"睡","context":"睡前"}`)); err != nil {
		t.Fatal(err)
	}

	// 重启：情境应还在
	p2 := New()
	if err := p2.Init(ictx, cfg); err != nil {
		t.Fatal(err)
	}
	p2.Stop()
	p2.mu.Lock()
	cur := p2.curContext
	p2.mu.Unlock()
	if cur != "睡前" {
		t.Errorf("重启后 curContext = %q", cur)
	}

	// 配置里删掉该情境再重启：不保留悬空的情境名
	p3 := New()
	if err := p3.Init(ictx, nil); err != nil {
		t.Fatal(err)
	}
	p3.Stop()
	p3.mu.Lock()
	cur = p3.curContext
	p3.mu.Unlock()
	if cur != "" {
		t.Errorf("情境被删除后应回默认，curContext = %q", cur)
	}
}

func TestPauseTool(t *testing.T) {
	p, _ := newInited(t, noTurn, nil)
	tool := p.Tools()[1]

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"minutes":480,"reason":"她说要睡了"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "8 小时") {
		t.Errorf("out = %q", out)
	}
	p.mu.Lock()
	until, next := p.pausedUntil, p.nextBeatLocked()
	p.mu.Unlock()
	if time.Until(until) < 7*time.Hour {
		t.Errorf("pausedUntil = %v", until)
	}
	if !next.Equal(until) {
		t.Errorf("暂停应把下一次心跳压后到暂停结束：next=%v until=%v", next, until)
	}

	// 超上限收拢并回告
	out, err = tool.Execute(context.Background(), json.RawMessage(`{"minutes":9999,"reason":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "上限") {
		t.Errorf("收拢事实应回告模型：%q", out)
	}
}

func TestPauseRejectedWhenStatic(t *testing.T) {
	p, _ := newInited(t, noTurn, map[string]any{"dynamic": false})
	if _, err := p.Tools()[1].Execute(context.Background(), json.RawMessage(`{"minutes":60,"reason":"x"}`)); err == nil {
		t.Fatal("固定节奏下暂停应被拒绝")
	}
}

func TestInteractiveTurnClearsPause(t *testing.T) {
	p, _ := newInited(t, noTurn, nil)
	if _, err := p.Tools()[1].Execute(context.Background(), json.RawMessage(`{"minutes":480,"reason":"睡"}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // 下一轮在暂停之后才开始
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{Interactive: true, StartedAt: time.Now(), EndedAt: time.Now()})
	p.mu.Lock()
	until := p.pausedUntil
	p.mu.Unlock()
	if !until.IsZero() {
		t.Errorf("真人说话应清除暂停，pausedUntil = %v", until)
	}
	// 后台轮次不清
	if _, err := p.Tools()[1].Execute(context.Background(), json.RawMessage(`{"minutes":480,"reason":"睡"}`)); err != nil {
		t.Fatal(err)
	}
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{Origin: "scheduler", EndedAt: time.Now()})
	p.mu.Lock()
	until = p.pausedUntil
	p.mu.Unlock()
	if until.IsZero() {
		t.Error("后台轮次不该清除暂停")
	}
}

func TestPauseSurvivesRestartUnlessSpokenTo(t *testing.T) {
	stateDir := t.TempDir()
	_, store := newInited(t, noTurn, nil)
	ictx := plugin.InitContext{
		StateDir: stateDir, Sessions: store, RunTurn: noTurn,
		NewSession: func() (string, error) { m, err := store.Create(); return m.ID, err },
	}

	p1 := New()
	if err := p1.Init(ictx, nil); err != nil {
		t.Fatal(err)
	}
	p1.Stop()
	if _, err := p1.Tools()[1].Execute(context.Background(), json.RawMessage(`{"minutes":480,"reason":"睡"}`)); err != nil {
		t.Fatal(err)
	}

	// 无人说话时重启：暂停延续
	p2 := New()
	if err := p2.Init(ictx, nil); err != nil {
		t.Fatal(err)
	}
	p2.Stop()
	p2.mu.Lock()
	until := p2.pausedUntil
	p2.mu.Unlock()
	if until.IsZero() {
		t.Fatal("重启不该丢掉未到期的暂停")
	}

	// 暂停设下之后真人说过话再重启：暂停不复活
	sid, _ := ictx.NewSession()
	_ = store.SetLastActive(sid, time.Now().Add(time.Minute))
	p3 := New()
	if err := p3.Init(ictx, nil); err != nil {
		t.Fatal(err)
	}
	p3.Stop()
	p3.mu.Lock()
	until = p3.pausedUntil
	p3.mu.Unlock()
	if !until.IsZero() {
		t.Errorf("设下暂停之后真人说过话，重启不该让暂停复活：%v", until)
	}
}

func TestStatusLinesShowPauseAndContext(t *testing.T) {
	p, _ := newInited(t, noTurn, map[string]any{"context_prompts": "[睡前]\n轻声。"})
	if _, err := p.Tools()[0].Execute(context.Background(), json.RawMessage(`{"minutes":60,"reason":"睡","context":"睡前"}`)); err != nil {
		t.Fatal(err)
	}
	lines := p.StatusLines("")
	if len(lines) != 1 || !strings.Contains(lines[0], "睡前") {
		t.Errorf("状态行应带情境：%v", lines)
	}
	if _, err := p.Tools()[1].Execute(context.Background(), json.RawMessage(`{"minutes":480,"reason":"睡"}`)); err != nil {
		t.Fatal(err)
	}
	lines = p.StatusLines("")
	if len(lines) != 1 || !strings.Contains(lines[0], "暂停中") {
		t.Errorf("状态行应报暂停：%v", lines)
	}
}

func TestColdWakeThresholdScalesWithInterval(t *testing.T) {
	now := time.Now()
	// 间隔 30 分钟：门槛 12 小时
	if got := gapNote("", "p", now.Add(-13*time.Hour), now, 30*time.Minute); !strings.Contains(got, coldWakeNoteZH) {
		t.Error("超过 12 小时应附冷唤醒提示")
	}
	if got := gapNote("", "p", now.Add(-11*time.Hour), now, 30*time.Minute); strings.Contains(got, coldWakeNoteZH) {
		t.Error("不足 12 小时不该附")
	}
	// 间隔 8 小时：门槛抬到 24 小时，13 小时不算「久」
	if got := gapNote("", "p", now.Add(-13*time.Hour), now, 8*time.Hour); strings.Contains(got, coldWakeNoteZH) {
		t.Error("心跳以天计时门槛应随间隔抬高")
	}
}
