package scheduler

import (
	"context"
	"errors"
	"time"

	"wen/internal/i18n"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

// loop 是调度循环：算出最近的到期时刻定时等待（30 秒兜底），到点把任务逐个派发出去。
// 派发本身只改状态与起 goroutine，从不在本循环里执行任务。
func (p *Plugin) loop(ctx context.Context) {
	defer p.wg.Done()
	for {
		wait := p.untilNext()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-p.wake: // 任务表变了，重算
			timer.Stop()
		case <-timer.C:
			_ = p.dispatchDueAt(ctx, p.nowTime())
		}
	}
}

// untilNext 返回距最近到期任务的时长，无任务或过远时用兜底轮询周期。
func (p *Plugin) untilNext() time.Duration { return p.untilNextAt(p.nowTime()) }

// untilNextAt 按 now 计算等待时长；nextAt 负责预测，可能落在未来。
func (p *Plugin) untilNextAt(now time.Time) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	wait := pollFallback
	for _, t := range p.tasks {
		next := t.nextAt(now)
		if next.IsZero() {
			continue
		}
		if d := next.Sub(now); d < wait {
			wait = d
		}
	}
	if wait < 0 {
		wait = 0
	}
	return wait
}

// NextBackgroundAt 返回所有任务里最近的一次计划时刻。
func (p *Plugin) NextBackgroundAt(now time.Time) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var next time.Time
	for _, t := range p.tasks {
		n := t.nextAt(now)
		if n.IsZero() {
			continue
		}
		if next.IsZero() || n.Before(next) {
			next = n
		}
	}
	return next, !next.IsZero()
}

// RunBackground 同步派发 now 时刻到期的任务；手动时间线下由评测调用。
func (p *Plugin) RunBackground(ctx context.Context, now time.Time) error {
	return p.dispatchDueAt(ctx, now)
}

// dispatchDue 是既有测试入口：按当前时钟派发。
func (p *Plugin) dispatchDue(ctx context.Context) { _ = p.dispatchDueAt(ctx, p.nowTime()) }

// runTask 是既有测试入口：按当前时钟执行单个任务。
func (p *Plugin) runTask(ctx context.Context, t *Task) { _ = p.runTaskAt(ctx, t, p.nowTime()) }

// taskMark 是派发前的状态快照，轮次未开始时用它回滚。
type taskMark struct {
	lastRun time.Time
	done    bool
	doneAt  time.Time
	note    string
}

// dispatchDueAt 逐个标记并派发到期任务。
//
// 不先把整批记为已派发：预算只够一轮时，后面的任务应保持待处理，而不是被标成
// Done 再当作失败。手动模式同步执行，遇到 ErrTurnNotStarted 回滚当前项并向上返回；
// 生产模式仍每个任务一个 goroutine。
func (p *Plugin) dispatchDueAt(ctx context.Context, now time.Time) error {
	p.mu.Lock()
	due := make([]*Task, 0)
	for _, t := range p.tasks {
		if t.dueAt(now) {
			due = append(due, t)
		}
	}
	manual := p.manual
	p.mu.Unlock()

	for _, t := range due {
		p.mu.Lock()
		// 同一轮里前一项可能已经改过状态（例如 once 完成后表被重写），重新核对。
		if !t.dueAt(now) {
			p.mu.Unlock()
			continue
		}
		mark := taskMark{lastRun: t.LastRun, done: t.Done, doneAt: t.DoneAt, note: t.Note}
		t.LastRun = now
		if t.Type == typeOnce {
			t.Done = true
			t.DoneAt = now
			t.Note = prompt.T(p.lang, "prompt.scheduler.note.running", "执行中")
		}
		if err := p.saveLocked(); err != nil {
			// 持有 p.mu 时不能调 p.langOf（它会再取同一把锁）；直接读字段。
			i18n.Logf(p.lang, "log.scheduler.save_failed", "scheduler: 保存任务状态失败: %v", err)
		}
		p.mu.Unlock()

		if manual {
			if err := p.runTaskAt(ctx, t, now); err != nil {
				if errors.Is(err, plugin.ErrTurnNotStarted) {
					p.rollbackTask(t, mark)
					return err
				}
			}
			continue
		}
		p.wg.Add(1)
		go func(t *Task, mark taskMark) {
			defer p.wg.Done()
			if err := p.runTaskAt(ctx, t, now); err != nil && errors.Is(err, plugin.ErrTurnNotStarted) {
				p.rollbackTask(t, mark)
			}
		}(t, mark)
	}
	return nil
}

// rollbackTask 把「轮次未开始」的任务恢复到派发前，保持待处理。
func (p *Plugin) rollbackTask(t *Task, mark taskMark) {
	p.mu.Lock()
	t.LastRun, t.Done, t.DoneAt, t.Note = mark.lastRun, mark.done, mark.doneAt, mark.note
	_ = p.saveLocked()
	p.mu.Unlock()
}

// runTaskAt 执行单个任务：独立 goroutine + recover + 超时，任务崩溃或卡死只影响自己。
// 会话忙时 once 任务限次重试，周期任务直接跳过（下个周期自然会来）。
// 手动模式下不真睡重试：会话锁在上一轮返回时已释放，重试只会拖住时间线。
func (p *Plugin) runTaskAt(ctx context.Context, t *Task, now time.Time) (err error) {
	defer func() {
		if r := recover(); r != nil {
			i18n.Logf(p.langOf(), "log.scheduler.task_panic", "scheduler: 任务 %s 执行时 panic（已隔离）: %v", t.ID, r)
			p.setNote(t, prompt.T(p.langOf(), "prompt.scheduler.note.panic", "执行异常"))
			err = nil
		}
	}()

	p.mu.Lock()
	runTurn, lang, manual := p.runTurn, p.lang, p.manual
	p.mu.Unlock()

	for attempt := 0; ; attempt++ {
		tctx, cancel := context.WithTimeout(ctx, runTimeout)
		sid, e := p.pickSession()
		if e != nil {
			cancel()
			i18n.Logf(p.langOf(), "log.scheduler.no_session", "scheduler: 任务 %s 找不到可用会话: %v", t.ID, e)
			p.setNote(t, prompt.T(lang, "prompt.scheduler.note.no_session", "找不到可用会话"))
			return nil
		}
		_, e = runTurn(tctx, sid, prompt.T(lang, "prompt.scheduler.task_prefix", taskPrefixZH)+t.Content)
		cancel()

		switch {
		case e == nil:
			p.setNote(t, prompt.Tf(lang, "prompt.scheduler.note.ok", "上次执行成功 %s", now.Format("01-02 15:04")))
			return nil
		case errors.Is(e, plugin.ErrTurnNotStarted):
			return e
		case errors.Is(e, plugin.ErrSessionBusy):
			if t.Type != typeOnce || manual {
				i18n.Logf(p.langOf(), "log.scheduler.session_busy", "scheduler: 会话忙，任务 %s 跳过本次", t.ID)
				p.setNote(t, prompt.T(lang, "prompt.scheduler.note.busy", "上次因会话忙跳过"))
				return nil
			}
			if attempt+1 >= busyRetryMax {
				i18n.Logf(p.langOf(), "log.scheduler.give_up", "scheduler: 任务 %s 重试 %d 次仍撞上会话忙，放弃本次", t.ID, busyRetryMax)
				p.setNote(t, prompt.T(lang, "prompt.scheduler.note.busy_gave_up", "会话持续繁忙，本次未执行"))
				return nil
			}
			select {
			case <-time.After(busyRetryEvery):
			case <-ctx.Done():
				return nil
			}
		default:
			if ctx.Err() != nil {
				return nil // 进程退出/插件停止，不记错
			}
			i18n.Logf(p.langOf(), "log.scheduler.task_failed", "scheduler: 任务 %s 执行失败: %v", t.ID, e)
			p.setNote(t, prompt.Tf(lang, "prompt.scheduler.note.failed", "上次执行失败：%s", truncate(e.Error(), 100)))
			return nil
		}
	}
}

// setNote 更新任务备注并持久化。
func (p *Plugin) setNote(t *Task, note string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t.Note = note
	_ = p.saveLocked()
}

// pickSession 把任务落在最近活跃的会话上，一个都没有时新建。
// 判定规则见 SessionQuery.LastActive，与心跳插件共用同一份。
func (p *Plugin) pickSession() (string, error) {
	p.mu.Lock()
	sessions, newSession := p.sessions, p.newSession
	p.mu.Unlock()

	id, _, err := sessions.LastActive()
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	if newSession == nil {
		return "", errors.New("没有会话且当前环境不支持新建")
	}
	return newSession()
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
