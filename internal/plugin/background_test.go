package plugin

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeWorker struct {
	*fakePlugin
	next  time.Time
	ok    bool
	runs  int
	panic bool
}

func (w *fakeWorker) NextBackgroundAt(time.Time) (time.Time, bool) { return w.next, w.ok }

func (w *fakeWorker) RunBackground(context.Context, time.Time) error {
	w.runs++
	if w.panic {
		panic("boom")
	}
	return nil
}

type fakeSettler struct {
	*fakePlugin
	requires []string
	order    *[]string
	did      int
	err      error
}

func (s *fakeSettler) Requires() []string { return s.requires }

func (s *fakeSettler) Settle(context.Context) (bool, error) {
	*s.order = append(*s.order, s.name)
	if s.err != nil {
		return false, s.err
	}
	if s.did > 0 {
		s.did--
		return true, nil
	}
	return false, nil
}

func TestBackgroundWorkersOrderAndDisabled(t *testing.T) {
	m := NewManager(InitContext{}, "")
	a := &fakeWorker{fakePlugin: &fakePlugin{name: "a"}, ok: true}
	b := &fakeWorker{fakePlugin: &fakePlugin{name: "b"}, ok: true}
	c := &fakeWorker{fakePlugin: &fakePlugin{name: "c"}, ok: true}
	for _, p := range []struct {
		w       *fakeWorker
		enabled bool
	}{{a, true}, {b, false}, {c, true}} {
		if err := m.Register(p.w, PluginConfig{Enabled: p.enabled}); err != nil {
			t.Fatal(err)
		}
	}
	got := m.BackgroundWorkers()
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Fatalf("BackgroundWorkers = %+v，期望按注册顺序只含 a、c", got)
	}
}

func TestRunBackgroundPanicIsRecovered(t *testing.T) {
	m := NewManager(InitContext{}, "")
	w := &fakeWorker{fakePlugin: &fakePlugin{name: "boom"}, panic: true, ok: true}
	if err := m.Register(w, PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.RunBackground(context.Background(), "boom", time.Now()); err == nil {
		t.Fatal("panic 应转成 error")
	}
	if w.runs != 1 {
		t.Fatalf("RunBackground 调用了 %d 次", w.runs)
	}
}

func TestSettleOrderFollowsRequires(t *testing.T) {
	m := NewManager(InitContext{}, "")
	order := []string{}
	roleplay := &fakePlugin{name: "roleplay"}
	memory := &fakeSettler{fakePlugin: &fakePlugin{name: "memory"}, order: &order}
	growth := &fakeSettler{fakePlugin: &fakePlugin{name: "growth"}, requires: []string{"roleplay", "memory"}, order: &order}

	// 故意按提示词注册顺序 growth → memory → roleplay 注册。
	if err := m.Register(growth, PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Register(memory, PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Register(roleplay, PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.ValidateSettleOrder(); err != nil {
		t.Fatal(err)
	}
	if err := m.Settle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "memory" || order[1] != "growth" {
		t.Fatalf("结算顺序 = %v，期望 memory 先于 growth", order)
	}
}

func TestSettleCycleFails(t *testing.T) {
	m := NewManager(InitContext{}, "")
	order := []string{}
	a := &fakeSettler{fakePlugin: &fakePlugin{name: "a"}, requires: []string{"b"}, order: &order}
	b := &fakeSettler{fakePlugin: &fakePlugin{name: "b"}, requires: []string{"a"}, order: &order}
	// 绕过依赖校验，直接构造出环：这里测的是结算排序本身。
	m.entries["a"] = &entry{plugin: a, enabled: true}
	m.entries["b"] = &entry{plugin: b, enabled: true}
	m.order = []string{"a", "b"}

	if err := m.ValidateSettleOrder(); err == nil {
		t.Fatal("依赖环应报错")
	}
}

func TestSettlePropagatesError(t *testing.T) {
	m := NewManager(InitContext{}, "")
	order := []string{}
	bad := &fakeSettler{fakePlugin: &fakePlugin{name: "bad"}, order: &order, err: errors.New("boom")}
	if err := m.Register(bad, PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Settle(context.Background()); err == nil {
		t.Fatal("Settle 应把插件错误向上返回")
	}
}

func TestSettleNotConverging(t *testing.T) {
	m := NewManager(InitContext{}, "")
	order := []string{}
	always := &fakeSettler{fakePlugin: &fakePlugin{name: "always"}, order: &order, did: maxSettleRounds + 10}
	if err := m.Register(always, PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Settle(context.Background()); err == nil {
		t.Fatal("永远有待办时应报不收敛")
	}
}
