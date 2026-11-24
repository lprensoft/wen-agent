package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wen/internal/clock"
	"wen/internal/plugin"
	"wen/internal/usage"
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

// midTurnBudget 是 Agent 预算错误的替身：同时认两个哨兵，并能报告那次拦截发生在
// 轮次开始之前（started=false）还是中途（started=true，可能已执行过工具）。
type midTurnBudget struct{ started bool }

func (e *midTurnBudget) Error() string { return "本月用量已达上限" }

func (e *midTurnBudget) Unwrap() []error {
	return []error{plugin.ErrBudgetExceeded, usage.ErrBudgetExceeded}
}

func (e *midTurnBudget) TurnStarted() bool { return e.started }

// 预算拦下分两种，处置相反：
//   - 入口被拦（started=false）：一次模型请求都没发出去，任务必须回到派发前，预算
//     恢复后错过的一次性提醒还能补发。
//   - 中途被拦（started=true）：这一轮已经动过外部世界，回滚会让它在额度恢复后从头
//     再跑一遍。任务保持已执行，只留一句备注。
func TestBudgetExceededRollbackByTurnStarted(t *testing.T) {
	for _, mid := range []bool{false, true} {
		now := time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local)
		var calls int
		p, store := newInited(t, func(context.Context, string, string) (string, error) {
			calls++
			return "", &midTurnBudget{started: mid}
		})
		_, _ = store.Create()
		p.clock = clock.Func(func() time.Time { return now })
		p.mu.Lock()
		p.manual = false
		task := &Task{ID: "b1", Content: "提醒喝水", Type: typeOnce, At: now.Add(-time.Minute), CreatedAt: now}
		p.tasks = []*Task{task}
		p.mu.Unlock()

		p.dispatchDue(context.Background())
		p.wg.Wait()

		if calls != 1 {
			t.Fatalf("mid=%v: 应真的尝试了一次，实际 %d", mid, calls)
		}
		p.mu.Lock()
		done, lastRun, doneAt, note := task.Done, task.LastRun, task.DoneAt, task.Note
		p.mu.Unlock()
		if mid {
			// 保持派发后的状态：这一轮算发生过，Once 任务因此已结束。
			if !done || !lastRun.Equal(now) || !doneAt.Equal(now) {
				t.Fatalf("中途被拦不该回滚：Done=%v LastRun=%v DoneAt=%v", done, lastRun, doneAt)
			}
			if !strings.Contains(note, "预算中途用尽") {
				t.Fatalf("应留下「中途停下」的备注，得到 %q", note)
			}
			// 它不会再次到期，也就不该按「被拦」退避。
			if wait := p.untilNextAt(now); wait != pollFallback {
				t.Errorf("中途停下不该记 blockedAt，等待应为兜底周期 %v，实际 %v", pollFallback, wait)
			}
			continue
		}
		if done || !lastRun.IsZero() || !doneAt.IsZero() || note != "" {
			t.Fatalf("入口拦下的任务应回到派发前：Done=%v LastRun=%v DoneAt=%v Note=%q",
				done, lastRun, doneAt, note)
		}
		// 它仍到期（nextAt 对过期的一次性任务返回 now），循环因此不能立刻再转一圈：
		// 至少等一个兜底轮询周期，被拦的这几次一分钱都不花。
		if wait := p.untilNextAt(now); wait < pollFallback {
			t.Fatalf("循环应等一个兜底轮询周期再试，实际 %v", wait)
		}
	}
}

// 手动时间线下两种情形也分得清：入口被拦时把错误向上返回（评测据此回滚时间线），
// 中途被拦则当作这次已经执行过，不再向上报错。
func TestBudgetExceededRollbackManual(t *testing.T) {
	now := time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local)
	p, _ := newInited(t, func(context.Context, string, string) (string, error) {
		return "", &midTurnBudget{}
	})
	p.manual = true
	p.clock = clock.Func(func() time.Time { return now })
	task := &Task{ID: "b1", Content: "提醒喝水", Type: typeOnce, At: now.Add(-time.Minute), CreatedAt: now}
	p.tasks = []*Task{task}

	err := p.dispatchDueAt(context.Background(), now)
	if !errors.Is(err, plugin.ErrBudgetExceeded) {
		t.Fatalf("应把预算错误向上返回，得到 %v", err)
	}
	if task.Done || !task.LastRun.IsZero() || !task.DoneAt.IsZero() || task.Note != "" {
		t.Fatalf("预算拦下的任务应回到派发前：%+v", task)
	}

	// 中途被拦：任务已执行过，不再向上报错（否则评测会把这一项当作没发生）。
	mid, _ := newInited(t, func(context.Context, string, string) (string, error) {
		return "", &midTurnBudget{started: true}
	})
	mid.manual = true
	mid.clock = clock.Func(func() time.Time { return now })
	midTask := &Task{ID: "b2", Content: "提醒喝水", Type: typeOnce, At: now.Add(-time.Minute), CreatedAt: now}
	mid.tasks = []*Task{midTask}
	if err := mid.dispatchDueAt(context.Background(), now); err != nil {
		t.Fatalf("中途被拦不该向上报错，得到 %v", err)
	}
	if !midTask.Done || !midTask.LastRun.Equal(now) {
		t.Fatalf("中途被拦的任务应保持已执行：%+v", midTask)
	}
	if !strings.Contains(midTask.Note, "预算中途用尽") {
		t.Fatalf("应留下「中途停下」的备注，得到 %q", midTask.Note)
	}
}

// 用户在任务执行期间用 cancel_task 取消它之后，回滚不能把「已取消」写回派发前的
// 待执行状态：那会让一个刚被取消的任务重新到期、再发一次。入口被预算拦下与轮次
// 未开始都要求回滚，这里两种都要挡住；中途被预算拦下只写备注，同样不能盖掉「已取消」。
func TestRollbackKeepsCancelledTask(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"入口被预算拦下", errors.Join(plugin.ErrBudgetExceeded, usage.ErrBudgetExceeded)},
		{"轮次未开始", plugin.ErrTurnNotStarted},
		{"中途被预算拦下", &midTurnBudget{started: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now := time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local)
			var p *Plugin
			// 这一轮自己调 cancel_task 取消自己，等价于模型在轮次里取消一个周期任务。
			p, _ = newInited(t, func(ctx context.Context, _, _ string) (string, error) {
				out, err := p.Tools()[2].Execute(ctx, json.RawMessage(`{"id":"tp"}`))
				if err != nil {
					return "", err
				}
				if !strings.Contains(out, "已取消") {
					t.Fatalf("取消回执异常: %s", out)
				}
				return "", c.err
			})
			p.manual = true
			p.clock = clock.Func(func() time.Time { return now })
			task := &Task{ID: "tp", Content: "巡检", Type: typeInterval, IntervalMin: 30,
				CreatedAt: now.Add(-time.Hour)}
			p.tasks = []*Task{task}

			if err := p.dispatchDueAt(context.Background(), now); err != nil {
				t.Fatalf("任务已被取消，回滚不该再向上报错，得到 %v", err)
			}
			p.mu.Lock()
			done, note, blockedAt := task.Done, task.Note, p.blockedAt
			p.mu.Unlock()
			if !done || !strings.Contains(note, "已取消") {
				t.Fatalf("取消结果被回滚覆盖：Done=%v Note=%q", done, note)
			}
			if task.dueAt(now) {
				t.Fatal("已取消的任务不该重新到期")
			}
			if !blockedAt.IsZero() {
				t.Fatalf("没有回滚就不该记 blockedAt：%v", blockedAt)
			}
		})
	}
}

// entryBudgetErr 是「入口就被预算拦下」的替身：一次模型请求都没发出去，回滚成待处理。
func entryBudgetErr() error {
	return errors.Join(plugin.ErrBudgetExceeded, usage.ErrBudgetExceeded)
}

// 两次周期任务重叠时，上一轮写下的「上次执行成功」不能被本轮的回滚判据当成
// 「有人动过任务」：本轮在入口就被预算拦下（一轮模型请求都没发出去），必须回到待
// 处理、额度恢复后立刻能重试，而不是等一个完整周期；显示用的备注仍是上一轮那条。
func TestRollbackKeepsPreviousRoundNote(t *testing.T) {
	prev := time.Date(2026, 3, 2, 8, 30, 0, 0, time.Local)
	now := prev.Add(30 * time.Minute)
	var calls atomic.Int32
	p, store := newInited(t, func(context.Context, string, string) (string, error) {
		if calls.Add(1) == 1 {
			return "", nil // 08:30 那一轮：正常成功
		}
		return "", entryBudgetErr() // 09:00 那一轮：入口被预算拦下
	})
	_, _ = store.Create()
	p.clock = clock.Func(func() time.Time { return now })
	task := &Task{ID: "tp", Content: "巡检", Type: typeInterval, IntervalMin: 30,
		CreatedAt: prev.Add(-time.Hour)}
	p.mu.Lock()
	p.manual = false
	p.tasks = []*Task{task}
	p.mu.Unlock()

	if err := p.dispatchDueAt(context.Background(), prev); err != nil {
		t.Fatal(err)
	}
	p.wg.Wait() // 上一轮跑完并写下了自己的备注
	p.mu.Lock()
	prevNote := task.Note
	p.mu.Unlock()
	if !strings.Contains(prevNote, "上次执行成功") {
		t.Fatalf("上一轮应留下成功备注，实际 %q", prevNote)
	}

	if err := p.dispatchDueAt(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	p.wg.Wait()

	p.mu.Lock()
	lastRun, note, blockedAt := task.LastRun, task.Note, p.blockedAt
	p.mu.Unlock()
	if !lastRun.Equal(prev) {
		t.Fatalf("入口被拦的周期任务应回滚 LastRun，实际 %v", lastRun)
	}
	if note != prevNote {
		t.Fatalf("上一轮的成功备注被回滚盖掉：%q", note)
	}
	if !task.dueAt(now) {
		t.Fatal("回滚后应立即可再次到期（额度恢复即可重试）")
	}
	if blockedAt.IsZero() {
		t.Fatal("真回滚了就该记 blockedAt，循环才不会空转")
	}
}

// 旧回合让位给新回合：派发序号已经变了（另一次派发把这任务推进过），正在收尾的
// 那一轮不再回滚，也不记 blockedAt。
func TestRollbackSkippedByNewerDispatch(t *testing.T) {
	now := time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	p, store := newInited(t, func(context.Context, string, string) (string, error) {
		entered <- struct{}{}
		<-release
		return "", entryBudgetErr()
	})
	_, _ = store.Create()
	p.clock = clock.Func(func() time.Time { return now })
	task := &Task{ID: "tp", Content: "巡检", Type: typeInterval, IntervalMin: 30,
		CreatedAt: now.Add(-time.Hour)}
	p.mu.Lock()
	p.manual = false
	p.tasks = []*Task{task}
	p.mu.Unlock()

	p.dispatchDue(context.Background())
	<-entered
	p.mu.Lock()
	p.dispatchSeq++
	task.runSeq = p.dispatchSeq // 模拟另一次派发已经推进到下一轮
	p.mu.Unlock()
	close(release)
	p.wg.Wait()

	p.mu.Lock()
	lastRun, blockedAt := task.LastRun, p.blockedAt
	p.mu.Unlock()
	if !lastRun.Equal(now) {
		t.Fatalf("有更新派发时旧回合不该回滚 LastRun，实际 %v", lastRun)
	}
	if !blockedAt.IsZero() {
		t.Fatalf("没有回滚就不该记 blockedAt：%v", blockedAt)
	}
}

// waitNote 等到任务备注里出现 want。两轮各自在 goroutine 里收尾，备注何时写下不确定，
// 断言之前得先等它真的落下来。
func waitNote(t *testing.T, p *Plugin, task *Task, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		p.mu.Lock()
		note := task.Note
		p.mu.Unlock()
		if strings.Contains(note, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等不到备注 %q，当前 %q", want, note)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitBlocked 等到入口回滚真的发生。回滚在同一把锁里恢复 LastRun、runSeq 并记下
// blockedAt，看到它非零就说明那一轮已经退回派发前。
func waitBlocked(t *testing.T, p *Plugin) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		p.mu.Lock()
		blocked := !p.blockedAt.IsZero()
		p.mu.Unlock()
		if blocked {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("等不到入口回滚")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 同一周期任务的两轮重叠：后一轮（09:01）中途被预算拦下、先写下「预算中途用尽」，
// 前一轮（09:00）随后成功返回。旧轮次不能把新轮次的结果盖成「上次执行成功」——
// LastRun 指向的是 09:01 那一次，列表显示的必须是那一次的真实结果。
func TestMidTurnBudgetNoteKeptOverOlderSuccess(t *testing.T) {
	base := time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local)
	later := base.Add(time.Minute)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	p, store := newInited(t, func(context.Context, string, string) (string, error) {
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			<-release
			return "", nil // 09:00 那一轮：慢，但最后成功
		}
		return "", &midTurnBudget{started: true} // 09:01 那一轮：中途被预算拦下
	})
	_, _ = store.Create()
	p.clock = clock.Func(func() time.Time { return base })
	task := &Task{ID: "tp", Content: "巡检", Type: typeInterval, IntervalMin: 1,
		CreatedAt: base.Add(-time.Hour)}
	p.mu.Lock()
	p.manual = false
	p.tasks = []*Task{task}
	p.mu.Unlock()

	if err := p.dispatchDueAt(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := p.dispatchDueAt(context.Background(), later); err != nil {
		t.Fatal(err)
	}
	waitNote(t, p, task, "预算中途用尽")
	close(release)
	p.wg.Wait()

	p.mu.Lock()
	lastRun, note := task.LastRun, task.Note
	p.mu.Unlock()
	if !lastRun.Equal(later) {
		t.Fatalf("LastRun 应停在新轮次（09:01），实际 %v", lastRun)
	}
	if !strings.Contains(note, "预算中途用尽") {
		t.Fatalf("新轮次的预算备注被旧轮次的成功覆盖：%q", note)
	}
}

// 后一次派发在入口就被预算拦下、真回滚成待处理时，必须把它占用的轮次一起退回：前一轮
// （还在跑）随后的成功备注因此重新有资格落地，列表显示的就是它真实的结果。
func TestOlderRoundNoteLandsAfterEntryRollback(t *testing.T) {
	base := time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local)
	later := base.Add(time.Minute)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	p, store := newInited(t, func(context.Context, string, string) (string, error) {
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			<-release
			return "", nil // 09:00 那一轮：慢，但最后成功
		}
		return "", entryBudgetErr() // 09:01 那一轮：入口被预算拦下
	})
	_, _ = store.Create()
	p.clock = clock.Func(func() time.Time { return base })
	task := &Task{ID: "tp", Content: "巡检", Type: typeInterval, IntervalMin: 1,
		CreatedAt: base.Add(-time.Hour)}
	p.mu.Lock()
	p.manual = false
	p.tasks = []*Task{task}
	p.mu.Unlock()

	if err := p.dispatchDueAt(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := p.dispatchDueAt(context.Background(), later); err != nil {
		t.Fatal(err)
	}
	waitBlocked(t, p) // 09:01 那一轮已经回滚，它占用的序号也退回了
	close(release)
	p.wg.Wait()

	p.mu.Lock()
	lastRun, note := task.LastRun, task.Note
	p.mu.Unlock()
	if !lastRun.Equal(base) {
		t.Fatalf("回滚应把 LastRun 退回 09:00，实际 %v", lastRun)
	}
	if !strings.Contains(note, "上次执行成功") {
		t.Fatalf("回滚后前一轮的成功备注没能写入：%q", note)
	}
}

// 09:00 的任务先派发、晚返回；09:01 的任务先派发、被入口预算拦下。09:00 那一轮在
// 「当前轮次已经是 09:01」的情况下写下成功备注（旧实现因此把它丢掉），随后 09:01
// 回滚：显示必须还是 09:00 的结果，LastRun 也回到它。
func TestOlderRoundNoteSurvivesNewerEntryRollback(t *testing.T) {
	base := time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local)
	later := base.Add(time.Minute)
	oldEntered := make(chan struct{}, 1)
	releaseOld := make(chan struct{})
	releaseNew := make(chan struct{})
	var calls atomic.Int32
	p, store := newInited(t, func(context.Context, string, string) (string, error) {
		if calls.Add(1) == 1 {
			oldEntered <- struct{}{}
			<-releaseOld
			return "", nil // 09:00 那一轮：慢，但最后成功
		}
		<-releaseNew
		return "", entryBudgetErr() // 09:01 那一轮：入口被预算拦下
	})
	_, _ = store.Create()
	p.clock = clock.Func(func() time.Time { return base })
	task := &Task{ID: "tp", Content: "巡检", Type: typeInterval, IntervalMin: 1,
		CreatedAt: base.Add(-time.Hour)}
	p.mu.Lock()
	p.manual = false
	p.tasks = []*Task{task}
	p.mu.Unlock()

	if err := p.dispatchDueAt(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	<-oldEntered
	if err := p.dispatchDueAt(context.Background(), later); err != nil {
		t.Fatal(err)
	}
	close(releaseOld) // 旧轮次这时才成功返回，而 runSeq 已经是 09:01 那一轮
	waitNote(t, p, task, "上次执行成功")
	close(releaseNew) // 新轮次在入口被拦，回滚
	waitBlocked(t, p)
	p.wg.Wait()

	p.mu.Lock()
	lastRun, note := task.LastRun, task.Note
	p.mu.Unlock()
	if !lastRun.Equal(base) {
		t.Fatalf("回滚应把 LastRun 退回 09:00，实际 %v", lastRun)
	}
	if !strings.Contains(note, "上次执行成功") {
		t.Fatalf("旧轮次的成功备注没能显示：%q", note)
	}
}

// 用户取消之后，在飞的那一轮再写下结果也不能把「已取消」换回来——否则任务看着
// 还在待办，用户以为取消没生效。
func TestCancelledTaskIgnoresLateRoundNote(t *testing.T) {
	now := time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	p, store := newInited(t, func(context.Context, string, string) (string, error) {
		entered <- struct{}{}
		<-release
		return "", nil // 迟到的成功
	})
	_, _ = store.Create()
	p.clock = clock.Func(func() time.Time { return now })
	task := &Task{ID: "tp", Content: "巡检", Type: typeInterval, IntervalMin: 30,
		CreatedAt: now.Add(-time.Hour)}
	p.mu.Lock()
	p.manual = false
	p.tasks = []*Task{task}
	p.mu.Unlock()

	if err := p.dispatchDueAt(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err := p.Tools()[2].Execute(context.Background(), json.RawMessage(`{"id":"tp"}`)); err != nil {
		t.Fatal(err)
	}
	close(release)
	p.wg.Wait()

	p.mu.Lock()
	done, note, rounds := task.Done, task.Note, len(task.roundNotes)
	p.mu.Unlock()
	if !done || !strings.Contains(note, "已取消") {
		t.Fatalf("取消结果被迟到的轮次备注盖掉：Done=%v Note=%q", done, note)
	}
	if rounds != 0 {
		t.Fatalf("取消后不该留下轮次备注，实际 %d 条", rounds)
	}
}

// 每轮备注表只留最近的若干轮，且显示始终取序号最大的那条。
func TestRoundNotesKeepRecentOnly(t *testing.T) {
	p, _ := newInited(t, noTurn)
	task := &Task{ID: "tp", Type: typeInterval, IntervalMin: 30}

	total := uint64(maxRoundNotes + 4)
	for i := uint64(1); i <= total; i++ {
		p.setNoteForRound(task, i, fmt.Sprintf("第%d轮", i))
	}

	p.mu.Lock()
	count, note := len(task.roundNotes), task.Note
	_, keptOldest := task.roundNotes[1]
	_, keptNewest := task.roundNotes[total]
	p.mu.Unlock()
	if count != maxRoundNotes {
		t.Fatalf("应只保留 %d 轮，实际 %d", maxRoundNotes, count)
	}
	if keptOldest || !keptNewest {
		t.Fatalf("应淘汰最旧的轮次、保留最新的：oldest=%v newest=%v", keptOldest, keptNewest)
	}
	if !strings.Contains(note, fmt.Sprintf("第%d轮", total)) {
		t.Fatalf("显示应取最新一轮的备注，实际 %q", note)
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
