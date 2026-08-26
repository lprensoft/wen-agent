package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

// tlPlugin 建一个开着时间线、关掉定期提炼的插件（隔离被测路径）。
func tlPlugin(t *testing.T, c *fakeComplete) *Plugin {
	t.Helper()
	p := newPluginWithComplete(t, c, map[string]any{"turn_extract": false, "timeline": true})
	t.Cleanup(p.Stop)
	return p
}

// turnAt 造一轮指定时刻的真人对话。
func turnAt(at time.Time, user, reply string) plugin.TurnEndEvent {
	return plugin.TurnEndEvent{
		SessionID: "s1", Interactive: true,
		UserInput: user, FinalText: reply,
		StartedAt: at, EndedAt: at,
	}
}

// wait 等后台 goroutine 收尾（OnTurnEnd 的工作都在 wg 里登记）。
func wait(p *Plugin) { p.wg.Wait() }

func TestDayBufAccumulates(t *testing.T) {
	p := tlPlugin(t, &fakeComplete{})
	// 用「今天的正午」：日期必须是今天（过去的日期会被并发的日切收束当昨天取走，
	// 生产上不存在这种输入），而固定正午避免 now+1h 在临近午夜时跨天。
	y, m, d := time.Now().Date()
	noon := time.Date(y, m, d, 12, 0, 0, 0, time.Local)
	p.OnTurnEnd(context.Background(), turnAt(noon, "早上聊的", "嗯"))
	p.OnTurnEnd(context.Background(), turnAt(noon.Add(time.Minute), "下午聊的", "好"))
	wait(p)

	s := p.snapshot()
	f := loadDayBuf(plugin.DomainDir(s.libBase, ""))
	if len(f.Days) != 1 {
		t.Fatalf("days = %d, want 1", len(f.Days))
	}
	for _, want := range []string{"早上聊的", "下午聊的"} {
		if !strings.Contains(f.Days[0].Text, want) {
			t.Errorf("缓冲缺少 %q：%s", want, f.Days[0].Text)
		}
	}
}

func TestDayBufCapDropsOldest(t *testing.T) {
	p := tlPlugin(t, &fakeComplete{})
	now := time.Now()
	long := strings.Repeat("字", 4000)
	for range 6 {
		p.OnTurnEnd(context.Background(), turnAt(now, "开头的标记", long))
		wait(p)
	}
	s := p.snapshot()
	f := loadDayBuf(plugin.DomainDir(s.libBase, ""))
	if len(f.Days[0].Text) > maxDayBufBytes {
		t.Errorf("缓冲 %d 字节超过上限 %d", len(f.Days[0].Text), maxDayBufBytes)
	}
}

func TestDaySwitchDistillsYesterday(t *testing.T) {
	c := &fakeComplete{replies: []string{"确定了搬家的日子，把书房先收了箱。"}}
	p := tlPlugin(t, c)
	yesterday := time.Now().Add(-24 * time.Hour)

	// 昨天聊了一轮（把日切水位也拨回昨天，模拟「昨天就在用」）
	p.OnTurnEnd(context.Background(), turnAt(yesterday, "商量搬家", "收拾书房"))
	wait(p)
	p.turnMu.Lock()
	p.lastTimeline = yesterday
	p.turnMu.Unlock()

	// 今天的第一轮触发收束
	p.OnTurnEnd(context.Background(), turnAt(time.Now(), "早", "早上好"))
	wait(p)

	if c.calls != 1 {
		t.Fatalf("收束调用了 %d 次，want 1", c.calls)
	}
	if !strings.Contains(c.prompts[0], "商量搬家") || !strings.Contains(c.prompts[0], dayOf(yesterday)) {
		t.Errorf("收束提示词不对：%s", c.prompts[0])
	}
	s := p.snapshot()
	tl := loadTimeline(plugin.DomainDir(s.libBase, ""))
	if len(tl.Entries) != 1 || tl.Entries[0].Date != dayOf(yesterday) ||
		!strings.Contains(tl.Entries[0].Summary, "搬家") {
		t.Fatalf("timeline = %+v", tl.Entries)
	}
	// 昨天的缓冲已被取走，今天的还在
	f := loadDayBuf(plugin.DomainDir(s.libBase, ""))
	if len(f.Days) != 1 || f.Days[0].Date != dayOf(time.Now()) {
		t.Errorf("daybuf = %+v", f.Days)
	}
}

func TestDaySwitchSkipsEmptyDay(t *testing.T) {
	c := &fakeComplete{replies: []string{"SKIP"}}
	p := tlPlugin(t, c)
	yesterday := time.Now().Add(-24 * time.Hour)
	p.OnTurnEnd(context.Background(), turnAt(yesterday, "在吗", "在"))
	wait(p)
	p.turnMu.Lock()
	p.lastTimeline = yesterday
	p.turnMu.Unlock()

	p.OnTurnEnd(context.Background(), turnAt(time.Now(), "早", "早"))
	wait(p)

	s := p.snapshot()
	tl := loadTimeline(plugin.DomainDir(s.libBase, ""))
	if len(tl.Entries) != 0 {
		t.Errorf("SKIP 的一天不该进时间线：%+v", tl.Entries)
	}
	// 缓冲同样被清掉：这一天已经处理过了
	f := loadDayBuf(plugin.DomainDir(s.libBase, ""))
	for _, d := range f.Days {
		if d.Date == dayOf(yesterday) {
			t.Error("已处理的日子应从缓冲移除")
		}
	}
}

func TestDistillFailureKeepsBuffer(t *testing.T) {
	c := &fakeComplete{err: context.DeadlineExceeded}
	p := tlPlugin(t, c)
	yesterday := time.Now().Add(-24 * time.Hour)
	p.OnTurnEnd(context.Background(), turnAt(yesterday, "重要的事", "记下了"))
	wait(p)
	p.turnMu.Lock()
	p.lastTimeline = yesterday
	p.turnMu.Unlock()

	p.OnTurnEnd(context.Background(), turnAt(time.Now(), "早", "早"))
	wait(p)

	s := p.snapshot()
	f := loadDayBuf(plugin.DomainDir(s.libBase, ""))
	found := false
	for _, d := range f.Days {
		if d.Date == dayOf(yesterday) {
			found = true
		}
	}
	if !found {
		t.Error("收束失败时缓冲应保留，等下次日切再试")
	}
}

func TestTimelineBlockMergesReadableDomains(t *testing.T) {
	p := tlPlugin(t, &fakeComplete{})
	s := p.snapshot()
	p.appendTimeline(plugin.DomainDir(s.libBase, ""), timelineEntry{Date: "2026-08-15", Summary: "共享域的一天"})
	p.appendTimeline(plugin.DomainDir(s.libBase, "inner"), timelineEntry{Date: "2026-08-16", Summary: "里域的一天"})

	// 里人格读两个域，最近在前
	inner := p.timelineBlock(plugin.WithScope(context.Background(),
		plugin.Scope{Write: "inner", Read: []string{"", "inner"}}), s)
	if !strings.Contains(inner, "里域的一天") || !strings.Contains(inner, "共享域的一天") {
		t.Errorf("inner block = %q", inner)
	}
	if strings.Index(inner, "2026-08-16") > strings.Index(inner, "2026-08-15") {
		t.Errorf("应最近在前：%q", inner)
	}

	// 表人格只读共享域：里域那行不可见
	outer := p.timelineBlock(plugin.WithScope(context.Background(),
		plugin.Scope{Write: "", Read: []string{""}}), s)
	if strings.Contains(outer, "里域") {
		t.Errorf("outer block 泄漏了里域：%q", outer)
	}
}

func TestTimelineBlockLimitsEntries(t *testing.T) {
	p := tlPlugin(t, &fakeComplete{})
	s := p.snapshot()
	dir := plugin.DomainDir(s.libBase, "")
	for _, d := range []string{"2026-08-10", "2026-08-11", "2026-08-12"} {
		p.appendTimeline(dir, timelineEntry{Date: d, Summary: "第 " + d + " 天"})
	}
	s.timelineDays = 2
	block := p.timelineBlock(context.Background(), s)
	if strings.Count(block, "\n- ") != 2 {
		t.Errorf("应只注入最近 2 条：%q", block)
	}
	if strings.Contains(block, "2026-08-10") {
		t.Errorf("最早的一条不该出现：%q", block)
	}
}

func TestTimelineDisabled(t *testing.T) {
	p := newPluginWithComplete(t, &fakeComplete{}, map[string]any{"turn_extract": false, "timeline": false})
	t.Cleanup(p.Stop)
	p.OnTurnEnd(context.Background(), turnAt(time.Now(), "你好", "你好"))
	wait(p)
	s := p.snapshot()
	f := loadDayBuf(plugin.DomainDir(s.libBase, ""))
	if len(f.Days) != 0 {
		t.Errorf("关闭时间线时不该攒缓冲：%+v", f.Days)
	}
}

func TestClipSummary(t *testing.T) {
	if got := clipSummary("「一句话\n带换行」"); got != "一句话 带换行" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("长", maxSummaryRunes+30)
	if got := clipSummary(long); len([]rune(got)) != maxSummaryRunes {
		t.Errorf("len = %d", len([]rune(got)))
	}
}

// ---------- 日记抬头 ----------

// 收束时把各插件给出的当日事实一并存进条目，注入时渲染成日记的抬头：
// 「日期 星期几，天气：…，正文」。
func TestTimelineRecordsDayFacts(t *testing.T) {
	c := &fakeComplete{replies: []string{"陪你聊了加班的事"}}
	p := New()
	asked := ""
	ictx := plugin.InitContext{
		StateDir: t.TempDir(),
		Complete: c.fn,
		DayFacts: func(date string) []string {
			asked = date
			return []string{"天气：小雨 24~31℃"}
		},
	}
	if err := p.Init(ictx, map[string]any{"turn_extract": false, "timeline": true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)

	// 攒一天（昨天），再让日切收束把它取走
	s := p.snapshot()
	yesterday := time.Now().AddDate(0, 0, -1)
	p.appendDayBuf(s, "", turnAt(yesterday, "我加班到十一点", "那我等你"))
	p.runTimeline(s)

	if asked != dayOf(yesterday) {
		t.Fatalf("问的日期不对: %q，期望 %q", asked, dayOf(yesterday))
	}
	block := p.timelineBlock(context.Background(), s)
	want := dayOf(yesterday) + " " + weekdayName("", yesterday) + "，天气：小雨 24~31℃，陪你聊了加班的事"
	if !strings.Contains(block, want) {
		t.Fatalf("日记抬头不符：\n%s\n期望含：%s", block, want)
	}
}

// 拿不到当日事实（天气插件没开、评测装配里没有这个入口）时照常写，只是没有抬头。
func TestTimelineWithoutDayFacts(t *testing.T) {
	p := tlPlugin(t, &fakeComplete{replies: []string{"普通的一天"}})
	s := p.snapshot()
	yesterday := time.Now().AddDate(0, 0, -1)
	p.appendDayBuf(s, "", turnAt(yesterday, "在吗", "在"))
	p.runTimeline(s)

	block := p.timelineBlock(context.Background(), s)
	want := dayOf(yesterday) + " " + weekdayName("", yesterday) + "，普通的一天"
	if !strings.Contains(block, want) {
		t.Fatalf("没有事实时应只有日期与星期：\n%s\n期望含：%s", block, want)
	}
}

// 老条目（没有 facts 字段）照旧渲染，不能因为升级就变形。
func TestRenderEntryWithoutFacts(t *testing.T) {
	got := renderEntry("", timelineEntry{Date: "2026-08-21", Summary: "旧的一行"})
	if got != "2026-08-21 周五，旧的一行" {
		t.Fatalf("老条目渲染不符: %q", got)
	}
	// 日期认不出来时只写日期本身，不猜星期几
	if got := renderEntry("", timelineEntry{Date: "不是日期", Summary: "x"}); got != "不是日期，x" {
		t.Fatalf("坏日期渲染不符: %q", got)
	}
}
