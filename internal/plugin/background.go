package plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"wen/internal/i18n"
)

// ErrTurnNotStarted 表示一轮对话因外部预算或限额没有开始。
//
// 它与「模型调用失败」不同：插件在轮次开始前改过的状态应当回滚，任务保持待处理。
// 由 RunTurn 的实现方返回，插件用 errors.Is 判断。生产路径不会产生它。
var ErrTurnNotStarted = errors.New("轮次未开始")

// BackgroundWorker 是能按时间自发做事、又能由外部显式驱动的插件。
//
// 生产里由插件自己的循环调用同一份逻辑；ManualTime 下由评测按虚拟时钟推进并同步
// 调用 RunBackground，插件不自起任何 goroutine。
type BackgroundWorker interface {
	// NextBackgroundAt 返回 now 时刻之后（或恰好 now）最近一次后台工作的时刻。
	// ok=false 表示没有安排。返回 <= now 表示已经到期。
	NextBackgroundAt(now time.Time) (time.Time, bool)
	// RunBackground 同步执行 now 时刻到期的全部后台工作，不得再起 goroutine。
	// 遇到 ErrTurnNotStarted 必须原样向上返回。
	RunBackground(ctx context.Context, now time.Time) error
}

// TurnSettler 由需要「本轮彻底返回后再跑下一轮」的插件实现（规划、反思、提炼）。
//
// 生产路径不调用；ManualTime 下由评测在轮次返回后调用，直到没有待办。
// 手动待办不得算进 WaitGroup：预算中止时 StopAll 要能正常返回。
type TurnSettler interface {
	// Settle 同步执行 OnTurnEnd 期间登记的工作；did=true 表示做了至少一件，
	// 调用方应再轮询一轮（执行可能触发其它插件的 OnTurnEnd）。
	Settle(ctx context.Context) (did bool, err error)
}

// BackgroundObserver 由需要观察外部状态（如忙碌公告板）的插件实现。
//
// 它只更新观察值，不排定工作、不启动轮次；ManualTime 下评测在每次推进时间线前
// 调用。查询（NextBackgroundAt）必须保持无副作用，因此状态转换放在这里。
type BackgroundObserver interface {
	ObserveBackground(now time.Time)
}

// BackgroundEntry 是 Manager 快照出来的一项后台工作。
type BackgroundEntry struct {
	Name   string
	Worker BackgroundWorker
}

// maxSettleRounds 是 Settle 的轮询上限：一轮里所有插件的 did 都为假才算收敛。
const maxSettleRounds = 50

// BackgroundWorkers 按注册顺序返回启用中的后台工作插件（锁内快照）。
func (m *Manager) BackgroundWorkers() []BackgroundEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]BackgroundEntry, 0)
	for _, name := range m.order {
		e := m.entries[name]
		if !e.enabled {
			continue
		}
		if w, ok := any(e.plugin).(BackgroundWorker); ok {
			out = append(out, BackgroundEntry{Name: name, Worker: w})
		}
	}
	return out
}

// RunBackground 同步执行指定插件的后台工作。插件不存在或已禁用时什么都不做。
// 回调在锁外进行，panic 转成 error，由调用方决定是否中止。
func (m *Manager) RunBackground(ctx context.Context, name string, now time.Time) error {
	m.mu.RLock()
	e, ok := m.entries[name]
	var w BackgroundWorker
	if ok && e.enabled {
		w, _ = any(e.plugin).(BackgroundWorker)
	}
	m.mu.RUnlock()
	if w == nil {
		return nil
	}
	return callSafely(m.lang(), name, "RunBackground", func() error {
		return w.RunBackground(ctx, now)
	})
}

// settleOrder 按依赖拓扑排出结算顺序：若 A Requires B 且两者都是启用中的
// TurnSettler，则 B 先于 A；同级按注册顺序。成环返回错误，不回退。
func (m *Manager) settleOrder() ([]named[TurnSettler], error) {
	type node struct {
		name string
		impl TurnSettler
		deps []string
	}

	m.mu.RLock()
	nodes := map[string]*node{}
	order := make([]string, 0)
	for _, name := range m.order {
		e := m.entries[name]
		if !e.enabled {
			continue
		}
		s, ok := any(e.plugin).(TurnSettler)
		if !ok {
			continue
		}
		nodes[name] = &node{name: name, impl: s, deps: RequiresOf(e.plugin)}
		order = append(order, name)
	}
	lang := m.ictx.Lang
	m.mu.RUnlock()

	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := make(map[string]int, len(nodes))
	stack := make([]string, 0, len(nodes))
	out := make([]named[TurnSettler], 0, len(nodes))

	var visit func(string) error
	visit = func(name string) error {
		switch state[name] {
		case done:
			return nil
		case visiting:
			path := append(append([]string(nil), stack...), name)
			return fmt.Errorf("%s", i18n.Tf(lang, "plugin.settle.cycle",
				"插件结算顺序存在依赖环：%s", strings.Join(path, " → ")))
		}
		state[name] = visiting
		stack = append(stack, name)
		for _, dep := range nodes[name].deps {
			if _, ok := nodes[dep]; ok {
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[name] = done
		out = append(out, named[TurnSettler]{name: name, impl: nodes[name].impl})
		return nil
	}
	for _, name := range order {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ValidateSettleOrder 在开跑前校验结算顺序。有环时立即失败，不烧任何模型调用。
func (m *Manager) ValidateSettleOrder() error {
	_, err := m.settleOrder()
	return err
}

// Settle 轮询调用各插件的 Settle，直到一轮里没有任何插件报告做过工作。
// 顺序按依赖拓扑，保证 memory 先于 growth 这类先后关系。
func (m *Manager) Settle(ctx context.Context) error {
	order, err := m.settleOrder()
	if err != nil {
		return err
	}
	for round := 0; round < maxSettleRounds; round++ {
		didAny := false
		for _, s := range order {
			var did bool
			err := callSafely(m.lang(), s.name, "Settle", func() error {
				var e error
				did, e = s.impl.Settle(ctx)
				return e
			})
			if err != nil {
				return err
			}
			if did {
				didAny = true
			}
		}
		if !didAny {
			return nil
		}
	}
	return fmt.Errorf("%s", i18n.Tf(m.lang(), "plugin.settle.not_converging",
		"插件结算在 %d 轮内没有收敛", maxSettleRounds))
}

// ObserveBackground 让启用中的 observer 记录 now 时刻的外部状态。
// 与查询不同，它只更新观察值，不返回排期。
func (m *Manager) ObserveBackground(now time.Time) {
	for _, o := range enabledAs[BackgroundObserver](m) {
		_ = callSafely(m.lang(), o.name, "ObserveBackground", func() error {
			o.impl.ObserveBackground(now)
			return nil
		})
	}
}

// callSafely 执行一次插件回调，panic 只记日志并转成 error。
// 与 safely 同形，区别是需要把错误交给调用方决定是否中止。
func callSafely(lang, name, what string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			i18n.Logf(lang, "log.plugin.callback_panic", "插件 %q 的 %s 回调 panic: %v", name, what, r)
			err = fmt.Errorf("%s", i18n.Tf(lang, "plugin.callback.panic",
				"插件 %q 的 %s 回调 panic: %v", name, what, r))
		}
	}()
	return fn()
}
