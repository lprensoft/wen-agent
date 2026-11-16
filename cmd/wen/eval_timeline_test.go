package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"wen/internal/availability"
	"wen/internal/clock"
	"wen/internal/plugin"
	"wen/internal/plugin/builtin/heartbeat"
	"wen/internal/session"
)

type fakeTimelineWorker struct {
	name  string
	next  func(now time.Time, runs int) (time.Time, bool)
	onRun func(ctx context.Context, now time.Time) error
	runs  []time.Time
}

func (w *fakeTimelineWorker) Name() string                                  { return w.name }
func (w *fakeTimelineWorker) Description() string                           { return "" }
func (w *fakeTimelineWorker) Init(plugin.InitContext, map[string]any) error { return nil }
func (w *fakeTimelineWorker) SystemPrompt() string                          { return "" }
func (w *fakeTimelineWorker) Tools() []plugin.Tool                          { return nil }

func (w *fakeTimelineWorker) NextBackgroundAt(now time.Time) (time.Time, bool) {
	return w.next(now, len(w.runs))
}

func (w *fakeTimelineWorker) RunBackground(ctx context.Context, now time.Time) error {
	if w.onRun != nil {
		if err := w.onRun(ctx, now); err != nil {
			return err
		}
	}
	w.runs = append(w.runs, now)
	return nil
}

func timelineManager(t *testing.T, workers ...*fakeTimelineWorker) *plugin.Manager {
	t.Helper()
	m := plugin.NewManager(plugin.InitContext{}, "")
	for _, w := range workers {
		if err := m.Register(w, plugin.PluginConfig{Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func TestAdvanceTimelineRunsInTimeOrder(t *testing.T) {
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	base := func(d time.Duration) (time.Time, bool) { return start.Add(d), true }
	a := &fakeTimelineWorker{name: "a", next: func(now time.Time, runs int) (time.Time, bool) {
		switch runs {
		case 0:
			return base(1 * time.Hour) // 10:00
		case 1:
			return base(3 * time.Hour) // 12:00
		}
		return time.Time{}, false
	}}
	b := &fakeTimelineWorker{name: "b", next: func(now time.Time, runs int) (time.Time, bool) {
		if runs == 0 {
			return base(2 * time.Hour) // 11:00
		}
		return time.Time{}, false
	}}
	m := timelineManager(t, a, b)
	clk := clock.NewManual(start)
	obs := &evalObserver{}
	budget := &evalTurnBudget{max: 10}

	if _, _, err := advanceTimeline(context.Background(), clk, m, obs, budget, start.Add(4*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if len(a.runs) != 2 || len(b.runs) != 1 {
		t.Fatalf("运行次数 a=%d b=%d，期望 2/1", len(a.runs), len(b.runs))
	}
	if a.runs[0].Hour() != 10 || b.runs[0].Hour() != 11 || a.runs[1].Hour() != 12 {
		t.Fatalf("顺序错误：a=%v b=%v", a.runs, b.runs)
	}
	if !clk.Now().Equal(start.Add(4 * time.Hour)) {
		t.Fatalf("时钟应停在 horizon，得到 %v", clk.Now())
	}
}

func TestRunDueNowDoesNotAdvance(t *testing.T) {
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	w := &fakeTimelineWorker{name: "a", next: func(now time.Time, runs int) (time.Time, bool) {
		if runs == 0 {
			return start.Add(time.Hour), true
		}
		return time.Time{}, false
	}}
	m := timelineManager(t, w)
	clk := clock.NewManual(start)
	if _, _, err := runDueNow(context.Background(), clk, m, &evalObserver{}, &evalTurnBudget{max: 10}, nil); err != nil {
		t.Fatal(err)
	}
	if len(w.runs) != 0 || !clk.Now().Equal(start) {
		t.Fatalf("tick 不应预支未来：runs=%v clock=%v", w.runs, clk.Now())
	}
}

func TestSameTimestampChainAllowed(t *testing.T) {
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	w := &fakeTimelineWorker{name: "a", next: func(now time.Time, runs int) (time.Time, bool) {
		if runs < 2 {
			return start.Add(time.Hour), true // 同刻事件链
		}
		return time.Time{}, false
	}}
	m := timelineManager(t, w)
	clk := clock.NewManual(start)
	if _, _, err := advanceTimeline(context.Background(), clk, m, &evalObserver{}, &evalTurnBudget{max: 10}, start.Add(2*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if len(w.runs) != 2 {
		t.Fatalf("同刻事件链应执行两次，得到 %d", len(w.runs))
	}
}

func TestWorkerThatNeverConsumesFails(t *testing.T) {
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	w := &fakeTimelineWorker{name: "stuck", next: func(now time.Time, runs int) (time.Time, bool) {
		return start, true // 永远回到同一时刻，且不消费
	}}
	m := timelineManager(t, w)
	clk := clock.NewManual(start)
	_, _, err := advanceTimeline(context.Background(), clk, m, &evalObserver{}, &evalTurnBudget{max: 1000}, start.Add(time.Hour), nil)
	if err == nil {
		t.Fatal("始终不消费待办的 worker 应报错")
	}
}

func TestBudgetAbortsTimeline(t *testing.T) {
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	budget := &evalTurnBudget{max: 1}
	obs := &evalObserver{}
	w := &fakeTimelineWorker{name: "a", next: func(now time.Time, runs int) (time.Time, bool) {
		if runs < 2 {
			return start.Add(time.Duration(runs+1) * time.Hour), true
		}
		return time.Time{}, false
	}}
	w.onRun = func(ctx context.Context, now time.Time) error {
		if err := budget.before(ctx); err != nil {
			return err
		}
		budget.after(ctx, nil)
		obs.OnTurnEnd(ctx, plugin.TurnEndEvent{Origin: "a", EndedAt: now, FinalText: "ok"})
		return nil
	}
	m := timelineManager(t, w)
	clk := clock.NewManual(start)
	_, _, err := advanceTimeline(context.Background(), clk, m, obs, budget, start.Add(3*time.Hour), nil)
	if err == nil {
		t.Fatal("预算用完后应中止")
	}
	if !budget.limited() {
		t.Fatal("limitHit 应为真")
	}
	if len(w.runs) != 1 {
		t.Fatalf("只应执行 1 轮，得到 %d", len(w.runs))
	}
	if used, attempts := budget.snapshot(); used != 1 || len(attempts) != 1 {
		t.Fatalf("预算 used=%d attempts=%d，期望 1/1", used, len(attempts))
	}
}

func TestTimelineStopsOnCancel(t *testing.T) {
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	w := &fakeTimelineWorker{name: "a", next: func(now time.Time, runs int) (time.Time, bool) {
		return start.Add(time.Duration(runs+1) * time.Hour), true
	}}
	m := timelineManager(t, w)
	clk := clock.NewManual(start)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := advanceTimeline(ctx, clk, m, &evalObserver{}, &evalTurnBudget{max: 10}, start.Add(10*time.Hour), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消应向上传播，得到 %v", err)
	}
}

// 到达 horizon 后仍要消费所有同刻到期的 worker，以及该时刻产生的事件链。
func TestHorizonConsumesAllDueWork(t *testing.T) {
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	horizon := start.Add(time.Hour)
	a := &fakeTimelineWorker{name: "a", next: func(now time.Time, runs int) (time.Time, bool) {
		if runs == 0 {
			return horizon, true
		}
		return time.Time{}, false
	}}
	b := &fakeTimelineWorker{name: "b", next: func(now time.Time, runs int) (time.Time, bool) {
		if runs == 0 {
			return horizon, true
		}
		return time.Time{}, false
	}}
	chain := &fakeTimelineWorker{name: "chain", next: func(now time.Time, runs int) (time.Time, bool) {
		if runs < 2 {
			return horizon, true
		}
		return time.Time{}, false
	}}
	m := timelineManager(t, a, b, chain)
	clk := clock.NewManual(start)
	if _, _, err := advanceTimeline(context.Background(), clk, m, &evalObserver{}, &evalTurnBudget{max: 10}, horizon, nil); err != nil {
		t.Fatal(err)
	}
	if len(a.runs) != 1 || len(b.runs) != 1 {
		t.Fatalf("终点同刻的 worker 都应执行，得到 a=%d b=%d", len(a.runs), len(b.runs))
	}
	if len(chain.runs) != 2 {
		t.Fatalf("终点同刻事件链应执行两次，得到 %d", len(chain.runs))
	}
}

type busyWindowWorker struct {
	name string
	runs int
}

func (w *busyWindowWorker) Name() string                                  { return w.name }
func (w *busyWindowWorker) Description() string                           { return "" }
func (w *busyWindowWorker) Init(plugin.InitContext, map[string]any) error { return nil }
func (w *busyWindowWorker) SystemPrompt() string                          { return "" }
func (w *busyWindowWorker) Tools() []plugin.Tool                          { return nil }
func (w *busyWindowWorker) NextBackgroundAt(now time.Time) (time.Time, bool) {
	t := w.at(now)
	return t, !t.IsZero()
}
func (w *busyWindowWorker) RunBackground(ctx context.Context, now time.Time) error {
	w.runs++
	if w.runs == 1 {
		availability.Set(availability.State{
			Source: "agenda", Activity: "开会", Level: availability.Heavy,
			Since: now, Until: now.Add(30 * time.Minute),
		})
	} else {
		availability.Clear("agenda")
	}
	return nil
}

func (w *busyWindowWorker) at(now time.Time) time.Time {
	start := time.Date(2026, 3, 2, 9, 30, 0, 0, time.Local)
	switch w.runs {
	case 0:
		return start
	case 1:
		return start.Add(30 * time.Minute)
	}
	return time.Time{}
}

// 忙碌结束后要留缓冲：手动时间线也必须记录公告板的状态转换。
func TestTimelineKeepsBusyGraceAfterActivity(t *testing.T) {
	availability.Reset()
	t.Cleanup(availability.Reset)
	clk := clock.NewManual(time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local))
	ctx := clock.With(context.Background(), clk)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu   sync.Mutex
		runs []time.Time
	)
	m := plugin.NewManager(plugin.InitContext{
		Lang:       "zh",
		Clock:      clk,
		ManualTime: true,
		Sessions:   store,
		StateDir:   t.TempDir(),
		RunTurn: func(ctx context.Context, _, _ string) (string, error) {
			mu.Lock()
			runs = append(runs, clock.Now(ctx))
			mu.Unlock()
			return "", nil
		},
		NewSession: func() (string, error) {
			meta, err := store.CreateAt(clk.Now())
			return meta.ID, err
		},
	}, "", plugin.WithStateDir(t.TempDir()))
	if _, err := store.CreateAt(clk.Now()); err != nil {
		t.Fatal(err)
	}
	busy := &busyWindowWorker{name: "busy"}
	if err := m.Register(busy, plugin.PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Register(heartbeat.New(), plugin.PluginConfig{
		Enabled: true,
		Config:  map[string]any{"dynamic": true, "interval_minutes": 30, "min_minutes": 30, "max_minutes": 30},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.StopAll)

	if _, _, err := advanceTimeline(ctx, clk, m, &evalObserver{}, &evalTurnBudget{max: 10},
		time.Date(2026, 3, 2, 10, 30, 0, 0, time.Local), nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(runs) != 1 || !runs[0].Equal(time.Date(2026, 3, 2, 10, 5, 0, 0, time.Local)) {
		t.Fatalf("忙碌结束后应等到 10:05 再开口，实际 %v", runs)
	}
}
