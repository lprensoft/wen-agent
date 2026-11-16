package health

import (
	"context"
	"testing"
	"time"

	"wen/internal/clock"
	"wen/internal/plugin"
)

func TestNextBackgroundAtFutureOnset(t *testing.T) {
	now := time.Date(2026, 3, 2, 8, 0, 0, 0, time.Local)
	clk := clock.NewManual(now)
	ctx := clock.With(context.Background(), clk)

	p := New()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir(), Clock: clk, ManualTime: true}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)

	if _, err := call(t, p, ctx, "set_condition", `{"name":"感冒","severity":"有点不舒服","onset_hours":5,"days":2,"care":"硬扛"}`); err != nil {
		t.Fatal(err)
	}

	next, ok := p.NextBackgroundAt(now)
	if !ok || !next.Equal(now.Add(5*time.Hour)) {
		t.Fatalf("下一事件 = %v ok=%v，期望 5 小时后", next, ok)
	}
	if err := p.RunBackground(ctx, now.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if next, ok := p.NextBackgroundAt(now.Add(5 * time.Hour)); ok {
		t.Fatalf("理由投过之后不应再调度，得到 %v", next)
	}
}
