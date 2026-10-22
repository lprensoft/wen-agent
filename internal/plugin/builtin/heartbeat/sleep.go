package heartbeat

import (
	"context"
	"errors"
	"time"

	"wen/internal/availability"
	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

// 睡觉：暂停时声明「你自己也在睡」，这段暂停就多出两层意思。
//
// 一是**睡着时整理上下文**。压缩是一次有损的整理，放在聊天中途（自动压缩在用量到
// 九成时触发）既让人干等，又在场景正热的时候把它切断；放在角色睡着、没人在等回复的
// 时候做正合适。醒来时上下文短了，身体那边按「醒着的时长」算出的困意也随之消退——
// 睡一觉对应的正是压缩。只在睡下一阵之后做（刚说完晚安，人可能还会回一句），且一觉
// 只做一次；被叫醒就作罢。
//
// 二是**被叫醒**。对方来消息时暂停会被取消，但那一轮模型并不知道自己刚才在睡觉，
// 于是醒得毫无痕迹。这里只交代事实（你在睡、被这条消息叫醒了），怎么回归模型。

const (
	defSleepCompactPercent = 30

	// sleepSettle 是睡下之后多久才整理上下文：说完晚安的那几分钟里对方常常还会回一句，
	// 那一句会把人叫醒，整理也就不必做了。
	sleepSettle = 20 * time.Minute

	// compactRetry 是会话正忙时下一次再试的间隔。
	compactRetry = 10 * time.Minute
)

// availSource 是心跳在忙碌公告板上的写入方名。
const availSource = "heartbeat"

// busyGrace 是忙完之后再等多久才主动开口：日程在活动结束时自己会跑一轮写经历，
// 心跳卡在同一刻只会撞上会话忙、白白跳过一拍。
const busyGrace = 5 * time.Minute

// publishSleepLocked 把「睡着」同步到忙碌公告板：自己睡着时宣布「不回」到醒来时刻，
// 否则撤掉。消息通道据此扣住入站消息，过一阵才算被吵醒（只是对方不在、自己没睡的
// 暂停不写——她本人并不忙）。调用方需持有 p.mu。
//
// 持锁写是安全的：心跳自己的订阅回调只往叫醒通道里投一下、不取 p.mu（见 Init），
// 其它订阅方的回调按公告板的约定只碰自己的锁。
func (p *Plugin) publishSleepLocked() {
	if p.asleep && p.pausedUntil.After(time.Now()) {
		availability.Set(availability.State{
			Source:   availSource,
			Activity: prompt.T(p.lang, "prompt.heartbeat.availability.asleep", "睡觉"),
			Level:    availability.Away,
			Since:    p.pausedAt,
			Until:    p.pausedUntil,
			Sleeping: true,
		})
		return
	}
	availability.Clear(availSource)
}

// clearPauseLocked 结束暂停，连同这一觉的状态一起清掉，并换一个代号——锁外还在跑的
// 整理任务回来时据此知道那一觉已经结束。调用方需持有 p.mu。
func (p *Plugin) clearPauseLocked() {
	p.pausedUntil, p.pausedAt = time.Time{}, time.Time{}
	p.asleep, p.sleepCompacted = false, false
	p.compactRetryAt = time.Time{}
	p.sleepGen++
}

// sleepCompactDueLocked 返回这一觉该整理上下文的时刻；不需要整理时返回零值。
// 调用方需持有 p.mu。
//
// 已经到了醒来时刻的一觉不再整理：电脑休眠跨过整段睡眠再恢复时，循环会同时看到
// 「早该整理」与「早该醒」，先补一次整理只会推迟醒来、白付一次模型调用。
func (p *Plugin) sleepCompactDueLocked() time.Time {
	if !p.dynamic || !p.asleep || p.sleepCompacted || p.compactPct <= 0 || p.compact == nil || p.status == nil {
		return time.Time{}
	}
	if !time.Now().Before(p.pausedUntil) {
		return time.Time{}
	}
	due := p.pausedAt.Add(sleepSettle)
	if p.compactRetryAt.After(due) {
		due = p.compactRetryAt
	}
	if !due.Before(p.pausedUntil) { // 醒得比睡熟还早：这一觉不整理
		return time.Time{}
	}
	return due
}

// sleepCompact 在角色睡着时压缩最近活跃的会话——醒来之后心跳与对话落的正是那个会话。
// 用量没到门槛就不压：上下文本来就短，整理一次只是白付一次模型调用。
//
// 查会话、问用量都在锁外，期间真人可能已经把人叫醒，所以动手压缩之前再认一次
// 「还是这一觉、还该整理」，回写结果时也只写回同一觉。认过之后到压缩拿到会话锁之间
// 仍有极短的窗口：真人轮次先拿到锁时压缩返回会话忙、按重试处理，而那时暂停已清、
// 代号已换，重试不会发生；压缩先拿到锁时真人轮次排在它后面。
func (p *Plugin) sleepCompact(ctx context.Context) {
	p.mu.Lock()
	if p.sleepCompactDueLocked().IsZero() { // 等待期间被叫醒了，或者配置变了
		p.mu.Unlock()
		return
	}
	gen := p.sleepGen
	lang, pct := p.lang, p.compactPct
	compact, status, notice, sessions := p.compact, p.status, p.notice, p.sessions
	p.mu.Unlock()

	stillAsleep := func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.sleepGen == gen && !p.sleepCompactDueLocked().IsZero()
	}

	done := true
	sid, _, err := sessions.LastActive()
	switch {
	case err != nil || sid == "":
		i18n.Logf(lang, "log.heartbeat.sleep_no_session", "heartbeat: 睡着时没有可整理的会话，跳过")
	default:
		load := 0.0
		if info, err := status(sid); err == nil {
			load = info.CompactLoad()
		}
		if percent := int(load * 100); percent < pct {
			i18n.Logf(lang, "log.heartbeat.sleep_compact_skip",
				"heartbeat: 会话 %s 的上下文只占自动压缩点的 %d%%，睡着时不整理", sid, percent)
			break
		}
		if !stillAsleep() {
			return // 已经醒了：这一觉的状态归新一轮管，什么都不写
		}
		cctx, cancel := context.WithTimeout(ctx, beatTimeout)
		err := compact(cctx, sid)
		cancel()
		switch {
		case errors.Is(err, plugin.ErrSessionBusy):
			done = false
		case err != nil:
			if ctx.Err() == nil { // 停止时的取消错误不值得记
				i18n.Logf(lang, "log.heartbeat.sleep_compact_failed", "heartbeat: 睡着时整理上下文失败: %v", err)
			}
		default:
			i18n.Logf(lang, "log.heartbeat.sleep_compacted", "heartbeat: 睡着时整理了会话 %s 的上下文", sid)
			if notice != nil {
				_ = notice(ctx, sid, i18n.T(lang, "plugin.heartbeat.notice.sleep_compacted",
					"💤 角色睡着的时候整理了一次上下文，较早的对话已换成摘要。"))
			}
		}
	}

	p.mu.Lock()
	if p.sleepGen != gen { // 整理期间被叫醒或换了一觉：结果不属于现在这一觉
		p.mu.Unlock()
		return
	}
	if done {
		p.sleepCompacted = true
	} else {
		p.compactRetryAt = time.Now().Add(compactRetry)
	}
	dir, st := p.snapshotStateLocked()
	p.mu.Unlock()
	persistState(dir, st) // 记下这一觉已经整理过：重启后不该再来一次
}

// pauseStateLocked 是暂停期间注入的心跳块。睡着时分两种：真人来了消息（被叫醒），
// 以及后台轮次（还睡着）。调用方需持有 p.mu。
func (p *Plugin) pauseStateLocked(interactive bool, until time.Duration) string {
	lang := p.lang
	switch {
	case p.asleep && interactive:
		return prompt.Tf(lang, "prompt.heartbeat.state_woken",
			"[心跳]\n你正在睡觉，原定约 %s后醒，是这条消息把你叫醒的。迷迷糊糊地回，还是醒过来好好说，"+
				"回完要不要接着睡，都由你定；要接着睡就再用 pause_heartbeat。", humanDur(lang, until))
	case p.asleep:
		return prompt.Tf(lang, "prompt.heartbeat.state_asleep",
			"[心跳]\n你正在睡觉，约 %s后醒。", humanDur(lang, until))
	default:
		return prompt.Tf(lang, "prompt.heartbeat.state_paused",
			"[心跳]\n主动开口已暂停，约 %s后恢复常规节奏。", humanDur(lang, until))
	}
}
