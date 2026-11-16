// Package cue 是插件间传递「开口理由」的公告板。
//
// 心跳解决了「什么时候开口」，没有解决「开口说什么」——无话找话时模型只能编进展
// 或重复问候。而各插件本就掌握着天然的开口理由：天气看到刚下起雨、定时任务到点、
// 记忆里存过要跟进的事。这里提供一个进程级的汇合点：产生理由的插件 Post，发起
// 主动开口的插件（心跳）在开口前 Take 走全部未过期的理由带进当轮提示词。
//
// 与 internal/imbot 同属插件间共享的基础设施，核心不知道它。设计上刻意保持哑：
//
//   - 只存内存，不落盘。检测状态的权威在产生方自己的持久化里（天气的观测缓存
//     本来就落盘），重启丢掉的只是「尚未说出口的那一句」，产生方的下一次检测
//     会重新发现仍然成立的理由；公告板自己落盘反而要回答「重启后这条还算数吗」。
//   - 同 (Source, Key) 幂等覆盖，产生方按事件（转变）投递而不是按状态投递，
//     消费后不会被同一事件反复灌回。
//   - 理由必须带过期时刻。「刚下起雨」放到两小时后说出口就是错的；没人消费时
//     （心跳关着）过期条目也会被后续操作清走，公告板不会无限增长。
package cue

import (
	"sync"
	"time"
)

// maxCues 是公告板的容量上限，超出时丢最早投递的。这是防失控的保险，
// 正常使用远到不了：理由是低频事件，且都带过期时刻。
const maxCues = 32

// Cue 是一条开口理由。
type Cue struct {
	Source string    // 投递方插件名
	Key    string    // 同源去重键：同 (Source, Key) 后投的覆盖先投的
	Text   string    // 给模型看的一句话，说清发生了什么
	Expire time.Time // 过期时刻，之后不再送达
}

var (
	mu     sync.Mutex
	cues   []Cue // 按投递顺序
	notify func()
)

// Post 投递一条理由。同 (Source, Key) 覆盖旧条目（保持原有位置），
// 随后叫醒消费方（若装了回调）。
//
// 这里刻意不判断「是不是已经过期」，也不顺手剪枝：那要看时钟，而投递方的时钟
// 未必是本进程的墙上时钟——插件的定时逻辑都是可注入时钟的（日程按假时钟算出
// 「16:30 回来」，过期时刻自然也在那条时间线上）。Post 若拿 time.Now() 去卡，
// 等于用另一条时间线否决投递方的判断，理由会被静默扔掉。过期与否统一由消费侧
// 的 Take / Pending 按各自的 now 裁定，它们本来就带这个参数。代价是一条已经
// 过期的理由会占着一个槽位直到下次消费，对 maxCues 那个量级无所谓。
func Post(c Cue) {
	// 过期时刻仍是硬要求，只是「有没有过期」不在这里判——空值等于没带，拒收
	if c.Text == "" || c.Expire.IsZero() {
		return
	}
	mu.Lock()
	replaced := false
	for i := range cues {
		if cues[i].Source == c.Source && cues[i].Key == c.Key {
			cues[i], replaced = c, true
			break
		}
	}
	if !replaced {
		cues = append(cues, c)
		if len(cues) > maxCues {
			cues = cues[len(cues)-maxCues:]
		}
	}
	fn := notify
	mu.Unlock()

	// 回调在锁外：消费方的回调可能取它自己的锁，嵌在这里会定死锁顺序
	if fn != nil {
		fn()
	}
}

// Take 取走全部未过期的理由（消费即清），按投递顺序返回。
func Take(now time.Time) []Cue {
	mu.Lock()
	defer mu.Unlock()
	pruneLocked(now)
	out := cues
	cues = nil
	return out
}

// Pending 报告是否有未过期的理由待消费。
func Pending(now time.Time) bool {
	mu.Lock()
	defer mu.Unlock()
	pruneLocked(now)
	return len(cues) > 0
}

// Reset 清空未消费的理由并卸下通知回调。评测在同一进程里连续跑时用它隔离；
// 生产路径没有调用点。
func Reset() {
	mu.Lock()
	cues = nil
	notify = nil
	mu.Unlock()
}

// Drop 撤回一条尚未消费的理由（按 Source+Key）。产生方发现理由不再成立时用——
// 已经送达的收不回，还没送达的不该再送出去（预报里的雨取消了，「明天有雨」就不能
// 再说）。撤回不存在的键什么都不发生。
func Drop(source, key string) {
	mu.Lock()
	defer mu.Unlock()
	for i := range cues {
		if cues[i].Source == source && cues[i].Key == key {
			cues = append(cues[:i], cues[i+1:]...)
			return
		}
	}
}

// SetNotify 安装（或用 nil 卸下）投递时的叫醒回调。单槽位：只有一个主动开口的
// 消费方，与 imbot.SetRouter 同款约定。
func SetNotify(fn func()) {
	mu.Lock()
	defer mu.Unlock()
	notify = fn
}

// pruneLocked 清掉已过期的条目。调用方须持 mu。
func pruneLocked(now time.Time) {
	kept := cues[:0]
	for _, c := range cues {
		if c.Expire.After(now) {
			kept = append(kept, c)
		}
	}
	cues = kept
}
