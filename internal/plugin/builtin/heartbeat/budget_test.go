package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
	"wen/internal/usage"
)

// 预算用尽时这一拍算过去了：lastBeat 不回滚、下一拍仍按正常间隔，
// 状态行说明后台为什么安静；预算恢复后的第一拍成功即清掉这个说明。
//
// 回滚（ErrTurnNotStarted 的处置）在这里是错的：预算要恢复得靠人改设置或等下个月，
// 回滚会让每一拍都被拦下、每一拍都立刻重试，变成紧凑循环。
func TestBudgetExceededKeepsBeat(t *testing.T) {
	fail := true
	calls := 0
	p, _ := newInited(t, func(context.Context, string, string) (string, error) {
		calls++
		if fail {
			return "", fmt.Errorf("本月用量已达上限: %w", plugin.ErrBudgetExceeded)
		}
		return "嗯", nil
	}, map[string]any{"interval_minutes": 30, "dynamic": false})

	now := time.Now()
	p.mu.Lock()
	p.lastBeat = now.Add(-time.Hour) // 常规心跳早已到期
	p.mu.Unlock()

	if err := p.beatAt(context.Background(), now); err != nil {
		t.Fatalf("预算用尽不该把错误抛给调度方: %v", err)
	}
	if calls != 1 {
		t.Fatalf("这一拍应真的尝试过一次，实际 %d 次", calls)
	}

	p.mu.Lock()
	beat, paused, next := p.lastBeat, p.budgetPaused, p.nextBeatLocked()
	p.mu.Unlock()
	if !beat.Equal(now) {
		t.Fatalf("预算用尽不该回滚 lastBeat: %v，期望 %v", beat, now)
	}
	if !paused {
		t.Fatal("应记下「后台因预算暂停」")
	}
	// 下一拍是正常间隔之后，不是立刻——紧凑循环正是要避免的
	if got := next.Sub(now); got != 30*time.Minute {
		t.Fatalf("下一拍应在 %v 之后，实际 %v", 30*time.Minute, got)
	}
	lines := p.StatusLines("")
	if len(lines) == 0 || !strings.Contains(lines[0], "预算") {
		t.Fatalf("状态行应说明预算用尽，得到 %v", lines)
	}

	// 预算恢复后的第一拍成功：说明清掉，状态行回到常规内容
	fail = false
	if err := p.beatAt(context.Background(), now.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	paused = p.budgetPaused
	p.mu.Unlock()
	if paused {
		t.Fatal("成功后应清掉「预算暂停」")
	}
	if got := p.StatusLines("")[0]; strings.Contains(got, "预算") {
		t.Fatalf("恢复后状态行不该再提预算: %q", got)
	}
}

// midTurnBudget 是 Agent 预算错误的替身：同时认两个哨兵，并说明那次拦截是否发生在
// 轮次开始之后。心跳不看这个标记——两种都按「这一拍算过去」处理——但那条判据必须
// 对两种形态都成立，否则中途停下会让 lastBeat 被回滚、下一拍立刻重来。
type midTurnBudget struct{}

func (*midTurnBudget) Error() string { return "本月用量已达上限" }

func (*midTurnBudget) Unwrap() []error {
	return []error{plugin.ErrBudgetExceeded, usage.ErrBudgetExceeded}
}

func (*midTurnBudget) TurnStarted() bool { return true }

func TestMidTurnBudgetStillKeepsBeat(t *testing.T) {
	p, _ := newInited(t, func(context.Context, string, string) (string, error) {
		return "", &midTurnBudget{}
	}, map[string]any{"interval_minutes": 30, "dynamic": false})

	now := time.Now()
	p.mu.Lock()
	p.lastBeat = now.Add(-time.Hour)
	p.mu.Unlock()

	if err := p.beatAt(context.Background(), now); err != nil {
		t.Fatalf("预算用尽不该把错误抛给调度方: %v", err)
	}
	p.mu.Lock()
	beat, paused := p.lastBeat, p.budgetPaused
	p.mu.Unlock()
	if !beat.Equal(now) {
		t.Fatalf("轮次中途被预算拦下同样不该回滚 lastBeat: %v，期望 %v", beat, now)
	}
	if !paused {
		t.Fatal("应记下「后台因预算暂停」")
	}
}

// 睡着/暂停的说明优先于预算：那是角色自己的安排，预算只是后台暂时不能动。
func TestBudgetStatusAfterPause(t *testing.T) {
	p := &Plugin{cur: 30 * time.Minute, lastBeat: time.Now()}
	p.budgetPaused = true
	p.pausedUntil = time.Now().Add(time.Hour)
	if got := p.StatusLines("")[0]; !strings.Contains(got, "暂停中") {
		t.Fatalf("暂停中的说明应优先，得到 %q", got)
	}
}

// 「预算暂停」只描述最近一拍：被拦之后任何非预算的结果（成功、会话忙、模型报错）
// 都说明后台又能动了，标记就得清掉。曾只在成功时清，于是预算恢复后接连几次模型
// 报错期间，状态行一直说「后台因预算暂停」——一句与事实相反的话。
func TestBudgetPausedClearedByAnyNonBudgetResult(t *testing.T) {
	fail := true
	p, _ := newInited(t, func(context.Context, string, string) (string, error) {
		if fail {
			// 真实的双哨兵形状：Agent 的预算错误同时认插件与核心两个哨兵
			return "", errors.Join(plugin.ErrBudgetExceeded, usage.ErrBudgetExceeded)
		}
		return "", errors.New("模型宕了")
	}, map[string]any{"interval_minutes": 30, "dynamic": false})

	now := time.Now()
	if err := p.beatAt(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	paused := p.budgetPaused
	p.mu.Unlock()
	if !paused {
		t.Fatal("被预算拦下应记下「后台因预算暂停」")
	}

	// 预算恢复了，但这一拍照旧失败（模型报错）：后台已经能动了，说明必须撤掉
	fail = false
	if err := p.beatAt(context.Background(), now.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	paused = p.budgetPaused
	p.mu.Unlock()
	if paused {
		t.Fatal("非预算的失败同样该清掉「预算暂停」")
	}
	if got := p.StatusLines("")[0]; strings.Contains(got, "预算") {
		t.Fatalf("状态行不该再提预算: %q", got)
	}
}
