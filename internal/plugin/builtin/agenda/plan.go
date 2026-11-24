package agenda

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"wen/internal/clock"
	"wen/internal/i18n"
	"wen/internal/plugin"
)

// pendingPlan 是手动时间线下登记的一次规划。
type pendingPlan struct {
	sid   string
	today string
}

// OnTurnEnd 是规划轮次的触发点：新的一天里第一轮对话（真人的，或心跳暂停到点后
// 的第一拍——那是「睡醒」）结束后，在同一个会话上起一轮【规划今天】。不另设固定时刻：
// 心跳关着、又没人说话的日子，没有任何人看得到这张表，排了也白排。
//
// 本方法在轮次收尾的同步路径上被调用。生产路径判断后起 goroutine；手动时间线只登记，
// 由评测在轮次彻底返回后调 Settle 同步执行。
func (p *Plugin) OnTurnEnd(ctx context.Context, ev plugin.TurnEndEvent) {
	if ev.Origin == p.Name() {
		return // 自己的轮次（规划、开始、结束）不触发
	}
	s := p.snapshot()
	if !s.autoPlan || s.runTurn == nil || s.base == "" {
		return
	}
	tag := plugin.ScopeFrom(ctx).Write
	st := p.storeFor(tag)
	if st == nil {
		return
	} // 用这一轮结束的时刻算「今天」：评测的时间线可能不在系统时间上，按 p.now()
	// 判断会去排另一天的表，而那一轮写下的消息也落在另一条时间线上。
	now := ev.EndedAt
	if now.IsZero() {
		now = p.nowOf(ctx)
	}
	today := s.today(now).Format(dateLayout)
	pl, err := st.LoadPlan()
	if err != nil {
		i18n.Logf(p.langOf(), "log.agenda.read_plan_failed", "agenda: 读取日程表失败: %v", err)
		return
	}
	if pl.Date == today {
		return // 今天已经排过
	}
	if pl.Attempts.Date == today && pl.Attempts.Count >= defMaxPlanAttempts {
		return // 今天已经试过两次，到明天再说
	}

	p.mu.Lock()
	if p.cancel == nil || p.planning[tag] {
		p.mu.Unlock()
		return
	}
	if p.manual {
		p.planPending[tag] = pendingPlan{sid: ev.SessionID, today: today}
		p.mu.Unlock()
		return
	}
	p.planning[tag] = true
	p.wg.Add(1)
	// 把这一轮的时间线带进后台，但只带「钟」不冻结「时刻」：规划可能等上几秒、
	// 重试几次，甚至跨过日界，拿触发那一刻的日期去排表会排到昨天，还会盖掉已经
	// 存在的今天。生产路径的 ctx 里没有钟，后台照旧用系统时间。
	pctx := p.ctx
	if c := clock.From(ctx); c != nil {
		pctx = clock.With(pctx, c)
	}
	p.mu.Unlock()
	go func() {
		defer p.wg.Done()
		p.runPlan(pctx, tag, ev.SessionID)
	}()
}

// Settle 同步执行手动时间线下登记的规划。按固定域顺序取，避免 map 的随机遍历。
func (p *Plugin) Settle(ctx context.Context) (bool, error) {
	p.mu.Lock()
	if len(p.planPending) == 0 {
		p.mu.Unlock()
		return false, nil
	}
	tags := make([]string, 0, len(p.planPending))
	for tag := range p.planPending {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	tag := tags[0]
	job := p.planPending[tag]
	delete(p.planPending, tag)
	if p.planning[tag] {
		p.mu.Unlock()
		return true, nil
	}
	p.planning[tag] = true
	p.mu.Unlock()

	p.runPlan(ctx, tag, job.sid)
	return true, nil
}

// runPlan 跑一次规划轮次：等触发那轮彻底收尾，清掉已过期的约定，再以【规划今天】
// 为一次性输入起轮次。会话忙限次重试；其它失败与「轮次成功但没提交表」各计一次
// 尝试，达到上限就留一条会话注记。
func (p *Plugin) runPlan(ctx context.Context, tag, sid string) {
	defer func() {
		p.mu.Lock()
		delete(p.planning, tag)
		delete(p.planSubmitted, tag)
		p.mu.Unlock()
	}()
	p.mu.Lock()
	delete(p.planSubmitted, tag) // 本次规划从「还没提交过」开始
	p.mu.Unlock()
	select {
	case <-time.After(p.planSettle):
	case <-ctx.Done():
		return
	}
	s := p.snapshot()
	st := p.storeFor(tag)
	if st == nil || s.runTurn == nil {
		return
	}
	nctx := plugin.WithScope(ctx, plugin.Scope{Write: tag})

	for attempt := 0; ; attempt++ {
		// 每次尝试前都重算「今天」，并重读日程表：忙重试可能隔上几秒甚至跨过日界，
		// 前台也可能在这期间把新一天的日程排好了。拿上一次的日期与输入重试，会排
		// 到昨天，还会覆盖别人刚排的表。
		now := p.nowOf(ctx)
		today := s.today(now).Format(dateLayout)
		if pl, err := st.LoadPlan(); err == nil {
			if pl.Date == today {
				return
			}
			if pl.Attempts.Date == today && pl.Attempts.Count >= defMaxPlanAttempts {
				return
			}
		}
		p.pruneCommitments(nctx, st, sid, today)

		cs, err := st.LoadCommitments()
		if err != nil {
			i18n.Logf(p.langOf(), "log.agenda.read_commitments_failed", "agenda: 读取约定失败: %v", err)
			return
		}
		var todayCs []Commitment
		for _, c := range cs {
			if c.Date == today {
				todayCs = append(todayCs, c)
			}
		}
		sortCommitments(todayCs)
		input := buildPlanPrompt(s.lang, now, todayCs)

		tctx, cancel := context.WithTimeout(ctx, runTimeout)
		tctx = plugin.WithEphemeralInput(tctx)
		_, err = s.runTurn(tctx, sid, input)
		cancel()
		switch {
		case err == nil:
			pl, lerr := st.LoadPlan()
			// 模型调用本身也可能跨过日界：表排在今天或更晚都算排成了，
			// 只有「还是旧的那一天 / 压根没排」才记一次失败。
			if lerr == nil && pl.Date >= today {
				return
			}
			p.countAttempt(nctx, st, sid, today, "模型没有提交日程表")
			return
		case errors.Is(err, plugin.ErrTurnNotStarted):
			// 轮次没有开始：不记尝试，任务保持待处理（预算中止会终止整场评测）。
			return
		case errors.Is(err, plugin.ErrBudgetExceeded):
			// 本月用量预算用尽：与「轮次没有开始」同一类，不记尝试。它不是「今天没排成」
			// 的事实，只是此刻没钱可用；预算恢复（调高上限、跨月）后照常再排，而「今天
			// 已经试过两次」的记账不该被它烧掉。
			return
		case errors.Is(err, plugin.ErrSessionBusy):
			if p.isManual() || attempt+1 >= defBusyRetryMax {
				i18n.Logf(p.langOf(), "log.agenda.plan_session_busy", "agenda: 会话 %s 持续繁忙，本次规划放弃", sid)
				return
			}
			select {
			case <-time.After(p.busyRetry):
			case <-ctx.Done():
				return
			}
		default:
			if ctx.Err() != nil {
				return // 插件停止，不记失败
			}
			i18n.Logf(p.langOf(), "log.agenda.plan_turn_failed", "agenda: 规划轮次失败: %v", err)
			p.countAttempt(nctx, st, sid, today, "规划轮次失败")
			return
		}
	}
}

// countAttempt 记一次失败的规划尝试，达到上限时留一条会话注记。
func (p *Plugin) countAttempt(ctx context.Context, st *Store, sid, today, why string) {
	pl, err := st.UpdatePlan(func(pl *Plan) (bool, error) {
		if pl.Attempts.Date != today {
			pl.Attempts = Attempts{Date: today}
		}
		pl.Attempts.Count++
		return true, nil
	})
	if err != nil {
		i18n.Logf(p.langOf(), "log.agenda.count_attempt_failed", "agenda: 记录规划尝试失败: %v", err)
		return
	}
	if pl.Attempts.Count >= defMaxPlanAttempts {
		p.postNotice(ctx, sid, fmt.Sprintf("今天的日程没有排成（%s，已尝试 %d 次），明天再试。", why, pl.Attempts.Count))
	}
}

// pruneCommitments 清掉日期已过的约定。没排进当天表的那些留一条注记——那是一件
// 答应过却被遗忘的事，至少让人看得到。
func (p *Plugin) pruneCommitments(ctx context.Context, st *Store, sid, today string) {
	var dropped []Commitment
	_, err := st.UpdateCommitments(func(list *[]Commitment, _ func() string) (bool, error) {
		kept := (*list)[:0]
		for _, c := range *list {
			if c.Date < today {
				if !c.Planned {
					dropped = append(dropped, c)
				}
				continue
			}
			kept = append(kept, c)
		}
		changed := len(kept) != len(*list)
		*list = kept
		return changed, nil
	})
	if err != nil {
		i18n.Logf(p.langOf(), "log.agenda.prune_failed", "agenda: 清理过期约定失败: %v", err)
		return
	}
	for _, c := range dropped {
		p.postNotice(ctx, sid, i18n.Tf(p.langOf(), "plugin.agenda.notice.commit_dropped",
			"%s的约定「%s」没有排进当天，已丢弃。", fmtDateCN(p.langOf(), c.Date, p.now().Location()), c.Title))
	}
}

// postNotice 往会话里留一行只给人看的注记；不可用时退到日志。
func (p *Plugin) postNotice(ctx context.Context, sid, text string) {
	s := p.snapshot()
	if s.notice == nil || sid == "" {
		i18n.Logf(p.langOf(), "log.agenda.notice", "agenda: %s", text)
		return
	}
	if err := s.notice(ctx, sid, text); err != nil {
		i18n.Logf(p.langOf(), "log.agenda.notice_failed", "agenda: 写会话注记失败（%s）: %v", text, err)
	}
}
