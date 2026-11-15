// Package clock 让「现在几点」成为一件可以被替换的事。
//
// 角色的一举一动都挂在时间上：关系多久没见、心情回落到哪、病好了没有、这条记忆是
// 几天前的。这些都读时钟，而时钟是唯一让人无法在几分钟内验证「过了一周会怎样」的
// 东西。这里提供一个进程外的钟：默认是系统时间，评测与测试可以换成一条自己推进的
// 时间线。
//
// 与 internal/cue 同属插件间共享的小机制，核心不知道它。约定只有一条：
//
//   - 时钟只回答「现在几点」：时间戳、日界、衰减、两条消息之间的间隔。
//   - 耗时仍用真实时间：等待了多久、超时、日志时延。推进八小时的是「这一天过去
//     了」，不是「这次调用跑了八小时」，拿虚拟钟去量耗时会把一切计时都算错。
//
// 钟跟着 context 走而不是做成全局变量：同一条时间线要同时被 agent 的落盘、插件的
// 提示词与工具看见，context 正好是这三条路径唯一的公共携带物；而全局钟会让同一
// 进程里的评测与真实对话互相影响。
package clock

import (
	"context"
	"sync"
	"time"
)

// Clock 是「现在几点」的唯一来源。
type Clock interface {
	Now() time.Time
}

// Func 把一个函数变成 Clock，便于测试里写死一个时刻。
type Func func() time.Time

// Now 实现 Clock。
func (f Func) Now() time.Time { return f() }

type ctxKey struct{}

// With 把钟放进 context，本次调用及其派生调用都用它。
//
// c 为 nil 时原样返回，不往 context 里塞一个会在取值时炸掉的东西。
func With(ctx context.Context, c Clock) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, c)
}

// from 返回 ctx 里的钟，没有时返回 nil。
func from(ctx context.Context) Clock {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(ctxKey{}).(Clock)
	return c
}

// Now 取 ctx 里的钟，没有就用系统时间。
//
// 回退是常态而不是异常：生产路径上没有任何地方装钟，绝大多数调用都会走这一条。
func Now(ctx context.Context) time.Time {
	return NowOr(ctx, time.Now)
}

// NowOr 取 ctx 里的钟，没有时返回 fallback 的时间。
//
// 给「插件自己也有一个可替换的钟」的地方用（日程、身体感知）：规则只有一条，
// ctx 里的钟优先，插件自己的兜底。生产里两者都是系统时间，测试替换的是后者。
func NowOr(ctx context.Context, fallback func() time.Time) time.Time {
	if c := from(ctx); c != nil {
		return c.Now()
	}
	return fallback()
}

// Manual 是一条手动推进的时间线，评测用它把「隔了三天」压进几秒钟。
//
// 带上锁是因为同一条时间线会被并发的几路读到（提示词、工具、落盘各在自己的
// goroutine 里），而推进只发生在评测主循环里。
type Manual struct {
	mu sync.Mutex
	t  time.Time
}

// NewManual 从某个时刻开始。
func NewManual(t time.Time) *Manual { return &Manual{t: t} }

// Now 实现 Clock。
func (m *Manual) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.t
}

// Advance 向前走 d。负数也允许——把时钟拨回去是测试的权利，不是运行时的错误。
func (m *Manual) Advance(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t = m.t.Add(d)
}

// Set 拨到某个时刻。
func (m *Manual) Set(t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t = t
}
