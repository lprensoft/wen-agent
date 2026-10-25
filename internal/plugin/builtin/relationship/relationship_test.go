package relationship

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"wen/internal/cue"
	"wen/internal/plugin"
)

func str(s string) *string { return &s }

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

func TestApplyRejectsOverLimit(t *testing.T) {
	s := NewStore("", t.TempDir())
	now := time.Now()
	cases := map[string]Update{
		"stage 过长":      {Stage: str(strings.Repeat("热", maxStageRunes+1))},
		"my_call 过长":    {MyCall: str(strings.Repeat("宁", maxCallRunes+1))},
		"their_call 过长": {TheirCall: str(strings.Repeat("舟", maxCallRunes+1))},
		"recent 过长":     {Recent: str(strings.Repeat("吵", maxRecentRunes+1))},
		"their_now 过长":  {TheirNow: str(strings.Repeat("忙", maxTheirNowRunes+1))},
		"bonds 过多":      {Bonds: &[]string{"一", "二", "三", "四", "五", "六"}},
		"单条 bond 过长":    {Bonds: &[]string{strings.Repeat("别", maxBondRunes+1)}},
	}
	for name, u := range cases {
		if _, _, err := s.Apply(u, now); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
	// 整批校验不通过就什么都不写
	if _, ok, _ := s.Load(); ok {
		t.Error("校验失败时不该落盘")
	}
	// 刚好到上限可以
	if _, _, err := s.Apply(Update{Stage: str(strings.Repeat("热", maxStageRunes)), Bonds: &[]string{"一", "二", "三", "四", "五"}}, now); err != nil {
		t.Errorf("恰好到上限不该报错: %v", err)
	}
}

func TestApplyClearAndBondsOverride(t *testing.T) {
	s := NewStore("", t.TempDir())
	now := time.Now()

	updated, cleared, err := s.Apply(Update{
		Stage: str("热恋"), MyCall: str(" 阿宁 "), Bonds: &[]string{"她不爱被催", " ", "晚安一定要说"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(updated, ",") != "阶段,你对对方的称呼,默契与禁区" || len(cleared) != 0 {
		t.Fatalf("updated=%v cleared=%v", updated, cleared)
	}
	snap, _, _ := s.Load()
	if snap.MyCall != "阿宁" || strings.Join(snap.Bonds, "|") != "她不爱被催|晚安一定要说" {
		t.Fatalf("snap = %+v", snap)
	}

	// bonds 整体覆盖而不是追加
	if _, _, err := s.Apply(Update{Bonds: &[]string{"别提前任"}}, now); err != nil {
		t.Fatal(err)
	}
	snap, _, _ = s.Load()
	if strings.Join(snap.Bonds, "|") != "别提前任" {
		t.Errorf("bonds 应整体覆盖: %v", snap.Bonds)
	}

	// 相同内容再传一遍：没有变化，不算更新
	updated, cleared, _ = s.Apply(Update{Bonds: &[]string{"别提前任"}, Stage: str("热恋")}, now)
	if len(updated) != 0 || len(cleared) != 0 {
		t.Errorf("相同内容不该算变化: updated=%v cleared=%v", updated, cleared)
	}

	// 空串清除字段、空数组清空 bonds；没提到的字段保持原样
	updated, cleared, err = s.Apply(Update{MyCall: str(""), Bonds: &[]string{}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != 0 || strings.Join(cleared, ",") != "你对对方的称呼,默契与禁区" {
		t.Fatalf("updated=%v cleared=%v", updated, cleared)
	}
	snap, ok, _ := s.Load()
	if !ok || snap.Stage != "热恋" || snap.MyCall != "" || len(snap.Bonds) != 0 {
		t.Fatalf("snap = %+v", snap)
	}

	// 清掉阶段：第一次成为「热恋」的那个日子是发生过的事，留着
	if _, _, err := s.Apply(Update{Stage: str(" ")}, now); err != nil {
		t.Fatal(err)
	}
	snap, ok, _ = s.Load()
	if !ok || snap.Stage != "" || len(snap.Dates) != 1 || snap.Dates[0].What != "热恋" || !snap.Dates[0].Auto {
		t.Fatalf("清掉阶段后日子应留着：%+v", snap)
	}

	// 全部清掉：视为无记录
	if _, _, err := s.Apply(Update{RemoveDate: str("热恋")}, now); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Load(); ok {
		t.Error("全部清除后应视为无记录")
	}
}

func TestApplyRecentAlwaysStampsNow(t *testing.T) {
	s := NewStore("", t.TempDir())
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.Local)
	if _, _, err := s.Apply(Update{Recent: str("吵了一架")}, t0); err != nil {
		t.Fatal(err)
	}
	// 同样的话再传一次：是「又发生了」，时刻要更新
	t1 := t0.AddDate(0, 0, 5)
	updated, _, _ := s.Apply(Update{Recent: str("吵了一架")}, t1)
	if strings.Join(updated, ",") != "最近变动" {
		t.Errorf("updated = %v", updated)
	}
	snap, _, _ := s.Load()
	if !snap.RecentAt.Equal(t1) {
		t.Errorf("RecentAt = %v, want %v", snap.RecentAt, t1)
	}
	// 清除 recent 时时刻一并清掉
	s.Apply(Update{Recent: str("")}, t1)
	snap, _, _ = s.Load()
	if snap.Recent != "" || !snap.RecentAt.IsZero() {
		t.Errorf("清除后应无残留: %+v", snap)
	}
}

func TestRenderExact(t *testing.T) {
	now := time.Date(2026, 8, 21, 10, 0, 0, 0, time.Local)
	snap := Snapshot{
		Stage:     "热恋",
		MyCall:    "阿宁",
		TheirCall: "小舟",
		Recent:    "为对方忘了纪念日吵过一架，昨天和好了",
		RecentAt:  now.AddDate(0, 0, -3),
		Bonds:     []string{"对方不爱被催", "晚安一定要说", "别提前任"},
		TheirNow:  "项目上线前在赶工",
	}
	want := "阶段：热恋。你叫对方「阿宁」，对方叫你「小舟」。\n" +
		"最近：3 天前为对方忘了纪念日吵过一架，昨天和好了。\n" +
		"默契与禁区：对方不爱被催；晚安一定要说；别提前任。\n" +
		"对方近况：项目上线前在赶工。"
	if got := renderSnapshot("", snap, now); got != want {
		t.Errorf("渲染不对:\n got: %q\nwant: %q", got, want)
	}

	// 缺项省句；今天的变动标「今天」；已有句号不重复加
	partial := Snapshot{TheirCall: "小舟", Recent: "表白了！", RecentAt: now.Add(-time.Hour)}
	want = "对方叫你「小舟」。\n最近：今天表白了！"
	if got := renderSnapshot("", partial, now); got != want {
		t.Errorf("部分渲染不对:\n got: %q\nwant: %q", got, want)
	}
	if got := renderSnapshot("", Snapshot{MyCall: "阿宁"}, now); got != "你叫对方「阿宁」。" {
		t.Errorf("只有我的称呼: %q", got)
	}
}

func TestRecentExpires(t *testing.T) {
	now := time.Now()
	fresh := Snapshot{Recent: "和好了", RecentAt: now.AddDate(0, 0, -29)}
	if got := renderSnapshot("", fresh, now); got != "最近：29 天前和好了。" {
		t.Errorf("29 天前应仍注入: %q", got)
	}
	stale := Snapshot{Stage: "老夫老妻", Recent: "和好了", RecentAt: now.AddDate(0, 0, -31)}
	if got := renderSnapshot("", stale, now); got != "阶段：老夫老妻。" {
		t.Errorf("31 天前的「最近」不该注入: %q", got)
	}
	// 手改的文件没有时刻：照样注入但不标时间
	noStamp := Snapshot{Recent: "和好了"}
	if got := renderSnapshot("", noStamp, now); got != "最近：和好了。" {
		t.Errorf("无时刻: %q", got)
	}
}

func TestTurnPromptSkipsWhenOnlyExpiredRecent(t *testing.T) {
	p := newTestPlugin(t)
	ctx := scopedCtx("")
	store := p.storeFor("")
	if _, _, err := store.Apply(Update{Recent: str("吵过一架")}, time.Now().AddDate(0, 0, -40)); err != nil {
		t.Fatal(err)
	}
	got, err := p.TurnPrompt(ctx, plugin.TurnEvent{})
	if err != nil || got != "" {
		t.Fatalf("只剩过期的「最近」不该注入: got=%q err=%v", got, err)
	}
	// 空库同样不注入
	if got, _ := newTestPlugin(t).TurnPrompt(ctx, plugin.TurnEvent{}); got != "" {
		t.Errorf("空库不该注入: %q", got)
	}
}

func TestToolRoundTrip(t *testing.T) {
	p := newTestPlugin(t)
	ctx := scopedCtx("")

	out := exec(t, p, ctx, `{"stage":"暧昧中","my_call":"阿宁","bonds":["晚安一定要说"],"recent":"第一次牵了手"}`)
	if out != "已更新：阶段、你对对方的称呼、最近变动、默契与禁区。" {
		t.Errorf("回执不对: %q", out)
	}
	got, err := p.TurnPrompt(ctx, plugin.TurnEvent{})
	if err != nil || got != "[关系]\n阶段：暧昧中。你叫对方「阿宁」。\n最近：今天第一次牵了手。\n默契与禁区：晚安一定要说。" {
		t.Errorf("注入不对: %q, %v", got, err)
	}

	// 只清一项，其余不动
	out = exec(t, p, ctx, `{"my_call":""}`)
	if out != "已清除：你对对方的称呼。" {
		t.Errorf("回执不对: %q", out)
	}
	got, _ = p.TurnPrompt(ctx, plugin.TurnEvent{})
	if !strings.Contains(got, "阶段：暧昧中。\n") || strings.Contains(got, "阿宁") {
		t.Errorf("没传的字段被动了或清除未生效: %q", got)
	}

	if out := exec(t, p, ctx, `{"stage":"暧昧中"}`); out != "没有实际变化。" {
		t.Errorf("无变化回执: %q", out)
	}
	if _, err := p.Tools()[0].Execute(ctx, json.RawMessage(`{}`)); err == nil {
		t.Error("空参数应报错")
	}
	if _, err := p.Tools()[0].Execute(ctx, json.RawMessage(`{"stage":"这个阶段的名字实在太长了"}`)); err == nil || !strings.Contains(err.Error(), "上限") {
		t.Errorf("超长应报错并说明上限: %v", err)
	}
}

func TestDomainsIsolated(t *testing.T) {
	p := newTestPlugin(t)
	exec(t, p, scopedCtx("outer"), `{"stage":"老友"}`)
	exec(t, p, scopedCtx("inner"), `{"stage":"热恋"}`)

	outer, _ := p.TurnPrompt(scopedCtx("outer"), plugin.TurnEvent{})
	inner, _ := p.TurnPrompt(scopedCtx("inner"), plugin.TurnEvent{})
	if !strings.Contains(outer, "老友") || strings.Contains(outer, "热恋") {
		t.Errorf("outer = %q", outer)
	}
	if !strings.Contains(inner, "热恋") || strings.Contains(inner, "老友") {
		t.Errorf("inner = %q", inner)
	}
	if shared, _ := p.TurnPrompt(scopedCtx(""), plugin.TurnEvent{}); shared != "" {
		t.Errorf("共享域不该看到任何人格的关系: %q", shared)
	}
}

func TestActionResetsAllDomains(t *testing.T) {
	p := newTestPlugin(t)
	exec(t, p, scopedCtx(""), `{"stage":"刚认识"}`)
	exec(t, p, scopedCtx("inner"), `{"stage":"热恋"}`)

	if err := p.StartAction(context.Background(), actionReset); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"", "inner"} {
		if got, _ := p.TurnPrompt(scopedCtx(tag), plugin.TurnEvent{}); got != "" {
			t.Errorf("域 %q 未被清空：%s", tag, got)
		}
	}
	st, err := p.ActionState(actionReset)
	if err != nil || st.Status != plugin.ActionDone || !strings.Contains(st.Message, "2 份") {
		t.Errorf("state=%+v err=%v", st, err)
	}
	if _, err := p.ActionState("nope"); err == nil {
		t.Error("未知操作应报错")
	}
}

func TestAgoText(t *testing.T) {
	now := time.Date(2026, 8, 21, 1, 0, 0, 0, time.Local) // 凌晨一点
	cases := map[string]time.Time{
		"":     {},
		"今天":   now.Add(-30 * time.Minute),
		"昨天":   now.Add(-2 * time.Hour), // 昨晚 23:00，按日历日是昨天
		"3 天前": now.AddDate(0, 0, -3),
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

func TestSystemPromptStableAndRequires(t *testing.T) {
	p := New()
	if p.SystemPrompt() != p.SystemPrompt() || p.SystemPrompt() == "" {
		t.Error("SystemPrompt 应非空且逐字节稳定")
	}
	// 有了记录之后也不变：会变的内容一律走 TurnPrompt
	q := newTestPlugin(t)
	before := q.SystemPrompt()
	exec(t, q, scopedCtx(""), `{"stage":"热恋"}`)
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
	p := newTestPlugin(t)
	all := p.SystemPrompt() + p.Description() + p.Tools()[0].Description() + string(p.Tools()[0].Schema())
	for _, bad := range []string{"Claude", "GPT", "Anthropic", "OpenAI", "助手"} {
		if strings.Contains(all, bad) {
			t.Errorf("提示词不应包含身份信息 %q", bad)
		}
	}
}

// ---------- 日子 ----------

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 10, 0, 0, 0, time.Local) }

// 关系第一次变成某个阶段时自动记下日子；热恋—冷战—热恋不会把最初那个日子覆盖掉。
func TestStageAutoDateFirstTimeOnly(t *testing.T) {
	s := NewStore("", t.TempDir())
	_, _, _ = s.Apply(Update{Stage: str("热恋")}, day(2026, 1, 10))
	_, _, _ = s.Apply(Update{Stage: str("冷战中")}, day(2026, 3, 1))
	_, _, _ = s.Apply(Update{Stage: str("热恋")}, day(2026, 3, 5))
	snap, _, _ := s.Load()
	got := map[string]string{}
	for _, d := range snap.Dates {
		if !d.Auto {
			t.Errorf("阶段变化记下的日子应标为自动：%+v", d)
		}
		got[d.What] = d.Date
	}
	if got["热恋"] != "2026-01-10" || got["冷战中"] != "2026-03-01" || len(got) != 2 {
		t.Errorf("只记第一次：%v", got)
	}
}

func TestAddAndRemoveDates(t *testing.T) {
	s := NewStore("", t.TempDir())
	now := day(2026, 10, 20)
	if _, _, err := s.Apply(Update{AddDate: &DateArg{What: "对方生日", Date: "05-12"}}, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Apply(Update{AddDate: &DateArg{What: "对方生日", Date: "05-13"}}, now); err != nil {
		t.Fatal(err)
	}
	snap, _, _ := s.Load()
	if len(snap.Dates) != 1 || snap.Dates[0].Date != "05-13" {
		t.Errorf("同名的改日子：%+v", snap.Dates)
	}
	for _, bad := range []DateArg{{What: "x", Date: "2026/05/12"}, {What: "x", Date: "5-12"}, {What: "", Date: "05-12"},
		{What: strings.Repeat("长", maxDateWhatRunes+1), Date: "05-12"}} {
		if _, _, err := s.Apply(Update{AddDate: &bad}, now); err == nil {
			t.Errorf("应拒绝：%+v", bad)
		}
	}
	if _, _, err := s.Apply(Update{RemoveDate: str("不存在")}, now); err == nil || !strings.Contains(err.Error(), "对方生日") {
		t.Errorf("去掉不存在的日子应报错并列出现有的：%v", err)
	}
	if _, _, err := s.Apply(Update{RemoveDate: str("对方生日")}, now); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Load(); ok {
		t.Error("去掉唯一的日子后应视为无记录")
	}
}

// 满了先挤掉最早那个自动记下的；全是模型记的就拒绝，要它先去掉一个。
func TestDatesCap(t *testing.T) {
	s := NewStore("", t.TempDir())
	now := day(2026, 10, 20)
	_, _, _ = s.Apply(Update{Stage: str("刚认识")}, now)
	for i := 1; i < maxDates; i++ {
		if _, _, err := s.Apply(Update{AddDate: &DateArg{What: "日子" + string(rune('A'+i)), Date: "01-0" + string(rune('0'+i%9+1))}}, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Apply(Update{AddDate: &DateArg{What: "再一个", Date: "02-01"}}, now); err != nil {
		t.Fatalf("满了应先挤掉自动记下的：%v", err)
	}
	snap, _, _ := s.Load()
	if dateIndex(snap.Dates, "刚认识") >= 0 || len(snap.Dates) != maxDates {
		t.Errorf("自动记下的那个应被挤掉：%+v", snap.Dates)
	}
	if _, _, err := s.Apply(Update{AddDate: &DateArg{What: "又一个", Date: "02-02"}}, now); err == nil || !strings.Contains(err.Error(), "remove_date") {
		t.Errorf("全是模型记的就拒绝并提示先去掉一个：%v", err)
	}
}

func TestDateHits(t *testing.T) {
	dates := []DateMark{
		{What: "对方生日", Date: "05-12"},
		{What: "第一次见面", Date: "2025-05-12"},
		{What: "热恋", Date: "2026-02-10", Auto: true},
	}
	cases := []struct {
		on   time.Time
		want []string
	}{
		{day(2026, 5, 12), []string{"「对方生日」", "「第一次见面」1 周年"}},
		{day(2026, 5, 12).AddDate(0, 0, 1), nil},
		{day(2026, 2, 10).AddDate(0, 0, 100), []string{"成为「热恋」满 100 天"}},
		{day(2025, 5, 12), []string{"「对方生日」"}}, // 第一次见面当天不算周年
		{day(2025, 5, 12).AddDate(0, 0, 1000), []string{"「第一次见面」满 1000 天"}},
	}
	for _, c := range cases {
		got := dateHits("", dates, c.on)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s：got %v want %v", c.on.Format(dateLayout), got, c.want)
		}
	}
	if got := dateHits("", []DateMark{{What: "x", Date: "2023-01-01"}}, day(2023, 1, 1).AddDate(0, 0, 1000)); len(got) != 1 || !strings.Contains(got[0], "1000") {
		t.Errorf("满一千天：%v", got)
	}
}

// [关系] 只在今天或明天碰上日子时多一行；平常不出现日子清单。
func TestRenderDatesTodayAndTomorrow(t *testing.T) {
	snap := Snapshot{Stage: "热恋", Dates: []DateMark{{What: "对方生日", Date: "05-12"}, {What: "纪念日", Date: "05-13"}}}
	got := renderSnapshot("", snap, day(2026, 5, 12))
	if !strings.Contains(got, "日子：今天是「对方生日」；明天是「纪念日」。") {
		t.Errorf("今天与明天：%q", got)
	}
	if got := renderSnapshot("", snap, day(2026, 6, 1)); strings.Contains(got, "日子") {
		t.Errorf("平常不该出现：%q", got)
	}
}

// 每天第一轮结束时投一条开口理由；只看表人格（共享域），一天只投一次。
func TestDateCuesSharedScopeOnce(t *testing.T) {
	p := newTestPlugin(t)
	now := day(2026, 5, 12)
	p.clock = func() time.Time { return now }
	t.Cleanup(func() { cue.Take(now.AddDate(1, 0, 0)) })
	for _, tag := range []string{"", "inner"} {
		_, _, _ = p.storeFor(tag).Apply(Update{AddDate: &DateArg{What: "生日" + tag, Date: "05-12"}}, now)
	}
	p.OnTurnEnd(scopedCtx("inner"), plugin.TurnEndEvent{})
	if cue.Pending(now) {
		t.Fatal("里人格的日子不该投开口理由")
	}
	p.OnTurnEnd(scopedCtx(""), plugin.TurnEndEvent{})
	got := cue.Take(now)
	if len(got) != 1 || !strings.Contains(got[0].Text, "「生日」") || got[0].Source != "relationship" {
		t.Fatalf("表人格的日子应投一条：%+v", got)
	}
	p.OnTurnEnd(scopedCtx(""), plugin.TurnEndEvent{})
	if cue.Pending(now) {
		t.Error("同一天不该再投")
	}
}

// 模型用 add_date 改过自动记下的那一天之后，再回到这个阶段不能又新记一个同名的日子。
func TestCorrectedStageDateIsNotDuplicated(t *testing.T) {
	s := NewStore("", t.TempDir())
	_, _, _ = s.Apply(Update{Stage: str("热恋")}, day(2026, 1, 10))
	_, _, _ = s.Apply(Update{AddDate: &DateArg{What: "热恋", Date: "2026-01-08"}}, day(2026, 2, 1)) // 记错了，改一下
	_, _, _ = s.Apply(Update{Stage: str("冷战中")}, day(2026, 3, 1))
	_, _, _ = s.Apply(Update{Stage: str("热恋")}, day(2026, 3, 5))
	snap, _, _ := s.Load()
	n := 0
	for _, d := range snap.Dates {
		if d.What == "热恋" {
			n++
			if d.Date != "2026-01-08" {
				t.Errorf("改过的日子不该被覆盖：%+v", d)
			}
		}
	}
	if n != 1 {
		t.Errorf("同名的日子只能有一个：%+v", snap.Dates)
	}
}

// 当天的提醒投出去之后又去掉或改了那个日子：公告板上的旧提醒要撤回，改成今天的要补投。
func TestChangingDatesWithdrawsStaleCues(t *testing.T) {
	p := newTestPlugin(t)
	now := day(2026, 5, 12)
	p.clock = func() time.Time { return now }
	t.Cleanup(func() { cue.Take(now.AddDate(1, 0, 0)) })
	tool := p.Tools()[0]
	exec := func(args string) {
		t.Helper()
		if _, err := tool.Execute(scopedCtx(""), json.RawMessage(args)); err != nil {
			t.Fatal(err)
		}
	}
	exec(`{"add_date":{"what":"对方生日","date":"05-12"}}`)
	p.OnTurnEnd(scopedCtx(""), plugin.TurnEndEvent{})
	if !cue.Pending(now) {
		t.Fatal("前提：今天的生日提醒已投出")
	}

	exec(`{"add_date":{"what":"对方生日","date":"05-13"}}`) // 记错了，其实是明天
	if cue.Pending(now) {
		t.Error("改期之后旧提醒应撤回")
	}
	exec(`{"add_date":{"what":"对方生日","date":"05-12"}}`) // 又改回今天
	if got := cue.Take(now); len(got) != 1 || !strings.Contains(got[0].Text, "对方生日") {
		t.Errorf("改成今天的应补投：%+v", got)
	}

	exec(`{"add_date":{"what":"纪念日","date":"05-12"}}`)
	exec(`{"remove_date":"纪念日"}`)
	exec(`{"remove_date":"对方生日"}`)
	if cue.Pending(now) {
		t.Error("去掉的日子不该还留着提醒")
	}
}
