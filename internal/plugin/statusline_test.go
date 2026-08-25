package plugin

import (
	"strings"
	"testing"
)

// reporterPlugin 是实现了 StatusReporter 的测试插件。
type reporterPlugin struct {
	fakePlugin
	lines []string
}

func (p *reporterPlugin) StatusLines(string) []string { return p.lines }

// 状态行只取启用插件、按注册顺序、丢弃空串；未实现接口的插件跳过。
func TestStatusLines(t *testing.T) {
	m := NewManager(InitContext{}, "")
	on := &reporterPlugin{fakePlugin: fakePlugin{name: "on"}, lines: []string{"A", "", "B"}}
	off := &reporterPlugin{fakePlugin: fakePlugin{name: "off"}, lines: []string{"X"}}
	plain := &fakePlugin{name: "plain"}

	if err := m.Register(on, PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Register(plain, PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Register(off, PluginConfig{Enabled: false}); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(m.StatusLines(""), "|")
	if got != "A|B" {
		t.Fatalf("StatusLines = %q，期望 \"A|B\"", got)
	}

	// 启用后应跟着出现，位置按注册顺序而非启用顺序
	if err := m.SetEnabled("off", true); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.StatusLines(""), "|"); got != "A|B|X" {
		t.Fatalf("启用后 StatusLines = %q，期望 \"A|B|X\"", got)
	}
}
