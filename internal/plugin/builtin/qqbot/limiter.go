package qqbot

import (
	"sync"
	"time"
)

// replyLimiter 追踪每条入站消息已被动回复的次数：官方限制同一 msg_id 在 60 分钟内
// 最多回复 4 条（超时或超次都只能改发主动消息）。msg_seq 同一 msg_id 下不可重复
// （重复会报 40054005），在一个随创建时刻变化的起点上递增：计数只在内存里，忙时
// 扣住的消息落盘、重启后放行时，同一个 msg_id 再从 1 数起就会撞上重启前用过的序号。
type replyLimiter struct {
	mu      sync.Mutex
	entries map[string]*replyEntry
}

type replyEntry struct {
	count int
	first time.Time
	base  int  // 序号起点，见 seqBase
	typed bool // 这条入站消息上已经发过「正在输入」
}

// typingReserve 是发「正在输入」之后至少要给正文留下的被动回复额度。
const typingReserve = 3

// seqBase 按秒取一个起点，每条 msg_id 至多用 replyLimit 个序号，不同秒的起点互不重叠。
func seqBase(now time.Time) int { return int(now.Unix()%100000) * replyLimit }

const (
	replyLimit  = 4
	replyWindow = time.Hour
	maxTracked  = 10000
)

func newReplyLimiter() *replyLimiter {
	return &replyLimiter{entries: map[string]*replyEntry{}}
}

// next 判定下一条出站消息能否挂在 msgID 上被动回复。
// 可以则返回 (true, 本次要用的 msg_seq) 并记账；不能（超窗/超次）返回 (false, 0)，
// 调用方应降级为主动消息。
func (l *replyLimiter) next(msgID string) (bool, int) {
	if msgID == "" {
		return false, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.nextLocked(msgID)
}

// nextLocked 是 next 的本体，调用方须持 mu。
func (l *replyLimiter) nextLocked(msgID string) (bool, int) {
	now := time.Now()
	e, ok := l.entries[msgID]
	if !ok {
		if len(l.entries) >= maxTracked {
			l.evictLocked(now)
		}
		e = &replyEntry{first: now, base: seqBase(now)}
		l.entries[msgID] = e
	}
	if now.Sub(e.first) > replyWindow || e.count >= replyLimit {
		return false, 0
	}
	e.count++
	return true, e.base + e.count
}

// typingSlot 判定能不能在 msgID 上发一次「正在输入」：每条入站消息至多一次（分条发送
// 时骨架会在每两段之间再要一次，每次都占额度的话四条额度两轮就耗光了），且发完之后
// 还得给正文留够 typingReserve 条。可以则记账并返回序号。
func (l *replyLimiter) typingSlot(msgID string) (bool, int) {
	if msgID == "" {
		return false, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.entries[msgID]; ok && (e.typed || replyLimit-e.count <= typingReserve) {
		return false, 0
	}
	ok, seq := l.nextLocked(msgID)
	if ok {
		l.entries[msgID].typed = true
	}
	return ok, seq
}

func (l *replyLimiter) evictLocked(now time.Time) {
	for id, e := range l.entries {
		if now.Sub(e.first) > replyWindow {
			delete(l.entries, id)
		}
	}
}
