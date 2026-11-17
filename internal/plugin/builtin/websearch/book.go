package websearch

import (
	"sync"
	"time"
)

// book 记两样东西：刚查过的查询，与最近一小时真的发出去过几次。
//
// 合并成一个结构是因为它们站在同一条路径上——「这次调用要不要真的发出去」——且生命
// 周期一致（一个插件实例一个）。分两把锁只会多两个死锁的机会。
type book struct {
	mu      sync.Mutex
	ttl     time.Duration
	size    int
	entries map[string]entry
	sent    []time.Time
}

type entry struct {
	at  time.Time
	out string
}

func newBook(ttl time.Duration, size int) *book {
	return &book{ttl: ttl, size: size, entries: map[string]entry{}}
}

// lookup 返回还没过期的结果。命中不续期：缓存的是「那一刻网上的说法」，每次命中都
// 顺延的话，一条结果能在上下文里活一整天，而那正好是它最不可信的时候。
func (b *book) lookup(key string, now time.Time) (string, time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[key]
	if !ok {
		return "", time.Time{}, false
	}
	if now.Sub(e.at) >= b.ttl {
		delete(b.entries, key)
		return "", time.Time{}, false
	}
	return e.out, e.at, true
}

func (b *book) store(key, out string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, e := range b.entries {
		if now.Sub(e.at) >= b.ttl {
			delete(b.entries, k)
		}
	}
	if len(b.entries) >= b.size {
		// 满了丢最早的那条。一个会话里同时活着的查询远远到不了这个数，这只是兜底。
		var oldestKey string
		var oldest time.Time
		for k, e := range b.entries {
			if oldestKey == "" || e.at.Before(oldest) {
				oldestKey, oldest = k, e.at
			}
		}
		delete(b.entries, oldestKey)
	}
	b.entries[key] = entry{at: now, out: out}
}

// allow 报告现在还能不能再发一次，不记账。记账由 record 在拿到结果之后做，理由见那边。
func (b *book) allow(perHour int, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune(now)
	return perHour <= 0 || len(b.sent) < perHour
}

// record 记下一次真正拿到结果的请求。
//
// 记账与放行分开，而不是「放行即记账」：失败（密钥错、被限流、断网）不占额度。否则
// 密钥填错时二十次 401 会把这一小时填满，人改好密钥回来却撞上「查得太多了」——那正是
// 最想立刻再试的时刻。
func (b *book) record(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, now)
}

// prune 丢掉一小时以前的时刻。只在锁内调用。
func (b *book) prune(now time.Time) {
	cut := now.Add(-time.Hour)
	kept := b.sent[:0]
	for _, t := range b.sent {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	b.sent = kept
}
