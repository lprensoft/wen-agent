package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"wen/internal/clock"
	"wen/internal/plugin"
)

const (
	defaultMaxBackgroundTurns = 100
	maxTimelineIterations     = 100000
	maxSameAtPerWorker        = 50
)

// evalTurnBudget 在插件发起的每一轮 RunTurn 入口扣预算。
// 未获预算的调用不计已尝试；observer 事后计数无法阻止同刻多任务一次跑完。
type evalTurnBudget struct {
	mu       sync.Mutex
	max      int
	used     int
	denied   int
	limitHit bool
	attempts []evalTurnAttempt
}

type evalTurnAttempt struct {
	At     time.Time
	Origin string
	Err    string
}

func (b *evalTurnBudget) before(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.max > 0 && b.used >= b.max {
		b.limitHit = true
		b.denied++
		return fmt.Errorf("%w: %s", plugin.ErrTurnNotStarted,
			trf("cli.eval.err.turn_budget", "后台轮次预算已用完（上限 %d）", b.max))
	}
	b.used++
	return nil
}

func (b *evalTurnBudget) after(ctx context.Context, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	a := evalTurnAttempt{
		At:     clock.Now(ctx),
		Origin: plugin.TurnOriginFrom(ctx),
	}
	if err != nil {
		a.Err = err.Error()
	}
	b.attempts = append(b.attempts, a)
}

func (b *evalTurnBudget) limited() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limitHit
}

func (b *evalTurnBudget) snapshot() (used int, attempts []evalTurnAttempt) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used, append([]evalTurnAttempt(nil), b.attempts...)
}

func (b *evalTurnBudget) deniedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.denied
}

// evalObserver 收集成功结束的后台轮次。它只实现 Plugin + TurnObserver，
// 不提供工具与提示词，也不声明分组，避免被评测的插件压制规则命中。
type evalObserver struct {
	mu     sync.Mutex
	events []plugin.TurnEndEvent
}

func (o *evalObserver) Name() string { return "eval_observer" }

func (o *evalObserver) Description() string { return "" }

func (o *evalObserver) Init(plugin.InitContext, map[string]any) error { return nil }

func (o *evalObserver) SystemPrompt() string { return "" }

func (o *evalObserver) Tools() []plugin.Tool { return nil }

func (o *evalObserver) OnTurnEnd(_ context.Context, ev plugin.TurnEndEvent) {
	if ev.Origin == "" {
		return // 前台轮次由脚本自己记录
	}
	o.mu.Lock()
	o.events = append(o.events, ev)
	o.mu.Unlock()
}

func (o *evalObserver) drain() []plugin.TurnEndEvent {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := o.events
	o.events = nil
	return out
}

// advanceTimeline 把时钟推进到 horizon，沿途执行所有到期的后台工作。
func advanceTimeline(ctx context.Context, clk *clock.Manual, mgr *plugin.Manager,
	obs *evalObserver, budget *evalTurnBudget, horizon time.Time, progress func(string, ...any)) ([]plugin.TurnEndEvent, int, error) {
	return runTimeline(ctx, clk, mgr, obs, budget, horizon, true, progress)
}

// runDueNow 不推进时钟，只执行当前已经到期的后台工作。
func runDueNow(ctx context.Context, clk *clock.Manual, mgr *plugin.Manager,
	obs *evalObserver, budget *evalTurnBudget, progress func(string, ...any)) ([]plugin.TurnEndEvent, int, error) {
	return runTimeline(ctx, clk, mgr, obs, budget, clk.Now(), false, progress)
}

func runTimeline(ctx context.Context, clk *clock.Manual, mgr *plugin.Manager,
	obs *evalObserver, budget *evalTurnBudget, horizon time.Time, allowAdvance bool,
	progress func(string, ...any)) ([]plugin.TurnEndEvent, int, error) {
	workers := mgr.BackgroundWorkers()
	if len(workers) == 0 {
		if allowAdvance {
			clk.Set(horizon)
		}
		return nil, 0, nil
	}

	var events []plugin.TurnEndEvent
	steps := 0
	sameAt := map[string]int{}
	lastAt := time.Time{}
	report := func(format string, a ...any) {
		if progress != nil {
			progress(format, a...)
		}
	}

	for iter := 0; ; iter++ {
		if iter >= maxTimelineIterations {
			return events, steps, fmt.Errorf("%s",
				trf("cli.eval.err.timeline_iterations", "时间线在 %d 步内没有推进到终点", maxTimelineIterations))
		}
		if err := ctx.Err(); err != nil {
			return events, steps, err
		}

		now := clk.Now()
		// 让需要观察外部状态的 worker（心跳的忙碌缓冲）在选事件前记录当前状态。
		mgr.ObserveBackground(now)

		pick := ""
		var at time.Time
		for _, w := range workers {
			next, ok := w.Worker.NextBackgroundAt(now)
			if !ok {
				continue
			}
			if next.Before(now) {
				next = now // 已到期：现在就跑
			}
			if next.After(horizon) {
				continue
			}
			if !allowAdvance && next.After(now) {
				continue // tick 只跑当前到期的事件，不预支未来
			}
			if pick == "" || next.Before(at) {
				pick, at = w.Name, next
			}
		}
		if pick == "" {
			if allowAdvance {
				clk.Set(horizon)
			}
			break
		}

		if allowAdvance && at.After(now) {
			clk.Set(at)
			sameAt = map[string]int{}
			lastAt = at
		} else if !at.Equal(lastAt) {
			sameAt = map[string]int{}
			lastAt = at
		}
		sameAt[pick]++
		if sameAt[pick] > maxSameAtPerWorker {
			return events, steps, fmt.Errorf("%s",
				trf("cli.eval.err.timeline_stuck",
					"插件 %s 在 %s 反复产生同一时刻的事件（超过 %d 次）",
					pick, at.Format("2006-01-02 15:04"), maxSameAtPerWorker))
		}

		clk.Set(at)
		if err := mgr.RunBackground(ctx, pick, at); err != nil {
			events = append(events, obs.drain()...)
			return events, steps, err
		}
		steps++
		if err := mgr.Settle(ctx); err != nil {
			events = append(events, obs.drain()...)
			return events, steps, err
		}
		events = append(events, obs.drain()...)
		if budget.limited() {
			return events, steps, fmt.Errorf("%s",
				trf("cli.eval.err.background_limit", "后台轮次达到上限 %d，评测中止", budget.max))
		}
		report(tr("cli.eval.timeline.event", "  · %s %s"), at.Format("2006-01-02 15:04"), pick)
	}
	return events, steps, nil
}
