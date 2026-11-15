package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"wen/internal/plugin"
)

type stateTestPlugin struct {
	name     string
	sections []plugin.StateSection
	lang     string
}

func (p *stateTestPlugin) Name() string                                  { return p.name }
func (p *stateTestPlugin) Description() string                           { return "测试状态插件" }
func (p *stateTestPlugin) Init(plugin.InitContext, map[string]any) error { return nil }
func (p *stateTestPlugin) SystemPrompt() string                          { return "" }
func (p *stateTestPlugin) Tools() []plugin.Tool                          { return nil }
func (p *stateTestPlugin) State(q plugin.StateQuery) []plugin.StateSection {
	p.lang = q.Lang
	return p.sections
}

func TestStateHandler(t *testing.T) {
	m := plugin.NewManager(plugin.InitContext{}, "")
	p := &stateTestPlugin{name: "presence", sections: []plugin.StateSection{{
		Key: "presence", Title: "Presence", Rows: []plugin.StateRow{{Label: "Location", Value: "书房"}},
	}}}
	if err := m.Register(p, plugin.PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	s := &Server{plugins: m}

	rec := httptest.NewRecorder()
	s.state(rec, httptest.NewRequest("GET", "/api/state?lang=en", nil))

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	if p.lang != "en" {
		t.Errorf("插件收到的语言 = %q，期望 en", p.lang)
	}
	var body plugin.StateSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Plugins) != 1 || body.Plugins[0].Name != "presence" || len(body.Plugins[0].Sections) != 1 {
		t.Fatalf("body = %+v", body)
	}
	if body.Plugins[0].CategoryKey != "other" {
		t.Errorf("category_key = %q，期望 other", body.Plugins[0].CategoryKey)
	}
}

func TestStateHandlerPinned(t *testing.T) {
	m := plugin.NewManager(plugin.InitContext{}, "")
	p := &stateTestPlugin{name: "mixed", sections: []plugin.StateSection{
		{Key: "on", Title: "On", Pinned: true, Summary: "pinned"},
		{Key: "off", Title: "Off"},
	}}
	if err := m.Register(p, plugin.PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	s := &Server{plugins: m}

	rec := httptest.NewRecorder()
	s.state(rec, httptest.NewRequest("GET", "/api/state?pinned=1", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var body plugin.StateSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Plugins) != 1 || len(body.Plugins[0].Sections) != 1 || body.Plugins[0].Sections[0].Key != "on" {
		t.Fatalf("body = %+v", body)
	}
}
