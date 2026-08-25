package presence

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

func newTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	p := New()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, nil); err != nil {
		t.Fatal(err)
	}
	return p
}

func scopedCtx(tag string) context.Context {
	if tag == "" {
		return context.Background()
	}
	return plugin.WithScope(context.Background(), plugin.Scope{Write: tag, Read: []string{tag}})
}

func exec(t *testing.T, p *Plugin, ctx context.Context, args string) string {
	t.Helper()
	out, err := p.Tools()[0].Execute(ctx, json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestInitRequiresStateDir(t *testing.T) {
	if err := New().Init(plugin.InitContext{}, nil); err == nil {
		t.Fatal("没有持久化目录应拒绝启用")
	}
}

func TestApplyUpdateAndClear(t *testing.T) {
	s := NewStore("", t.TempDir())
	now := time.Now()

	updated, cleared, err := s.Apply(map[string]string{"location": "卧室", "attire": "睡裙"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(updated, ",") != "所在,穿着" || len(cleared) != 0 {
		t.Fatalf("updated=%v cleared=%v", updated, cleared)
	}

	// 空串清除；没提到的字段保持原样
	updated, cleared, err = s.Apply(map[string]string{"attire": " "}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != 0 || strings.Join(cleared, ",") != "穿着" {
		t.Fatalf("updated=%v cleared=%v", updated, cleared)
	}
	snap, ok, _ := s.Load()
	if !ok || snap["location"].Text != "卧室" {
		t.Fatalf("snap = %+v", snap)
	}
	if _, has := snap["attire"]; has {
		t.Error("attire 应已被清除")
	}
}

func TestApplyRejectsUnknownField(t *testing.T) {
	s := NewStore("", t.TempDir())
	if _, _, err := s.Apply(map[string]string{"mood": "开心"}, time.Now()); err == nil {
		t.Fatal("未知字段应报错")
	}
}

func TestApplyClipsText(t *testing.T) {
	s := NewStore("", t.TempDir())
	long := strings.Repeat("长", 100) + "\n换行也压掉"
	if _, _, err := s.Apply(map[string]string{"location": long}, time.Now()); err != nil {
		t.Fatal(err)
	}
	snap, _, _ := s.Load()
	got := snap["location"].Text
	if len([]rune(got)) != 60 {
		t.Errorf("len = %d, want 60", len([]rune(got)))
	}
	if strings.Contains(got, "\n") {
		t.Error("换行应被压掉")
	}
}

func TestApplyAllClearedRemovesFile(t *testing.T) {
	s := NewStore("", t.TempDir())
	if _, _, err := s.Apply(map[string]string{"location": "卧室"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Apply(map[string]string{"location": ""}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Load(); ok {
		t.Error("全部清除后应视为无记录")
	}
}

func TestTurnPromptRendersSnapshot(t *testing.T) {
	p := newTestPlugin(t)
	ctx := scopedCtx("")
	exec(t, p, ctx, `{"location":"书房","posture":"坐在窗边","focus":"雨声"}`)

	got, err := p.TurnPrompt(ctx, plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[当下状态]", "所在：书房", "姿态：坐在窗边", "感官焦点：雨声"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q，got:\n%s", want, got)
		}
	}
	// 新鲜字段不标时间
	if strings.Contains(got, "前记下") {
		t.Errorf("新鲜字段不该标时间：%s", got)
	}
	// 没写过的字段整行不出现
	if strings.Contains(got, "穿着") {
		t.Errorf("空字段不该出现：%s", got)
	}
}

func TestTurnPromptEmptyWithoutRecord(t *testing.T) {
	p := newTestPlugin(t)
	got, err := p.TurnPrompt(scopedCtx(""), plugin.TurnEvent{})
	if err != nil || got != "" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestRenderMarksStaleFields(t *testing.T) {
	now := time.Now()
	snap := Snapshot{
		"location": {Text: "卧室", Updated: now.Add(-3 * time.Hour)},
		"focus":    {Text: "雨声", Updated: now.Add(-time.Minute)},
	}
	got := renderSnapshot("", snap, now)
	if !strings.Contains(got, "所在：卧室（3 小时前记下）") {
		t.Errorf("旧字段应标时间：%s", got)
	}
	if strings.Contains(got, "雨声（") {
		t.Errorf("新字段不该标时间：%s", got)
	}
}

func TestDomainsIsolated(t *testing.T) {
	p := newTestPlugin(t)
	exec(t, p, scopedCtx("outer"), `{"location":"办公室"}`)
	exec(t, p, scopedCtx("inner"), `{"location":"卧室"}`)

	outer, _ := p.TurnPrompt(scopedCtx("outer"), plugin.TurnEvent{})
	inner, _ := p.TurnPrompt(scopedCtx("inner"), plugin.TurnEvent{})
	if !strings.Contains(outer, "办公室") || strings.Contains(outer, "卧室") {
		t.Errorf("outer = %q", outer)
	}
	if !strings.Contains(inner, "卧室") || strings.Contains(inner, "办公室") {
		t.Errorf("inner = %q", inner)
	}
}

func TestToolDistinguishesAbsentFromEmpty(t *testing.T) {
	p := newTestPlugin(t)
	ctx := scopedCtx("")
	exec(t, p, ctx, `{"location":"书房","attire":"衬衫"}`)

	// 只清 attire，location 没传应保持原样
	out := exec(t, p, ctx, `{"attire":""}`)
	if !strings.Contains(out, "已清除：穿着") {
		t.Errorf("out = %q", out)
	}
	got, _ := p.TurnPrompt(ctx, plugin.TurnEvent{})
	if !strings.Contains(got, "书房") {
		t.Errorf("没传的字段被动了：%s", got)
	}
}

func TestToolRequiresAtLeastOneField(t *testing.T) {
	p := newTestPlugin(t)
	if _, err := p.Tools()[0].Execute(scopedCtx(""), json.RawMessage(`{}`)); err == nil {
		t.Fatal("空参数应报错")
	}
}

func TestActionClearsAllDomains(t *testing.T) {
	p := newTestPlugin(t)
	exec(t, p, scopedCtx(""), `{"location":"客厅"}`)
	exec(t, p, scopedCtx("inner"), `{"location":"卧室"}`)

	if err := p.StartAction(context.Background(), actionClear); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"", "inner"} {
		if got, _ := p.TurnPrompt(scopedCtx(tag), plugin.TurnEvent{}); got != "" {
			t.Errorf("域 %q 未被清空：%s", tag, got)
		}
	}
	st, err := p.ActionState(actionClear)
	if err != nil || st.Status != plugin.ActionDone {
		t.Errorf("state=%+v err=%v", st, err)
	}
}

func TestPromptsCarryNoIdentity(t *testing.T) {
	p := newTestPlugin(t)
	all := p.SystemPrompt() + p.Description() + p.Tools()[0].Description()
	for _, bad := range []string{"Claude", "GPT", "Anthropic", "OpenAI", "助手"} {
		if strings.Contains(all, bad) {
			t.Errorf("提示词不应包含身份信息 %q", bad)
		}
	}
}
