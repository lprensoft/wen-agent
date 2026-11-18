package agenda

import (
	"context"
	"errors"
	"time"

	"wen/internal/cue"
	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

// backCueTTL 是「刚回来」这条开口理由的有效期，与天气转变同一口径。
const backCueTTL = 90 * time.Minute

// 活动轮次的种类。
type activityKind int

const (
	kindStart   activityKind = iota // 到点了，决定去不去
	kindEnd                         // 该结束了，写一句经历
	kindCatchUp                     // 离线期间整项都过去了，补记一句
)

// name 是这一档在会话注记与日志里的说法，跟随语言。
func (k activityKind) name(lang string) string {
	switch k {
	case kindStart:
		return i18n.T(lang, "plugin.agenda.kind.start", "开始")
	case kindCatchUp:
		return i18n.T(lang, "plugin.agenda.kind.catch_up", "补记")
	}
	return i18n.T(lang, "plugin.agenda.kind.end", "结束")
}

// loop 是时钟：算出最近一个到点时刻定时等待（30 秒兜底），到点派发。派发只改状态与
// 起 goroutine，从不在本循环里跑轮次。表改动经 wake 通道重算。
func (p *Plugin) loop(ctx context.Context, wake <-chan struct{}) {
	defer p.wg.Done()
	for {
		timer := time.NewTimer(p.untilNext())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wake:
			timer.Stop()
		case <-timer.C:
			p.tickAt(ctx, p.now())
		}
	}
}

// NextBackgroundAt 返回下一次需要调度维护工作的时刻。
// 查询负责预测，执行（tickAt）负责判断是否到期。
func (p *Plugin) NextBackgroundAt(now time.Time) (time.Time, bool) {
	return p.nextDueAt(now)
}

// RunBackground 同步执行 now 时刻到期的维护工作；手动时间线下由评测调用。
func (p *Plugin) RunBackground(ctx context.Context, now time.Time) error {
	return p.tickAt(ctx, now)
}

// untilNext 返回距最近一个到点时刻的时长：未派发的开始、已开始未结束的结束、与对方
// 约定的提前提醒、忙碌清理、承诺收束；启动宽限期内不早于宽限结束。
func (p *Plugin) untilNext() time.Duration {
	now := p.now()
	wait := pollFallback
	if next, ok := p.nextDueAt(now); ok {
		if d := next.Sub(now); d < wait {
			wait = d
		}
	}
	if wait < 0 {
		wait = 0
	}
	p.mu.RLock()
	graceEnd := p.startedAt.Add(p.startupGrace)
	p.mu.RUnlock()
	if g := graceEnd.Sub(now); g > wait {
		wait = g
	}
	return wait
}

// nextDueAt 预测下一次维护工作的时刻。
//
// 活动模型轮次（开始/结束/补记）只受 runActivities 控制；提前提醒、忙碌状态清理与
// 承诺收束各自按自己的条件调度——关闭自动活动轮次不等于关掉整张表。
// 已到期的候选钳到 now，交由 tickAt 判断并消费。
func (p *Plugin) nextDueAt(now time.Time) (time.Time, bool) {
	s := p.snapshot()
	day := s.today(now)
	today := day.Format(dateLayout)

	var next time.Time
	consider := func(t time.Time) {
		if t.Before(now) {
			t = now
		}
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	for _, tag := range p.allDomains() {
		pl, err := p.storeFor(tag).LoadPlan()
		if err != nil || pl.Date != today {
			continue
		}
		for i := range pl.Items {
			it := &pl.Items[i]
			start, end := it.span(day, s.dayStartHour)

			// 与对方的约定快到了：投一条开口理由，到 start 为止有效。
			// 不受 runActivities 控制；已过 start 就不再提醒。
			if it.WithUser && it.Status == statusPlanned && it.SoonFired.IsZero() &&
				s.remindBefore > 0 && start.After(now) {
				consider(start.Add(-s.remindBefore))
			}
			// 进行中的项到点了：撤掉忙碌状态。用独立的 BusyCleared，不借用结束轮次标记。
			if it.Status == statusOngoing && !it.BusyCleared {
				consider(end)
			}
			if !s.runActivities {
				continue
			}
			switch {
			case it.Status == statusPlanned && it.StartFired.IsZero():
				consider(start)
			case (it.Status == statusPlanned || it.Status == statusOngoing) && it.EndFired.IsZero():
				consider(end)
			}
		}
	}
	// 承诺过期收束：没有活动的日子也要能触发。
	if at, ok := p.nextPromiseSweepAt(now); ok {
		consider(at)
	}
	return next, !next.IsZero()
}

// nextPromiseSweepAt 返回最早一条待收束承诺的日始时刻；已过期的返回 now。
func (p *Plugin) nextPromiseSweepAt(now time.Time) (time.Time, bool) {
	s := p.snapshot()
	var next time.Time
	for _, tag := range p.allDomains() {
		st := p.storeFor(tag)
		if st == nil {
			continue
		}
		list, err := st.LoadPromises()
		if err != nil {
			continue
		}
		for _, pr := range list {
			if pr.settled() {
				continue
			}
			d, err := time.ParseInLocation(dateLayout, pr.Date, now.Location())
			if err != nil {
				continue
			}
			sweep := d.AddDate(0, 0, promiseGraceDays+1)
			at := time.Date(sweep.Year(), sweep.Month(), sweep.Day(), s.dayStartHour, 0, 0, 0, now.Location())
			if at.Before(now) {
				at = now
			}
			if next.IsZero() || at.Before(next) {
				next = at
			}
		}
	}
	return next, !next.IsZero()
}

// due 是一次派发决定。
type due struct {
	tag  string
	item Item
	kind activityKind
}

// tickAt 扫全部域的表，把到点的项标记后派发，并执行提醒、忙碌清理与承诺收束。
// 标记（start_fired / end_fired）先落盘再跑轮次：重启不重发，轮次跑多久都不会被
// 重复派发。启动宽限期内什么都不做——刚起来的那一秒人多半正要自己说话。
func (p *Plugin) tickAt(ctx context.Context, now time.Time) error {
	s := p.snapshot()
	p.mu.RLock()
	graceEnd := p.startedAt.Add(p.startupGrace)
	manual := p.manual
	p.mu.RUnlock()
	if now.Before(graceEnd) {
		return nil
	}
	dispatch := s.runActivities && s.runTurn != nil
	day := s.today(now)
	today := day.Format(dateLayout)

	var dues []due
	for _, tag := range p.allDomains() {
		st := p.storeFor(tag)
		_, err := st.UpdatePlan(func(pl *Plan) (bool, error) {
			if pl.Date != today {
				return false, nil
			}
			changed := false
			for i := range pl.Items {
				it := &pl.Items[i]
				start, end := it.span(day, s.dayStartHour)
				// 进行中的项到点了：不管派不派轮次，忙碌状态都该撤。撤过就记下，
				// 不再重复调度。
				if it.Status == statusOngoing && !end.After(now) && !it.BusyCleared {
					clearAvailability()
					it.BusyCleared = true
					changed = true
				}
				// 与对方的约定快到了：投一条开口理由，到 start 为止有效
				if it.WithUser && it.Status == statusPlanned && s.remindBefore > 0 && it.SoonFired.IsZero() &&
					start.After(now) && start.Sub(now) <= s.remindBefore {
					it.SoonFired = now
					changed = true
					cue.Post(cue.Cue{Source: availabilitySource, Key: soonKey(tag, it.ID), Domain: tag,
						Text: prompt.Tf(s.lang, "prompt.agenda.cue.soon", "再过 %s是和对方的约定：%s（%s）。",
							humanMinutes(s.lang, start.Sub(now)), it.Title, it.Start),
						Expire: start})
				}
				if !dispatch {
					continue
				}
				switch {
				case it.Status == statusPlanned && it.StartFired.IsZero() && !start.After(now):
					it.StartFired = now
					changed = true
					if !end.After(now) {
						// 整项都过去了：跳过开始，直接补记一次结束
						it.EndFired = now
						dues = append(dues, due{tag, *it, kindCatchUp})
					} else {
						dues = append(dues, due{tag, *it, kindStart})
					}
				case (it.Status == statusPlanned || it.Status == statusOngoing) && !it.StartFired.IsZero() &&
					it.EndFired.IsZero() && !end.After(now):
					it.EndFired = now
					changed = true
					dues = append(dues, due{tag, *it, kindEnd})
				}
			}
			return changed, nil
		})
		if err != nil {
			i18n.Logf(p.langOf(), "log.agenda.scan_failed", "agenda: 扫表失败: %v", err)
		}
	}
	p.sweepPromises(ctx, day, now)

	var firstErr error
	for _, d := range dues {
		if manual {
			if err := p.runActivity(ctx, d, now); err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		p.wg.Add(1)
		go func(d due) {
			defer p.wg.Done()
			_ = p.runActivity(ctx, d, now)
		}(d)
	}
	return firstErr
}

// tick 是既有入口：按当前时钟扫一遍。
func (p *Plugin) tick(ctx context.Context) { _ = p.tickAt(ctx, p.now()) }

// sweepPromises 把过了宽限期还没了结的事标成「没做成」，并为每条留一行注记。
//
// 不这么做的话，台账里会攒下一批永远 pending 的条目：注入侧已经按日期把它们滤掉了，
// 于是模型再也看不见，而它们仍然占着条数上限、也仍然出现在 list_promises 里，
// 表现成「明明了结过却还挂着」。收束改的是状态，因此天然幂等——扫过一遍之后
// 它们不再是 pending，下一拍什么都不会发生，不需要另外记「上次扫到哪天」。
//
// 注记而不是开口理由：这是给人看的一笔账，不该变成角色主动找上来说的话。
func (p *Plugin) sweepPromises(ctx context.Context, day, now time.Time) {
	cutoff := day.AddDate(0, 0, -promiseGraceDays).Format(dateLayout)
	var missed []Promise
	for _, tag := range p.allDomains() {
		if _, err := p.storeFor(tag).UpdatePromises(func(ps *[]Promise, _ func() string) (bool, error) {
			changed := false
			for i := range *ps {
				pr := &(*ps)[i]
				if pr.settled() || pr.Date >= cutoff {
					continue
				}
				pr.Status, pr.Settled = promiseMissed, now
				missed = append(missed, *pr)
				changed = true
			}
			return changed, nil
		}); err != nil {
			i18n.Logf(p.langOf(), "log.agenda.sweep_failed", "agenda: 收束答应过的事失败: %v", err)
		}
	}
	if len(missed) == 0 {
		return
	}
	s := p.snapshot()
	if s.sessions == nil {
		return
	}
	sid, _, err := s.sessions.LastActive()
	if err != nil || sid == "" {
		return
	}
	for _, pr := range missed {
		p.postNotice(ctx, sid, i18n.Tf(s.lang, "plugin.agenda.notice.promise_missed",
			"%s答应的「%s」（%s）到期没有了结，已记为没做成。",
			promiseByShort(s.lang, pr.By), pr.Title, fmtDateCN(s.lang, pr.Date, now.Location())))
	}
}

// runActivity 跑一次开始 / 结束轮次：落在最近活跃的会话上，一个都没有时不新建
// ——没人看的开始与结束只进文件。会话忙每分钟重试，超出上限放弃并留注记。
func (p *Plugin) runActivity(ctx context.Context, d due, now time.Time) (err error) {
	defer func() {
		if r := recover(); r != nil {
			i18n.Logf(p.langOf(), "log.agenda.activity_panic", "agenda: %s「%s」的%s轮次 panic（已隔离）: %v", d.item.ID, d.item.Title, d.kind.name(p.langOf()), r)
			err = nil
		}
	}()
	s := p.snapshot()
	if s.runTurn == nil || s.sessions == nil {
		return nil
	}
	sid, _, err := s.sessions.LastActive()
	if err != nil || sid == "" {
		i18n.Logf(p.langOf(), "log.agenda.no_session", "agenda: 没有可用会话，「%s」的%s轮次跳过", d.item.Title, d.kind.name(p.langOf()))
		return nil
	}
	day := s.today(now)
	var input string
	switch d.kind {
	case kindStart:
		start, _ := d.item.span(day, s.dayStartHour)
		input = buildStartPrompt(s.lang, now, d.item, now.Sub(start), s.startGrace)
	case kindEnd:
		input = buildEndPrompt(s.lang, now, d.item, false)
	default:
		input = buildEndPrompt(s.lang, now, d.item, true)
	}
	nctx := plugin.WithScope(ctx, plugin.Scope{Write: d.tag})

	manual := p.isManual()
	for attempt := 0; ; attempt++ {
		tctx, cancel := context.WithTimeout(ctx, runTimeout)
		tctx = plugin.WithEphemeralInput(tctx)
		var runErr error
		_, runErr = s.runTurn(tctx, sid, input)
		cancel()
		switch {
		case runErr == nil:
			if d.kind != kindStart {
				p.postBackCue(d)
			}
			return nil
		case errors.Is(runErr, plugin.ErrTurnNotStarted):
			p.rollbackActivity(d.tag, d.item.ID, d.kind)
			return runErr
		case errors.Is(runErr, plugin.ErrSessionBusy):
			if manual || attempt+1 >= defBusyRetryMax {
				i18n.Logf(p.langOf(), "log.agenda.session_busy", "agenda: 会话 %s 持续繁忙，「%s」的%s轮次放弃", sid, d.item.Title, d.kind.name(p.langOf()))
				p.postNotice(nctx, sid, i18n.Tf(s.lang, "plugin.agenda.notice.busy_gave_up",
					"「%s」的%s轮次因会话持续繁忙而放弃。", d.item.Title, d.kind.name(s.lang)))
				return nil
			}
			select {
			case <-time.After(p.busyRetry):
			case <-ctx.Done():
				return nil
			}
		default:
			if ctx.Err() == nil {
				i18n.Logf(p.langOf(), "log.agenda.activity_failed", "agenda: 「%s」的%s轮次失败: %v", d.item.Title, d.kind.name(p.langOf()), runErr)
				// 轮次挂了不等于什么都没发生：经历是工具当场落盘的，模型可能已经写好了，
				// 只是收尾那次模型调用没回来（实际见过：done 已经记上，随后的调用等了
				// 六十秒没等到响应头）。这条理由本来就只看落盘的结果（见 postBackCue），
				// 所以成败都投——那一项确实已经过去了，人也确实该知道她回来了。
				if d.kind != kindStart {
					p.postBackCue(d)
				}
			}
			return nil
		}
	}
}

// rollbackActivity 把「轮次未开始」的活动恢复成待派发，保持待处理。
func (p *Plugin) rollbackActivity(tag, id string, kind activityKind) {
	st := p.storeFor(tag)
	if st == nil {
		return
	}
	_, _ = st.UpdatePlan(func(pl *Plan) (bool, error) {
		it := pl.item(id)
		if it == nil {
			return false, nil
		}
		switch kind {
		case kindStart:
			it.StartFired = time.Time{}
		case kindEnd:
			it.EndFired = time.Time{}
		case kindCatchUp:
			it.StartFired, it.EndFired = time.Time{}, time.Time{}
		}
		return true, nil
	})
}

// isManual 报告当前是否运行在手动时间线上。
func (p *Plugin) isManual() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.manual
}

// postBackCue 在结束轮次成功后投一条「刚回来」的开口理由，不依赖模型是否调了工具：
// 经历以它落盘的版本为准，没写就只说回来了。
func (p *Plugin) postBackCue(d due) {
	lang := p.langOf()
	text := prompt.Tf(lang, "prompt.agenda.cue.back", "你刚结束「%s」（%s 回来）", d.item.Title, d.item.End)
	if st := p.storeFor(d.tag); st != nil {
		if pl, err := st.LoadPlan(); err == nil {
			if it := pl.item(d.item.ID); it != nil && it.Outcome != "" && it.Status == statusDone {
				text += prompt.Tf(lang, "prompt.agenda.cue.back_outcome", "：%s", it.Outcome)
			}
		}
	}
	cue.Post(cue.Cue{Source: availabilitySource, Key: backKey(d.tag, d.item.ID), Domain: d.tag, Text: text + "。", Expire: p.now().Add(backCueTTL)})
}

// soonKey 是「快到了」开口理由的键：带可见域与活动 ID。
//
// 两个域的日程各自都从 a1 编号，只带 ID 的键会让撤回一个域的提醒时
// 连另一个域那条一起撤掉。
func soonKey(tag, id string) string { return "soon|" + tag + "|" + id }

// backKey 是「刚结束一项」开口理由的键，与 soonKey 同形：带域，避免两个域的
// 同名项互相覆盖。
func backKey(tag, id string) string { return "back|" + tag + "|" + id }
