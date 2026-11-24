package scheduler

import (
	"context"
	"errors"
	"sort"
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
	// 刚被预算拦下的任务仍原样到期（nextAt 对已过期的一次性任务返回 now），不压住的
	// 话循环会空转：每一圈都判到期、都去挑会话、都被拦。至少等到下一个兜底轮询周期
	// 再试；这期间别的任务最多被推迟这么一点，而它本来也是按兜底周期轮询的。
	if !p.blockedAt.IsZero() {
		if d := p.blockedAt.Add(pollFallback).Sub(now); d > wait {
			wait = d
		}
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

// runTask 是既有测试入口：按当前时钟把任务当作它当前那一轮执行。
func (p *Plugin) runTask(ctx context.Context, t *Task) {
	p.mu.Lock()
	dispatched := t.runSeq
	p.mu.Unlock()
	_ = p.runTaskAt(ctx, t, dispatched, p.nowTime())
}

// taskState 是派发前的任务状态快照（上一次执行、结束标记、备注与派发序号）。真正
// 回滚时把它写回去；但「这一轮还要不要回滚」不看它，看派发序号（见 rollbackTask）。
// 带 runSeq 是为了回滚时能把任务退回派发前那一轮——后一次派发在入口被拦下、真回滚
// 之后，前一轮（还在跑）的备注写入要重新有资格落地。
type taskState struct {
	lastRun time.Time
	done    bool
	doneAt  time.Time
	note    string
	runSeq  uint64 // 派发前那一轮的序号
}

// stateOf 取任务当前的状态快照；mark 因此带着上一轮的序号。调用方需持有 p.mu。
func stateOf(t *Task) taskState {
	return taskState{lastRun: t.LastRun, done: t.Done, doneAt: t.DoneAt, note: t.Note, runSeq: t.runSeq}
}

// dispatchDueAt 逐个标记并派发到期任务。
//
// 不先把整批记为已派发：预算只够一轮时，后面的任务应保持待处理，而不是被标成
// Done 再当作失败。手动模式同步执行，遇到「轮次未开始」或预算拦下就回滚当前项并
// 向上返回；生产模式仍每个任务一个 goroutine。
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
		mark := stateOf(t)
		t.LastRun = now
		if t.Type == typeOnce {
			t.Done = true
			t.DoneAt = now
			t.Note = prompt.T(p.lang, "prompt.scheduler.note.running", "执行中")
		}
		// 派发序号就是「这一轮」的凭据：回滚只认它。期间用户取消（周期任务的 Done）
		// 或另一次派发又推进过，序号/取消标记就对不上，不该写回派发前。
		p.dispatchSeq++
		t.runSeq = p.dispatchSeq
		dispatched := t.runSeq
		if err := p.saveLocked(); err != nil {
			// 持有 p.mu 时不能调 p.langOf（它会再取同一把锁）；直接读字段。
			i18n.Logf(p.lang, "log.scheduler.save_failed", "scheduler: 保存任务状态失败: %v", err)
		}
		p.mu.Unlock()

		if manual {
			if err := p.runTaskAt(ctx, t, dispatched, now); err != nil && p.rollbackOn(err, t, mark, dispatched, now) {
				return err
			}
			continue
		}
		p.wg.Add(1)
		go func(t *Task, mark taskState, dispatched uint64) {
			defer p.wg.Done()
			if err := p.runTaskAt(ctx, t, dispatched, now); err != nil {
				p.rollbackOn(err, t, mark, dispatched, now)
			}
		}(t, mark, dispatched)
	}
	return nil
}

// rollbackOn 按错误决定「这一项要不要回滚成待处理」：轮次未开始、以及入口就被
// 预算拦下的任务算「这一轮没有发生」，应保持原样，返回 true。
//
// 预算恢复（用户调高上限、跨月）之后，错过的一次性提醒因此还能补发。代价是这类
// 任务会一直到期、一直重试，但被拦的那一次不发任何模型请求，循环也按下一次兜底
// 轮询（30 秒）来——见 untilNextAt 里的 blockedAt。
//
// 例外是「轮次中途被预算拦下」：那一轮已经拿到过响应，工具可能已经执行、消息可能
// 已经发出。回滚会让它在额度恢复后从头重跑一遍，同一件事做两次。这种情形保持
// 「已执行」（LastRun=now；once 的 Done=true），只留一句备注说明它停在了哪里。
//
// 返回值是「这一次真的回滚了」。手动模式据此决定要不要把错误向上交给评测：回滚
// 不了说明这一项已经被别处推进过（用户取消），评测不必再为它回退时间线。
func (p *Plugin) rollbackOn(err error, t *Task, mark taskState, dispatched uint64, now time.Time) bool {
	if errors.Is(err, plugin.ErrBudgetExceeded) {
		if plugin.BudgetStoppedMidTurn(err) {
			// 任务保持「已执行」，只留一句备注。用户若在这期间取消了它，「已取消」不能被
			// 预算备注盖掉——那句话会让人以为它还在待办（setNoteForRound 挡这类写入）。
			p.setNoteForRound(t, dispatched, prompt.T(p.langOf(), "prompt.scheduler.note.budget_mid_turn",
				"预算中途用尽，本次已停止（已发生的操作不会重做）"))
			return false
		}
		return p.rollbackTask(t, mark, dispatched, true, now)
	}
	if errors.Is(err, plugin.ErrTurnNotStarted) {
		return p.rollbackTask(t, mark, dispatched, false, now)
	}
	return false
}

// rollbackTask 把「这一轮没有发生过」的任务恢复到派发前，保持待处理，返回是否真的
// 回滚了。blocked 为真时顺带记下被预算拦下的时刻（见 untilNextAt）。
//
// 判据是派发序号，不是完整状态快照：含 Note 的快照会把「上一轮刚写下的成功备注」
// 当成有人动过任务，于是这一轮永远回滚不了，额度恢复后要干等一个完整周期。序号
// 对不上说明另一次派发已经把它推进到下一轮，那就不该覆盖（旧回合让位给新回合）。
//
// 序号一致时还要看取消：
//   - 周期任务没有「执行中」的 Done（派发只改 LastRun），Done 只可能是用户用
//     cancel_task 取消的，此时什么都不改、也不记 blockedAt。
//   - 一次性任务在派发时就把 Done 置 true 表示执行中，不能据此判成取消；序号一致
//     即恢复完整的派发前快照。
//
// 周期任务回滚只恢复 LastRun 与 runSeq：Note/Done/DoneAt 属于「刚刚那一轮写下的
// 结果」，上一轮的成功备注不该被这次没发生的派发盖掉。这一轮既然当作没发生，它在
// roundNotes 里的结论也要删掉，显示用的 Note 再按剩下的表刷新（没有更早的结论就保持
// 原样——那可能来自重启前，删掉它只会让列表变成空白）。runSeq 退回 mark 里那一轮，
// 是为了让前一轮（可能还在跑）随后的备注写入重新有资格落地——这次派发既然当作没
// 发生，它占用的序号也不该继续挡着前一轮。
func (p *Plugin) rollbackTask(t *Task, mark taskState, dispatched uint64, blocked bool, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t.runSeq != dispatched {
		return false
	}
	if t.Type == typeOnce {
		t.LastRun, t.Done, t.DoneAt, t.Note = mark.lastRun, mark.done, mark.doneAt, mark.note
		// 一次性任务只有一轮，它的结论整个作废；mark.note 已经恢复显示。
		t.roundNotes = nil
	} else {
		if t.Done {
			return false // 用户取消了：不该写回待处理
		}
		t.LastRun = mark.lastRun
		delete(t.roundNotes, dispatched)
		if note := latestRoundNote(t); note != "" {
			t.Note = note
		}
	}
	t.runSeq = mark.runSeq
	if blocked {
		p.blockedAt = now
	}
	_ = p.saveLocked()
	return true
}

// runTaskAt 执行单个任务：独立 goroutine + recover + 超时，任务崩溃或卡死只影响自己。
// 会话忙时 once 任务限次重试，周期任务直接跳过（下个周期自然会来）。
// 手动模式下不真睡重试：会话锁在上一轮返回时已释放，重试只会拖住时间线。
//
// dispatched 是派发时拿到的那一轮序号：备注按轮次记进每轮备注表，显示取最新有结论
// 的那一轮，重叠的旧轮次因此既不会把新轮次的结论盖掉，也不会因为新轮次没跑成而
// 连自己的结论都留不下（见 setNoteForRound）。
func (p *Plugin) runTaskAt(ctx context.Context, t *Task, dispatched uint64, now time.Time) (err error) {
	defer func() {
		if r := recover(); r != nil {
			i18n.Logf(p.langOf(), "log.scheduler.task_panic", "scheduler: 任务 %s 执行时 panic（已隔离）: %v", t.ID, r)
			p.setNoteForRound(t, dispatched, prompt.T(p.langOf(), "prompt.scheduler.note.panic", "执行异常"))
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
			p.setNoteForRound(t, dispatched, prompt.T(lang, "prompt.scheduler.note.no_session", "找不到可用会话"))
			return nil
		}
		_, e = runTurn(tctx, sid, prompt.T(lang, "prompt.scheduler.task_prefix", taskPrefixZH)+t.Content)
		cancel()

		switch {
		case e == nil:
			p.setNoteForRound(t, dispatched, prompt.Tf(lang, "prompt.scheduler.note.ok", "上次执行成功 %s", now.Format("01-02 15:04")))
			return nil
		case errors.Is(e, plugin.ErrBudgetExceeded):
			// 预算用尽不是这次执行失败。它有两种形态，都由派发方按
			// plugin.BudgetStoppedMidTurn 分辨：入口被拦时轮次压根没开始，回滚成
			// 待处理、额度恢复后还能补发；中途被拦时这一轮已经动过外部世界，不能重做。
			return e
		case errors.Is(e, plugin.ErrTurnNotStarted):
			return e
		case errors.Is(e, plugin.ErrSessionBusy):
			if t.Type != typeOnce || manual {
				i18n.Logf(p.langOf(), "log.scheduler.session_busy", "scheduler: 会话忙，任务 %s 跳过本次", t.ID)
				p.setNoteForRound(t, dispatched, prompt.T(lang, "prompt.scheduler.note.busy", "上次因会话忙跳过"))
				return nil
			}
			if attempt+1 >= busyRetryMax {
				i18n.Logf(p.langOf(), "log.scheduler.give_up", "scheduler: 任务 %s 重试 %d 次仍撞上会话忙，放弃本次", t.ID, busyRetryMax)
				p.setNoteForRound(t, dispatched, prompt.T(lang, "prompt.scheduler.note.busy_gave_up", "会话持续繁忙，本次未执行"))
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
			p.setNoteForRound(t, dispatched, prompt.Tf(lang, "prompt.scheduler.note.failed", "上次执行失败：%s", truncate(e.Error(), 100)))
			return nil
		}
	}
}

// setNote 更新任务备注并持久化。它不认轮次，执行结果一律走 setNoteForRound，否则
// 重叠的两轮会互相覆盖。
func (p *Plugin) setNote(t *Task, note string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t.Note = note
	_ = p.saveLocked()
}

// setNoteForRound 写下 dispatched 那一轮的结果备注。
//
// 判据不是「任务是否还停在当前 runSeq」：周期任务的两轮可以重叠（09:00 那一轮还
// 在跑，09:01 又派发了一轮），谁先完成与谁先派发无关。只认当前轮会把先派发、后
// 完成那一轮的结果丢掉——而它恰恰是 LastRun 指向的那一轮（后一轮入口被预算拦下、
// 回滚之后）。所以每一轮的结论都记进 roundNotes（派发序号 → 备注），显示用的 Note
// 取序号最大的那条：新轮次一旦写下结论就压住旧的（后一轮中途被预算拦下时，旧轮次
// 晚回来的「上次执行成功」不能把它盖掉）；旧轮次的结论也不会因为新轮次迟迟没有
// 结论而消失（新轮次被回滚时正是如此）。
//
// 用户取消留下的「已取消」不能被盖掉，否则任务看着还在待办：周期任务被取消
// （Done）时直接不写。一次性任务在执行中 Done 本来就是 true（派发时置的），不据此
// 判取消；它被取消/过期时同样会清空 roundNotes（见 cancelTool、catchUpAt）。
func (p *Plugin) setNoteForRound(t *Task, dispatched uint64, note string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t.Type != typeOnce && t.Done {
		return
	}
	if t.roundNotes == nil {
		t.roundNotes = make(map[uint64]string, maxRoundNotes)
	}
	t.roundNotes[dispatched] = note
	trimRoundNotes(t)
	t.Note = latestRoundNote(t)
	_ = p.saveLocked()
}

// maxRoundNotes 是每轮备注表保留的轮次数。够覆盖「重叠几轮、还都等着写结论」的情形，
// 又不会让长跑进程按轮数无限攒。
const maxRoundNotes = 8

// trimRoundNotes 只留序号最大的 maxRoundNotes 轮。调用方需持有 p.mu。
func trimRoundNotes(t *Task) {
	if len(t.roundNotes) <= maxRoundNotes {
		return
	}
	seqs := make([]uint64, 0, len(t.roundNotes))
	for s := range t.roundNotes {
		seqs = append(seqs, s)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for _, s := range seqs[:len(seqs)-maxRoundNotes] {
		delete(t.roundNotes, s)
	}
}

// latestRoundNote 返回序号最大的已记录备注，即最新完成那一轮的结论。调用方需持有 p.mu。
func latestRoundNote(t *Task) string {
	var latest uint64
	var note string
	found := false
	for s, n := range t.roundNotes {
		if !found || s > latest {
			latest, note, found = s, n, true
		}
	}
	return note
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
