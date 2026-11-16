package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wen/internal/clock"
	"wen/internal/plugin"
)

func TestDailyDueAtBoundary(t *testing.T) {
	now := time.Date(2026, 3, 2, 8, 30, 0, 0, time.Local)
	task := &Task{ID: "d", Type: typeDaily, DailyTime: "08:30"}

	if !task.dueAt(now) {
		t.Fatal("恰好到点应算到期")
	}
	if got := task.nextAt(now); !got.Equal(now) {
		t.Fatalf("nextAt = %v，期望 now", got)
	}

	// 跑过之后同一时刻不应再次到期，下一次排到明天。
	task.LastRun = now
	if task.dueAt(now) {
		t.Fatal("同刻再次到期")
	}
	if got := task.nextAt(now); !got.Equal(now.AddDate(0, 0, 1)) {
		t.Fatalf("nextAt = %v，期望明天 08:30", got)
	}

	tomorrow := now.AddDate(0, 0, 1)
	if !task.dueAt(tomorrow) {
		t.Fatal("次日应再次到期")
	}
}

func TestDailyDispatchesOnceAtSameInstant(t *testing.T) {
	now := time.Date(2026, 3, 2, 8, 30, 0, 0, time.Local)
	var calls int
	p, _ := newInited(t, func(context.Context, string, string) (string, error) {
		calls++
		return "", nil
	})
	p.manual = true
	p.clock = clock.Func(func() time.Time { return now })
	p.tasks = []*Task{{ID: "d", Type: typeDaily, DailyTime: "08:30", CreatedAt: now}}

	if err := p.dispatchDueAt(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("恰好到点应执行一次，实际 %d", calls)
	}
	if err := p.dispatchDueAt(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("同刻再次 tick 不应重复执行，实际 %d", calls)
	}
	if err := p.dispatchDueAt(context.Background(), now.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("次日应再执行一次，实际 %d", calls)
	}
}

func TestBudgetRollbackKeepsTasksPending(t *testing.T) {
	now := time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local)
	var calls int
	p, _ := newInited(t, func(context.Context, string, string) (string, error) {
		calls++
		if calls > 1 {
			return "", plugin.ErrTurnNotStarted
		}
		return "", nil
	})
	p.manual = true
	p.clock = clock.Func(func() time.Time { return now })
	p.tasks = []*Task{
		{ID: "a", Type: typeOnce, At: now.Add(-time.Minute), Content: "a"},
		{ID: "b", Type: typeOnce, At: now.Add(-time.Minute), Content: "b"},
		{ID: "c", Type: typeOnce, At: now.Add(-time.Minute), Content: "c"},
	}

	err := p.dispatchDueAt(context.Background(), now)
	if !errors.Is(err, plugin.ErrTurnNotStarted) {
		t.Fatalf("应向上返回预算中止，得到 %v", err)
	}
	if calls != 2 {
		t.Fatalf("只应尝试两轮（成功一次、拒绝一次），实际 %d", calls)
	}
	if !p.tasks[0].Done {
		t.Fatal("第一个任务应已完成")
	}
	for _, task := range p.tasks[1:] {
		if task.Done || !task.LastRun.IsZero() {
			t.Fatalf("未获预算的任务应保持待处理：%+v", task)
		}
	}
}

func TestIntervalDueAndNext(t *testing.T) {
	created := time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local)
	task := &Task{Type: typeInterval, IntervalMin: 60, CreatedAt: created}
	if task.dueAt(created) {
		t.Fatal("刚创建不应立刻到期")
	}
	at := created.Add(time.Hour)
	if !task.dueAt(at) {
		t.Fatal("满一个周期应到期")
	}
	if got := task.nextAt(at); !got.Equal(at) {
		t.Fatalf("nextAt = %v，期望 now", got)
	}
	task.LastRun = at
	if got := task.nextAt(at); !got.Equal(at.Add(time.Hour)) {
		t.Fatalf("下一次 = %v，期望一小时后", got)
	}
}

// 保存失败时不能因为持锁再取锁而死锁。
func TestDispatchSurvivesSaveFailure(t *testing.T) {
	now := time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local)
	p, _ := newInited(t, noTurn)
	p.manual = true
	p.clock = clock.Func(func() time.Time { return now })

	file := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.stateDir = filepath.Join(file, "plugins") // 父路径是文件，saveLocked 必然失败
	p.tasks = []*Task{{ID: "a", Type: typeOnce, At: now.Add(-time.Minute), Content: "x"}}

	done := make(chan struct{})
	go func() {
		_ = p.dispatchDueAt(context.Background(), now)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("保存失败时派发不应死锁")
	}
}
