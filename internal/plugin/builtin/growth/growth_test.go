package growth

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"wen/internal/plugin"
)

// clock 是一只可拨的钟，测试用它跨过逻辑日。
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestPlugin(t *testing.T, cfg map[string]any, run plugin.RunTurnFunc, notice plugin.NoticeFunc) (*Plugin, *clock) {
	t.Helper()
	p := New()
	c := &clock{t: time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)}
	p.now = c.now
	p.settle, p.busyRetry = 0, 0
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir(), RunTurn: run, Notice: notice}, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	return p, c
}

// reflecting 是反思轮次里的 ctx：发起方是 growth，可见域按 tag。
func reflecting(tag string) context.Context {
	ctx := plugin.WithTurnOrigin(context.Background(), "growth")
	return plugin.WithScope(ctx, plugin.Scope{Write: tag})
}

// claimed 模拟一轮反思：发起方是 growth、可见域按 tag，并像真实轮次那样在裁决之后
// 经 TurnPrompt 认领这个域——不认领的话两个改动工具一律拒绝。
func claimed(t *testing.T, p *Plugin, tag string) context.Context {
	t.Helper()
	ctx := reflecting(tag)
	if _, err := p.TurnPrompt(ctx, plugin.TurnEvent{SessionID: "s-" + tag}); err != nil {
		t.Fatal(err)
	}
	return ctx
}

func call(t *testing.T, tool plugin.Tool, ctx context.Context, args string) (string, error) {
	t.Helper()
	return tool.Execute(ctx, json.RawMessage(args))
}

func load(t *testing.T, p *Plugin, tag string) State {
	t.Helper()
	st, err := p.storeFor(tag).Load()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestToolsOnlyInReflection(t *testing.T) {
	p, _ := newTestPlugin(t, nil, nil, nil)
	for _, tool := range p.Tools() {
		_, err := call(t, tool, context.Background(), `{"action":"add","text":"喝手冲","basis":"昨天","why":"本来就爱咖啡"}`)
		if err == nil || !strings.Contains(err.Error(), "反思") {
			t.Errorf("%s 在平常轮次里应被拒绝：%v", tool.Name(), err)
		}
	}
	// 别的插件发起的后台轮次同样不行
	ctx := plugin.WithTurnOrigin(context.Background(), "heartbeat")
	if _, err := call(t, p.Tools()[0], ctx, `{"action":"add","text":"x","basis":"y","why":"z"}`); err == nil {
		t.Error("心跳轮次里也不该能改")
	}
}

func TestDetailAddModifyLetGoWithQuota(t *testing.T) {
	p, _ := newTestPlugin(t, map[string]any{"daily_changes": 3}, nil, nil)
	ctx := claimed(t, p, "")
	grow := p.Tools()[0]

	out, err := call(t, grow, ctx, `{"action":"add","text":"早上会冲一杯手冲","basis":"10-19 买了手冲壶","why":"本来就离不开咖啡"}`)
	if err != nil || !strings.Contains(out, "d1") {
		t.Fatalf("add: %q %v", out, err)
	}
	if _, err := call(t, grow, ctx, `{"action":"modify","id":"d1","text":"早上会冲两杯手冲","basis":"一杯不够","why":"还是爱咖啡"}`); err != nil {
		t.Fatal(err)
	}
	st := load(t, p, "")
	if len(st.Details) != 1 || st.Details[0].Text != "早上会冲两杯手冲" || st.Quota.Used != 2 {
		t.Fatalf("modify 后：%+v", st)
	}
	if _, err := call(t, grow, ctx, `{"action":"let_go","id":"d1","basis":"腻了","why":"口味会变"}`); err != nil {
		t.Fatal(err)
	}
	// 第四次超过限额
	_, err = call(t, grow, ctx, `{"action":"add","text":"爱散步","basis":"x","why":"y"}`)
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("超过每日限额应被拒绝：%v", err)
	}
	st = load(t, p, "")
	if len(st.Details) != 0 || len(st.History) != 3 {
		t.Errorf("放下后：%+v", st)
	}
	lines, _ := p.storeFor("").ReadLog(10)
	if len(lines) != 3 || lines[1].Prev != "早上会冲一杯手冲" {
		t.Errorf("成长记录：%+v", lines)
	}
}

func TestDetailRequiresWhyAndRejectsLongText(t *testing.T) {
	p, _ := newTestPlugin(t, nil, nil, nil)
	ctx := claimed(t, p, "")
	grow := p.Tools()[0]
	if _, err := call(t, grow, ctx, `{"action":"add","text":"x","basis":"y","why":" "}`); err == nil {
		t.Error("说不清为什么这还是自己的，不该记")
	}
	long := strings.Repeat("长", maxTextRunes+1)
	if _, err := call(t, grow, ctx, `{"action":"add","text":"`+long+`","basis":"y","why":"z"}`); err == nil || !strings.Contains(err.Error(), "太长") {
		t.Errorf("超长应报错而不是截断：%v", err)
	}
}

func TestDetailsCapForcesReplace(t *testing.T) {
	p, c := newTestPlugin(t, map[string]any{"max_details": 2, "daily_changes": 10}, nil, nil)
	ctx := claimed(t, p, "")
	grow := p.Tools()[0]
	for _, txt := range []string{"a", "b"} {
		if _, err := call(t, grow, ctx, `{"action":"add","text":"`+txt+`","basis":"x","why":"y"}`); err != nil {
			t.Fatal(err)
		}
	}
	c.add(24 * time.Hour) // 换一天，限额不是这里要测的
	_, err := call(t, grow, ctx, `{"action":"add","text":"c","basis":"x","why":"y"}`)
	if err == nil || !strings.Contains(err.Error(), "let_go") {
		t.Errorf("满了应提示先放下或替换：%v", err)
	}
}

func TestProposeOneAtATimeThenApproveAndUndo(t *testing.T) {
	p, _ := newTestPlugin(t, nil, nil, nil)
	ctx := claimed(t, p, "")
	propose := p.Tools()[1]
	if _, err := call(t, propose, ctx, `{"text":"说话比以前直了","basis":"这几天都直说","why":"本来就嘴硬心软"}`); err != nil {
		t.Fatal(err)
	}
	_, err := call(t, propose, ctx, `{"text":"更黏人了","basis":"x","why":"y"}`)
	if err == nil || !strings.Contains(err.Error(), "说话比以前直了") {
		t.Fatalf("已有一条在等时应拒绝并说出是哪条：%v", err)
	}
	// 待批准的那条不注入：没批准就不算数
	if got, _ := p.TurnPrompt(plugin.WithScope(context.Background(), plugin.Scope{}), plugin.TurnEvent{}); got != "" {
		t.Errorf("待批准的不该注入：%q", got)
	}
	if acts := p.Actions(); !strings.Contains(acts[0].Detail, "说话比以前直了") {
		t.Errorf("批准按钮的说明应接着那条提议：%+v", acts[0])
	}

	if err := p.StartAction(context.Background(), actionApprove); err != nil {
		t.Fatal(err)
	}
	st := load(t, p, "")
	if st.Pending != nil || len(st.Core) != 1 {
		t.Fatalf("批准后：%+v", st)
	}
	got, _ := p.TurnPrompt(plugin.WithScope(context.Background(), plugin.Scope{}), plugin.TurnEvent{})
	if !strings.Contains(got, "[这些日子的变化]") || !strings.Contains(got, "说话比以前直了") {
		t.Errorf("批准后应注入：%q", got)
	}
	if lines := p.StatusLines(""); len(lines) != 1 || !strings.Contains(lines[0], "大变化 1 条") {
		t.Errorf("状态行：%v", lines)
	}

	if err := p.StartAction(context.Background(), actionUndo); err != nil {
		t.Fatal(err)
	}
	if st := load(t, p, ""); len(st.Core) != 0 || st.Pending != nil {
		t.Errorf("撤销批准后既不在已定的里、也不回到待批准：%+v", st)
	}
}

func TestRejectAndUndoDetail(t *testing.T) {
	p, _ := newTestPlugin(t, nil, nil, nil)
	ctx := claimed(t, p, "")
	_, _ = call(t, p.Tools()[1], ctx, `{"text":"变得爱热闹","basis":"x","why":"y"}`)
	p.Actions() // 设置页先把列表拉出来，用户看到了这一条
	if err := p.StartAction(context.Background(), actionReject); err != nil {
		t.Fatal(err)
	}
	if st := load(t, p, ""); st.Pending != nil || len(st.Core) != 0 {
		t.Errorf("驳回后：%+v", st)
	}
	_, _ = call(t, p.Tools()[0], ctx, `{"action":"add","text":"爱吃辣","basis":"x","why":"y"}`)
	_, _ = call(t, p.Tools()[0], ctx, `{"action":"modify","id":"d2","text":"很爱吃辣","basis":"x","why":"y"}`)
	if err := p.StartAction(context.Background(), actionUndo); err != nil {
		t.Fatal(err)
	}
	if st := load(t, p, ""); len(st.Details) != 1 || st.Details[0].Text != "爱吃辣" {
		t.Errorf("撤销修改应还原成改之前：%+v", st.Details)
	}
	state, _ := p.ActionState(actionUndo)
	if state.Status != plugin.ActionDone {
		t.Errorf("操作状态：%+v", state)
	}
	if err := p.StartAction(context.Background(), actionLog); err != nil {
		t.Fatal(err)
	}
	if state, _ := p.ActionState(actionLog); !strings.Contains(state.Message, "驳回大变化") || !strings.Contains(state.Message, "撤销") {
		t.Errorf("成长记录应列出驳回与撤销：%q", state.Message)
	}
}

func TestScopesGrowSeparately(t *testing.T) {
	p, _ := newTestPlugin(t, nil, nil, nil)
	_, _ = call(t, p.Tools()[0], claimed(t, p, "inner"), `{"action":"add","text":"夜里爱听雨","basis":"x","why":"y"}`)
	outer, _ := p.TurnPrompt(plugin.WithScope(context.Background(), plugin.Scope{}), plugin.TurnEvent{})
	inner, _ := p.TurnPrompt(plugin.WithScope(context.Background(), plugin.Scope{Write: "inner"}), plugin.TurnEvent{})
	if outer != "" || !strings.Contains(inner, "夜里爱听雨") {
		t.Errorf("成长属于人格：outer=%q inner=%q", outer, inner)
	}
	if err := p.StartAction(context.Background(), actionClear); err != nil {
		t.Fatal(err)
	}
	if inner, _ := p.TurnPrompt(plugin.WithScope(context.Background(), plugin.Scope{Write: "inner"}), plugin.TurnEvent{}); inner != "" {
		t.Errorf("清空应覆盖各人格：%q", inner)
	}
}

func TestRenderBlockDegrades(t *testing.T) {
	st := State{Core: []Entry{{Text: "说话直了"}}}
	for i := range 10 {
		st.Details = append(st.Details, Entry{Text: strings.Repeat("细", 20) + string(rune('a'+i)), Since: time.Now()})
	}
	full := renderBlock("", st, 1<<20)
	if !strings.Contains(full, "起）") {
		t.Fatalf("预算够时带日期：%q", full)
	}
	small := renderBlock("", st, len(full)/2)
	if len(small) > len(full)/2 || !strings.Contains(small, "说话直了") || !strings.Contains(small, "另有") {
		t.Errorf("预算紧时保住核心、注明另有几条：%d 字节 %q", len(small), small)
	}
	if !strings.Contains(small, "细细细细细细细细细细细细细细细细细细细细j") {
		t.Errorf("应保留最近的那条：%q", small)
	}
}

// ---------- 反思的触发 ----------

type turnRec struct {
	mu     sync.Mutex
	inputs []string
	err    error
	ch     chan struct{}
	onTurn func(sid string)
}

func (r *turnRec) run(_ context.Context, sid, input string) (string, error) {
	r.mu.Lock()
	r.inputs = append(r.inputs, input)
	err, on := r.err, r.onTurn
	r.mu.Unlock()
	if on != nil {
		on(sid)
	}
	r.ch <- struct{}{}
	return "", err
}

func (r *turnRec) wait(t *testing.T) {
	t.Helper()
	select {
	case <-r.ch:
	case <-time.After(3 * time.Second):
		t.Fatal("等待反思轮次超时")
	}
}

func (r *turnRec) none(t *testing.T) {
	t.Helper()
	select {
	case <-r.ch:
		t.Fatal("不该起反思轮次")
	case <-time.After(150 * time.Millisecond):
	}
}

func endTurn(p *Plugin, interactive bool) {
	p.OnTurnEnd(plugin.WithScope(context.Background(), plugin.Scope{}),
		plugin.TurnEndEvent{SessionID: "s1", Interactive: interactive, EndedAt: time.Now()})
}

func TestReflectOncePerDayAfterConversation(t *testing.T) {
	rec := &turnRec{ch: make(chan struct{}, 4)}
	var notices []string
	var nmu sync.Mutex
	p, c := newTestPlugin(t, nil, rec.run, func(_ context.Context, _, text string) error {
		nmu.Lock()
		notices = append(notices, text)
		nmu.Unlock()
		return nil
	})
	// 反思轮次里模型记下一条，模拟真实的工具调用（发起方由 Manager 注入，这里手工补上）
	rec.onTurn = func(sid string) {
		ctx := reflecting("")
		_, _ = p.TurnPrompt(ctx, plugin.TurnEvent{SessionID: sid}) // 裁决之后认领，与真实轮次一样
		_, _ = p.Tools()[0].Execute(ctx, json.RawMessage(`{"action":"add","text":"爱上手冲","basis":"x","why":"y"}`))
	}

	endTurn(p, true) // 第一次见：从今天起算，不补反思
	rec.none(t)
	c.add(24 * time.Hour)
	endTurn(p, true) // 新的一天、昨天有人来过：反思
	rec.wait(t)
	waitIdle(t, p)
	if !strings.Contains(rec.inputs[0], "【成长·反思】") {
		t.Errorf("反思输入：%q", rec.inputs[0])
	}
	nmu.Lock()
	if len(notices) != 1 || !strings.Contains(notices[0], "爱上手冲") {
		t.Errorf("反思做了改动应留一条注记：%v", notices)
	}
	nmu.Unlock()

	endTurn(p, true) // 同一天再来：不再反思
	rec.none(t)
}

func TestNoReflectWithoutConversation(t *testing.T) {
	rec := &turnRec{ch: make(chan struct{}, 4)}
	p, c := newTestPlugin(t, nil, rec.run, nil)
	endTurn(p, false) // 第一次见（心跳）：起算
	c.add(24 * time.Hour)
	endTurn(p, false) // 昨天没人来过：跳过
	rec.none(t)
}

func TestReflectRetriesWhenBusy(t *testing.T) {
	rec := &turnRec{ch: make(chan struct{}, 8), err: plugin.ErrSessionBusy}
	p, c := newTestPlugin(t, nil, rec.run, nil)
	endTurn(p, true)
	c.add(24 * time.Hour)
	endTurn(p, true)
	for range p.busyMax {
		rec.wait(t)
	}
	waitIdle(t, p)
	if st := load(t, p, ""); st.LastReflect != logicalDay(c.now()) {
		t.Errorf("忙到放弃也记作今天反思过（明天再说）：%q", st.LastReflect)
	}
}

func TestReflectOffKeepsGrowth(t *testing.T) {
	rec := &turnRec{ch: make(chan struct{}, 4)}
	p, c := newTestPlugin(t, map[string]any{"daily_reflect": false}, rec.run, nil)
	endTurn(p, true)
	c.add(24 * time.Hour)
	endTurn(p, true)
	rec.none(t)
}

// waitIdle 等反思 goroutine 收尾（它在 runTurn 返回后还要写状态与注记）。
func waitIdle(t *testing.T, p *Plugin) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		busy := len(p.reflecting) > 0
		p.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("反思没有收尾")
}

// endTurnIn 是某个人格上的一轮收尾。
func endTurnIn(p *Plugin, tag, sid string, interactive bool) {
	p.OnTurnEnd(plugin.WithScope(context.Background(), plugin.Scope{Write: tag}),
		plugin.TurnEndEvent{SessionID: sid, Interactive: interactive, EndedAt: time.Now()})
}

// 触发时是里人格，等到真正跑的时候会话已切回表人格：表人格不该看到里人格的成长清单，
// 工具也不能改到里人格上；记账落在真正跑了反思的那个人格上，里人格仍然欠着一次。
func TestReflectionFollowsPersonaDecidedAtRunTime(t *testing.T) {
	rec := &turnRec{ch: make(chan struct{}, 4)}
	p, c := newTestPlugin(t, nil, rec.run, nil)
	yesterday := logicalDay(c.now())
	for _, tag := range []string{"", "inner"} {
		_, _ = p.storeFor(tag).Update(func(st *State) (bool, []LogEntry, error) {
			st.LastReflect, st.Dirty = yesterday, true
			if tag == "inner" {
				st.Details = []Entry{{ID: "d1", Text: "里人格的秘密习惯"}}
			}
			return true, nil, nil
		})
	}
	c.add(24 * time.Hour)

	var listing string
	var innerErr error
	rec.onTurn = func(sid string) {
		// Agent 在会话锁内重新裁决：此刻是表人格
		listing, _ = p.TurnPrompt(reflecting(""), plugin.TurnEvent{SessionID: sid})
		_, innerErr = p.Tools()[0].Execute(reflecting("inner"), json.RawMessage(`{"action":"add","text":"x","basis":"y","why":"z"}`))
	}
	endTurnIn(p, "inner", "s1", false) // 触发时是里人格
	rec.wait(t)
	waitIdle(t, p)

	if strings.Contains(listing, "里人格的秘密习惯") || !strings.Contains(listing, "[你现在的成长]") {
		t.Errorf("表人格拿到的应是它自己的清单：%q", listing)
	}
	if innerErr == nil {
		t.Error("没认领的里人格不该被改")
	}
	today := logicalDay(c.now())
	if st := load(t, p, ""); st.LastReflect != today {
		t.Errorf("表人格真的反思了，应记作今天反思过：%q", st.LastReflect)
	}
	if st := load(t, p, "inner"); st.LastReflect != yesterday {
		t.Errorf("里人格这次没反思，应仍然欠着：%q", st.LastReflect)
	}
}

// 同一个人格今天已经反思过：后到的那一轮不认领，只叫它什么都别做。
func TestSecondReflectionSameDayIsNotClaimed(t *testing.T) {
	p, c := newTestPlugin(t, nil, nil, nil)
	_, _ = p.storeFor("").Update(func(st *State) (bool, []LogEntry, error) {
		st.LastReflect = logicalDay(c.now())
		return true, nil, nil
	})
	out, _ := p.TurnPrompt(reflecting(""), plugin.TurnEvent{SessionID: "s1"})
	if !strings.Contains(out, "今天已经回头看过了") {
		t.Errorf("今天反思过的人格不该再认领：%q", out)
	}
	if _, err := call(t, p.Tools()[0], reflecting(""), `{"action":"add","text":"x","basis":"y","why":"z"}`); err == nil {
		t.Error("没认领就不能改")
	}
}

// 设置页显示的是表人格那条提议，之后里人格新提了一条：按下批准处理的仍是看到的那一条；
// 看到的那条在别处被处理掉了，就拒绝并请用户刷新，而不是批准一条没读过的。
func TestApproveActsOnTheShownProposal(t *testing.T) {
	p, c := newTestPlugin(t, nil, nil, nil)
	_, _ = call(t, p.Tools()[1], claimed(t, p, ""), `{"text":"说话直了","basis":"x","why":"y"}`)
	p.Actions() // 用户看到的是「说话直了」
	c.add(time.Minute)
	_, _ = call(t, p.Tools()[1], claimed(t, p, "inner"), `{"text":"里人格的变化","basis":"x","why":"y"}`)

	if err := p.StartAction(context.Background(), actionApprove); err != nil {
		t.Fatal(err)
	}
	if st := load(t, p, ""); len(st.Core) != 1 || st.Core[0].Text != "说话直了" {
		t.Errorf("应批准看到的那一条：%+v", st.Core)
	}
	if st := load(t, p, "inner"); st.Pending == nil {
		t.Error("没看到的那一条不该被处理")
	}

	// 界面停在「里人格的变化」，它在别处被驳回了，又冒出一条新的：按下去必须拒绝
	p.Actions()
	_, _ = p.storeFor("inner").Update(func(st *State) (bool, []LogEntry, error) {
		st.Pending = &Entry{ID: "c99", Text: "另一条没给人看过的", Since: c.now()}
		return true, nil, nil
	})
	err := p.StartAction(context.Background(), actionApprove)
	if err == nil || !strings.Contains(err.Error(), "刷新") {
		t.Errorf("看到的那条变了，应请用户刷新：%v", err)
	}
	if st := load(t, p, "inner"); st.Pending == nil || len(st.Core) != 0 {
		t.Errorf("没看过的那条不该被批准：%+v", st)
	}
}

// 心跳触发的反思进行期间，有人来聊过（哪怕是另一个会话）：收尾不能把这次对话的标记
// 一起清掉，否则次日的反思会被跳过。
func TestReflectionKeepsActivityThatHappenedDuringIt(t *testing.T) {
	for _, during := range []bool{true, false} {
		rec := &turnRec{ch: make(chan struct{}, 4)}
		p, c := newTestPlugin(t, nil, rec.run, nil)
		endTurnIn(p, "", "s1", true) // 第一天有人来过
		c.add(24 * time.Hour)
		rec.onTurn = func(sid string) {
			_, _ = p.TurnPrompt(reflecting(""), plugin.TurnEvent{SessionID: sid})
			if during {
				c.add(time.Minute)
				endTurnIn(p, "", "s2", true) // 反思进行中，另一个会话里有人说话
			}
		}
		endTurnIn(p, "", "s1", false) // 心跳触发
		rec.wait(t)
		waitIdle(t, p)
		if st := load(t, p, ""); st.Dirty != during {
			t.Errorf("反思期间有人来过=%v，收尾后 Dirty 应为 %v，实际 %v", during, during, st.Dirty)
		}
	}
}
