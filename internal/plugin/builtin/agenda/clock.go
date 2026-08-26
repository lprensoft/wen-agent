package agenda

import (
	"context"
	"errors"
	"log"
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

// String 只给日志用，保持中文。给人看的那一处走 name。
func (k activityKind) String() string {
	switch k {
	case kindStart:
		return "开始"
	case kindCatchUp:
		return "补记"
	}
	return "结束"
}

// name 是会话注记里的说法，跟随界面语言。
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
			p.tick(ctx)
		}
	}
}

// untilNext 返回距最近一个到点时刻的时长：未派发的开始、已开始未结束的结束、与对方
// 约定的提前提醒；启动宽限期内不早于宽限结束。
func (p *Plugin) untilNext() time.Duration {
	s := p.snapshot()
	p.mu.RLock()
	graceEnd := p.startedAt.Add(p.startupGrace)
	p.mu.RUnlock()
	now := p.now()
	day := s.today(now)
	today := day.Format(dateLayout)

	wait := pollFallback
	consider := func(t time.Time) {
		if d := t.Sub(now); d < wait {
			wait = d
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
			switch {
			case it.Status == statusPlanned && it.StartFired.IsZero():
				consider(start)
				if it.WithUser && s.remindBefore > 0 && it.SoonFired.IsZero() {
					consider(start.Add(-s.remindBefore))
				}
			case (it.Status == statusPlanned || it.Status == statusOngoing) && it.EndFired.IsZero():
				consider(end)
			}
		}
	}
	if wait < 0 {
		wait = 0
	}
	if g := graceEnd.Sub(now); g > wait {
		wait = g
	}
	return wait
}

// due 是一次派发决定。
type due struct {
	tag  string
	item Item
	kind activityKind
}

// tick 扫全部域的表，把到点的项标记后派发。标记（start_fired / end_fired）先落盘再跑
// 轮次：重启不重发，轮次跑多久都不会被重复派发。启动宽限期内什么都不做——刚起来那
// 一秒人多半正要自己说话。
func (p *Plugin) tick(ctx context.Context) {
	s := p.snapshot()
	p.mu.RLock()
	graceEnd := p.startedAt.Add(p.startupGrace)
	p.mu.RUnlock()
	now := p.now()
	if now.Before(graceEnd) {
		return
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
				// 进行中的项到点了：不管派不派轮次，忙碌状态都该撤
				if it.Status == statusOngoing && !end.After(now) {
					clearAvailability()
				}
				// 与对方的约定快到了：投一条开口理由，到 start 为止有效
				if it.WithUser && it.Status == statusPlanned && s.remindBefore > 0 && it.SoonFired.IsZero() &&
					start.After(now) && start.Sub(now) <= s.remindBefore {
					it.SoonFired = now
					changed = true
					cue.Post(cue.Cue{Source: availabilitySource, Key: "soon|" + it.ID,
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
			log.Printf("agenda: 扫表失败: %v", err)
		}
	}
	p.sweepPromises(ctx, day)

	for _, d := range dues {
		p.wg.Add(1)
		go p.runActivity(ctx, d, now)
	}
}

// sweepPromises 把过了宽限期还没了结的事标成「没做成」，并为每条留一行注记。
//
// 不这么做的话，台账里会攒下一批永远 pending 的条目：注入侧已经按日期把它们滤掉了，
// 于是模型再也看不见，而它们仍然占着条数上限、也仍然出现在 list_promises 里，
// 表现成「明明了结过却还挂着」。收束改的是状态，因此天然幂等——扫过一遍之后
// 它们不再是 pending，下一拍什么都不会发生，不需要另外记「上次扫到哪天」。
//
// 注记而不是开口理由：这是给人看的一笔账，不该变成角色主动找上来说的话。
func (p *Plugin) sweepPromises(ctx context.Context, day time.Time) {
	cutoff := day.AddDate(0, 0, -promiseGraceDays).Format(dateLayout)
	now := p.now()
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
			log.Printf("agenda: 收束答应过的事失败: %v", err)
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
func (p *Plugin) runActivity(ctx context.Context, d due, now time.Time) {
	defer p.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("agenda: %s「%s」的%s轮次 panic（已隔离）: %v", d.item.ID, d.item.Title, d.kind, r)
		}
	}()
	s := p.snapshot()
	if s.runTurn == nil || s.sessions == nil {
		return
	}
	sid, _, err := s.sessions.LastActive()
	if err != nil || sid == "" {
		log.Printf("agenda: 没有可用会话，「%s」的%s轮次跳过", d.item.Title, d.kind)
		return
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

	for attempt := 0; ; attempt++ {
		tctx, cancel := context.WithTimeout(ctx, runTimeout)
		tctx = plugin.WithEphemeralInput(tctx)
		_, err := s.runTurn(tctx, sid, input)
		cancel()
		switch {
		case err == nil:
			if d.kind != kindStart {
				p.postBackCue(d)
			}
			return
		case errors.Is(err, plugin.ErrSessionBusy):
			if attempt+1 >= defBusyRetryMax {
				log.Printf("agenda: 会话 %s 持续繁忙，「%s」的%s轮次放弃", sid, d.item.Title, d.kind)
				p.postNotice(nctx, sid, i18n.Tf(s.lang, "plugin.agenda.notice.busy_gave_up",
					"「%s」的%s轮次因会话持续繁忙而放弃。", d.item.Title, d.kind.name(s.lang)))
				return
			}
			select {
			case <-time.After(p.busyRetry):
			case <-ctx.Done():
				return
			}
		default:
			if ctx.Err() == nil {
				log.Printf("agenda: 「%s」的%s轮次失败: %v", d.item.Title, d.kind, err)
				// 轮次挂了不等于什么都没发生：经历是工具当场落盘的，模型可能已经写好了，
				// 只是收尾那次模型调用没回来（实际见过：done 已经记上，随后的调用等了
				// 六十秒没等到响应头）。这条理由本来就只看落盘的结果（见 postBackCue），
				// 所以成败都投——那一项确实已经过去了，人也确实该知道她回来了。
				if d.kind != kindStart {
					p.postBackCue(d)
				}
			}
			return
		}
	}
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
	cue.Post(cue.Cue{Source: availabilitySource, Key: "back|" + d.item.ID, Text: text + "。", Expire: p.now().Add(backCueTTL)})
}
