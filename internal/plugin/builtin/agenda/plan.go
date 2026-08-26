package agenda

import (
	"context"
	"errors"
	"fmt"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// OnTurnEnd 是规划轮次的触发点：新的一天里第一轮对话（真人的，或心跳暂停到点后
// 的第一拍——那是「睡醒」）结束后，在同一个会话上起一轮【规划今天】。不另设固定时刻：
// 心跳关着、又没人说话的日子，没有任何人看得到这张表，排了也白排。
//
// 本方法在轮次收尾的同步路径上被调用，只做判断与起 goroutine。
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
	}
	today := s.today(p.now()).Format(dateLayout)
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
	p.planning[tag] = true
	p.wg.Add(1)
	pctx := p.ctx
	p.mu.Unlock()
	go p.runPlan(pctx, tag, ev.SessionID, today)
}

// runPlan 跑一次规划轮次：等触发那轮彻底收尾，清掉已过期的约定，再以【规划今天】
// 为一次性输入起轮次。会话忙限次重试；其它失败与「轮次成功但没提交表」各计一次
// 尝试，达到上限就留一条会话注记。
func (p *Plugin) runPlan(ctx context.Context, tag, sid, today string) {
	defer p.wg.Done()
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
	input := buildPlanPrompt(s.lang, p.now(), todayCs)

	for attempt := 0; ; attempt++ {
		tctx, cancel := context.WithTimeout(ctx, runTimeout)
		tctx = plugin.WithEphemeralInput(tctx)
		_, err := s.runTurn(tctx, sid, input)
		cancel()
		switch {
		case err == nil:
			pl, lerr := st.LoadPlan()
			if lerr == nil && pl.Date == today {
				return
			}
			p.countAttempt(nctx, st, sid, today, "模型没有提交日程表")
			return
		case errors.Is(err, plugin.ErrSessionBusy):
			if attempt+1 >= defBusyRetryMax {
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
