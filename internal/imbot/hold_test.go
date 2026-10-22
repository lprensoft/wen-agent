package imbot

import (
	"context"
	"strings"
	"testing"
	"time"

	"wen/internal/availability"
)

// busy 在忙碌公告板上写一条状态，用例结束时撤掉：公告板是进程级的。
func busy(t *testing.T, st availability.State) {
	t.Helper()
	if st.Source == "" {
		st.Source = "agenda"
	}
	availability.Set(st)
	t.Cleanup(func() { availability.Clear(st.Source) })
}

func holdingCore(t *testing.T, rec *turnRecorder, tweak func(*Config)) (*Core, *fakeSender) {
	t.Helper()
	return newCore(t, rec.run, func(cfg *Config) {
		cfg.HoldWhenBusy = true
		cfg.WakeAfter = DefaultWakeAfter
		if tweak != nil {
			tweak(cfg)
		}
	})
}

func TestReleaseAtByLevel(t *testing.T) {
	c, _ := holdingCore(t, newTurnRecorder(), nil)
	now := time.Now()
	until := now.Add(2 * time.Hour)
	cases := []struct {
		name string
		st   availability.State
		want time.Time
	}{
		{"轻忙立即", availability.State{Level: availability.Light, Until: until}, now},
		{"重忙抽空看一眼", availability.State{Level: availability.Heavy, Until: until}, now.Add(heavyHold)},
		{"重忙但快忙完了", availability.State{Level: availability.Heavy, Until: now.Add(time.Minute)}, now.Add(time.Minute)},
		{"不回等到忙完", availability.State{Level: availability.Away, Until: until}, until},
		{"睡着攒一阵被吵醒", availability.State{Level: availability.Away, Until: until, Sleeping: true}, now.Add(DefaultWakeAfter)},
		{"快睡醒了就随之醒", availability.State{Level: availability.Away, Until: now.Add(time.Minute), Sleeping: true}, now.Add(time.Minute)},
	}
	for _, tc := range cases {
		tc.st.Source = "agenda"
		availability.Set(tc.st)
		if got := c.releaseAt(now, now); !got.Equal(tc.want) {
			t.Errorf("%s: releaseAt = %v, want %v", tc.name, got.Sub(now), tc.want.Sub(now))
		}
		availability.Clear("agenda")
	}
	if got := c.releaseAt(now, now); !got.Equal(now) {
		t.Errorf("空闲时不扣：%v", got.Sub(now))
	}

	// 被动回复窗口封顶
	c.cfg.ReplyWindow = 45 * time.Minute
	busy(t, availability.State{Level: availability.Away, Until: until})
	if got := c.releaseAt(now, now); !got.Equal(now.Add(45 * time.Minute)) {
		t.Errorf("应按回复窗口封顶：%v", got.Sub(now))
	}
}

func TestHoldUntilBusyEndsThenOneTurn(t *testing.T) {
	rec := newTurnRecorder()
	c, _ := holdingCore(t, rec, nil)
	busy(t, availability.State{Activity: "开会", Level: availability.Away, Until: time.Now().Add(300 * time.Millisecond)})

	c.Handle(context.Background(), Message{UserID: "u1", Text: "在吗"})
	c.Handle(context.Background(), Message{UserID: "u1", Text: "下班一起吃饭？"})
	rec.expectNone(t, 150*time.Millisecond)
	if got := rec.expect(t); got != "在吗\n下班一起吃饭？" {
		t.Errorf("忙完后扣着的几条应合成一轮：%q", got)
	}
}

func TestHoldReleasedWhenBusyClearsEarly(t *testing.T) {
	rec := newTurnRecorder()
	c, _ := holdingCore(t, rec, nil)
	busy(t, availability.State{Activity: "开会", Level: availability.Away, Until: time.Now().Add(time.Hour)})

	c.Handle(context.Background(), Message{UserID: "u1", Text: "在吗"})
	rec.expectNone(t, 100*time.Millisecond)
	availability.Clear("agenda") // 会提前开完了
	if got := rec.expect(t); got != "在吗" {
		t.Errorf("忙完应立即放行：%q", got)
	}
}

func TestHoldNotAppliedToCommandsOrWhenOff(t *testing.T) {
	rec := newTurnRecorder()
	c, f := holdingCore(t, rec, nil)
	busy(t, availability.State{Activity: "开会", Level: availability.Away, Until: time.Now().Add(time.Hour)})
	c.Handle(context.Background(), Message{UserID: "u1", Text: "/help"})
	if got := f.expect(t); !strings.Contains(got.text, "/new") {
		t.Errorf("命令不该被扣：%q", got.text)
	}

	rec2 := newTurnRecorder()
	off, _ := newCore(t, rec2.run, nil) // 开关默认关
	off.Handle(context.Background(), Message{UserID: "u1", Text: "在吗"})
	if got := rec2.expect(t); got != "在吗" {
		t.Errorf("开关关着时不扣：%q", got)
	}
}

func TestHeldMessagesSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	rec := newTurnRecorder()
	busy(t, availability.State{Activity: "开会", Level: availability.Away, Until: time.Now().Add(time.Hour)})
	c1, _ := holdingCore(t, rec, func(cfg *Config) { cfg.StateDir = dir })
	c1.Handle(context.Background(), Message{UserID: "u1", Text: "在吗"})
	rec.expectNone(t, 100*time.Millisecond)
	c1.Stop()

	availability.Clear("agenda") // 重启期间忙完了
	holdingCore(t, rec, func(cfg *Config) { cfg.StateDir = dir })
	if got := rec.expect(t); got != "在吗" {
		t.Errorf("重启后应把扣着的放出去：%q", got)
	}
}

func TestHeldTurnKnowsItIsLate(t *testing.T) {
	held := make(chan context.Context, 1)
	c, _ := newCore(t, func(ctx context.Context, _, _ string) (string, error) {
		held <- ctx
		return "嗯", nil
	}, func(cfg *Config) { cfg.HoldWhenBusy = true })
	busy(t, availability.State{Activity: "开会", Level: availability.Away, Until: time.Now().Add(100 * time.Millisecond)})
	c.Handle(context.Background(), Message{UserID: "u1", Text: "在吗"})
	select {
	case ctx := <-held:
		h, ok := ctx.Value(heldKey{}).(heldInfo)
		if !ok || h.activity != "开会" || h.asleep {
			t.Errorf("放行那一轮应带着扣留信息：%+v %v", h, ok)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待轮次超时")
	}

	// 注入的那一行：不到一分钟不提，久了说清多久、在忙什么
	msg := Message{heldSince: time.Now().Add(-25 * time.Minute), heldFor: "开会"}
	if got := heldNote(withHeld(context.Background(), msg), ""); !strings.Contains(got, "25 分钟") || !strings.Contains(got, "开会") {
		t.Errorf("heldNote = %q", got)
	}
	msg.heldAsleep = true
	if got := heldNote(withHeld(context.Background(), msg), ""); !strings.Contains(got, "睡着了") {
		t.Errorf("睡着时的说法：%q", got)
	}
	msg.heldSince = time.Now().Add(-10 * time.Second)
	if got := heldNote(withHeld(context.Background(), msg), ""); got != "" {
		t.Errorf("不到一分钟不该提：%q", got)
	}
}
