package heartbeat

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"wen/internal/availability"
	"wen/internal/plugin"
)

// cleanBoard 清掉本包测试可能写到忙碌公告板上的记录：公告板是进程级的，留着会串到别的用例。
func cleanBoard(t *testing.T) {
	t.Helper()
	availability.Clear(availSource)
	availability.Clear("agenda")
	t.Cleanup(func() {
		availability.Clear(availSource)
		availability.Clear("agenda")
	})
}

func TestSleepIsPublishedToBoard(t *testing.T) {
	cleanBoard(t)
	p, _ := newInited(t, noTurn, nil)

	pause(t, p, `{"minutes":480,"reason":"睡","asleep":true}`)
	st, ok := availability.Current(time.Now())
	if !ok || st.Source != availSource || st.Level != availability.Away || st.Activity != "睡觉" {
		t.Fatalf("睡着应宣布「不回」：%+v %v", st, ok)
	}
	wakeUp(p)
	if _, ok := availability.Current(time.Now()); ok {
		t.Error("被叫醒后应撤掉")
	}

	// 只是对方不在：她本人不忙，不写
	pause(t, p, `{"minutes":120,"reason":"她出门了"}`)
	if _, ok := availability.Current(time.Now()); ok {
		t.Error("没睡就不该宣布忙")
	}

	// 插件停掉：没人负责叫醒，撤掉
	pause(t, p, `{"minutes":480,"reason":"睡","asleep":true}`)
	p.Stop()
	if _, ok := availability.Current(time.Now()); ok {
		t.Error("插件停掉后不该留着睡眠")
	}
}

func TestBusyDefersNextBeat(t *testing.T) {
	cleanBoard(t)
	p, _ := newInited(t, noTurn, nil)
	until := time.Now().Add(time.Hour)

	availability.Set(availability.State{Source: "agenda", Activity: "开会", Level: availability.Heavy, Until: until})
	p.mu.Lock()
	next := p.nextBeatLocked()
	p.mu.Unlock()
	if next.Before(until.Add(busyGrace)) {
		t.Errorf("重忙时下一拍应压到忙完之后：next=%v until=%v", next, until)
	}

	// 轻忙不压
	availability.Set(availability.State{Source: "agenda", Activity: "散步", Level: availability.Light, Until: until})
	p.mu.Lock()
	next = p.nextBeatLocked()
	p.mu.Unlock()
	if !next.Before(until) {
		t.Errorf("轻忙不该压后：next=%v", next)
	}

	// 自己的睡眠不压醒来那一拍：睡着时下一拍就是醒来时刻
	availability.Clear("agenda")
	pause(t, p, `{"minutes":60,"reason":"午睡","asleep":true}`)
	p.mu.Lock()
	next, wake := p.nextBeatLocked(), p.pausedUntil
	p.mu.Unlock()
	if !next.Equal(wake) {
		t.Errorf("醒来那一拍不该被自己的睡眠压后：next=%v wake=%v", next, wake)
	}
}

// 心跳在持锁时宣布「睡着」，而它自己也订阅了公告板：订阅回调要是取 p.mu 就是自锁。
// 这里让循环与订阅都活着，暂停与叫醒必须在限时内完成。
func TestPublishingSleepDoesNotDeadlock(t *testing.T) {
	cleanBoard(t)
	_, store := newInited(t, noTurn, nil)
	p := New()
	if err := p.Init(plugin.InitContext{
		StateDir: t.TempDir(), Sessions: store, RunTurn: noTurn,
		NewSession: func() (string, error) { m, err := store.Create(); return m.ID, err },
	}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = p.Tools()[1].Execute(context.Background(), json.RawMessage(`{"minutes":480,"reason":"睡","asleep":true}`))
		time.Sleep(2 * time.Millisecond)
		wakeUp(p)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("宣布睡着时卡死了")
	}
}

// 日程到点或提前做完时会撤掉忙碌状态，叫醒循环来重算的那一刻状态已经没了：
// 缓冲要从「忙实际结束的时刻」起算，而不是跟着状态一起消失。
func TestBusyGraceSurvivesClear(t *testing.T) {
	cleanBoard(t)
	p, _ := newInited(t, noTurn, nil)
	p.mu.Lock()
	p.lastBeat = time.Now().Add(-2 * time.Hour) // 常规时刻早就过了
	p.mu.Unlock()

	availability.Set(availability.State{Source: "agenda", Activity: "开会", Level: availability.Heavy, Until: time.Now().Add(time.Hour)})
	p.mu.Lock()
	_ = p.nextBeatLocked() // 循环看到了忙
	p.mu.Unlock()

	availability.Clear("agenda") // 提前开完
	cleared := time.Now()
	p.mu.Lock()
	next := p.nextBeatLocked()
	p.mu.Unlock()
	if next.Before(cleared.Add(busyGrace - time.Second)) {
		t.Errorf("提前忙完也要留出缓冲：next 距清除 %v", next.Sub(cleared))
	}

	// 自然到点（没人 Clear，状态过期被清掉）：缓冲从原定的 Until 起算
	until := time.Now().Add(30 * time.Millisecond)
	availability.Set(availability.State{Source: "agenda", Activity: "跑步", Level: availability.Heavy, Until: until})
	p.mu.Lock()
	_ = p.nextBeatLocked()
	p.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	p.mu.Lock()
	next = p.nextBeatLocked()
	p.mu.Unlock()
	if !next.Equal(until.Add(busyGrace)) {
		t.Errorf("到点结束时缓冲应从 Until 起算：next-until = %v", next.Sub(until))
	}
}
