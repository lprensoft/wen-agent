package clock

import (
	"context"
	"testing"
	"time"
)

func TestNowFallsBackToWallClock(t *testing.T) {
	before := time.Now()
	got := Now(context.Background())
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("没有装钟时应当返回系统时间，得到 %v", got)
	}
	// nil 的 ctx 也走同一条回退：工具链里偶尔会有拿不到 ctx 的调用点
	if got := Now(nil); got.Before(before) || got.After(after) {
		t.Fatalf("nil ctx 应当回退到系统时间，得到 %v", got)
	}
}

func TestWithClock(t *testing.T) {
	fixed := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	ctx := With(context.Background(), Func(func() time.Time { return fixed }))
	if got := Now(ctx); !got.Equal(fixed) {
		t.Fatalf("Now = %v，期望 %v", got, fixed)
	}
	// 派生调用继承钟：这是它能同时被提示词、工具与落盘看见的前提
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	if got := Now(child); !got.Equal(fixed) {
		t.Fatalf("派生的 ctx 丢了钟: %v", got)
	}
}

func TestWithNilIsIgnored(t *testing.T) {
	ctx := With(context.Background(), nil)
	before := time.Now()
	if got := Now(ctx); got.Before(before.Add(-time.Second)) {
		t.Fatalf("nil 钟不该覆盖系统时间，得到 %v", got)
	}
}

func TestInnerClockWins(t *testing.T) {
	outer := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	inner := outer.Add(72 * time.Hour)
	ctx := With(context.Background(), NewManual(outer))
	ctx = With(ctx, NewManual(inner))
	if got := Now(ctx); !got.Equal(inner) {
		t.Fatalf("Now = %v，期望内层的 %v", got, inner)
	}
}

func TestManualAdvance(t *testing.T) {
	start := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	m := NewManual(start)
	if got := m.Now(); !got.Equal(start) {
		t.Fatalf("初始 Now = %v", got)
	}
	m.Advance(8 * time.Hour)
	if got, want := m.Now(), start.Add(8*time.Hour); !got.Equal(want) {
		t.Fatalf("推进后 Now = %v，期望 %v", got, want)
	}
	m.Advance(-time.Hour)
	if got, want := m.Now(), start.Add(7*time.Hour); !got.Equal(want) {
		t.Fatalf("往回拨后 Now = %v，期望 %v", got, want)
	}
	back := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.Set(back)
	if got := m.Now(); !got.Equal(back) {
		t.Fatalf("Set 后 Now = %v", got)
	}
}

// NowOr 是「ctx 优先、插件自己的钟兜底」这条规则的唯一定义。
func TestNowOr(t *testing.T) {
	fixed := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	fallback := func() time.Time { return fixed }
	// 没有钟时用兜底
	if got := NowOr(context.Background(), fallback); !got.Equal(fixed) {
		t.Fatalf("没有钟时应当用兜底，得到 %v", got)
	}
	// 有钟时压过兜底
	later := fixed.Add(72 * time.Hour)
	ctx := With(context.Background(), NewManual(later))
	if got := NowOr(ctx, fallback); !got.Equal(later) {
		t.Fatalf("有钟时应当用它，得到 %v", got)
	}
}

// 同一条时间线会被并发的几路读到，推进时也不能读到撕裂的时间。
func TestManualConcurrent(t *testing.T) {
	m := NewManual(time.Unix(0, 0))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			m.Advance(time.Minute)
		}
	}()
	for i := 0; i < 100; i++ {
		if got := m.Now(); got.IsZero() {
			t.Error("读到零值时间")
		}
	}
	<-done
	if got, want := m.Now(), time.Unix(6000, 0); !got.Equal(want) {
		t.Fatalf("Now = %v，期望 %v", got, want)
	}
}
