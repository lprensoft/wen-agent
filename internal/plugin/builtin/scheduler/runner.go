package scheduler

import (
	"context"
	"errors"
	"log"
	"time"

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
			p.dispatchDue(ctx)
		}
	}
}

// untilNext 返回距最近到期任务的时长，无任务或过远时用兜底轮询周期。
func (p *Plugin) untilNext() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	wait := pollFallback
	for _, t := range p.tasks {
		next := t.nextRun(now)
		if next.IsZero() {
			continue
		}
		if d := time.Until(next); d < wait {
			wait = d
		}
	}
	if wait < 0 {
		wait = 0
	}
	return wait
}

// dispatchDue 派发所有已到期的任务：先改状态并持久化（once 标记已派发、周期记 LastRun，
// 防止执行期间被重复派发），再各自开 goroutine 执行。
func (p *Plugin) dispatchDue(ctx context.Context) {
	now := time.Now()
	p.mu.Lock()
	var due []*Task
	for _, t := range p.tasks {
		next := t.nextRun(now)
		if next.IsZero() || next.After(now) {
			continue
		}
		t.LastRun = now
		if t.Type == typeOnce {
			t.Done = true
			t.DoneAt = now
			t.Note = prompt.T(p.lang, "prompt.scheduler.note.running", "执行中")
		}
		due = append(due, t)
	}
	if len(due) > 0 {
		_ = p.saveLocked()
	}
	p.mu.Unlock()

	for _, t := range due {
		p.wg.Add(1)
		go p.runTask(ctx, t)
	}
}

// runTask 执行单个任务：独立 goroutine + recover + 超时，任务崩溃或卡死只影响自己。
// 会话忙时 once 任务限次重试，周期任务直接跳过（下个周期自然会来）。
func (p *Plugin) runTask(ctx context.Context, t *Task) {
	defer p.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("scheduler: 任务 %s 执行时 panic（已隔离）: %v", t.ID, r)
			p.setNote(t, prompt.T(p.langOf(), "prompt.scheduler.note.panic", "执行异常"))
		}
	}()

	p.mu.Lock()
	runTurn, lang := p.runTurn, p.lang
	p.mu.Unlock()

	for attempt := 0; ; attempt++ {
		tctx, cancel := context.WithTimeout(ctx, runTimeout)
		sid, err := p.pickSession()
		if err != nil {
			cancel()
			log.Printf("scheduler: 任务 %s 找不到可用会话: %v", t.ID, err)
			p.setNote(t, prompt.T(lang, "prompt.scheduler.note.no_session", "找不到可用会话"))
			return
		}
		_, err = runTurn(tctx, sid, prompt.T(lang, "prompt.scheduler.task_prefix", taskPrefixZH)+t.Content)
		cancel()

		switch {
		case err == nil:
			p.setNote(t, prompt.Tf(lang, "prompt.scheduler.note.ok", "上次执行成功 %s", time.Now().Format("01-02 15:04")))
			return
		case errors.Is(err, plugin.ErrSessionBusy):
			if t.Type != typeOnce {
				log.Printf("scheduler: 会话忙，周期任务 %s 跳过本次", t.ID)
				p.setNote(t, prompt.T(lang, "prompt.scheduler.note.busy", "上次因会话忙跳过"))
				return
			}
			if attempt+1 >= busyRetryMax {
				log.Printf("scheduler: 任务 %s 重试 %d 次仍撞上会话忙，放弃本次", t.ID, busyRetryMax)
				p.setNote(t, prompt.T(lang, "prompt.scheduler.note.busy_gave_up", "会话持续繁忙，本次未执行"))
				return
			}
			select {
			case <-time.After(busyRetryEvery):
			case <-ctx.Done():
				return
			}
		default:
			if ctx.Err() != nil {
				return // 进程退出/插件停止，不记错
			}
			log.Printf("scheduler: 任务 %s 执行失败: %v", t.ID, err)
			p.setNote(t, prompt.Tf(lang, "prompt.scheduler.note.failed", "上次执行失败：%s", truncate(err.Error(), 100)))
			return
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
