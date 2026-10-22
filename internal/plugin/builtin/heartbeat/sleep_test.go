package heartbeat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
	"wen/internal/session"
)

func pause(t *testing.T, p *Plugin, args string) {
	t.Helper()
	if _, err := p.Tools()[1].Execute(context.Background(), json.RawMessage(args)); err != nil {
		t.Fatal(err)
	}
}

// 对方说晚安、角色在同一轮里暂停到早上：那是这一轮的结论，轮次收尾不该把它清掉。
// 此前不分先后一律清掉，于是真人对话里设下的暂停全都活不过当轮。
func TestPauseSetInTurnSurvivesTurnEnd(t *testing.T) {
	p, _ := newInited(t, noTurn, nil)
	started := time.Now()
	time.Sleep(time.Millisecond) // 让暂停时刻严格晚于轮次开始
	pause(t, p, `{"minutes":480,"reason":"她说要睡了","asleep":true}`)
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{Interactive: true, StartedAt: started, EndedAt: time.Now()})
	p.mu.Lock()
	until, asleep := p.pausedUntil, p.asleep
	p.mu.Unlock()
	if until.IsZero() || !asleep {
		t.Fatalf("当轮设下的暂停不该被当轮清掉：until=%v asleep=%v", until, asleep)
	}

	// 下一轮真人来消息：暂停设于这一轮开始之前，人被叫醒
	time.Sleep(2 * time.Millisecond)
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{Interactive: true, StartedAt: time.Now(), EndedAt: time.Now()})
	p.mu.Lock()
	until, asleep = p.pausedUntil, p.asleep
	p.mu.Unlock()
	if !until.IsZero() || asleep {
		t.Errorf("被叫醒后暂停与睡眠都应清掉：until=%v asleep=%v", until, asleep)
	}
}

func TestTurnPromptKnowsAboutSleep(t *testing.T) {
	p, _ := newInited(t, noTurn, nil)
	pause(t, p, `{"minutes":480,"reason":"睡","asleep":true}`)

	woken, _ := p.TurnPrompt(plugin.WithInteractive(context.Background()), plugin.TurnEvent{})
	if !strings.Contains(woken, "叫醒") {
		t.Errorf("睡着时来了消息应当知道是被叫醒的：%q", woken)
	}
	bg, _ := p.TurnPrompt(context.Background(), plugin.TurnEvent{})
	if !strings.Contains(bg, "正在睡觉") || strings.Contains(bg, "叫醒") {
		t.Errorf("后台轮次只说还睡着：%q", bg)
	}

	// 只是对方不在：仍按暂停说
	pause(t, p, `{"minutes":120,"reason":"她出门了"}`)
	if got, _ := p.TurnPrompt(plugin.WithInteractive(context.Background()), plugin.TurnEvent{}); !strings.Contains(got, "已暂停") {
		t.Errorf("没睡就不该说成睡觉：%q", got)
	}
	if lines := p.StatusLines(""); len(lines) != 1 || !strings.Contains(lines[0], "暂停中") {
		t.Errorf("状态行：%v", lines)
	}
}

// sleepyPlugin 起一个心跳插件，装上假的压缩与状态通道，并备好一个最近活跃的会话。
func sleepyPlugin(t *testing.T, load float64, compactErr error) (*Plugin, *session.Store, *[]string) {
	t.Helper()
	p, store := newInited(t, noTurn, nil)
	meta, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetLastActive(meta.ID, time.Now().Add(-time.Hour))
	var calls []string
	p.mu.Lock()
	p.compact = func(_ context.Context, sid string) error { calls = append(calls, sid); return compactErr }
	p.status = func(string) (plugin.StatusInfo, error) {
		return plugin.StatusInfo{HasSession: true, CompactAt: 1000, EstTokens: int(load * 1000), MeasuredTokens: -1}, nil
	}
	p.mu.Unlock()
	return p, store, &calls
}

// fallAsleep 设下一段睡眠，并把入睡时刻拨回过去，让「睡熟了」的等待已经过去。
func fallAsleep(t *testing.T, p *Plugin, ago time.Duration) {
	t.Helper()
	pause(t, p, `{"minutes":480,"reason":"睡","asleep":true}`)
	p.mu.Lock()
	p.pausedAt = time.Now().Add(-ago)
	p.mu.Unlock()
}

func TestSleepCompactsOnceWhenLoaded(t *testing.T) {
	p, _, calls := sleepyPlugin(t, 0.6, nil)
	fallAsleep(t, p, sleepSettle+time.Minute)

	p.mu.Lock()
	due := p.sleepCompactDueLocked()
	p.mu.Unlock()
	if due.IsZero() || due.After(time.Now()) {
		t.Fatalf("睡熟之后应当到了整理的时候：due=%v", due)
	}
	p.sleepCompact(context.Background())
	if len(*calls) != 1 {
		t.Fatalf("应当整理一次：%v", *calls)
	}
	p.mu.Lock()
	due = p.sleepCompactDueLocked()
	p.mu.Unlock()
	if !due.IsZero() {
		t.Error("一觉只整理一次")
	}
}

func TestSleepCompactWaitsAndSkips(t *testing.T) {
	// 刚睡下：还没到整理的时候
	p, _, _ := sleepyPlugin(t, 0.6, nil)
	fallAsleep(t, p, time.Minute)
	p.mu.Lock()
	due := p.sleepCompactDueLocked()
	p.mu.Unlock()
	if !due.After(time.Now()) {
		t.Errorf("刚睡下不该立刻整理：due=%v", due)
	}

	// 用量没到门槛：不压，但这一觉也不再试
	p, _, calls := sleepyPlugin(t, 0.1, nil)
	fallAsleep(t, p, sleepSettle+time.Minute)
	p.sleepCompact(context.Background())
	p.mu.Lock()
	done := p.sleepCompacted
	p.mu.Unlock()
	if len(*calls) != 0 || !done {
		t.Errorf("上下文很短时不该整理：calls=%v done=%v", *calls, done)
	}

	// 只是对方不在，自己没睡：不整理
	p, _, _ = sleepyPlugin(t, 0.6, nil)
	pause(t, p, `{"minutes":480,"reason":"她出门了"}`)
	p.mu.Lock()
	p.pausedAt = time.Now().Add(-time.Hour)
	due = p.sleepCompactDueLocked()
	p.mu.Unlock()
	if !due.IsZero() {
		t.Error("没睡就不整理")
	}

	// 门槛填 0：关掉
	p, _, _ = sleepyPlugin(t, 0.6, nil)
	fallAsleep(t, p, time.Hour)
	p.mu.Lock()
	p.compactPct = 0
	due = p.sleepCompactDueLocked()
	p.mu.Unlock()
	if !due.IsZero() {
		t.Error("门槛为 0 时不整理")
	}
}

func TestSleepCompactRetriesWhenBusy(t *testing.T) {
	p, _, calls := sleepyPlugin(t, 0.6, plugin.ErrSessionBusy)
	fallAsleep(t, p, sleepSettle+time.Minute)
	p.sleepCompact(context.Background())
	p.mu.Lock()
	done, due := p.sleepCompacted, p.sleepCompactDueLocked()
	p.mu.Unlock()
	if len(*calls) != 1 || done {
		t.Fatalf("会话忙不算整理过：calls=%v done=%v", *calls, done)
	}
	if left := time.Until(due); left < compactRetry-time.Minute || left > compactRetry {
		t.Errorf("应当隔一阵再试：还剩 %v", left)
	}
}

func TestSleepSurvivesRestart(t *testing.T) {
	_, store := newInited(t, noTurn, nil)
	ictx := plugin.InitContext{
		StateDir: t.TempDir(), Sessions: store, RunTurn: noTurn,
		NewSession: func() (string, error) { m, err := store.Create(); return m.ID, err },
	}
	p1 := New()
	if err := p1.Init(ictx, nil); err != nil {
		t.Fatal(err)
	}
	p1.Stop()
	pause(t, p1, `{"minutes":480,"reason":"睡","asleep":true}`)
	p1.mu.Lock()
	p1.sleepCompacted = true
	dir, st := p1.snapshotStateLocked()
	p1.mu.Unlock()
	persistState(dir, st)

	p2 := New()
	if err := p2.Init(ictx, nil); err != nil {
		t.Fatal(err)
	}
	p2.Stop()
	p2.mu.Lock()
	asleep, done := p2.asleep, p2.sleepCompacted
	p2.mu.Unlock()
	if !asleep || !done {
		t.Errorf("重启后应当还睡着、且记得这一觉整理过：asleep=%v done=%v", asleep, done)
	}
}

// wakeUp 模拟真人来消息：一轮在暂停之后开始并收尾。先等一下，让轮次开始时刻严格晚于
// 暂停时刻——Windows 上紧挨着的两次 time.Now() 可能落在同一刻，那样会被当成当轮设下的暂停。
func wakeUp(p *Plugin) {
	time.Sleep(2 * time.Millisecond)
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{Interactive: true, StartedAt: time.Now(), EndedAt: time.Now()})
}

// 查用量期间人被叫醒：旧任务不该再压缩，也不该往状态里写任何东西。
func TestSleepCompactAbortsWhenWokenBeforeCompacting(t *testing.T) {
	p, _, calls := sleepyPlugin(t, 0.6, nil)
	fallAsleep(t, p, sleepSettle+time.Minute)
	p.mu.Lock()
	p.status = func(string) (plugin.StatusInfo, error) {
		wakeUp(p) // 锁外调用，正是真人轮次能插进来的那个空当
		return plugin.StatusInfo{HasSession: true, CompactAt: 1000, EstTokens: 600, MeasuredTokens: -1}, nil
	}
	p.mu.Unlock()

	p.sleepCompact(context.Background())
	p.mu.Lock()
	asleep, done := p.asleep, p.sleepCompacted
	p.mu.Unlock()
	if len(*calls) != 0 {
		t.Errorf("醒了之后不该再压缩：%v", *calls)
	}
	if asleep || done {
		t.Errorf("旧任务不该回写状态：asleep=%v done=%v", asleep, done)
	}
}

// 压缩期间人被叫醒、随后又睡下：旧任务的结果不属于新的一觉。
func TestSleepCompactDoesNotMarkNextSleep(t *testing.T) {
	p, _, _ := sleepyPlugin(t, 0.6, nil)
	fallAsleep(t, p, sleepSettle+time.Minute)
	p.mu.Lock()
	p.compact = func(context.Context, string) error {
		wakeUp(p)
		pause(t, p, `{"minutes":480,"reason":"又睡了","asleep":true}`)
		return nil
	}
	p.mu.Unlock()

	p.sleepCompact(context.Background())
	p.mu.Lock()
	asleep, done := p.asleep, p.sleepCompacted
	p.mu.Unlock()
	if !asleep || done {
		t.Errorf("新的一觉不该被标成已整理：asleep=%v done=%v", asleep, done)
	}
}

// 电脑休眠跨过整段睡眠再恢复：已经到了醒来时刻，不该先补一次整理。
func TestSleepCompactSkippedAfterSleepEnded(t *testing.T) {
	p, _, calls := sleepyPlugin(t, 0.6, nil)
	fallAsleep(t, p, 9*time.Hour)
	p.mu.Lock()
	p.pausedUntil = time.Now().Add(-time.Hour)
	due := p.sleepCompactDueLocked()
	p.mu.Unlock()
	if !due.IsZero() {
		t.Errorf("已经醒了的一觉不该再排整理：due=%v", due)
	}
	p.sleepCompact(context.Background())
	if len(*calls) != 0 {
		t.Errorf("不该压缩：%v", *calls)
	}
}
