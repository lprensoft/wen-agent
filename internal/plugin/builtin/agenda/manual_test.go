package agenda

import (
	"context"
	"errors"
	"testing"
	"time"

	"wen/internal/plugin"
)

// 关闭活动轮次后，进行中的项到点只清理一次忙碌状态。
func TestBusyCleanupOnceWithoutActivities(t *testing.T) {
	h := newHarness(t, false, map[string]any{"run_activities": false})
	h.setPlan(t, Item{
		ID: "a", Title: "写方案", Start: "08:00", End: "09:00",
		Status: statusOngoing, Busy: "重忙",
	})
	h.clk.set(at(10, 0))

	h.p.tickAt(context.Background(), at(10, 0))
	plan := h.plan(t)
	it := plan.item("a")
	if it == nil || !it.BusyCleared {
		t.Fatal("到点后应清掉忙碌状态并记下 BusyCleared")
	}
	if next, ok := h.p.nextDueAt(at(10, 0)); ok {
		t.Fatalf("清理后不应再返回同一事件，得到 %v", next)
	}

	// 再跑一次也不该产生新的到期事件。
	h.p.tickAt(context.Background(), at(10, 0))
	if next, ok := h.p.nextDueAt(at(10, 0)); ok {
		t.Fatalf("重复 tick 后仍返回事件：%v", next)
	}
}

// 没有其他 worker 时，查询也要能预测未来的提醒时刻。
func TestReminderPredictedBeforeWindow(t *testing.T) {
	h := newHarness(t, false, map[string]any{"run_activities": false})
	h.setPlan(t, Item{
		ID: "c", Title: "见面", Start: "14:00", End: "15:00",
		Status: statusPlanned, WithUser: true,
	})

	next, ok := h.p.nextDueAt(at(10, 0))
	if !ok {
		t.Fatal("应预测到提醒时刻")
	}
	want := at(14, 0).Add(-defRemindBeforeMin * time.Minute)
	if !next.Equal(want) {
		t.Fatalf("下一个事件 = %v，期望 %v", next, want)
	}

	// 跳过开始时间后，提醒窗口已过，不应再调度。
	if next, ok := h.p.nextDueAt(at(14, 30)); ok {
		t.Fatalf("超过 start 后不应再提醒，得到 %v", next)
	}
}

// 关闭活动轮次后，承诺过期仍能收束。
func TestPromiseSweepWithoutActivities(t *testing.T) {
	h := newHarness(t, false, map[string]any{"run_activities": false})
	now := at(10, 0)
	h.clk.set(now)
	old := now.AddDate(0, 0, -(promiseGraceDays + 2)).Format(dateLayout)
	st := h.p.storeFor("")
	if st == nil {
		t.Fatal("缺少共享域的库")
	}
	if _, err := st.UpdatePromises(func(ps *[]Promise, _ func() string) (bool, error) {
		*ps = append(*ps, Promise{
			ID: "p1", Date: old, Title: "带菜", By: promiseBySelf,
			Status: promisePending, Created: now,
		})
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}

	if next, ok := h.p.nextDueAt(now); !ok || !next.Equal(now) {
		t.Fatalf("已过期未收束的承诺应立即到期，得到 %v ok=%v", next, ok)
	}
	h.p.tickAt(context.Background(), now)
	ps := h.promises(t)
	if len(ps) != 1 || ps[0].Status != promiseMissed {
		t.Fatalf("承诺应收束为没做成：%+v", ps)
	}
	if next, ok := h.p.nextDueAt(now); ok {
		t.Fatalf("收束后不应再返回同一事件：%v", next)
	}
}

// 预算拒绝不是模型执行失败：活动应回滚为待派发。
func TestActivityRollsBackWhenTurnNotStarted(t *testing.T) {
	h := newHarness(t, false, nil)
	h.p.manual = true
	h.p.startupGrace = 0
	h.runTurn = func(context.Context, string, string) (string, error) {
		return "", plugin.ErrTurnNotStarted
	}
	h.setPlan(t, Item{ID: "a", Title: "写方案", Start: "14:00", End: "15:00", Status: statusPlanned})
	h.clk.set(at(14, 0))

	err := h.p.tickAt(context.Background(), at(14, 0))
	if !errors.Is(err, plugin.ErrTurnNotStarted) {
		t.Fatalf("应向上返回预算中止，得到 %v", err)
	}
	plan := h.plan(t)
	it := plan.item("a")
	if it == nil || !it.StartFired.IsZero() {
		t.Fatalf("未开始的轮次不应留下派发记录：%+v", it)
	}
}

// BusyCleared 只对应当前这段活动：改时间并重新公布忙碌后，新结束时刻要再清一次。
func TestBusyClearedResetsWhenActivityExtended(t *testing.T) {
	h := newHarness(t, false, map[string]any{"run_activities": false})
	h.setPlan(t, Item{
		ID: "a", Title: "写方案", Start: "08:00", End: "09:00",
		Status: statusOngoing, Busy: "重忙",
	})
	h.clk.set(at(10, 0))
	h.p.tickAt(context.Background(), at(10, 0))
	plan := h.plan(t)
	if it := plan.item("a"); it == nil || !it.BusyCleared {
		t.Fatal("第一次结束应清理并记下 BusyCleared")
	}

	h.call(t, "update_day_plan", `{"id":"a","end":"11:00","status":"ongoing"}`)
	plan = h.plan(t)
	if it := plan.item("a"); it == nil || it.BusyCleared {
		t.Fatal("改时间后 BusyCleared 应重置")
	}

	h.clk.set(at(11, 0))
	h.p.tickAt(context.Background(), at(11, 0))
	plan = h.plan(t)
	if it := plan.item("a"); it == nil || !it.BusyCleared {
		t.Fatal("新的结束时刻应再次清理")
	}
}
