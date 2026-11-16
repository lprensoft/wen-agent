package heartbeat

import (
	"context"
	"testing"
	"time"

	"wen/internal/availability"
	"wen/internal/clock"
)

func TestManualClockDrivesBeat(t *testing.T) {
	now := time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local)
	var calls int
	p, _ := newInited(t, func(context.Context, string, string) (string, error) {
		calls++
		return "在呢", nil
	}, map[string]any{"dynamic": false, "interval_minutes": 30})

	p.clockMu.Lock()
	p.clock = clock.Func(func() time.Time { return now })
	p.clockMu.Unlock()
	p.mu.Lock()
	p.manual = true
	p.lastBeat = now
	p.lastActive = now
	p.mu.Unlock()

	next, ok := p.NextBackgroundAt(now)
	if !ok || !next.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("下一拍 = %v ok=%v，期望 30 分钟后", next, ok)
	}
	if err := p.RunBackground(context.Background(), now.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("心跳应执行一次，实际 %d", calls)
	}
	next, ok = p.NextBackgroundAt(now.Add(30 * time.Minute))
	if !ok || !next.Equal(now.Add(60*time.Minute)) {
		t.Fatalf("执行后下一拍 = %v ok=%v，期望再隔 30 分钟", next, ok)
	}
}

// 心跳到点本身不应触发衰减，否则这一拍会被自己放大的间隔推迟。
func TestBeatAtDueTimeNotPostponedByDecay(t *testing.T) {
	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local)
	var calls int
	p, _ := newInited(t, func(context.Context, string, string) (string, error) {
		calls++
		return "", nil
	}, map[string]any{"dynamic": true, "interval_minutes": 30, "min_minutes": 5, "max_minutes": 120})

	p.clockMu.Lock()
	p.clock = clock.Func(func() time.Time { return start })
	p.clockMu.Unlock()
	p.mu.Lock()
	p.manual = true
	p.lastBeat = start.Add(time.Minute) // 09:01 真人聊过
	p.lastActive = start.Add(time.Minute)
	p.lastDecay = start // 上一次衰减检查在 09:00
	p.cur = 30 * time.Minute
	p.mu.Unlock()

	// 09:15、09:30 的检查都不满足空闲条件；它们仍应推进检查时刻。
	if err := p.RunBackground(context.Background(), start.Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := p.RunBackground(context.Background(), start.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("09:30 前不应有心跳，实际 %d", calls)
	}
	if p.cur != 30*time.Minute {
		t.Fatalf("检查不满足条件时不应衰减，cur=%v", p.cur)
	}

	// 09:31 心跳到点：应执行，而不是被额外衰减推到 09:46。
	if err := p.RunBackground(context.Background(), start.Add(31*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("到点心跳应执行一次，实际 %d", calls)
	}
}

// 手动时间线要自己维护忙碌结束后的缓冲：日程清掉公告板后，心跳不能立刻插进来。
func TestObserveBackgroundKeepsBusyGrace(t *testing.T) {
	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local)
	p, _ := newInited(t, func(context.Context, string, string) (string, error) { return "", nil },
		map[string]any{"dynamic": true, "interval_minutes": 30})

	p.clockMu.Lock()
	p.clock = clock.Func(func() time.Time { return start })
	p.clockMu.Unlock()
	p.mu.Lock()
	p.manual = true
	p.lastBeat = start
	p.lastActive = start
	p.lastDecay = start
	p.cur = 30 * time.Minute
	p.mu.Unlock()

	availability.Set(availability.State{
		Source: "agenda", Activity: "开会", Level: availability.Heavy,
		Since: start.Add(30 * time.Minute), Until: start.Add(60 * time.Minute),
	})
	t.Cleanup(func() { availability.Clear("agenda") })

	// 忙碌期间记录一次；活动结束后再记录一次，应留下 5 分钟缓冲。
	p.ObserveBackground(start.Add(30 * time.Minute))
	availability.Clear("agenda")
	p.ObserveBackground(start.Add(60 * time.Minute))

	// 衰减检查本身也到期时，查询会先返回它；这里只验证心跳本身没有提前。
	p.mu.Lock()
	p.lastDecay = start.Add(60 * time.Minute)
	p.mu.Unlock()
	next, ok := p.NextBackgroundAt(start.Add(60 * time.Minute))
	if !ok || !next.Equal(start.Add(65*time.Minute)) {
		t.Fatalf("下一次心跳 = %v ok=%v，期望忙完后 5 分钟", next, ok)
	}
}

// 心跳与衰减同刻到期时，同一调用里先心跳、再衰减，生产与回放结果一致。
func TestRunBackgroundDecaysAfterBeatAtSameInstant(t *testing.T) {
	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local)
	var calls int
	p, _ := newInited(t, func(context.Context, string, string) (string, error) {
		calls++
		return "", nil
	}, map[string]any{"dynamic": true, "interval_minutes": 30, "min_minutes": 5, "max_minutes": 120})

	p.clockMu.Lock()
	p.clock = clock.Func(func() time.Time { return start })
	p.clockMu.Unlock()
	p.mu.Lock()
	p.manual = true
	p.lastBeat = start
	p.lastActive = start
	p.lastDecay = start.Add(15 * time.Minute) // 09:30 与心跳同刻
	p.cur = 30 * time.Minute
	p.mu.Unlock()

	if err := p.RunBackground(context.Background(), start.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("心跳应执行一次，实际 %d", calls)
	}
	if p.cur != 45*time.Minute {
		t.Fatalf("同刻到期的心跳后应继续衰减一档，cur=%v", p.cur)
	}
}

// ticker 早几毫秒触发不应被当成「还没到期」而跳过一轮。
func TestDecayToleratesTickerJitter(t *testing.T) {
	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local)
	p, _ := newInited(t, func(context.Context, string, string) (string, error) { return "", nil },
		map[string]any{"dynamic": true, "interval_minutes": 45, "min_minutes": 45, "max_minutes": 120})

	p.clockMu.Lock()
	p.clock = clock.Func(func() time.Time { return start })
	p.clockMu.Unlock()
	p.mu.Lock()
	p.lastActive = start.Add(-time.Hour) // 一直没人说话
	p.lastDecay = start.Add(5 * time.Millisecond)
	p.cur = 45 * time.Minute
	p.mu.Unlock()

	// 计划检查在 09:15:00.005，ticker 在 09:15:00.001 到达。
	p.maybeDecayAt(start.Add(15*time.Minute + time.Millisecond))
	if p.cur != 68*time.Minute {
		t.Fatalf("早几毫秒的 tick 也应衰减，cur=%v", p.cur)
	}
	if want := start.Add(15*time.Minute + 5*time.Millisecond); !p.lastDecay.Equal(want) {
		t.Fatalf("检查时刻应按计划推进，lastDecay=%v，期望 %v", p.lastDecay, want)
	}
}

// 暂停结束时不补算暂停期间跳过的衰减检查：醒来后只做一次检查。
func TestPauseResumeDoesNotBackfillDecay(t *testing.T) {
	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local)
	pauseEnd := start.Add(8 * time.Hour)
	var calls int
	p, _ := newInited(t, func(context.Context, string, string) (string, error) {
		calls++
		return "", nil
	}, map[string]any{"dynamic": true, "interval_minutes": 30, "min_minutes": 5, "max_minutes": 120})

	p.clockMu.Lock()
	p.clock = clock.Func(func() time.Time { return pauseEnd })
	p.clockMu.Unlock()
	p.mu.Lock()
	p.manual = true
	p.lastBeat = start
	p.lastActive = start.Add(-time.Hour)
	p.lastDecay = start
	p.cur = 30 * time.Minute
	p.pausedAt = start
	p.pausedUntil = pauseEnd
	p.mu.Unlock()

	if err := p.RunBackground(context.Background(), pauseEnd); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("暂停结束应心跳一次，实际 %d", calls)
	}
	if p.cur != 45*time.Minute {
		t.Fatalf("暂停期间的检查不应补算，醒来后 cur=%v，期望 45m", p.cur)
	}
}

// 非整刻恢复后，生产循环的等待时间也要包含重置后的下一次衰减检查。
func TestPauseResumeSchedulesNextDecayAt(t *testing.T) {
	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local)
	resume := time.Date(2026, 3, 2, 17, 7, 0, 0, time.Local)
	var calls int
	p, _ := newInited(t, func(context.Context, string, string) (string, error) {
		calls++
		return "", nil
	}, map[string]any{"dynamic": true, "interval_minutes": 30, "min_minutes": 5, "max_minutes": 120})

	p.clockMu.Lock()
	p.clock = clock.Func(func() time.Time { return resume })
	p.clockMu.Unlock()
	p.mu.Lock()
	p.manual = true
	p.lastBeat = start
	p.lastActive = start.Add(-time.Hour)
	p.lastDecay = start
	p.cur = 30 * time.Minute
	p.pausedAt = start
	p.pausedUntil = resume
	p.mu.Unlock()

	if err := p.RunBackground(context.Background(), resume); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || p.cur != 45*time.Minute {
		t.Fatalf("恢复后应心跳一次并衰减一档：calls=%d cur=%v", calls, p.cur)
	}
	if got := p.nextWakeAt(resume); !got.Equal(resume.Add(15 * time.Minute)) {
		t.Fatalf("下一次醒来 = %v，期望 %v", got, resume.Add(15*time.Minute))
	}
}
