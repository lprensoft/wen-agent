package unspoken

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

func newTestPlugin(t *testing.T, cfg map[string]any) *Plugin {
	t.Helper()
	p := New()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, cfg); err != nil {
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

func call(t *testing.T, p *Plugin, ctx context.Context, name, args string) (string, error) {
	t.Helper()
	for _, tool := range p.Tools() {
		if tool.Name() == name {
			return tool.Execute(ctx, json.RawMessage(args))
		}
	}
	t.Fatalf("没有工具 %s", name)
	return "", nil
}

func ok(t *testing.T, p *Plugin, ctx context.Context, name, args string) string {
	t.Helper()
	out, err := call(t, p, ctx, name, args)
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

func TestStoreKeepValidatesAndEvicts(t *testing.T) {
	s := NewStore("", t.TempDir())
	now := time.Now()

	if _, err := s.Keep("  ", now, 3); err == nil {
		t.Error("空内容应报错")
	}
	if _, err := s.Keep(strings.Repeat("话", maxTextRunes+1), now, 3); err == nil || !strings.Contains(err.Error(), "上限") {
		t.Errorf("超长应报错并说明上限: %v", err)
	}
	if _, err := s.Keep(strings.Repeat("话", maxTextRunes), now, 3); err != nil {
		t.Errorf("恰好到上限不该报错: %v", err)
	}
	if es, _ := s.List(); len(es) != 1 {
		t.Fatalf("校验失败的不该落盘: %d 条", len(es))
	}

	// 同样的话不重复记
	res, err := s.Keep(strings.Repeat("话", maxTextRunes), now, 3)
	if err != nil || !res.Duplicate || res.Index != 1 {
		t.Errorf("重复应被识别: %+v, %v", res, err)
	}

	// 满了淘汰最早的一条
	s.Keep("第二条", now, 3)
	s.Keep("第三条", now, 3)
	res, err = s.Keep("第四条", now, 3)
	if err != nil || res.Index != 3 || len(res.Dropped) != 1 || res.Dropped[0] != strings.Repeat("话", maxTextRunes) {
		t.Fatalf("应淘汰最早的一条: %+v, %v", res, err)
	}
	es, _ := s.List()
	if len(es) != 3 || es[0].Text != "第二条" || es[2].Text != "第四条" {
		t.Errorf("淘汰后的顺序不对: %+v", es)
	}

	// 上限被调小：一次淘汰到位
	res, _ = s.Keep("第五条", now, 2)
	if len(res.Dropped) != 2 || res.Index != 2 {
		t.Errorf("上限调小后应淘汰多条: %+v", res)
	}
}

func TestStoreLetGoByIndexAndFragment(t *testing.T) {
	s := NewStore("", t.TempDir())
	now := time.Now()
	if _, err := s.LetGo(1, ""); err == nil {
		t.Error("空清单应报错")
	}
	for _, text := range []string{"他忘了纪念日，说不在意其实还在意", "想约他周末去看展，还没开口", "他答应的事还没做"} {
		s.Keep(text, now, 8)
	}

	if _, err := s.LetGo(0, ""); err == nil {
		t.Error("既无序号也无片段应报错")
	}
	if _, err := s.LetGo(4, ""); err == nil || !strings.Contains(err.Error(), "共 3 条") {
		t.Errorf("越界序号应报错并列清单: %v", err)
	}
	// 片段命中多条：报错并带原序号
	if _, err := s.LetGo(0, "他"); err == nil || !strings.Contains(err.Error(), "3. 他答应的事还没做") {
		t.Errorf("多条命中应列出带原序号的候选: %v", err)
	}
	if _, err := s.LetGo(0, "没有的话"); err == nil {
		t.Error("无命中应报错")
	}
	// 片段唯一命中（大小写不敏感）
	got, err := s.LetGo(0, "看展")
	if err != nil || got.Text != "想约他周末去看展，还没开口" {
		t.Fatalf("按片段放下: %+v, %v", got, err)
	}
	// 按序号
	got, err = s.LetGo(2, "")
	if err != nil || got.Text != "他答应的事还没做" {
		t.Fatalf("按序号放下: %+v, %v", got, err)
	}
	es, _ := s.List()
	if len(es) != 1 || es[0].Text != "他忘了纪念日，说不在意其实还在意" {
		t.Errorf("剩余不对: %+v", es)
	}
	// 放完最后一条：文件删掉，视为空
	if _, err := s.LetGo(1, ""); err != nil {
		t.Fatal(err)
	}
	if es, _ := s.List(); len(es) != 0 {
		t.Errorf("应为空: %+v", es)
	}
}

func TestRenderExactAndDegrade(t *testing.T) {
	now := time.Date(2026, 8, 21, 10, 0, 0, 0, time.Local)
	es := []Entry{
		{Text: "他忘了纪念日，说不在意其实还在意", Created: now.AddDate(0, 0, -3)},
		{Text: "想约他周末去看展，还没开口", Created: now.Add(-time.Hour)},
	}
	want := "- 3 天前：他忘了纪念日，说不在意其实还在意\n- 今天：想约他周末去看展，还没开口"
	if got := renderEntries("", es, 0, now); got != want {
		t.Errorf("全量渲染不对:\n got: %q\nwant: %q", got, want)
	}
	// 预算不够：只留最近的，并注明另有几条
	degraded := renderEntries("", es, len(want)-1, now)
	if degraded != "- 今天：想约他周末去看展，还没开口\n（另有 1 条更早的未列出）" {
		t.Errorf("降级不对: %q", degraded)
	}
	// 一条都放不下：只报条数
	if got := renderEntries("", es, 10, now); got != "（共 2 条，注入预算不足以列出）" {
		t.Errorf("最低一级降级不对: %q", got)
	}
	// 没有时刻（手改的文件）不标时间
	if got := renderEntries("", []Entry{{Text: "没时间戳"}}, 0, now); got != "- 没时间戳" {
		t.Errorf("无时刻: %q", got)
	}
}

func TestAgoText(t *testing.T) {
	now := time.Date(2026, 8, 21, 1, 0, 0, 0, time.Local) // 凌晨一点
	cases := map[string]time.Time{
		"":      {},
		"今天":    now.Add(-30 * time.Minute),
		"昨天":    now.Add(-2 * time.Hour), // 昨晚 23:00，按日历日是昨天
		"3 天前":  now.AddDate(0, 0, -3),
		"2 个月前": now.AddDate(0, 0, -65),
		"1 年前":  now.AddDate(0, 0, -400),
	}
	for want, at := range cases {
		if got := agoText("", at, now); got != want {
			t.Errorf("agoText(%v) = %q, want %q", at, got, want)
		}
	}
	if got := agoText("", now.Add(24*time.Hour), now); got != "" {
		t.Errorf("时钟倒流不该标: %q", got)
	}
}

func TestToolsRoundTrip(t *testing.T) {
	p := newTestPlugin(t, map[string]any{"max_entries": 2})
	ctx := scopedCtx("")

	if tp, _ := p.TurnPrompt(ctx, plugin.TurnEvent{}); tp != "" {
		t.Errorf("空清单不该注入: %q", tp)
	}
	out := ok(t, p, ctx, "keep_unspoken", `{"text":"他忘了纪念日，说不在意其实还在意"}`)
	if out != "已记下（第 1 条）。" {
		t.Errorf("回执: %q", out)
	}
	out = ok(t, p, ctx, "keep_unspoken", `{"text":"他忘了纪念日，说不在意其实还在意"}`)
	if out != "已经记着这一条了（第 1 条）。" {
		t.Errorf("重复回执: %q", out)
	}
	ok(t, p, ctx, "keep_unspoken", `{"text":"想约他周末去看展，还没开口"}`)
	out = ok(t, p, ctx, "keep_unspoken", `{"text":"他答应的事还没做"}`)
	if out != "已记下（第 2 条）。清单已满（上限 2 条），放下了最早的：「他忘了纪念日，说不在意其实还在意」。" {
		t.Errorf("淘汰回执: %q", out)
	}

	tp, err := p.TurnPrompt(ctx, plugin.TurnEvent{})
	if err != nil || tp != "[心里话]\n- 今天：想约他周末去看展，还没开口\n- 今天：他答应的事还没做" {
		t.Errorf("注入不对: %q, %v", tp, err)
	}

	if _, err := call(t, p, ctx, "keep_unspoken", fmt.Sprintf(`{"text":%q}`, strings.Repeat("长", maxTextRunes+1))); err == nil {
		t.Error("超长应报错")
	}
	out = ok(t, p, ctx, "let_go", `{"text":"看展"}`)
	if out != "已放下：「想约他周末去看展，还没开口」。" {
		t.Errorf("放下回执: %q", out)
	}
	out = ok(t, p, ctx, "let_go", `{"index":1}`)
	if out != "已放下：「他答应的事还没做」。" {
		t.Errorf("按序号放下回执: %q", out)
	}
	if tp, _ := p.TurnPrompt(ctx, plugin.TurnEvent{}); tp != "" {
		t.Errorf("放完后不该注入: %q", tp)
	}
	if _, err := call(t, p, ctx, "let_go", `{}`); err == nil {
		t.Error("空清单上放下应报错")
	}
}

func TestTurnPromptDegradesByConfig(t *testing.T) {
	p := newTestPlugin(t, map[string]any{"max_inject_bytes": 256})
	ctx := scopedCtx("")
	for i := 0; i < 4; i++ {
		ok(t, p, ctx, "keep_unspoken", fmt.Sprintf(`{"text":"第 %d 件事，%s"}`, i+1, strings.Repeat("字", 40)))
	}
	tp, _ := p.TurnPrompt(ctx, plugin.TurnEvent{})
	if len(tp)-len(stateHeaderZH)-1 > 256 {
		t.Errorf("正文超出预算: %d 字节", len(tp))
	}
	if !strings.Contains(tp, "第 4 件事") || strings.Contains(tp, "第 1 件事") || !strings.Contains(tp, "条更早的未列出") {
		t.Errorf("应只留最近的并注明另有几条: %q", tp)
	}
}

func TestDomainsIsolated(t *testing.T) {
	p := newTestPlugin(t, nil)
	ok(t, p, scopedCtx("outer"), "keep_unspoken", `{"text":"表面上的事"}`)
	ok(t, p, scopedCtx("inner"), "keep_unspoken", `{"text":"里面的事"}`)

	outer, _ := p.TurnPrompt(scopedCtx("outer"), plugin.TurnEvent{})
	inner, _ := p.TurnPrompt(scopedCtx("inner"), plugin.TurnEvent{})
	if !strings.Contains(outer, "表面上的事") || strings.Contains(outer, "里面的事") {
		t.Errorf("outer = %q", outer)
	}
	if !strings.Contains(inner, "里面的事") || strings.Contains(inner, "表面上的事") {
		t.Errorf("inner = %q", inner)
	}
	if shared, _ := p.TurnPrompt(scopedCtx(""), plugin.TurnEvent{}); shared != "" {
		t.Errorf("共享域不该看到任何人格的心里话: %q", shared)
	}
	// 放下也只在本域里找
	if _, err := call(t, p, scopedCtx("outer"), "let_go", `{"text":"里面"}`); err == nil {
		t.Error("outer 不该能放下 inner 的条目")
	}
}

func TestActionClearsAllDomains(t *testing.T) {
	p := newTestPlugin(t, nil)
	ok(t, p, scopedCtx(""), "keep_unspoken", `{"text":"一件事"}`)
	ok(t, p, scopedCtx("inner"), "keep_unspoken", `{"text":"另一件事"}`)

	if err := p.StartAction(context.Background(), actionClear); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"", "inner"} {
		if got, _ := p.TurnPrompt(scopedCtx(tag), plugin.TurnEvent{}); got != "" {
			t.Errorf("域 %q 未被清空：%s", tag, got)
		}
	}
	st, err := p.ActionState(actionClear)
	if err != nil || st.Status != plugin.ActionDone || !strings.Contains(st.Message, "2 份") {
		t.Errorf("state=%+v err=%v", st, err)
	}
	if _, err := p.ActionState("nope"); err == nil {
		t.Error("未知操作应报错")
	}
}

func TestSystemPromptStableAndRequires(t *testing.T) {
	p := New()
	if p.SystemPrompt() != p.SystemPrompt() || p.SystemPrompt() == "" {
		t.Error("SystemPrompt 应非空且逐字节稳定")
	}
	// 有了记录之后也不变：会变的内容一律走 TurnPrompt
	q := newTestPlugin(t, nil)
	before := q.SystemPrompt()
	ok(t, q, scopedCtx(""), "keep_unspoken", `{"text":"一件事"}`)
	if q.SystemPrompt() != before {
		t.Error("SystemPrompt 不该随状态变化")
	}
	if r := p.Requires(); len(r) != 1 || r[0] != "roleplay" {
		t.Errorf("Requires = %v", r)
	}
	if p.Category() != plugin.CategoryPersona {
		t.Errorf("Category = %q", p.Category())
	}
}

func TestPromptsCarryNoIdentity(t *testing.T) {
	p := newTestPlugin(t, nil)
	all := p.SystemPrompt() + p.Description()
	for _, tool := range p.Tools() {
		all += tool.Description() + string(tool.Schema())
	}
	for _, bad := range []string{"Claude", "GPT", "Anthropic", "OpenAI", "助手"} {
		if strings.Contains(all, bad) {
			t.Errorf("提示词不应包含身份信息 %q", bad)
		}
	}
}
