package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
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

// stateEditTestPlugin 在只读测试插件之上记录一次修正请求。
type stateEditTestPlugin struct {
	stateTestPlugin
	last plugin.StateEditRequest
	err  error
}

func (p *stateEditTestPlugin) EditState(req plugin.StateEditRequest) error {
	p.last = req
	return p.err
}

func TestEditPluginState(t *testing.T) {
	m := plugin.NewManager(plugin.InitContext{}, "")
	p := &stateEditTestPlugin{stateTestPlugin: stateTestPlugin{name: "people"}}
	if err := m.Register(p, plugin.PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	s := &Server{plugins: m}

	body := `{"domain":"","section":"people","row":"林舟","op":"edit","values":{"relation":"同事"}}`
	req := httptest.NewRequest("POST", "/api/plugins/people/state/edit?lang=en", strings.NewReader(body))
	req.SetPathValue("name", "people")
	rec := httptest.NewRecorder()
	s.editPluginState(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if p.last.Section != "people" || p.last.Row != "林舟" || p.last.Lang != "en" {
		t.Fatalf("插件收到的请求 = %+v", p.last)
	}
	if p.last.Values["relation"] != "同事" {
		t.Fatalf("值没有原样交回：%+v", p.last.Values)
	}

	// 记忆标题不截断，行身份可能超过几百字节；只要在请求体上限内就该放行。
	longRow := strings.Repeat("记", 100) // 300 字节
	body = `{"domain":"","section":"index","row":"` + longRow + `","op":"delete"}`
	req = httptest.NewRequest("POST", "/api/plugins/people/state/edit", strings.NewReader(body))
	req.SetPathValue("name", "people")
	rec = httptest.NewRecorder()
	s.editPluginState(rec, req)
	if rec.Code != 200 {
		t.Fatalf("长行身份 status = %d body = %s", rec.Code, rec.Body)
	}
	if p.last.Row != longRow {
		t.Fatalf("长行身份被截断：%d 字节", len(p.last.Row))
	}
}

// 非法域名、坏 JSON 与超量值都在 HTTP 层拒绝，不进插件。
func TestEditPluginStateRejectsBadInput(t *testing.T) {
	m := plugin.NewManager(plugin.InitContext{}, "")
	p := &stateEditTestPlugin{stateTestPlugin: stateTestPlugin{name: "people"}}
	if err := m.Register(p, plugin.PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	s := &Server{plugins: m}

	cases := []string{
		`{"domain":"../x","section":"people","op":"delete","row":"a"}`,
		`{"domain":"","section":"people","op":"` + strings.Repeat("x", 100) + `"}`,
		`not json`,
	}
	for _, body := range cases {
		req := httptest.NewRequest("POST", "/api/plugins/people/state/edit", strings.NewReader(body))
		req.SetPathValue("name", "people")
		rec := httptest.NewRecorder()
		s.editPluginState(rec, req)
		if rec.Code != 400 {
			t.Errorf("body=%q status = %d，期望 400", body, rec.Code)
		}
	}
	if p.last.Op != "" {
		t.Errorf("坏请求不该进插件：%+v", p.last)
	}
}
