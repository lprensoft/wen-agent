// Package skillroots 是「除用户自己那个技能目录之外，还有哪些目录里放着技能」的
// 进程级登记处。
//
// 与 internal/cue、internal/availability 同级的共享基础设施：核心不知道它，插件
// 之间靠它对话。存在的理由是外源插件包里的技能与用户自己写的技能应该出现在**同一份
// 清单**里、共用同一个 read_skill——装了三个插件包就多出三个同名工具，是最坏的做法。
//
// 登记而不是让技能插件自己去翻插件包目录：那样技能插件就得认识插件包的格式，而
// 「哪些包当前是启用的」还得再问一遍插件系统。反过来由插件包自己在启用时登记、
// 禁用时摘掉，开关天然生效。
package skillroots

import (
	"sort"
	"sync"
)

// Root 是一处技能根目录：其下每个子目录一个技能。
type Root struct {
	Owner string // 登记者（插件名），同一个 Owner 的登记互相覆盖
	Dir   string // 技能根目录
	Label string // 来源说明，给人看的
}

var (
	mu    sync.Mutex
	roots = map[string]Root{}
	subs  []func()
)

// Set 登记（或覆盖）一处技能根目录。
func Set(r Root) {
	if r.Owner == "" || r.Dir == "" {
		return
	}
	mu.Lock()
	old, existed := roots[r.Owner]
	roots[r.Owner] = r
	mu.Unlock()
	if !existed || old != r {
		notify()
	}
}

// Clear 摘掉某个登记者的技能根目录。
func Clear(owner string) {
	mu.Lock()
	_, existed := roots[owner]
	delete(roots, owner)
	mu.Unlock()
	if existed {
		notify()
	}
}

// List 返回全部登记，按登记者名排序。
//
// 顺序必须稳定：技能清单要拼进系统提示词，而那段的返回值按约定得在轮次之间逐字节
// 一致，否则整段前缀的提示词缓存永远命中不了。
func List() []Root {
	mu.Lock()
	out := make([]Root, 0, len(roots))
	for _, r := range roots {
		out = append(out, r)
	}
	mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Owner < out[j].Owner })
	return out
}

// Subscribe 注册一个「登记有变动」的回调，返回取消订阅的函数。
//
// 需要它是因为登记的时机不定：插件包在启动期注册，可能排在技能插件之后；用户也
// 可能在设置页上随时开关一个插件包。没有通知的话，技能插件只在自己 Init 的那一刻
// 看过一眼，此后新装的技能要重启才出现。
func Subscribe(fn func()) func() {
	mu.Lock()
	subs = append(subs, fn)
	idx := len(subs) - 1
	mu.Unlock()
	return func() {
		mu.Lock()
		if idx < len(subs) {
			subs[idx] = nil
		}
		mu.Unlock()
	}
}

// notify 在锁外逐个通知订阅者：回调里多半要回头调 List，锁内调用就是自锁。
func notify() {
	mu.Lock()
	fns := make([]func(), 0, len(subs))
	for _, fn := range subs {
		if fn != nil {
			fns = append(fns, fn)
		}
	}
	mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// Reset 清空全部登记与订阅，只给测试用。
func Reset() {
	mu.Lock()
	roots = map[string]Root{}
	subs = nil
	mu.Unlock()
}
