package heartbeat

import (
	"context"
	"errors"
	"strings"
	"time"

	"wen/internal/i18n"

	"wen/internal/cue"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

// loop 是心跳插件唯一的常驻 goroutine：按当前间隔发心跳、按固定周期做空闲衰减。
// 下一次心跳时刻始终按「上次心跳 + 当前间隔」推算，衰减 tick 与间隔调整都不会
// 重置倒计时——否则每 15 分钟一次的衰减检查会让更长的心跳间隔永远走不完。
func (p *Plugin) loop(ctx context.Context) {
	defer p.wg.Done()
	decay := time.NewTicker(decayEvery)
	defer decay.Stop()

	for {
		p.mu.Lock()
		next := p.nextBeatLocked()
		// 睡着时要整理上下文的话，那一刻总在暂停结束（也就是下一拍）之前
		due := p.sleepCompactDueLocked()
		tidy := !due.IsZero() && due.Before(next)
		if tidy {
			next = due
		}
		p.mu.Unlock()

		timer := time.NewTimer(time.Until(next)) // 已过期时零延时触发
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-p.wake: // 间隔被动态调整，重算下一次心跳时刻
			timer.Stop()
		case <-decay.C:
			timer.Stop()
			p.maybeDecay()
		case <-timer.C:
			if tidy {
				p.sleepCompact(ctx)
			} else {
				p.beat(ctx)
			}
		}
	}
}

// nextBeatLocked 推算下一次心跳时刻：常规是「上次心跳 + 当前间隔」，暂停把它
// 压后到暂停结束——暂停不改间隔本身，到点后按原节奏继续。调用方需持有 p.mu。
//
// 公告板上有待说的开口理由时，把下一拍提前到「上次心跳 + 最快间隔」：理由驱动的
// 开口不该等满一个常规周期，「刚下起雨」等一小时再说就凉了。只在动态节奏下提前
// （固定节奏的含义就是雷打不动）；暂停仍然压过一切——睡着的人不为一场雨叫醒，
// 理由等不到那时自会过期。
func (p *Plugin) nextBeatLocked() time.Time {
	next := p.lastBeat.Add(p.cur)
	if p.dynamic && cue.Pending(time.Now()) {
		if c := p.lastBeat.Add(p.minIv); c.Before(next) {
			next = c
		}
	}
	if p.pausedUntil.After(next) {
		return p.pausedUntil
	}
	return next
}

// resetClockLocked 把心跳倒计时的起点推到 at，并唤醒循环重算下一次心跳时刻。
// 只前推不回拨：事件时间戳缺失或落后于已记录的起点时保持原值，避免把倒计时拨回过去
// 反而立刻触发一次心跳。调用方需持有 p.mu。
func (p *Plugin) resetClockLocked(at time.Time) {
	if at.IsZero() {
		at = time.Now()
	}
	if !at.After(p.lastBeat) {
		return
	}
	p.lastBeat = at
	select { // 循环正挂在旧的到期时刻上，不叫醒它这次重置要到下次心跳后才生效
	case p.wake <- struct{}{}:
	default:
	}
}

// beat 执行一次心跳：挑最近活跃的会话（没有就新建），以心跳提示词跑一轮对话。
// 会话忙时跳过本次——心跳排队毫无意义，下个周期自然会再来。
func (p *Plugin) beat(ctx context.Context) {
	p.mu.Lock()
	now := time.Now()
	// 暂停到点后的第一拍是「睡醒」：节奏恢复到基础值——睡前的快慢都属于睡前那段
	// 对话，人睡一觉起来回到平常状态。与 OnTurnEnd 的「见人恢复常态」是同一条规则，
	// 只是醒来这一次连快节奏也不保留：隔了一觉，热聊的势头已经不在了。
	if p.dynamic && !p.pausedUntil.IsZero() && !now.Before(p.pausedUntil) {
		p.cur = p.normalize(p.base)
		p.adjusted = false
		p.clearPauseLocked()
	}
	p.lastBeat = now
	runTurn, beatPrompt, cur := p.runTurn, p.beatPromptLocked(), p.cur
	lang := p.lang
	dir, st := p.snapshotStateLocked()
	p.mu.Unlock()

	// 记下这次心跳发生的时刻——重启后就是靠它推算下一次的
	persistState(dir, st)

	sid, lastActive, err := p.pickSession()
	if err != nil {
		i18n.Logf(p.langOf(), "log.heartbeat.no_session", "heartbeat: 找不到可用会话，本次心跳跳过: %v", err)
		return
	}
	tctx, cancel := context.WithTimeout(ctx, beatTimeout)
	defer cancel()
	// 心跳提示词是一次性输入：只发给当轮模型，不留在后续上下文，界面不按用户消息展示
	tctx = plugin.WithEphemeralInput(tctx)
	cues := cue.Take(time.Now())
	input := withCues(lang, gapNote(lang, beatPrompt, lastActive, time.Now(), cur), cues)
	if _, err := runTurn(tctx, sid, input); err != nil {
		// 理由没送达就放回公告板等下一拍（已过期的会被 Post 拒收），别让一次
		// 会话忙把「刚下起雨」整个吞掉
		for _, c := range cues {
			cue.Post(c)
		}
		if errors.Is(err, plugin.ErrSessionBusy) {
			i18n.Logf(p.langOf(), "log.heartbeat.session_busy", "heartbeat: 会话 %s 忙，本次心跳跳过", sid)
		} else if ctx.Err() == nil { // 停止时的取消错误不值得记
			i18n.Logf(p.langOf(), "log.heartbeat.turn_failed", "heartbeat: 心跳轮次失败: %v", err)
		}
	}
}

// withCues 把待说的开口理由附在心跳输入末尾。理由给出的是「发生了什么」，
// 说不说、怎么说仍归模型——与当下不相干的理由硬播出来比沉默更出戏。
func withCues(lang, beatPrompt string, cues []cue.Cue) string {
	if len(cues) == 0 {
		return beatPrompt
	}
	var b strings.Builder
	b.WriteString(beatPrompt)
	b.WriteString(prompt.T(lang, "prompt.heartbeat.cues_header", "\n\n【值得开口的事】"))
	for _, c := range cues {
		b.WriteString("\n- ")
		b.WriteString(c.Text)
	}
	b.WriteString(prompt.T(lang, "prompt.heartbeat.cues_tail",
		"\n值得说就自然地提起，不逐条播报；与当下不相干的可以不提。"))
	return b.String()
}

// pickSession 返回最近活跃的会话，一个都没有时新建一个。
// 挑选规则与第二个返回值的含义见 SessionQuery.LastActive——那条规则同时被
// 定时任务插件用着，放在核心里只写一遍。
func (p *Plugin) pickSession() (string, time.Time, error) {
	p.mu.Lock()
	sessions, newSession := p.sessions, p.newSession
	p.mu.Unlock()

	id, activeAt, err := sessions.LastActive()
	if err != nil {
		return "", time.Time{}, err
	}
	if id != "" {
		return id, activeAt, nil
	}
	if newSession == nil {
		return "", time.Time{}, errors.New("没有会话且当前环境不支持新建")
	}
	sid, err := newSession()
	return sid, time.Time{}, err // 刚建的会话没有“上次对话”可言
}

// coldWakeNoteZH 是冷唤醒的重定向提示。间隔一长，会话里最后的话题早已过时，
// 顺着上文接话只会显得没睡醒；先回顾再开口才接得自然。
const coldWakeNoteZH = "距上次对话已久：先回顾已保存的记忆与最近经历，再决定要不要开口、说什么，不要顺着久远的上文接话。"

// coldWakeAfter 是冷唤醒的最低门槛；实际门槛取它与三个当前间隔中的较大者——
// 心跳本来就以天计的人，半天不算「久」。
const coldWakeAfter = 12 * time.Hour

// gapNote 在心跳提示词末尾附上距上次真人对话的时长。
//
// 模型能从环境块知道“现在几点”，却无从得知“上一条消息是什么时候”——历史消息
// 进上下文时只带 role 与 content，时间戳留在了盘上。没有这一行，提示词只能写“很久
// 没聊了”这类写死的模糊措辞，而刚聊完五分钟与隔了一夜显然该说不同的话。
//
// 时间未知或不足一分钟时不附：“未知”只是噪声，而刚聊完就心跳本就不应发生（真人
// 轮次会重置心跳时钟）。间隔超过冷唤醒门槛时另附一句重定向提示。
func gapNote(lang, beatPrompt string, lastActive, now time.Time, cur time.Duration) string {
	if lastActive.IsZero() {
		return beatPrompt
	}
	gap := now.Sub(lastActive)
	if gap < time.Minute {
		return beatPrompt
	}
	out := beatPrompt + prompt.Tf(lang, "prompt.heartbeat.gap_note", "\n\n【距上次对话】%s",
		humanDur(lang, gap))
	if gap >= max(coldWakeAfter, 3*cur) {
		out += "\n" + prompt.T(lang, "prompt.heartbeat.cold_wake", coldWakeNoteZH)
	}
	return out
}

// maybeDecay 空闲衰减：距最近真人交互超过一个当前间隔时，把间隔放缓一档（×1.5），
// 直到最慢间隔。动态心跳关闭时不衰减——那是「固定节奏」的含义。
// 暂停期间也不衰减：这段安静是模型自己定下的，不是「没人想聊」的证据，
// 衰减再计一次就是把同一份沉默记了两笔账（暂停到点由 beat 恢复成基础节奏）。
// 豁免只覆盖暂停本身——到点之后若仍无人来聊，又是自然安静，衰减照常恢复计数。
func (p *Plugin) maybeDecay() {
	p.mu.Lock()
	if !p.dynamic || p.cur >= p.maxIv || time.Now().Before(p.pausedUntil) {
		p.mu.Unlock()
		return
	}
	if !p.lastActive.IsZero() && time.Since(p.lastActive) < p.cur {
		p.mu.Unlock()
		return
	}
	p.cur = p.normalize(p.cur * 3 / 2)
	p.adjusted = true
	next := p.cur
	dir, st := p.snapshotStateLocked()
	p.mu.Unlock()

	// 写盘放在锁外：持锁写会自锁，而为了绕开自锁另起 goroutine
	// 就会让这次写脱离循环的生命周期（Stop 等不到它）
	persistState(dir, st)
	i18n.Logf(p.langOf(), "log.heartbeat.decayed", "heartbeat: 无人聊天，心跳放缓至 %v", next)
}

// OnTurnEnd 观察每轮对话：真人交互的轮次刷新活跃时间并重置心跳时钟。
// 后台轮次（含心跳自己）一律忽略——否则心跳会不断自我续命。
// 本方法在轮次收尾的同步路径上被调用，必须快速返回。
//
// 节奏规则：**见人恢复常态**。衰减攒出来的慢节奏（或模型设的「她今天忙」式慢速）
// 在真人开口的那一刻就过时了，机械恢复到基础间隔；仍然忙的话模型看得到对话，会
// 再设一次——那正是它实测中最可靠触发的场景。比基础值快的设定保留：热聊中调快
// 的节奏不该被一条新消息打回去。往快超过常态是带社交判断的决定，仍归模型。
func (p *Plugin) OnTurnEnd(_ context.Context, ev plugin.TurnEndEvent) {
	if ev.Origin != "" || !ev.Interactive {
		return
	}
	p.mu.Lock()
	p.lastActive = ev.EndedAt
	// 真人刚聊完，心跳倒计时从此刻重新开始：心跳是「没人说话时才主动开口」的机制，
	// 聊天途中插进来的心跳既打断对话，也让间隔配置失去意义。
	p.resetClockLocked(ev.EndedAt)
	// 状态变化要落盘，但只在确实变了时才写——本方法在轮次收尾的同步路径上，
	// 不为每轮都付一次写盘。
	persist := false
	if p.dynamic && p.cur > p.base {
		p.cur = p.normalize(p.base)
		p.adjusted = false
		persist = true
	}
	// 人来说话就是醒了：暂停即刻作废（不落盘的话重启后暂停会复活）。
	// 只作废**这一轮开始之前**设下的暂停：对方说晚安、角色在同一轮里暂停到早上，是这一轮
	// 的结论，不是被这一轮打断的——此前这里不分先后一律清掉，于是在真人对话里设下的暂停
	// 全都活不过当轮。StartedAt 缺失（旧的调用方）时按旧规则处理。
	if !p.pausedUntil.IsZero() && (ev.StartedAt.IsZero() || p.pausedAt.Before(ev.StartedAt)) {
		p.clearPauseLocked()
		persist = true
	}
	var dir string
	var st state
	if persist {
		dir, st = p.snapshotStateLocked()
	}
	p.mu.Unlock()
	if persist {
		persistState(dir, st)
	}
}
