package agenda

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wen/internal/availability"
	"wen/internal/cue"
	"wen/internal/plugin"
)

// ---------- 测试替身 ----------

// fakeSessions 是只读会话查询的替身：一个固定的「最近活跃会话」。
type fakeSessions struct{ id string }

func (f fakeSessions) LastActive() (string, time.Time, error) { return f.id, time.Time{}, nil }
func (f fakeSessions) LastInteraction() (time.Time, error)    { return time.Time{}, nil }
func (f fakeSessions) Exists(id string) bool                  { return id == f.id }

// fakeLookup 是人物库查询的替身。
type fakeLookup struct{ names []string }

func (f fakeLookup) Known(_ context.Context, name string) (string, bool) {
	for _, n := range f.names {
		if strings.EqualFold(n, name) {
			return n, true
		}
	}
	return "", false
}
func (f fakeLookup) Names(context.Context) []string { return f.names }

// clock 是可拨动的时钟。
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// at 构造本地时区的某一时刻（2026-08-21 是周五）。
func at(hh, mm int) time.Time {
	return time.Date(2026, 8, 21, hh, mm, 0, 0, time.Local)
}

type harness struct {
	p        *Plugin
	clk      *clock
	calls    atomic.Int32
	inputs   chan string
	notices  chan string
	runTurn  func(ctx context.Context, sid, input string) (string, error)
	sessions fakeSessions
}

// newHarness 建一个已 Init 的插件。running 为假时 Init 后立刻 Stop：用例直接调内部方法
// 驱动，后台循环留着只会和它们抢状态；要测触发链路的用例传 running=true，并把启动
// 宽限拨到很长，让循环什么都不做。
func newHarness(t *testing.T, running bool, cfg map[string]any) *harness {
	t.Helper()
	h := &harness{clk: &clock{t: at(7, 32)}, inputs: make(chan string, 16), notices: make(chan string, 16),
		sessions: fakeSessions{id: "s1"}}
	h.p = New(fakeLookup{names: []string{"林舟", "母亲", "老陈"}})
	h.p.now = h.clk.now
	h.p.planSettle = 0
	h.p.busyRetry = time.Millisecond
	if running {
		h.p.startupGrace = time.Hour
	} else {
		h.p.startupGrace = 0
	}
	h.runTurn = func(ctx context.Context, sid, input string) (string, error) { return "", nil }
	ictx := plugin.InitContext{
		StateDir: t.TempDir(),
		Sessions: h.sessions,
		RunTurn: func(ctx context.Context, sid, input string) (string, error) {
			h.calls.Add(1)
			select {
			case h.inputs <- input:
			default:
			}
			return h.runTurn(ctx, sid, input)
		},
		Notice: func(_ context.Context, _ string, text string) error {
			h.notices <- text
			return nil
		},
	}
	if err := h.p.Init(ictx, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.p.Stop)
	if !running {
		h.p.Stop()
	}
	cue.Take(time.Now()) // 清掉别的用例留下的理由
	availability.Clear(availabilitySource)
	return h
}

func (h *harness) tool(name string) plugin.Tool {
	for _, tl := range h.p.Tools() {
		if tl.Name() == name {
			return tl
		}
	}
	return nil
}

func (h *harness) call(t *testing.T, name string, args string) string {
	t.Helper()
	out, err := h.tool(name).Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s(%s) 出错: %v", name, args, err)
	}
	return out
}

func (h *harness) fail(t *testing.T, name string, args string) error {
	t.Helper()
	_, err := h.tool(name).Execute(context.Background(), json.RawMessage(args))
	if err == nil {
		t.Fatalf("%s(%s) 应当报错", name, args)
	}
	return err
}

// plan 读出共享域今天的表。
func (h *harness) plan(t *testing.T) Plan {
	t.Helper()
	pl, err := h.p.storeFor("").LoadPlan()
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

func (h *harness) commitments(t *testing.T) []Commitment {
	t.Helper()
	cs, err := h.p.storeFor("").LoadCommitments()
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// setPlan 直接落一张表（绕过工具），items 按 title/start/end 给。
func (h *harness) setPlan(t *testing.T, items ...Item) {
	t.Helper()
	day := logicalDay(h.clk.now(), defDayStartHour)
	for i := range items {
		if items[i].ID == "" {
			items[i].ID = "a" + string(rune('1'+i))
		}
		if items[i].Status == "" {
			items[i].Status = statusPlanned
		}
		if items[i].Flex == "" {
			items[i].Flex = flexTry
		}
		if items[i].Busy == "" {
			items[i].Busy = defaultBusy
		}
	}
	if err := h.p.storeFor("").SavePlan(Plan{Date: day.Format(dateLayout), Weekday: weekdayCN(day), PlannedAt: h.clk.now(), Items: items}); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

func recvInput(t *testing.T, h *harness) string {
	t.Helper()
	select {
	case in := <-h.inputs:
		return in
	case <-time.After(5 * time.Second):
		t.Fatal("没有等到轮次输入")
		return ""
	}
}

// ---------- 日期与时刻 ----------

func TestLogicalDay(t *testing.T) {
	if d := logicalDay(at(4, 59), 5); d.Day() != 20 {
		t.Fatalf("04:59 应归前一天，得到 %v", d)
	}
	if d := logicalDay(at(5, 0), 5); d.Day() != 21 {
		t.Fatalf("05:00 应归当天，得到 %v", d)
	}
}

func TestSpanCrossMidnight(t *testing.T) {
	day := logicalDay(at(12, 0), 5)
	it := Item{Start: "23:00", End: "01:00"}
	s, e := it.span(day, 5)
	if s.Day() != 21 || e.Day() != 22 || e.Hour() != 1 {
		t.Fatalf("跨午夜的项应结束在次日 01:00，得到 %v-%v", s, e)
	}
	late := Item{Start: "01:00", End: "02:00"}
	s, _ = late.span(day, 5)
	if s.Day() != 22 {
		t.Fatalf("一天起点之前的时刻属于次日凌晨，得到 %v", s)
	}
}

// ---------- set_day_plan ----------

const planArgs = `{"items":[
 {"title":"晨跑","start":"7:00","end":"07:40","place":"江边","flex":"可挪"},
 {"title":"和林舟在图书馆查资料","start":"14:00","end":"16:30","with":["林舟"],"flex":"尽量守","busy":"重忙"},
 {"title":"和对方吃晚饭","start":"18:30","end":"20:00","with_user":true,"flex":"可挪","busy":"不回"}
]}`

func TestSetPlan(t *testing.T) {
	h := newHarness(t, false, nil)
	out := h.call(t, "set_day_plan", planArgs)
	// 回执末尾那句终结语不是装饰：只给一份状态清单时，模型会把它读成进展汇报，
	// 接着微调重排（真实事故里连提了十三次）。
	want := "今天（周五）的安排已定，共 3 项：\na1 07:00-07:40 晨跑（可挪）\n" +
		"a2 14:00-16:30 和林舟在图书馆查资料（尽量守）\na3 18:30-20:00 和对方吃晚饭（不能动）\n" +
		"表已排定，这一轮到此为止：不要再次提交，也不必再调用别的工具。"
	if out != want {
		t.Fatalf("回执不符：\n%s", out)
	}
	pl := h.plan(t)
	if pl.Date != "2026-08-21" || len(pl.Items) != 3 || pl.Items[2].Flex != flexFixed || pl.Items[1].Busy != "重忙" {
		t.Fatalf("表落盘不符: %+v", pl)
	}
	if pl.Items[1].With[0] != "林舟" {
		t.Fatalf("同行的人应换成库里的写法: %+v", pl.Items[1].With)
	}
}

func TestSetPlanRejects(t *testing.T) {
	h := newHarness(t, false, map[string]any{"max_items": 2})
	if err := h.fail(t, "set_day_plan", planArgs); !strings.Contains(err.Error(), "最多排 2 项") {
		t.Fatalf("项数上限措辞不符: %v", err)
	}
	h2 := newHarness(t, false, nil)
	err := h2.fail(t, "set_day_plan", `{"items":[{"title":"x","start":"25:00","end":"01:00","flex":"可挪"}]}`)
	if !strings.Contains(err.Error(), "不合法") {
		t.Fatalf("时间校验措辞不符: %v", err)
	}
	err = h2.fail(t, "set_day_plan", `{"items":[{"title":"x","start":"10:00","end":"11:00","with":["小周"],"flex":"可挪"}]}`)
	if !strings.Contains(err.Error(), "没有叫「小周」的人物") || !strings.Contains(err.Error(), "林舟、母亲、老陈") {
		t.Fatalf("人物校验应列候选: %v", err)
	}
}

// 规划轮次里第二次提交要被挡回去：那句「排好后用 set_day_plan 提交」每次迭代都会被
// 重新读到，模型会一版一版微调着重提交（真实事故里连提十三次）。
func TestSetPlanRejectsResubmitWithinPlanningTurn(t *testing.T) {
	h := newHarness(t, false, nil)
	h.p.mu.Lock()
	h.p.planning[""] = true // 模拟规划轮次进行中
	h.p.mu.Unlock()

	h.call(t, "set_day_plan", planArgs) // 第一次正常受理
	err := h.fail(t, "set_day_plan", planArgs)
	if !strings.Contains(err.Error(), "本轮不要再提交") {
		t.Fatalf("重复提交应被挡回: %v", err)
	}

	// 规划轮次之外（对话里说「重排一下今天」）仍然允许重排
	h.p.mu.Lock()
	delete(h.p.planning, "")
	h.p.mu.Unlock()
	h.call(t, "set_day_plan", planArgs)
}

// 字段缺了和填错了要分开说：都报「只能是……」的话，模型会去改一个本来就没写的字段。
func TestSetPlanDistinguishesMissingFromInvalid(t *testing.T) {
	h := newHarness(t, false, nil)
	err := h.fail(t, "set_day_plan", `{"items":[{"title":"x","start":"10:00","end":"11:00"}]}`)
	if !strings.Contains(err.Error(), "缺少 flex") {
		t.Fatalf("缺字段应说「缺少」: %v", err)
	}
	err = h.fail(t, "set_day_plan", `{"items":[{"title":"x","start":"10:00","end":"11:00","flex":"随便"}]}`)
	if !strings.Contains(err.Error(), "的 flex 只能是") {
		t.Fatalf("填错值应说「只能是」: %v", err)
	}
	// 把一件事拆成两个对象是模型偶发的错法，报错要点出这一点
	err = h.fail(t, "set_day_plan", `{"items":[
	 {"title":"x","start":"10:00","end":"11:00"},
	 {"place":"家里","flex":"可挪","busy":"轻忙"}]}`)
	if !strings.Contains(err.Error(), "不要拆成两段") {
		t.Fatalf("拆项应点出成因: %v", err)
	}
}

func TestSetPlanOverlapOnlyNotes(t *testing.T) {
	h := newHarness(t, false, nil)
	out := h.call(t, "set_day_plan", `{"items":[
	 {"title":"a","start":"14:00","end":"16:30","flex":"可挪"},
	 {"title":"b","start":"16:00","end":"17:00","flex":"可挪"}]}`)
	if !strings.Contains(out, "14:00-16:30 与 16:00-17:00 重叠") {
		t.Fatalf("重叠应告知: %s", out)
	}
}

func TestSetPlanRequiresTodayCommitments(t *testing.T) {
	h := newHarness(t, false, nil)
	h.call(t, "add_commitment", `{"date":"2026-08-21","start":"18:30","end":"20:00","title":"和对方吃晚饭","with_user":true}`)
	err := h.fail(t, "set_day_plan", `{"items":[{"title":"晨跑","start":"07:00","end":"07:40","flex":"可挪"}]}`)
	if !strings.Contains(err.Error(), "[c1] 18:30-20:00 和对方吃晚饭") {
		t.Fatalf("缺约定应点名: %v", err)
	}
	// 与对方的约定时间不能改
	err = h.fail(t, "set_day_plan", `{"items":[{"title":"和对方吃晚饭","start":"19:00","end":"20:00","flex":"可挪","from_commitment":"c1"}]}`)
	if !strings.Contains(err.Error(), "时间 18:30-20:00 不能改") {
		t.Fatalf("约定时间校验措辞不符: %v", err)
	}
	out := h.call(t, "set_day_plan", `{"items":[{"title":"和对方吃晚饭","start":"18:30","end":"20:00","flex":"可挪","from_commitment":"c1"}]}`)
	if !strings.Contains(out, "（不能动，来自约定 c1）") {
		t.Fatalf("回执应标注来源: %s", out)
	}
	if cs := h.commitments(t); !cs[0].Planned {
		t.Fatal("排进表后约定应标 planned")
	}
}

func TestReplanKeepsFiredMarks(t *testing.T) {
	h := newHarness(t, false, nil)
	h.call(t, "set_day_plan", planArgs)
	fired := at(7, 0)
	_, _ = h.p.storeFor("").UpdatePlan(func(pl *Plan) (bool, error) {
		pl.Items[0].StartFired, pl.Items[0].Status = fired, statusOngoing
		return true, nil
	})
	h.call(t, "set_day_plan", planArgs)
	pl := h.plan(t)
	if !pl.Items[0].StartFired.Equal(fired) {
		t.Fatal("重排应保留派发记录")
	}
	if pl.Items[0].Status != statusPlanned {
		t.Fatal("重排应清状态")
	}
}

// ---------- update_day_plan ----------

func TestUpdateUserItemNeedsAgreement(t *testing.T) {
	h := newHarness(t, false, nil)
	h.call(t, "set_day_plan", planArgs)
	for _, args := range []string{
		`{"id":"a3","start":"19:00"}`,
		`{"id":"a3","status":"cancelled","outcome":"不想去"}`,
		`{"id":"a3","status":"deferred","defer_to":"2026-08-23","outcome":"改天"}`,
		`{"id":"a3","status":"skipped","outcome":"累了"}`,
	} {
		err := h.fail(t, "update_day_plan", args)
		if !strings.Contains(err.Error(), "和对方的约定") || !strings.Contains(err.Error(), "agreed_by_user") {
			t.Fatalf("拒绝时应说明规则与出口: %v", err)
		}
	}
	out := h.call(t, "update_day_plan", `{"id":"a3","start":"19:00","agreed_by_user":true}`)
	if !strings.Contains(out, "时间改为 19:00-20:00") {
		t.Fatalf("对方同意后应可改: %s", out)
	}
	// 与对方一起的项进行中不写忙碌状态
	h.call(t, "update_day_plan", `{"id":"a3","status":"ongoing"}`)
	if _, ok := availability.Current(h.clk.now()); ok {
		t.Fatal("与对方一起的项不该写忙碌状态")
	}
}

func TestUpdateDeferredCreatesCommitment(t *testing.T) {
	h := newHarness(t, false, nil)
	h.call(t, "set_day_plan", planArgs)
	out := h.call(t, "update_day_plan", `{"id":"a2","status":"deferred","defer_to":"2026-08-23 14:00","outcome":"林舟临时有事"}`)
	if !strings.Contains(out, "已延期到 8 月 23 日（周日） 14:00，登记为约定 c1") {
		t.Fatalf("回执不符: %s", out)
	}
	cs := h.commitments(t)
	if len(cs) != 1 || cs[0].Date != "2026-08-23" || cs[0].Note != "从 8/21 延期：林舟临时有事" || cs[0].With[0] != "林舟" {
		t.Fatalf("延期生成的约定不符: %+v", cs)
	}
	if h.plan(t).Items[1].Status != statusDeferred {
		t.Fatal("项应标为延期")
	}
	if err := h.fail(t, "update_day_plan", `{"id":"a1","status":"deferred","defer_to":"2026-08-21","outcome":"x"}`); !strings.Contains(err.Error(), "今天之后") {
		t.Fatalf("延期到今天应拒绝: %v", err)
	}
	if err := h.fail(t, "update_day_plan", `{"id":"a1","status":"skipped"}`); !strings.Contains(err.Error(), "原因") {
		t.Fatalf("跳过要写原因: %v", err)
	}
}

func TestUpdateAvailability(t *testing.T) {
	h := newHarness(t, false, nil)
	h.call(t, "set_day_plan", planArgs)
	h.clk.set(at(14, 2))
	out := h.call(t, "update_day_plan", `{"id":"a2","status":"ongoing"}`)
	if out != "a2 已标为进行中（到 16:30）。" {
		t.Fatalf("回执不符: %s", out)
	}
	st, ok := availability.Current(h.clk.now())
	if !ok || st.Level != availability.Heavy || st.Activity != "和林舟在图书馆查资料" || !st.Until.Equal(at(16, 30)) {
		t.Fatalf("进行中应写忙碌状态: %+v ok=%v", st, ok)
	}
	h.clk.set(at(16, 35))
	h.call(t, "update_day_plan", `{"id":"a2","status":"done","outcome":"资料没查到多少，聊了一下午"}`)
	if _, ok := availability.Current(h.clk.now()); ok {
		t.Fatal("做完应清忙碌状态")
	}
	if err := h.fail(t, "update_day_plan", `{"id":"a2","status":"ongoing"}`); !strings.Contains(err.Error(), "已经做完") {
		t.Fatalf("终态不能再改: %v", err)
	}
}

// ---------- 约定 ----------

func TestAddCommitment(t *testing.T) {
	h := newHarness(t, false, nil)
	if err := h.fail(t, "add_commitment", `{"date":"2026-08-20","start":"10:00","title":"x"}`); !strings.Contains(err.Error(), "已经过去了") {
		t.Fatalf("过去的日期应拒绝: %v", err)
	}
	out := h.call(t, "add_commitment", `{"date":"2026-08-23","start":"10:00","title":"陪母亲复查","with":["母亲"]}`)
	if out != "已登记约定 c1：8 月 23 日（周日） 10:00 陪母亲复查（尽量守）。" {
		t.Fatalf("回执不符: %s", out)
	}
	// 今天的：表未排时留在约定里；表已排时直接追加进表
	out = h.call(t, "add_commitment", `{"date":"2026-08-21","start":"21:00","title":"给老陈送钥匙","with":["老陈"]}`)
	if !strings.Contains(out, "排表时会要求把它排进去") {
		t.Fatalf("表未排时的回执不符: %s", out)
	}
	h.call(t, "set_day_plan", `{"items":[{"title":"给老陈送钥匙","start":"21:00","end":"21:10","flex":"可挪","from_commitment":"c2"}]}`)
	out = h.call(t, "add_commitment", `{"date":"2026-08-21","start":"18:30","end":"20:00","title":"和对方吃晚饭","with_user":true}`)
	if !strings.Contains(out, "并加进今天的表（a2）") {
		t.Fatalf("表已排时应追加: %s", out)
	}
	pl := h.plan(t)
	if len(pl.Items) != 2 || pl.Items[1].FromCommitment != "c3" || pl.Items[1].Flex != flexFixed {
		t.Fatalf("追加的项不符: %+v", pl.Items)
	}
	// 取消后的号不复用
	h.call(t, "cancel_commitment", `{"id":"c1","reason":"复查改期"}`)
	out = h.call(t, "add_commitment", `{"date":"2026-08-25","start":"10:00","title":"再去"}`)
	if !strings.Contains(out, "约定 c4") {
		t.Fatalf("取消过的号不该复用: %s", out)
	}
}

func TestCancelCommitment(t *testing.T) {
	h := newHarness(t, false, nil)
	h.call(t, "set_day_plan", `{"items":[]}`)
	h.call(t, "add_commitment", `{"date":"2026-08-21","start":"18:30","end":"20:00","title":"和对方吃晚饭","with_user":true}`)
	cue.Post(cue.Cue{Source: availabilitySource, Key: "soon|a1", Text: "x", Expire: time.Now().Add(time.Hour)})
	if err := h.fail(t, "cancel_commitment", `{"id":"c1","reason":"不想去"}`); !strings.Contains(err.Error(), "agreed_by_user") {
		t.Fatalf("与对方的约定取消要先商量: %v", err)
	}
	out := h.call(t, "cancel_commitment", `{"id":"c1","reason":"对方改天","agreed_by_user":true}`)
	if !strings.Contains(out, "今天表里的 a1 一并标为取消") {
		t.Fatalf("回执不符: %s", out)
	}
	if cue.Pending(time.Now()) {
		t.Fatal("取消后应撤回 soon 理由")
	}
	if pl := h.plan(t); pl.Items[0].Status != statusCancelled || pl.Items[0].Outcome != "对方改天" {
		t.Fatalf("表上的项应一并取消: %+v", pl.Items[0])
	}
	if len(h.commitments(t)) != 0 {
		t.Fatal("约定应已移除")
	}
}

func TestListCommitments(t *testing.T) {
	h := newHarness(t, false, nil)
	if out := h.call(t, "list_commitments", `{}`); out != "没有登记任何约定。" {
		t.Fatalf("空列表回执不符: %s", out)
	}
	h.call(t, "add_commitment", `{"date":"2026-08-30","start":"14:00","title":"和对方去看展","with_user":true,"place":"美术馆"}`)
	h.call(t, "add_commitment", `{"date":"2026-08-23","start":"10:00","title":"陪母亲复查","with":["母亲"],"note":"带上片子"}`)
	out := h.call(t, "list_commitments", `{}`)
	want := "共 2 条：\n- [c2] 08-23（周日） 10:00 陪母亲复查（尽量守），和 母亲；带上片子\n- [c1] 08-30（周日） 14:00 和对方去看展（不能动），和对方，在 美术馆"
	if out != want {
		t.Fatalf("列表不符：\n%s", out)
	}
	if out := h.call(t, "list_commitments", `{"date":"2026-08-23"}`); !strings.HasPrefix(out, "共 1 条") {
		t.Fatalf("按日期过滤不符: %s", out)
	}
}

// ---------- 注入 ----------

func TestRenderPromptSamples(t *testing.T) {
	h := newHarness(t, false, nil)
	tp := func() string {
		out, err := h.p.TurnPrompt(context.Background(), plugin.TurnEvent{})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := tp(); got != "[今日安排]（周五）\n今天的安排尚未排定。" {
		t.Fatalf("未排样例不符：\n%s", got)
	}
	h.call(t, "add_commitment", `{"date":"2026-08-21","start":"18:30","end":"20:00","title":"和对方吃晚饭","with_user":true}`)
	h.call(t, "add_commitment", `{"date":"2026-08-23","start":"10:00","title":"陪母亲复查","with":["母亲"]}`)
	if got := tp(); got != "[今日安排]（周五）\n今天的安排尚未排定。\n今天已定的约定：18:30 和对方吃晚饭（不能动）。\n[未来约定]\n08-23（周日） 10:00 陪母亲复查（尽量守）" {
		t.Fatalf("未排但有约定的样例不符：\n%s", got)
	}
	h.call(t, "set_day_plan", `{"items":[
	 {"title":"晨跑","start":"07:00","end":"07:40","flex":"可挪"},
	 {"title":"和林舟在图书馆查资料","start":"14:00","end":"16:30","with":["林舟"],"flex":"尽量守","busy":"重忙"},
	 {"title":"和对方吃晚饭","start":"18:30","end":"20:00","with_user":true,"flex":"不能动","from_commitment":"c1"}]}`)
	h.clk.set(at(7, 45))
	h.call(t, "update_day_plan", `{"id":"a1","status":"done","outcome":"跑了五公里，雨前回来的"}`)
	h.clk.set(at(14, 2))
	h.call(t, "update_day_plan", `{"id":"a2","status":"ongoing"}`)
	h.clk.set(at(14, 40))
	want := "[今日安排]（周五）\n此刻：和林舟在图书馆查资料（14:00-16:30，尽量守），已进行 40 分钟。\n之后：18:30 和对方吃晚饭（不能动）。\n今天已做：晨跑——跑了五公里，雨前回来的。\n[未来约定]\n08-23（周日） 10:00 陪母亲复查（尽量守）"
	if got := tp(); got != want {
		t.Fatalf("进行中样例不符：\n%s", got)
	}
	h.clk.set(at(16, 20))
	h.call(t, "update_day_plan", `{"id":"a2","status":"done","outcome":"资料没查到多少，聊了一下午"}`)
	want = "[今日安排]（周五）\n此刻：空闲。下一项 18:30 和对方吃晚饭（不能动），还有 2 小时 10 分钟。\n今天已做：晨跑——跑了五公里，雨前回来的；和林舟在图书馆查资料——资料没查到多少，聊了一下午。\n[未来约定]\n08-23（周日） 10:00 陪母亲复查（尽量守）"
	if got := tp(); got != want {
		t.Fatalf("空闲样例不符：\n%s", got)
	}
	// 空表
	h2 := newHarness(t, false, nil)
	h2.call(t, "set_day_plan", `{"items":[]}`)
	out, _ := h2.p.TurnPrompt(context.Background(), plugin.TurnEvent{})
	if out != "[今日安排]（周五）\n今天没有安排，整天空着。" {
		t.Fatalf("空表样例不符：\n%s", out)
	}
}

func TestRenderBudgetDegrades(t *testing.T) {
	now := at(15, 0)
	long := strings.Repeat("经", 70)
	pl := Plan{Date: "2026-08-21", Items: []Item{
		{ID: "a1", Title: "晨跑", Start: "07:00", End: "07:40", Flex: flexMovable, Status: statusDone, Outcome: long},
		{ID: "a2", Title: "查资料", Start: "10:00", End: "12:00", Flex: flexTry, Status: statusDone, Outcome: long},
		{ID: "a3", Title: "写稿", Start: "14:00", End: "16:30", Flex: flexTry, Status: statusOngoing},
		{ID: "a4", Title: "和对方吃晚饭", Start: "18:30", End: "20:00", Flex: flexFixed, Status: statusPlanned, WithUser: true},
	}}
	var cs []Commitment
	for i := 0; i < 8; i++ {
		cs = append(cs, Commitment{ID: "c", Date: "2026-08-2" + string(rune('2'+i%8)), Start: "10:00", Title: strings.Repeat("约", 25), Flex: flexTry})
	}
	s := settings{dayStartHour: 5, maxCommitInject: 8, maxInjectBytes: 0}
	full := renderPrompt(pl, cs, nil, now, s)
	if !strings.Contains(full, long) {
		t.Fatal("不设预算时应全列")
	}
	// 第一级：去掉经历
	s.maxInjectBytes = len(full) - 1
	l1 := renderPrompt(pl, cs, nil, now, s)
	if strings.Contains(l1, long) || !strings.Contains(l1, "今天已做：晨跑；查资料。") {
		t.Fatalf("第一级应去掉经历只留标题：\n%s", l1)
	}
	// 第二级：已做压成条数
	s.maxInjectBytes = len(l1) - 1
	l2 := renderPrompt(pl, cs, nil, now, s)
	if !strings.Contains(l2, "今天已做 2 项。") || !strings.Contains(l2, "08-2") {
		t.Fatalf("第二级应把已做项压成条数、约定仍列出：\n%s", l2)
	}
	// 第三级：约定只剩条数
	s.maxInjectBytes = len(l2) - 1
	l3 := renderPrompt(pl, cs, nil, now, s)
	if !strings.Contains(l3, "共 8 条，可用 list_commitments 查看。") || strings.Contains(l3, "08-2") {
		t.Fatalf("第三级约定只剩条数：\n%s", l3)
	}
	for _, out := range []string{l1, l2, l3} {
		if !strings.Contains(out, "此刻：写稿（14:00-16:30，尽量守），已进行 1 小时。") || !strings.Contains(out, "之后：18:30 和对方吃晚饭（不能动）。") {
			t.Fatalf("进行中与下一项永不丢：\n%s", out)
		}
	}
}

func TestSystemPromptStable(t *testing.T) {
	h := newHarness(t, false, nil)
	if h.p.SystemPrompt() != h.p.SystemPrompt() || !strings.HasPrefix(h.p.SystemPrompt(), "[日程]") {
		t.Fatal("SystemPrompt 应逐字节稳定")
	}
}

// ---------- 规划触发 ----------

func (h *harness) turnEnd(origin string) {
	h.p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{SessionID: "s1", Origin: origin, EndedAt: h.clk.now()})
}

func (h *harness) waitPlanning() {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.p.mu.RLock()
		busy := h.p.planning[""]
		h.p.mu.RUnlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPlanTriggeredByHeartbeatTurn(t *testing.T) {
	h := newHarness(t, true, nil)
	h.call(t, "add_commitment", `{"date":"2026-08-21","start":"18:30","end":"20:00","title":"和对方吃晚饭","with_user":true}`)
	var ephemeral atomic.Bool
	h.runTurn = func(ctx context.Context, sid, input string) (string, error) {
		ephemeral.Store(plugin.IsEphemeralInput(ctx))
		_, err := h.tool("set_day_plan").Execute(ctx, json.RawMessage(`{"items":[{"title":"和对方吃晚饭","start":"18:30","end":"20:00","flex":"不能动","from_commitment":"c1"}]}`))
		return "", err
	}
	h.turnEnd("heartbeat")
	in := recvInput(t, h)
	if !strings.HasPrefix(in, "【规划今天】现在是 周五 08-21 07:32") || !strings.Contains(in, "- [c1] 18:30-20:00 和对方吃晚饭（不能动）") {
		t.Fatalf("规划输入不符：\n%s", in)
	}
	h.waitPlanning()
	if !ephemeral.Load() {
		t.Fatal("规划输入应标记为一次性")
	}
	if h.plan(t).Date != "2026-08-21" || h.plan(t).Attempts.Count != 0 {
		t.Fatalf("排成后不该计失败: %+v", h.plan(t))
	}
	// 已排定 / 自己的轮次都不再触发
	h.turnEnd("")
	h.turnEnd("agenda")
	h.waitPlanning()
	if h.calls.Load() != 1 {
		t.Fatalf("不该再触发，调用了 %d 次", h.calls.Load())
	}
}

func TestPlanAttemptsLimit(t *testing.T) {
	h := newHarness(t, true, nil)
	h.turnEnd("") // 模型没提交表
	h.waitPlanning()
	waitFor(t, "第一次尝试记账", func() bool { return h.plan(t).Attempts.Count == 1 })
	h.turnEnd("")
	h.waitPlanning()
	waitFor(t, "第二次尝试记账", func() bool { return h.plan(t).Attempts.Count == 2 })
	select {
	case n := <-h.notices:
		if !strings.Contains(n, "今天的日程没有排成") {
			t.Fatalf("注记措辞不符: %s", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("两次失败后应留注记")
	}
	h.turnEnd("")
	h.waitPlanning()
	if h.calls.Load() != 2 {
		t.Fatalf("两次失败后不再尝试，调用了 %d 次", h.calls.Load())
	}
	// 到了第二天又可以试
	h.clk.set(at(5, 0).AddDate(0, 0, 1))
	h.turnEnd("")
	h.waitPlanning()
	waitFor(t, "第二天再试", func() bool { return h.calls.Load() == 3 })
}

func TestPlanBusyRetry(t *testing.T) {
	h := newHarness(t, true, nil)
	var n atomic.Int32
	h.runTurn = func(ctx context.Context, sid, input string) (string, error) {
		if n.Add(1) < 3 {
			return "", plugin.ErrSessionBusy
		}
		h.setPlan(t, Item{Title: "x", Start: "10:00", End: "11:00"})
		return "", nil
	}
	h.turnEnd("")
	waitFor(t, "忙重试", func() bool { return h.calls.Load() == 3 })
	h.waitPlanning()
	if h.plan(t).Attempts.Count != 0 {
		t.Fatal("忙重试不计失败")
	}
}

func TestPlanPrunesPastCommitments(t *testing.T) {
	h := newHarness(t, true, nil)
	h.clk.set(at(10, 0).AddDate(0, 0, -2))
	h.call(t, "add_commitment", `{"date":"2026-08-19","start":"10:00","title":"老陈那边取包裹"}`)
	h.clk.set(at(7, 32))
	h.turnEnd("")
	h.waitPlanning()
	select {
	case n := <-h.notices:
		if n != "8 月 19 日（周三）的约定「老陈那边取包裹」没有排进当天，已丢弃。" {
			t.Fatalf("注记措辞不符: %s", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("没排进当天的过期约定应留注记")
	}
	if len(h.commitments(t)) != 0 {
		t.Fatal("过期约定应清掉")
	}
}

// ---------- 时钟 ----------

func (h *harness) tick() {
	h.p.tick(context.Background())
	h.p.wg.Wait()
}

func TestTickStartOnTimeAndLate(t *testing.T) {
	h := newHarness(t, false, nil)
	h.setPlan(t, Item{Title: "和林舟在图书馆查资料", Start: "14:00", End: "16:30"})
	h.clk.set(at(13, 0))
	h.tick()
	if h.calls.Load() != 0 {
		t.Fatal("没到点不该派发")
	}
	var firedWhenRun atomic.Bool
	h.runTurn = func(ctx context.Context, sid, input string) (string, error) {
		pl := h.plan(t)
		firedWhenRun.Store(!pl.Items[0].StartFired.IsZero())
		return "", nil
	}
	h.clk.set(at(14, 2))
	h.tick()
	in := recvInput(t, h)
	if !strings.HasPrefix(in, "【日程·开始】现在 14:02，到了「和林舟在图书馆查资料」（14:00-16:30，尽量守）的时间。\n看看") {
		t.Fatalf("开始输入不符：\n%s", in)
	}
	if !firedWhenRun.Load() {
		t.Fatal("派发前应先落盘 start_fired")
	}
	h.tick()
	if h.calls.Load() != 1 {
		t.Fatal("已派发的不重发")
	}

	// 超过宽限：注明晚了多久
	h2 := newHarness(t, false, nil)
	h2.setPlan(t, Item{Title: "晨跑", Start: "08:00", End: "08:40"})
	h2.clk.set(at(8, 20))
	h2.tick()
	if in := recvInput(t, h2); !strings.Contains(in, "已经晚了 20 分钟，先决定还去不去。") {
		t.Fatalf("超过宽限应注明：\n%s", in)
	}
}

func TestTickEndAndCatchUp(t *testing.T) {
	h := newHarness(t, false, nil)
	h.setPlan(t, Item{Title: "和林舟在图书馆查资料", Start: "14:00", End: "16:30", Status: statusOngoing, StartFired: at(14, 0)})
	h.runTurn = func(ctx context.Context, sid, input string) (string, error) {
		_, err := h.tool("update_day_plan").Execute(ctx, json.RawMessage(`{"id":"a1","status":"done","outcome":"聊了一下午"}`))
		return "", err
	}
	h.clk.set(at(16, 35))
	h.tick()
	if in := recvInput(t, h); !strings.HasPrefix(in, "【日程·结束】现在 16:35，「和林舟在图书馆查资料」（14:00-16:30）该结束了。") {
		t.Fatalf("结束输入不符：\n%s", in)
	}
	cues := cue.Take(h.clk.now())
	if len(cues) != 1 || cues[0].Key != "back|a1" || cues[0].Text != "你刚结束「和林舟在图书馆查资料」（16:30 回来）：聊了一下午。" ||
		!cues[0].Expire.Equal(at(16, 35).Add(90*time.Minute)) {
		t.Fatalf("结束后应投「刚回来」理由: %+v", cues)
	}

	// 整项都过去了：跳过开始，直接补记
	h2 := newHarness(t, false, nil)
	h2.setPlan(t, Item{Title: "晨跑", Start: "08:00", End: "08:40"})
	h2.clk.set(at(9, 0))
	h2.tick()
	if in := recvInput(t, h2); !strings.Contains(in, "在你离线期间已经过去，补记一句经历即可") {
		t.Fatalf("补记输入不符：\n%s", in)
	}
	if it := h2.plan(t).Items[0]; it.StartFired.IsZero() || it.EndFired.IsZero() {
		t.Fatal("补记应同时落两个派发记录")
	}
	if h2.calls.Load() != 1 {
		t.Fatal("补记只派一次")
	}
}

func TestTickIgnoresYesterdayAndGrace(t *testing.T) {
	h := newHarness(t, false, nil)
	day := at(0, 0).AddDate(0, 0, -1)
	_ = h.p.storeFor("").SavePlan(Plan{Date: day.Format(dateLayout), Items: []Item{{ID: "a1", Title: "x", Start: "10:00", End: "11:00", Status: statusPlanned, Flex: flexTry, Busy: defaultBusy}}})
	h.clk.set(at(12, 0))
	h.tick()
	if h.calls.Load() != 0 {
		t.Fatal("昨天的表不派任何轮次")
	}

	h2 := newHarness(t, false, nil)
	h2.p.mu.Lock()
	h2.p.startupGrace = 2 * time.Minute
	h2.p.startedAt = at(9, 59)
	h2.p.mu.Unlock()
	h2.setPlan(t, Item{Title: "x", Start: "10:00", End: "11:00"})
	h2.clk.set(at(10, 0))
	h2.tick()
	if h2.calls.Load() != 0 {
		t.Fatal("启动宽限内不派发")
	}
	if w := h2.p.untilNext(); w != time.Minute {
		t.Fatalf("宽限内下一次等待应到宽限结束，得到 %v", w)
	}
	h2.clk.set(at(10, 1))
	h2.tick()
	if h2.calls.Load() != 1 {
		t.Fatal("宽限期满应补发")
	}
}

func TestSoonCue(t *testing.T) {
	h := newHarness(t, false, nil)
	h.setPlan(t, Item{Title: "和对方吃晚饭", Start: "18:30", End: "20:00", WithUser: true, Flex: flexFixed})
	h.clk.set(at(17, 0))
	h.tick()
	if cue.Pending(h.clk.now()) {
		t.Fatal("提前 90 分钟不该投")
	}
	h.clk.set(at(17, 40))
	h.tick()
	cues := cue.Take(h.clk.now())
	if len(cues) != 1 || cues[0].Key != "soon|a1" || cues[0].Text != "再过 50 分钟是和对方的约定：和对方吃晚饭（18:30）。" || !cues[0].Expire.Equal(at(18, 30)) {
		t.Fatalf("提前提醒不符: %+v", cues)
	}
	h.tick()
	if cue.Pending(h.clk.now()) {
		t.Fatal("同一项不重投")
	}
	if h.calls.Load() != 0 {
		t.Fatal("提醒不是轮次")
	}
}

func TestBusyRetryExhaustedLeavesNotice(t *testing.T) {
	h := newHarness(t, false, nil)
	h.setPlan(t, Item{Title: "晨跑", Start: "08:00", End: "08:40"})
	h.runTurn = func(context.Context, string, string) (string, error) { return "", plugin.ErrSessionBusy }
	h.clk.set(at(8, 1))
	h.tick()
	if h.calls.Load() != defBusyRetryMax {
		t.Fatalf("应重试 %d 次，实际 %d", defBusyRetryMax, h.calls.Load())
	}
	select {
	case n := <-h.notices:
		if n != "「晨跑」的开始轮次因会话持续繁忙而放弃。" {
			t.Fatalf("注记措辞不符: %s", n)
		}
	default:
		t.Fatal("放弃时应留注记")
	}
}

func TestNoSessionNoTurn(t *testing.T) {
	h := newHarness(t, false, nil)
	h.p.mu.Lock()
	h.p.sessions = fakeSessions{}
	h.p.mu.Unlock()
	h.setPlan(t, Item{Title: "晨跑", Start: "08:00", End: "08:40"})
	h.clk.set(at(8, 1))
	h.tick()
	if h.calls.Load() != 0 {
		t.Fatal("没有会话时不跑轮次、不新建会话")
	}
}

func TestRunActivitiesOff(t *testing.T) {
	h := newHarness(t, false, map[string]any{"run_activities": false})
	h.setPlan(t, Item{Title: "晨跑", Start: "08:00", End: "08:40"})
	h.clk.set(at(8, 1))
	h.tick()
	if h.calls.Load() != 0 || !h.plan(t).Items[0].StartFired.IsZero() {
		t.Fatal("关掉自动轮次时既不派发也不标记")
	}
}

func TestRebuildAvailabilityOnInit(t *testing.T) {
	h := newHarness(t, false, nil)
	h.setPlan(t, Item{Title: "写稿", Start: "14:00", End: "16:30", Status: statusOngoing, Busy: "不回"})
	h.clk.set(at(15, 0))
	if err := h.p.Init(plugin.InitContext{StateDir: filepath.Dir(h.p.base), Sessions: h.sessions}, nil); err != nil {
		t.Fatal(err)
	}
	h.p.Stop()
	st, ok := availability.Current(h.clk.now())
	if !ok || st.Activity != "写稿" || st.Level != availability.Away {
		t.Fatalf("重启后应从进行中的项重建忙碌状态: %+v ok=%v", st, ok)
	}
	// 到点后时钟顺带撤掉
	h.clk.set(at(16, 31))
	h.tick()
	if _, ok := availability.Current(h.clk.now()); ok {
		t.Fatal("项的 end 过去后应撤忙碌状态")
	}
}

func TestStopExitsPromptly(t *testing.T) {
	h := newHarness(t, true, nil)
	started := make(chan struct{})
	h.runTurn = func(ctx context.Context, sid, input string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	h.turnEnd("")
	<-started
	done := make(chan struct{})
	go func() { h.p.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 应让后台轮次随 ctx 取消而退出")
	}
	h.p.Stop() // 幂等
	h.turnEnd("")
	if h.calls.Load() != 1 {
		t.Fatal("停止后不再起规划轮次")
	}
}

func TestInitReentrant(t *testing.T) {
	h := newHarness(t, true, nil)
	ictx := plugin.InitContext{StateDir: t.TempDir(), Sessions: h.sessions, RunTurn: func(context.Context, string, string) (string, error) { return "", nil }}
	if err := h.p.Init(ictx, map[string]any{"max_items": 9}); err != nil {
		t.Fatal(err)
	}
	if h.p.snapshot().maxItems != hardMaxItems {
		t.Fatal("max_items 不得超过硬上限")
	}
	h.p.Stop()
	h.p.Stop()
}

func TestStatusLines(t *testing.T) {
	h := newHarness(t, false, nil)
	if got := h.p.StatusLines(""); len(got) != 1 || got[0] != "📅 日程：今天尚未排表" {
		t.Fatalf("未排状态不符: %v", got)
	}
	h.call(t, "set_day_plan", planArgs)
	h.clk.set(at(7, 45))
	h.call(t, "update_day_plan", `{"id":"a1","status":"done","outcome":"跑完了"}`)
	h.clk.set(at(14, 2))
	h.call(t, "update_day_plan", `{"id":"a2","status":"ongoing"}`)
	if got := h.p.StatusLines("")[0]; got != "📅 日程：此刻 和林舟在图书馆查资料（到 16:30）；今天 3 项，已完成 1" {
		t.Fatalf("进行中状态不符: %s", got)
	}
	h.clk.set(at(16, 35))
	h.call(t, "update_day_plan", `{"id":"a2","status":"done","outcome":"聊了一下午"}`)
	if got := h.p.StatusLines("")[0]; got != "📅 日程：空闲，下一项 18:30；今天 3 项，已完成 2" {
		t.Fatalf("空闲状态不符: %s", got)
	}
}

func TestScopedStores(t *testing.T) {
	h := newHarness(t, false, nil)
	inner := plugin.WithScope(context.Background(), plugin.Scope{Write: "inner", Read: []string{"inner"}})
	if _, err := h.tool("set_day_plan").Execute(inner, json.RawMessage(planArgs)); err != nil {
		t.Fatal(err)
	}
	if h.plan(t).Date != "" {
		t.Fatal("里人格的表不该写进共享域")
	}
	out, _ := h.p.TurnPrompt(context.Background(), plugin.TurnEvent{})
	if !strings.Contains(out, "尚未排定") {
		t.Fatal("共享域看不到里人格的表")
	}
	// 后台轮次落在别的域：按 id 在可读域里找得到就写回它所在的域
	if _, err := h.tool("update_day_plan").Execute(inner, json.RawMessage(`{"id":"a1","status":"ongoing"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.tool("update_day_plan").Execute(context.Background(), json.RawMessage(`{"id":"a1","status":"done","outcome":"x"}`)); err != nil {
		t.Fatalf("不限制可读域时应找得到: %v", err)
	}
	outer := plugin.WithScope(context.Background(), plugin.Scope{Write: "outer", Read: []string{"outer"}})
	if _, err := h.tool("update_day_plan").Execute(outer, json.RawMessage(`{"id":"a2","status":"ongoing"}`)); err == nil || !strings.Contains(err.Error(), "没有 id 为") {
		t.Fatalf("不可读域的项应报没有这一项: %v", err)
	}
	if got := h.p.StatusLines("")[0]; got != "📅 日程：今天 3 项，已完成 1（按人格分开的表）" {
		t.Fatalf("只有分域的表时状态只报条数: %s", got)
	}
}

func TestConcurrentUpdatesSerialized(t *testing.T) {
	h := newHarness(t, false, nil)
	h.setPlan(t, Item{Title: "x", Start: "10:00", End: "11:00"})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = h.p.storeFor("").UpdatePlan(func(pl *Plan) (bool, error) {
				pl.Attempts.Count++
				return true, nil
			})
		}()
	}
	wg.Wait()
	if h.plan(t).Attempts.Count != 20 {
		t.Fatalf("并发写应串行不丢，得到 %d", h.plan(t).Attempts.Count)
	}
}

func TestBrokenFileReported(t *testing.T) {
	h := newHarness(t, false, nil)
	st := h.p.storeFor("")
	_ = st.writeJSON(st.planPath(), "not a plan")
	if _, err := st.LoadPlan(); err == nil || !strings.Contains(err.Error(), "损坏") {
		t.Fatalf("损坏的文件应报错: %v", err)
	}
}

// 结束轮次挂了，但经历已经落盘：「刚回来」这条理由照投。
//
// 实际见过的场景是模型标完 done 之后那次模型调用超时——经历在表里，人却不知道她
// 回来了，因为这条理由当初只挂在「轮次成功」那一支上。
func TestBackCueAfterFailedEndTurn(t *testing.T) {
	h := newHarness(t, false, nil)
	h.setPlan(t, Item{Title: "和林舟在图书馆查资料", Start: "14:00", End: "16:30",
		Status: statusOngoing, StartFired: at(14, 0)})
	h.runTurn = func(ctx context.Context, sid, input string) (string, error) {
		// 先把经历写进表里，再让这一轮以网络错误告终
		if _, err := h.tool("update_day_plan").Execute(ctx,
			json.RawMessage(`{"id":"a1","status":"done","outcome":"聊了一下午"}`)); err != nil {
			return "", err
		}
		return "", errors.New("http2: timeout awaiting response headers")
	}
	h.clk.set(at(16, 35))
	h.tick()
	recvInput(t, h)

	cues := cue.Take(h.clk.now())
	if len(cues) != 1 || cues[0].Key != "back|a1" ||
		cues[0].Text != "你刚结束「和林舟在图书馆查资料」（16:30 回来）：聊了一下午。" {
		t.Fatalf("轮次失败也该投「刚回来」: %+v", cues)
	}
}

// 会话持续繁忙而放弃的那条路不投：轮次根本没跑起来，模型也就没机会记下经历，
// 而且那时候人正在对面说话，不需要一条「找个话头开口」的理由。
func TestNoBackCueWhenSessionBusy(t *testing.T) {
	h := newHarness(t, false, nil)
	h.setPlan(t, Item{Title: "晨跑", Start: "08:00", End: "08:40", Status: statusOngoing, StartFired: at(8, 0)})
	h.runTurn = func(context.Context, string, string) (string, error) { return "", plugin.ErrSessionBusy }
	h.clk.set(at(8, 45))
	h.tick()
	<-h.notices // 放弃时留的那条注记
	if cue.Pending(h.clk.now()) {
		t.Fatal("会话繁忙放弃时不该投「刚回来」")
	}
}

// ---------- 答应过的事 ----------

func (h *harness) promises(t *testing.T) []Promise {
	t.Helper()
	ps, err := h.p.storeFor("").LoadPromises()
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

func (h *harness) turnPrompt(t *testing.T) string {
	t.Helper()
	out, err := h.p.TurnPrompt(context.Background(), plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func dateOf(d time.Time) string { return d.Format(dateLayout) }

// 这是整件事的起点：随口答应的一句话此前无处可去，只能以对话文本活着，
// 于是每晚被重说一遍、措辞永远是「明天」。记下来之后它有了日期与状态。
func TestPromiseRecordedAndInjected(t *testing.T) {
	h := newHarness(t, false, nil)
	tomorrow := dateOf(at(0, 0).AddDate(0, 0, 1))
	out := h.call(t, "add_promise",
		`{"title":"给对方带两个菜","date":"`+tomorrow+`","by":"self","note":"红烧肉和汤"}`)
	if !strings.Contains(out, "p1") {
		t.Fatalf("回执应带编号: %s", out)
	}
	ps := h.promises(t)
	if len(ps) != 1 || ps[0].Status != promisePending || ps[0].By != promiseBySelf || ps[0].Date != tomorrow {
		t.Fatalf("落盘不符: %+v", ps)
	}
	inj := h.turnPrompt(t)
	if !strings.Contains(inj, promiseHeader) || !strings.Contains(inj, "你答应：给对方带两个菜") {
		t.Fatalf("注入里应有这条: %s", inj)
	}
	if !strings.Contains(inj, "红烧肉和汤") {
		t.Fatalf("0 档应带备注: %s", inj)
	}
}

// 了结之后就不该再出现——这正是「已经带过的菜第二天又被提起」要防的。
func TestSettledPromiseLeavesInjection(t *testing.T) {
	h := newHarness(t, false, nil)
	today := dateOf(at(0, 0))
	h.call(t, "add_promise", `{"title":"给对方带两个菜","date":"`+today+`","by":"self"}`)
	if inj := h.turnPrompt(t); !strings.Contains(inj, "带两个菜") {
		t.Fatalf("了结前应在注入里: %s", inj)
	}
	out := h.call(t, "settle_promise", `{"id":"p1","result":"done","outcome":"早上带走了"}`)
	if !strings.Contains(out, "做到了") {
		t.Fatalf("回执不符: %s", out)
	}
	if inj := h.turnPrompt(t); strings.Contains(inj, "带两个菜") || strings.Contains(inj, promiseHeader) {
		t.Fatalf("了结后不该再注入: %s", inj)
	}
	// 但账还在，回头能查到
	if listed := h.call(t, "list_promises", `{"include_settled":true}`); !strings.Contains(listed, "早上带走了") {
		t.Fatalf("已了结的应能查到: %s", listed)
	}
	if listed := h.call(t, "list_promises", `{}`); !strings.Contains(listed, "没有答应过的事") {
		t.Fatalf("默认只列没了结的: %s", listed)
	}
}

// 重复了结不报错但也不覆盖：与「做成了的事不重复做」同一条约定。
func TestSettleTwiceIsNoop(t *testing.T) {
	h := newHarness(t, false, nil)
	today := dateOf(at(0, 0))
	h.call(t, "add_promise", `{"title":"还书","date":"`+today+`","by":"user"}`)
	h.call(t, "settle_promise", `{"id":"p1","result":"done","outcome":"还了"}`)
	out := h.call(t, "settle_promise", `{"id":"p1","result":"missed","outcome":"改口"}`)
	if !strings.Contains(out, "早就了结过了") {
		t.Fatalf("重复了结应当明说: %s", out)
	}
	if ps := h.promises(t); ps[0].Status != promiseDone || ps[0].Outcome != "还了" {
		t.Fatalf("重复了结不该覆盖: %+v", ps[0])
	}
}

// 两个方向必须在字面上分得开，否则模型会去催对方做它自己答应的事。
func TestPromiseDirectionIsExplicit(t *testing.T) {
	h := newHarness(t, false, nil)
	today := dateOf(at(0, 0))
	h.call(t, "add_promise", `{"title":"带菜","date":"`+today+`","by":"self"}`)
	h.call(t, "add_promise", `{"title":"还书","date":"`+today+`","by":"user"}`)
	inj := h.turnPrompt(t)
	if !strings.Contains(inj, "你答应：带菜") || !strings.Contains(inj, "对方答应：还书") {
		t.Fatalf("方向应当写明: %s", inj)
	}
	if err := h.fail(t, "add_promise", `{"title":"x","date":"`+today+`","by":"他"}`); !strings.Contains(err.Error(), "by 只能是") {
		t.Fatalf("方向必须给准: %v", err)
	}
}

// 过期不兑现的会自动收束成「没做成」并停止注入：一条永远挂着的待办，
// 就是换了个载体的同一种病。
func TestOverduePromiseSweptToMissed(t *testing.T) {
	h := newHarness(t, false, nil)
	today := at(0, 0)
	h.call(t, "add_promise", `{"title":"带菜","date":"`+dateOf(today)+`","by":"self"}`)

	// 到期当天与次日都还在（宽限一天：对方可能晚上才兑现）
	h.tick()
	if ps := h.promises(t); ps[0].Status != promisePending {
		t.Fatalf("到期当天不该收束: %+v", ps[0])
	}
	h.clk.set(today.AddDate(0, 0, 1).Add(10 * time.Hour))
	h.tick()
	if ps := h.promises(t); ps[0].Status != promisePending {
		t.Fatalf("宽限期内不该收束: %+v", ps[0])
	}
	if inj := h.turnPrompt(t); !strings.Contains(inj, "到日子了还没了结") {
		t.Fatalf("过了日子应当标出来: %s", inj)
	}

	// 再过一天：收束成没做成，留注记，且不再注入
	h.clk.set(today.AddDate(0, 0, 2).Add(10 * time.Hour))
	h.tick()
	ps := h.promises(t)
	if ps[0].Status != promiseMissed || ps[0].Settled.IsZero() {
		t.Fatalf("过了宽限应收束成没做成: %+v", ps[0])
	}
	select {
	case n := <-h.notices:
		if !strings.Contains(n, "没做成") || !strings.Contains(n, "带菜") {
			t.Errorf("注记内容不符: %s", n)
		}
	default:
		t.Error("收束时应留一条注记")
	}
	if inj := h.turnPrompt(t); strings.Contains(inj, "带菜") {
		t.Fatalf("收束后不该再注入: %s", inj)
	}
	// 幂等：再扫一次不再产生注记
	h.tick()
	select {
	case n := <-h.notices:
		t.Errorf("收束应当幂等，不该再留注记: %s", n)
	default:
	}
}

// 注入体量必须有硬上限，且超出时保住「还有几条」这个存在性信息。
func TestPromiseInjectionIsBounded(t *testing.T) {
	h := newHarness(t, false, nil)
	today := dateOf(at(0, 0))
	for i := 0; i < maxPromiseShown+3; i++ {
		h.call(t, "add_promise",
			fmt.Sprintf(`{"title":"第 %d 件","date":"%s","by":"self"}`, i, today))
	}
	inj := h.turnPrompt(t)
	if n := strings.Count(inj, "你答应："); n != maxPromiseShown {
		t.Errorf("注入条数应封顶在 %d，实际 %d", maxPromiseShown, n)
	}
	if !strings.Contains(inj, "另有 3 条") {
		t.Errorf("超出的条数要报出来: %s", inj)
	}
}

// 只能记今天或以后要兑现的事：过去的日子记进来立刻就是过期状态，没有意义。
func TestPromiseRejectsPastDate(t *testing.T) {
	h := newHarness(t, false, nil)
	past := dateOf(at(0, 0).AddDate(0, 0, -1))
	if err := h.fail(t, "add_promise", `{"title":"x","date":"`+past+`","by":"self"}`); !strings.Contains(err.Error(), "已经过去") {
		t.Fatalf("过去的日期应当拒绝: %v", err)
	}
}
