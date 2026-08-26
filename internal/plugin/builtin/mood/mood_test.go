package mood

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

func newTestPlugin(t *testing.T, cfg map[string]any) *Plugin {
	t.Helper()
	p := New()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, cfg); err != nil {
		t.Fatalf("Init 失败: %v", err)
	}
	return p
}

func scoped(write string, read []string) context.Context {
	return plugin.WithScope(context.Background(), plugin.Scope{Write: write, Read: read})
}

func TestInitRejectsWithoutStateDir(t *testing.T) {
	if err := New().Init(plugin.InitContext{}, nil); err == nil {
		t.Fatal("没有持久化目录时应当拒绝启用")
	}
}

func TestInitDefaultsAndReentrant(t *testing.T) {
	dir := t.TempDir()
	p := New()
	if err := p.Init(plugin.InitContext{StateDir: dir}, nil); err != nil {
		t.Fatal(err)
	}
	if s := p.snapshot(); s.decayPerHour != defaultDecayPerHour || s.maxDelta != defaultMaxDelta {
		t.Fatalf("默认配置 = %+v", s)
	}
	old := p.storeFor("")
	if err := p.Init(plugin.InitContext{StateDir: dir}, map[string]any{
		"decay_per_hour": 20, "max_delta_per_call": 5,
	}); err != nil {
		t.Fatal(err)
	}
	if p.storeFor("") == old {
		t.Error("重新 Init 后域库表应当被重建")
	}
	if s := p.snapshot(); s.decayPerHour != 20 || s.maxDelta != 5 {
		t.Errorf("新配置未生效: %+v", s)
	}
}

func TestSystemPromptIsStaticAndCheap(t *testing.T) {
	// 未 Init 的插件也会被列表接口调到，不能崩也不能读盘
	got := New().SystemPrompt()
	for _, want := range []string{"adjust_mood", "先用 adjust_mood", "按平静起算", "不报数值"} {
		if !strings.Contains(got, want) {
			t.Errorf("system 提示词缺少 %q", want)
		}
	}
}

func TestBands(t *testing.T) {
	cases := []struct {
		v    int
		want string
	}{
		{-100, "消沉"}, {-70, "消沉"}, {-69, "难过"}, {-40, "难过"},
		{-39, "低落"}, {-15, "低落"}, {-14, "平静"}, {0, "平静"}, {14, "平静"},
		{15, "愉快"}, {39, "愉快"}, {40, "开心"}, {69, "开心"}, {70, "雀跃"}, {100, "雀跃"},
	}
	for _, c := range cases {
		if got := band("", c.v); got != c.want {
			t.Errorf("band(%d) = %q，期望 %q", c.v, got, c.want)
		}
	}
}

func TestRenderState(t *testing.T) {
	if got := renderState("", State{Value: 0}); got != "此刻：平静。" {
		t.Errorf("平静时 = %q", got)
	}
	got := renderState("", State{Value: 22, Reason: "她夸了新剪的头发"})
	if !strings.Contains(got, "愉快（+22）") || !strings.Contains(got, "起因：她夸了新剪的头发") {
		t.Errorf("渲染 = %q", got)
	}
	if !strings.Contains(renderState("", State{Value: -30}), "（-30）") {
		t.Error("负值应当带符号显示")
	}
}

func TestTurnPromptEmptyWhenNoRecord(t *testing.T) {
	p := newTestPlugin(t, nil)
	got, err := p.TurnPrompt(scoped("", nil), plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("没有记录时不该注入，得到 %q", got)
	}
}

func TestTurnPromptRendersCurrent(t *testing.T) {
	p := newTestPlugin(t, map[string]any{"decay_per_hour": 0})
	ctx := scoped("", nil)
	if _, err := (&adjustTool{p: p}).Execute(ctx, json.RawMessage(`{"delta":22,"reason":"她夸了新剪的头发"}`)); err != nil {
		t.Fatal(err)
	}
	got, err := p.TurnPrompt(ctx, plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{stateHeaderFor(""), "愉快（+22）", "她夸了新剪的头发"} {
		if !strings.Contains(got, want) {
			t.Errorf("注入缺少 %q，实际:\n%s", want, got)
		}
	}
}

// 心情只读写本轮的写入域：两个人格各有各的心情，互不影响也互不可见。
func TestMoodIsPerDomain(t *testing.T) {
	p := newTestPlugin(t, map[string]any{"decay_per_hour": 0})
	outer := scoped("", []string{})
	inner := scoped("inner", []string{"inner"})
	tool := &adjustTool{p: p}

	if _, err := tool.Execute(outer, json.RawMessage(`{"delta":30,"reason":"表人格很开心"}`)); err != nil {
		t.Fatal(err)
	}
	// 里人格此时还没有自己的心情，不该继承表人格那一份
	if got, err := p.TurnPrompt(inner, plugin.TurnEvent{}); err != nil || got != "" {
		t.Fatalf("里人格读到了 %q err=%v，期望空", got, err)
	}

	if _, err := tool.Execute(inner, json.RawMessage(`{"delta":-25,"reason":"里人格不太好"}`)); err != nil {
		t.Fatal(err)
	}
	outerPrompt, err := p.TurnPrompt(outer, plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	innerPrompt, err := p.TurnPrompt(inner, plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outerPrompt, "+30") || strings.Contains(outerPrompt, "里人格") {
		t.Errorf("表人格的心情被里人格影响了:\n%s", outerPrompt)
	}
	if !strings.Contains(innerPrompt, "-25") || strings.Contains(innerPrompt, "表人格") {
		t.Errorf("里人格的心情不对:\n%s", innerPrompt)
	}
}

func TestAdjustToolEcho(t *testing.T) {
	p := newTestPlugin(t, map[string]any{"decay_per_hour": 0, "max_delta_per_call": 60})
	out, err := (&adjustTool{p: p}).Execute(scoped("", nil), json.RawMessage(`{"delta":45,"reason":"久别重逢"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "开心") || !strings.Contains(out, "+45") {
		t.Errorf("回显 = %q", out)
	}
}

// 拦截生效时要把规则告诉模型，否则它只会换个更大的数字再试一次。
func TestAdjustToolReportsClamping(t *testing.T) {
	p := newTestPlugin(t, map[string]any{"max_delta_per_call": 10, "decay_per_hour": 0})
	out, err := (&adjustTool{p: p}).Execute(scoped("", nil), json.RawMessage(`{"delta":80,"reason":"太夸张了"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "±10") || !strings.Contains(out, "+80") {
		t.Errorf("回显应当说明被收了以及原报的值，实际 = %q", out)
	}
	if !strings.Contains(out, "+10）") {
		t.Errorf("回显的心情值应当是收敛后的 +10，实际 = %q", out)
	}
}

func TestAdjustToolRejectsBadArgs(t *testing.T) {
	p := newTestPlugin(t, nil)
	tool := &adjustTool{p: p}
	if _, err := tool.Execute(scoped("", nil), json.RawMessage(`{"delta":0,"reason":"没事发生"}`)); err == nil {
		t.Error("变化量为 0 应当报错")
	}
	if _, err := tool.Execute(scoped("", nil), json.RawMessage(`不是 JSON`)); err == nil {
		t.Error("参数格式错误应当报错")
	}
	var probe map[string]any
	if err := json.Unmarshal(tool.Schema(), &probe); err != nil {
		t.Errorf("schema 不是合法 JSON: %v", err)
	}
}

// 隔了一夜再对话，心情应当已经回落，而不是接着昨天的值继续。
func TestMoodDecaysBetweenSessions(t *testing.T) {
	p := newTestPlugin(t, map[string]any{"decay_per_hour": 5})
	ctx := scoped("", nil)
	store := p.storeFor("")
	yesterday := time.Now().Add(-8 * time.Hour)
	if _, _, err := store.Apply(60, 100, 5, "昨晚很开心", yesterday); err != nil {
		t.Fatal(err)
	}
	got, err := p.TurnPrompt(ctx, plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "+20") {
		t.Errorf("八小时后应当回落到 +20，实际:\n%s", got)
	}
}

func TestResetActionWipesAllDomains(t *testing.T) {
	p := newTestPlugin(t, map[string]any{"decay_per_hour": 0})
	outer := scoped("", []string{})
	inner := scoped("inner", []string{"inner"})
	tool := &adjustTool{p: p}
	if _, err := tool.Execute(outer, json.RawMessage(`{"delta":30,"reason":"开心"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(inner, json.RawMessage(`{"delta":-30,"reason":"难过"}`)); err != nil {
		t.Fatal(err)
	}

	if st, _ := p.ActionState(actionReset); st.Status != plugin.ActionIdle {
		t.Errorf("初始状态 = %q，期望 idle", st.Status)
	}
	if err := p.StartAction(context.Background(), actionReset); err != nil {
		t.Fatalf("重置失败: %v", err)
	}
	if st, _ := p.ActionState(actionReset); st.Status != plugin.ActionDone {
		t.Errorf("重置后状态 = %q，期望 done", st.Status)
	}
	for _, ctx := range []context.Context{outer, inner} {
		if got, err := p.TurnPrompt(ctx, plugin.TurnEvent{}); err != nil || got != "" {
			t.Errorf("重置后仍注入 %q err=%v", got, err)
		}
	}
	if err := p.StartAction(context.Background(), "不存在"); err == nil {
		t.Error("未知操作应当报错")
	}
}
