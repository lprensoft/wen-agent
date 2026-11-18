package plugin

import (
	"strings"
	"testing"
)

// editPlugin 同时实现 StateReporter 与 StateEditor，记录最后一次请求。
type editPlugin struct {
	fakePlugin
	sections []StateSection
	lastReq  StateEditRequest
	panic    bool
	editErr  error
	calls    int
}

func (p *editPlugin) State(StateQuery) []StateSection { return p.sections }

func (p *editPlugin) EditState(req StateEditRequest) error {
	if p.panic {
		panic("edit boom")
	}
	p.calls++
	p.lastReq = req
	return p.editErr
}

func stateEditRequest() StateEditRequest {
	return StateEditRequest{
		Lang: "en", Domain: "inner", Section: "people", Row: "林舟", Op: "edit",
		Values: map[string]string{"relation": "同事"},
	}
}

// 只有启用且实现了 StateEditor 的插件能收到修正；其余情况都拒绝。

func TestStateEditTouched(t *testing.T) {
	req := StateEditRequest{
		Values:    map[string]string{"a": "new", "b": "same", "c": "only"},
		Originals: map[string]string{"a": "old", "b": "same"},
	}
	if !req.Touched("a") {
		t.Error("改了值应算 touched")
	}
	if req.Touched("b") {
		t.Error("原样带回不算 touched")
	}
	if !req.Touched("c") {
		t.Error("没有原始值的字段应算 touched（旧客户端）")
	}
	if req.Touched("missing") {
		t.Error("没提交的字段不算 touched")
	}
	legacy := StateEditRequest{Values: map[string]string{"a": "any"}}
	if !legacy.Touched("a") {
		t.Error("没有 Originals 时应按 touched 处理")
	}
}

func TestEditStateRouting(t *testing.T) {
	m := NewManager(InitContext{Lang: "en"}, "")
	ed := &editPlugin{fakePlugin: fakePlugin{name: "ed"}}
	plain := &fakePlugin{name: "plain"}
	off := &editPlugin{fakePlugin: fakePlugin{name: "off"}}
	for _, p := range []Plugin{ed, plain, off} {
		if err := m.Register(p, PluginConfig{Enabled: p != off}); err != nil {
			t.Fatal(err)
		}
	}

	req := stateEditRequest()
	if err := m.EditState("ed", req); err != nil {
		t.Fatalf("EditState = %v", err)
	}
	if ed.calls != 1 || ed.lastReq.Row != "林舟" || ed.lastReq.Values["relation"] != "同事" {
		t.Fatalf("插件收到的请求 = %+v（调用 %d 次）", ed.lastReq, ed.calls)
	}

	if err := m.EditState("off", req); err == nil {
		t.Error("禁用的插件不应收到修正")
	}
	if err := m.EditState("plain", req); err == nil {
		t.Error("未实现 StateEditor 的插件不应收到修正")
	}
	if err := m.EditState("missing", req); err == nil {
		t.Error("不存在的插件不应收到修正")
	}
}

// 插件 panic 要变成一条错误返回，而不是把界面请求打挂。
func TestEditStateRecoversPanic(t *testing.T) {
	m := NewManager(InitContext{Lang: "en"}, "")
	p := &editPlugin{fakePlugin: fakePlugin{name: "boom"}, panic: true}
	if err := m.Register(p, PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.EditState("boom", stateEditRequest()); err == nil {
		t.Fatal("panic 应转成错误")
	}
}

// pinned 查询只带走 Summary：编辑描述与行身份都要裁掉，且不能改动插件自己的切片。
func TestStatePinnedStripsEdits(t *testing.T) {
	m := NewManager(InitContext{}, "")
	row := StateRow{
		Value: "林舟", ID: "林舟",
		Edits: []StateEdit{{Op: "edit", Label: "修改", Fields: []StateField{{Key: "relation", Label: "关系", Value: "同事"}}}},
	}
	p := &editPlugin{
		fakePlugin: fakePlugin{name: "people"},
		sections: []StateSection{{
			Key: "people", Title: "People", Pinned: true, Summary: "1",
			Rows:  []StateRow{row},
			Edits: []StateEdit{{Op: "add", Label: "新增"}},
		}},
	}
	if err := m.Register(p, PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}

	snap := m.State("", true)
	if len(snap.Plugins) != 1 || len(snap.Plugins[0].Sections) != 1 {
		t.Fatalf("State = %+v", snap)
	}
	s := snap.Plugins[0].Sections[0]
	if len(s.Edits) != 0 {
		t.Errorf("pinned 块仍带编辑描述：%+v", s.Edits)
	}
	if len(s.Rows) != 1 || len(s.Rows[0].Edits) != 0 || s.Rows[0].ID != "" {
		t.Errorf("pinned 行仍带编辑描述：%+v", s.Rows[0])
	}
	// 插件自己的切片不能被裁：下一次全量查询还要用。
	if len(p.sections[0].Edits) != 1 || len(p.sections[0].Rows[0].Edits) != 1 || p.sections[0].Rows[0].ID != "林舟" {
		t.Errorf("pinned 查询改动了插件返回的切片：%+v", p.sections[0])
	}
}

// 编辑描述计入字节上限：不把字段值算进去，插件就能借表单绕开每插件上限。
func TestStateEditFieldsCountTowardsCap(t *testing.T) {
	m := NewManager(InitContext{}, "")
	big := strings.Repeat("x", maxStatePerPlugin+1)
	p := &editPlugin{
		fakePlugin: fakePlugin{name: "big"},
		sections: []StateSection{{
			Key: "a", Title: "A",
			Rows: []StateRow{{Value: "v", Edits: []StateEdit{{Op: "edit", Label: "修改", Fields: []StateField{{Key: "k", Value: big}}}}}},
		}},
	}
	if err := m.Register(p, PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if snap := m.State("", false); len(snap.Plugins) != 0 || !snap.Truncated {
		t.Fatalf("超上限后 State = %+v，期望空且标记截断", snap)
	}
}

// 编辑描述本身也有硬上限：单块 4 个操作、单操作 8 个字段，插件切片不被改动。
func TestStateEditsAreClamped(t *testing.T) {
	m := NewManager(InitContext{}, "")
	edits := make([]StateEdit, 6)
	for i := range edits {
		fields := make([]StateField, 10)
		for j := range fields {
			fields[j] = StateField{Key: "k", Label: "字段"}
		}
		edits[i] = StateEdit{Op: "edit", Label: "修改", Fields: fields}
	}
	p := &editPlugin{
		fakePlugin: fakePlugin{name: "many"},
		sections:   []StateSection{{Key: "a", Title: "A", Edits: edits}},
	}
	if err := m.Register(p, PluginConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	got := m.State("", false).Plugins[0].Sections[0].Edits
	if len(got) != maxStateEdits {
		t.Fatalf("操作数 = %d，期望 %d", len(got), maxStateEdits)
	}
	if len(got[0].Fields) != maxStateFields {
		t.Fatalf("字段数 = %d，期望 %d", len(got[0].Fields), maxStateFields)
	}
	if len(p.sections[0].Edits) != 6 || len(p.sections[0].Edits[0].Fields) != 10 {
		t.Error("裁编辑描述改动了插件返回的切片")
	}
}
