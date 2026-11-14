package plugin

import (
	"strings"
	"testing"
)

// statePlugin 是实现了 StateReporter 的测试插件。
type statePlugin struct {
	fakePlugin
	sections []StateSection
	panic    bool
	category string
	lastQ    StateQuery
}

func (p *statePlugin) State(q StateQuery) []StateSection {
	if p.panic {
		panic("state boom")
	}
	p.lastQ = q
	return p.sections
}

func (p *statePlugin) Category() string { return p.category }

// 只取启用插件、按注册顺序、跳过没有块的插件；语言原样传给实现。
func TestStateCollectsEnabledInOrder(t *testing.T) {
	m := NewManager(InitContext{}, "")
	on := &statePlugin{fakePlugin: fakePlugin{name: "on"}, sections: []StateSection{
		{Key: "a", Title: "A", Rows: []StateRow{{Label: "所在", Value: "书房"}}},
	}}
	plain := &fakePlugin{name: "plain"}
	off := &statePlugin{fakePlugin: fakePlugin{name: "off"}, sections: []StateSection{{Key: "x", Title: "X"}}}
	empty := &statePlugin{fakePlugin: fakePlugin{name: "empty"}}

	for _, p := range []Plugin{on, plain, off, empty} {
		if err := m.Register(p, PluginConfig{Enabled: p != off}); err != nil {
			t.Fatal(err)
		}
	}

	snap := m.State("en")
	if len(snap.Plugins) != 1 || snap.Plugins[0].Name != "on" {
		t.Fatalf("State = %+v，期望只收到 on", snap)
	}
	if on.lastQ.Lang != "en" {
		t.Errorf("实现收到的语言 = %q，期望 en", on.lastQ.Lang)
	}
}

// 单个插件 panic 只少一块，后面的插件照常出现。
func TestStateIsolatesPanic(t *testing.T) {
	m := NewManager(InitContext{}, "")
	bad := &statePlugin{fakePlugin: fakePlugin{name: "bad"}, panic: true}
	good := &statePlugin{fakePlugin: fakePlugin{name: "good"}, sections: []StateSection{{Key: "a", Title: "A"}}}
	for _, p := range []Plugin{bad, good} {
		if err := m.Register(p, PluginConfig{Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	snap := m.State("")
	if len(snap.Plugins) != 1 || snap.Plugins[0].Name != "good" {
		t.Fatalf("panic 后 State = %+v，期望只剩 good", snap)
	}
}

// 行数超过上限时截断并累加 More；插件自己的切片不能被改动。
func TestStateRowCapDoesNotMutatePlugin(t *testing.T) {
	m := NewManager(InitContext{}, "")
	rows := make([]StateRow, maxStateRows+7)
	for i := range rows {
		rows[i] = StateRow{Value: "v"}
	}
	p := &statePlugin{fakePlugin: fakePlugin{name: "many"}, sections: []StateSection{{Key: "a", Title: "A", Rows: rows}}}
	if err := m.Register(p, PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}

	snap := m.State("")
	if len(snap.Plugins) != 1 {
		t.Fatalf("State = %+v，期望一个插件", snap)
	}
	if snap.Plugins[0].Truncated {
		t.Fatalf("行截断不该置 Truncated：%+v", snap.Plugins[0])
	}
	got := snap.Plugins[0].Sections[0]
	if len(got.Rows) != maxStateRows || got.More != 7 {
		t.Fatalf("rows=%d more=%d，期望 %d / 7", len(got.Rows), got.More, maxStateRows)
	}
	if len(p.sections[0].Rows) != maxStateRows+7 || p.sections[0].More != 0 {
		t.Errorf("插件返回的切片被改动了：len=%d more=%d", len(p.sections[0].Rows), p.sections[0].More)
	}
}

// 单块超过字节上限时整块丢掉；总上限触顶时停止收集并标记。
func TestStateByteCaps(t *testing.T) {
	m := NewManager(InitContext{}, "")
	big := &statePlugin{fakePlugin: fakePlugin{name: "big"}, sections: []StateSection{{
		Key: "a", Title: "A", Rows: []StateRow{{Value: strings.Repeat("x", maxStatePerPlugin+1)}},
	}}}
	if err := m.Register(big, PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if snap := m.State(""); len(snap.Plugins) != 0 || !snap.Truncated {
		t.Fatalf("超字节上限后 State = %+v，期望空且标记截断", snap)
	}
}

// 总上限按插件累加：前面装得下，最后一份装不下就停，并标记截断。
func TestStateTotalCap(t *testing.T) {
	m := NewManager(InitContext{}, "")
	// 每份 40 KiB，十三份必然超过 512 KiB
	chunk := strings.Repeat("y", 40<<10)
	for i := 0; i < 13; i++ {
		p := &statePlugin{
			fakePlugin: fakePlugin{name: string(rune('a' + i))},
			sections:   []StateSection{{Key: "a", Title: "A", Rows: []StateRow{{Value: chunk}}}},
		}
		if err := m.Register(p, PluginConfig{Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	snap := m.State("")
	if !snap.Truncated {
		t.Fatal("期望标记总上限截断")
	}
	if len(snap.Plugins) >= 13 {
		t.Fatalf("总上限没有生效，收到 %d 个插件", len(snap.Plugins))
	}
}
