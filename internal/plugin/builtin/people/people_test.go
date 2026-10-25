package people

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

func str(s string) *string { return &s }

func testLimits() Limits { return Limits{MaxPeople: 3} }

func TestStoreUpsertCreateAndUpdate(t *testing.T) {
	s := NewStore("", t.TempDir())
	now := time.Now()

	// 新建必须有关系
	if _, err := s.Upsert(Update{Name: "林舟"}, now, testLimits()); err == nil || !strings.Contains(err.Error(), "relation") {
		t.Fatalf("只有名字的新人物应被拒并点名 relation: %v", err)
	}
	res, err := s.Upsert(Update{Name: " 林舟 ", Relation: str("大学同学")}, now, testLimits())
	if err != nil || !res.Created || res.Name != "林舟" {
		t.Fatalf("登记失败: %+v, %v", res, err)
	}
	ps, _ := s.List()
	if len(ps) != 1 || ps[0].Closeness != defaultCloseness || !ps[0].LastMet.IsZero() {
		t.Fatalf("新人物应默认「认识」且没有来往记录: %+v", ps)
	}

	// 更新只动传了的字段，变化进回执
	later := now.Add(time.Hour)
	res, err = s.Upsert(Update{Name: "林舟", Closeness: str("熟"), MetNow: true, LastNote: str("一起吃了晚饭")}, later, testLimits())
	if err != nil || res.Created {
		t.Fatalf("更新失败: %+v, %v", res, err)
	}
	if len(res.Changes) != 2 || res.Changes[0] != "亲近度 认识→熟" || res.Changes[1] != "上次互动记为此刻" {
		t.Errorf("变化描述不对: %v", res.Changes)
	}
	ps, _ = s.List()
	if ps[0].Relation != "大学同学" || ps[0].Closeness != "熟" || !ps[0].LastMet.Equal(later) || ps[0].LastNote != "一起吃了晚饭" {
		t.Errorf("更新后的记录不对: %+v", ps[0])
	}

	// 什么都没变：回执为空，不报错
	res, err = s.Upsert(Update{Name: "林舟", Closeness: str("熟")}, later, testLimits())
	if err != nil || len(res.Changes) != 0 {
		t.Errorf("无变化的更新应返回空变化: %+v, %v", res, err)
	}

	// 大小写不敏感
	if _, err := s.Upsert(Update{Name: "alice", Relation: str("同事")}, now, testLimits()); err != nil {
		t.Fatal(err)
	}
	res, _ = s.Upsert(Update{Name: "Alice", Profile: str("爱喝咖啡")}, later, testLimits())
	if res.Created || res.Name != "alice" {
		t.Errorf("同名（不分大小写）应视为同一人: %+v", res)
	}
}

func TestStoreErrors(t *testing.T) {
	s := NewStore("", t.TempDir())
	now := time.Now()
	cases := map[string]Update{
		"空名字":   {Name: "  ", Relation: str("x")},
		"名字过长":  {Name: strings.Repeat("名", maxNameRunes+1), Relation: str("x")},
		"对方本人":  {Name: "对方", Relation: str("x")},
		"亲近度非法": {Name: "甲", Relation: str("x"), Closeness: str("很熟")},
		"设定过长":  {Name: "乙", Relation: str("x"), Profile: str(strings.Repeat("字", maxProfileRunes+1))},
	}
	for name, u := range cases {
		if _, err := s.Upsert(u, now, testLimits()); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
	// 人数上限：拒绝并把规则告诉模型
	for _, n := range []string{"一", "二", "三"} {
		if _, err := s.Upsert(Update{Name: n, Relation: str("x")}, now, testLimits()); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.Upsert(Update{Name: "四", Relation: str("x")}, now, testLimits())
	if err == nil || !strings.Contains(err.Error(), "上限") || !strings.Contains(err.Error(), "remove_person") {
		t.Errorf("超出人数上限应拒绝并说明怎么办: %v", err)
	}
	// 已有的人仍可更新
	if _, err := s.Upsert(Update{Name: "一", Closeness: str("熟")}, now, testLimits()); err != nil {
		t.Errorf("上限不该挡住更新: %v", err)
	}
}

func TestStoreRemove(t *testing.T) {
	s := NewStore("", t.TempDir())
	s.Upsert(Update{Name: "林舟", Relation: str("同学")}, time.Now(), testLimits())
	if _, err := s.Remove("不存在"); err == nil || !strings.Contains(err.Error(), "林舟") {
		t.Errorf("移除不存在的人应报错并列候选: %v", err)
	}
	if got, err := s.Remove("林舟"); err != nil || got != "林舟" {
		t.Fatalf("Remove: %q, %v", got, err)
	}
	if ps, _ := s.List(); len(ps) != 0 {
		t.Errorf("移除后应为空: %+v", ps)
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
		"倒流不标":  now.Add(24 * time.Hour),
	}
	for want, at := range cases {
		got := agoText("", at, now)
		if want == "倒流不标" {
			want = ""
		}
		if got != want {
			t.Errorf("agoText(%v) = %q, want %q", at, got, want)
		}
	}
}

func TestRenderDegrade(t *testing.T) {
	now := time.Now()
	ps := []Person{
		{Name: "林舟", Relation: "大学同学", Closeness: "熟", LastMet: now.AddDate(0, 0, -3), LastNote: "一起吃了晚饭"},
		{Name: "母亲", Relation: "家人", Closeness: "亲近"},
	}
	full := renderPeople("", ps, 0, now)
	if !strings.Contains(full, "林舟（大学同学，熟）上次来往 3 天前：一起吃了晚饭") || !strings.HasSuffix(full, "母亲（家人，亲近）") {
		t.Errorf("全量渲染不对:\n%s", full)
	}
	compact := renderPeople("", ps, len(full)-1, now)
	if compact != "林舟（熟）\n母亲（亲近）" {
		t.Errorf("第一级降级应只留名字与亲近度: %q", compact)
	}
	count := renderPeople("", ps, 10, now)
	if !strings.Contains(count, "共 2 人") || !strings.Contains(count, "list_people") {
		t.Errorf("第二级降级应只报人数: %q", count)
	}
}

func TestInitRequiresStateDir(t *testing.T) {
	if err := New().Init(plugin.InitContext{}, nil); err == nil {
		t.Fatal("没有持久化目录应拒绝启用")
	}
}

func newTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	p := New()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{"max_people": 5}); err != nil {
		t.Fatal(err)
	}
	return p
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

func TestToolsRoundTrip(t *testing.T) {
	p := newTestPlugin(t)
	ctx := context.Background()

	if tp, _ := p.TurnPrompt(ctx, plugin.TurnEvent{}); tp != "" {
		t.Errorf("空库不该注入: %q", tp)
	}
	out, err := call(t, p, ctx, "upsert_person", `{"name":"林舟","relation":"大学同学","profile":"话少，爱拍照","closeness":"熟"}`)
	if err != nil || !strings.Contains(out, "已登记 林舟（熟，大学同学）") {
		t.Fatalf("登记: %q, %v", out, err)
	}
	out, err = call(t, p, ctx, "upsert_person", `{"name":"林舟","met_now":true,"last_note":"一起看了展"}`)
	if err != nil || !strings.Contains(out, "上次互动记为此刻") {
		t.Fatalf("更新: %q, %v", out, err)
	}
	if _, err := call(t, p, ctx, "upsert_person", `{"name":"对方","relation":"恋人"}`); err == nil {
		t.Error("对方本人应被拒绝登记")
	}

	tp, err := p.TurnPrompt(ctx, plugin.TurnEvent{})
	if err != nil || !strings.HasPrefix(tp, stateHeaderFor("")) || !strings.Contains(tp, "林舟（大学同学，熟）上次来往 今天：一起看了展") {
		t.Errorf("注入块不对: %q, %v", tp, err)
	}
	out, _ = call(t, p, ctx, "list_people", `{}`)
	if !strings.Contains(out, "共 1 人") || !strings.Contains(out, "话少，爱拍照") {
		t.Errorf("list_people 应含设定: %q", out)
	}
	out, _ = call(t, p, ctx, "list_people", `{"keyword":"没有的词"}`)
	if !strings.Contains(out, "没有符合条件的") {
		t.Errorf("关键词无命中: %q", out)
	}
	if _, err := call(t, p, ctx, "remove_person", `{"name":"林舟"}`); err == nil {
		t.Error("移除缺 reason 应报错")
	}
	out, err = call(t, p, ctx, "remove_person", `{"name":"林舟","reason":"搬去外地了"}`)
	if err != nil || !strings.Contains(out, "已移除 林舟") {
		t.Fatalf("移除: %q, %v", out, err)
	}
	if tp, _ := p.TurnPrompt(ctx, plugin.TurnEvent{}); tp != "" {
		t.Errorf("移除后不该再注入: %q", tp)
	}
}

func TestScopeSeparation(t *testing.T) {
	p := newTestPlugin(t)
	inner := plugin.WithScope(context.Background(), plugin.Scope{Write: "inner", Read: []string{}})
	shared := plugin.WithScope(context.Background(), plugin.Scope{Read: []string{}})
	all := context.Background() // Read 为 nil：不限制

	if _, err := call(t, p, inner, "upsert_person", `{"name":"小满","relation":"里人格的朋友"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, p, shared, "upsert_person", `{"name":"母亲","relation":"家人"}`); err != nil {
		t.Fatal(err)
	}

	// 共享域看不到 inner 的人，inner 看得到共享的人
	tp, _ := p.TurnPrompt(shared, plugin.TurnEvent{})
	if strings.Contains(tp, "小满") || !strings.Contains(tp, "母亲") {
		t.Errorf("共享域注入泄漏了 inner: %q", tp)
	}
	tp, _ = p.TurnPrompt(inner, plugin.TurnEvent{})
	if !strings.Contains(tp, "小满") || !strings.Contains(tp, "母亲") {
		t.Errorf("inner 应看到两个域: %q", tp)
	}
	// 计数也按可读域
	out, _ := call(t, p, shared, "list_people", `{}`)
	if !strings.Contains(out, "共 1 人") {
		t.Errorf("共享域的计数不该含 inner: %q", out)
	}

	// 从不限制的域更新 inner 的人，应写回 inner 而不是在共享域再建一份
	if _, err := call(t, p, all, "upsert_person", `{"name":"小满","closeness":"亲近"}`); err != nil {
		t.Fatal(err)
	}
	if ps, _ := p.storeFor("").List(); len(ps) != 1 {
		t.Errorf("共享库不该多出 inner 的人: %+v", ps)
	}
	if ps, _ := p.storeFor("inner").List(); len(ps) != 1 || ps[0].Closeness != "亲近" {
		t.Errorf("更新应落在 inner 库: %+v", ps)
	}
}

func TestLookup(t *testing.T) {
	p := newTestPlugin(t)
	ctx := context.Background()
	call(t, p, ctx, "upsert_person", `{"name":"Lin Zhou","relation":"同学"}`)
	call(t, p, ctx, "upsert_person", `{"name":"母亲","relation":"家人"}`)

	var lk Lookup = p
	if got, ok := lk.Known(ctx, "lin zhou"); !ok || got != "Lin Zhou" {
		t.Errorf("Known 应不分大小写并返回规范写法: %q, %v", got, ok)
	}
	if _, ok := lk.Known(ctx, "小周"); ok {
		t.Error("不认识的人应返回 false")
	}
	if ns := lk.Names(ctx); len(ns) != 2 || ns[0] != "Lin Zhou" || ns[1] != "母亲" {
		t.Errorf("Names 应按记录顺序: %v", ns)
	}
	// 可见域生效
	shared := plugin.WithScope(ctx, plugin.Scope{Read: []string{}})
	call(t, p, plugin.WithScope(ctx, plugin.Scope{Write: "inner", Read: []string{}}), "upsert_person", `{"name":"小满","relation":"朋友"}`)
	if _, ok := lk.Known(shared, "小满"); ok {
		t.Error("共享域不该认识 inner 的人")
	}
}

func TestSystemPromptStableAndRequires(t *testing.T) {
	p := New()
	if p.SystemPrompt() != p.SystemPrompt() || p.SystemPrompt() == "" {
		t.Error("SystemPrompt 应非空且逐字节稳定")
	}
	if r := p.Requires(); len(r) != 1 || r[0] != "roleplay" {
		t.Errorf("Requires = %v", r)
	}
}

// 近况：带上听说的时刻注入，过了有效期不注入；空串清除。
func TestLately(t *testing.T) {
	s := NewStore("", t.TempDir())
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.Local)
	if _, err := s.Upsert(Update{Name: "小林", Relation: str("同事"), Lately: str("在闹离职")}, now, testLimits()); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.List()
	if got := renderPerson("", ps[0], now.AddDate(0, 0, 1), true); !strings.Contains(got, "近况（昨天听说）：在闹离职") {
		t.Errorf("近况应带上听说的时刻：%q", got)
	}
	if got := renderPerson("", ps[0], now.Add(latelyTTL+24*time.Hour), true); strings.Contains(got, "近况") {
		t.Errorf("过期的近况不该注入：%q", got)
	}
	res, err := s.Upsert(Update{Name: "小林", Lately: str("")}, now, testLimits())
	if err != nil || len(res.Changes) != 1 {
		t.Fatalf("清除近况：%+v %v", res, err)
	}
	if ps, _ := s.List(); ps[0].Lately != "" || !ps[0].LatelyAt.IsZero() {
		t.Errorf("清除后：%+v", ps[0])
	}
	if _, err := s.Upsert(Update{Name: "小林", Lately: str(strings.Repeat("长", maxLatelyRunes+1))}, now, testLimits()); err == nil {
		t.Error("超长应报错")
	}
}
