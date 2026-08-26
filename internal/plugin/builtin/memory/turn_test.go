package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"wen/internal/plugin"
)

// blockingComplete 在收到调用后先通知、再等放行，用来观察提炼在途时的行为。
type blockingComplete struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func newBlockingComplete() *blockingComplete {
	return &blockingComplete{started: make(chan struct{}, 8), release: make(chan struct{})}
}

func (b *blockingComplete) fn(ctx context.Context, _ string) (string, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	b.started <- struct{}{}
	select {
	case <-b.release:
		return "[]", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (b *blockingComplete) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// turnCfg 是定期提炼的最小配置：间隔 5 轮（配置项允许的下限）。
func turnCfg(extra map[string]any) map[string]any {
	cfg := map[string]any{"turn_extract": true, "turn_extract_every": 5}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

// chat 造一轮足够长的真人对话（短于 minWindowBytes 的窗口会被判为不值得提炼）。
func chat(i int, at time.Time) plugin.TurnEndEvent {
	return plugin.TurnEndEvent{
		SessionID:   "s1",
		Interactive: true,
		UserInput:   fmt.Sprintf("第 %d 个问题，%s", i, strings.Repeat("内容", 12)),
		FinalText:   fmt.Sprintf("第 %d 个回答，%s", i, strings.Repeat("内容", 12)),
		StartedAt:   at,
		EndedAt:     at.Add(time.Second),
	}
}

// runTurns 送入 n 轮对话，每轮间隔一分钟。
func runTurns(p *Plugin, n int) {
	base := time.Now()
	for i := range n {
		p.OnTurnEnd(context.Background(), chat(i, base.Add(time.Duration(i)*time.Minute)))
	}
}

func TestTurnExtractTriggersAtInterval(t *testing.T) {
	c := newBlockingComplete()
	p := newPluginWithComplete(t, &fakeComplete{}, turnCfg(nil))
	p.mu.Lock()
	p.complete = c.fn
	p.mu.Unlock()
	t.Cleanup(p.Stop)

	runTurns(p, 4)
	if c.count() != 0 {
		t.Fatalf("不到间隔就提炼了：%d 次", c.count())
	}

	runTurns(p, 1) // 第 5 轮
	select {
	case <-c.started:
	case <-time.After(2 * time.Second):
		t.Fatal("到达间隔后未触发提炼")
	}
	close(c.release)
}

func TestTurnExtractIgnoresNonInteractive(t *testing.T) {
	c := &fakeComplete{}
	p := newPluginWithComplete(t, c, turnCfg(nil))
	t.Cleanup(p.Stop)

	// 心跳与定时任务这类机器自发的轮次不该计入，否则挂着心跳的会话会把
	// 自己的独白提炼成记忆
	base := time.Now()
	for i := range 10 {
		ev := chat(i, base.Add(time.Duration(i)*time.Minute))
		ev.Interactive = false
		ev.Origin = "heartbeat"
		p.OnTurnEnd(context.Background(), ev)
	}
	time.Sleep(50 * time.Millisecond)
	if c.calls != 0 {
		t.Errorf("非交互轮次不应触发提炼，实际 %d 次", c.calls)
	}
}

func TestTurnExtractCountsRemoteIMTurns(t *testing.T) {
	c := newBlockingComplete()
	p := newPluginWithComplete(t, &fakeComplete{}, turnCfg(nil))
	p.mu.Lock()
	p.complete = c.fn
	p.mu.Unlock()
	t.Cleanup(p.Stop)

	// 远程 IM 的轮次 Origin 非空，但那头坐着的是真人，恰恰最该记
	base := time.Now()
	for i := range 5 {
		ev := chat(i, base.Add(time.Duration(i)*time.Minute))
		ev.Origin = "qq_bot"
		p.OnTurnEnd(context.Background(), ev)
	}
	select {
	case <-c.started:
	case <-time.After(2 * time.Second):
		t.Fatal("远程 IM 的真人轮次应计入提炼间隔")
	}
	close(c.release)
}

func TestTurnExtractSkipsTinyWindow(t *testing.T) {
	c := &fakeComplete{}
	p := newPluginWithComplete(t, c, turnCfg(nil))
	t.Cleanup(p.Stop)

	base := time.Now()
	for i := range 8 {
		at := base.Add(time.Duration(i) * time.Minute)
		p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{
			SessionID: "s1", Interactive: true,
			UserInput: "嗯", FinalText: "好的",
			StartedAt: at, EndedAt: at.Add(time.Second),
		})
	}
	time.Sleep(50 * time.Millisecond)
	if c.calls != 0 {
		t.Errorf("内容过少的窗口不该调模型，实际 %d 次", c.calls)
	}
	// 内容留着并进下个窗口，而不是被丢掉
	p.turnMu.Lock()
	turns := len(p.windows[windowKey{session: "s1"}].turns)
	p.turnMu.Unlock()
	if turns != 8 {
		t.Errorf("被跳过的窗口应继续累计，实际 %d 轮", turns)
	}
}

func TestTurnExtractFlushesAfterIdleGap(t *testing.T) {
	c := newBlockingComplete()
	p := newPluginWithComplete(t, &fakeComplete{}, turnCfg(nil))
	p.mu.Lock()
	p.complete = c.fn
	p.mu.Unlock()
	t.Cleanup(p.Stop)

	base := time.Now()
	runTurns(p, 2) // 不到间隔

	// 隔了足够久再开口：上一段话题多半已经结束，应先把攒着的提炼掉
	late := chat(9, base.Add(idleFlushGap+time.Hour))
	p.OnTurnEnd(context.Background(), late)
	select {
	case <-c.started:
	case <-time.After(2 * time.Second):
		t.Fatal("长间隔后应先提炼上一段窗口")
	}
	close(c.release)

	// 新的一轮另起窗口，没有被并进刚提炼掉的那段
	p.turnMu.Lock()
	w := p.windows[windowKey{session: "s1"}]
	p.turnMu.Unlock()
	if w == nil || len(w.turns) != 1 {
		t.Errorf("长间隔后的那轮应落进新窗口: %+v", w)
	}
}

func TestTurnExtractSingleFlight(t *testing.T) {
	c := newBlockingComplete()
	p := newPluginWithComplete(t, &fakeComplete{}, turnCfg(nil))
	p.mu.Lock()
	p.complete = c.fn
	p.mu.Unlock()
	t.Cleanup(p.Stop)

	runTurns(p, 5)
	<-c.started // 第一次提炼卡住不返回

	runTurns(p, 10) // 期间又攒够了两个窗口
	time.Sleep(50 * time.Millisecond)
	if n := c.count(); n != 1 {
		t.Errorf("在途提炼未完成时不应再起一次，实际 %d 次", n)
	}
	close(c.release)
}

func TestTurnExtractSeparatesScopes(t *testing.T) {
	c := &fakeComplete{}
	p := newPluginWithComplete(t, c, turnCfg(nil))
	t.Cleanup(p.Stop)

	base := time.Now()
	for i := range 4 {
		at := base.Add(time.Duration(i) * time.Minute)
		p.OnTurnEnd(plugin.WithScope(context.Background(), plugin.Scope{Write: "inner"}), chat(i, at))
		p.OnTurnEnd(context.Background(), chat(i, at))
	}
	time.Sleep(50 * time.Millisecond)
	if c.calls != 0 {
		t.Fatalf("两个可见域各攒各的，都还不到间隔，实际调用 %d 次", c.calls)
	}
	p.turnMu.Lock()
	defer p.turnMu.Unlock()
	if len(p.windows) != 2 {
		t.Errorf("同一会话的两个可见域应各有一个窗口，实际 %d 个", len(p.windows))
	}
	if w := p.windows[windowKey{session: "s1", tag: "inner"}]; w == nil || w.scope.Write != "inner" {
		t.Errorf("可见域应在轮次结束时当场记下: %+v", w)
	}
}

func TestTurnExtractCapsWindowGrowth(t *testing.T) {
	c := newBlockingComplete()
	p := newPluginWithComplete(t, &fakeComplete{}, turnCfg(nil))
	p.mu.Lock()
	p.complete = c.fn
	p.mu.Unlock()
	t.Cleanup(p.Stop)

	runTurns(p, 5)
	<-c.started // 提炼卡住，后续窗口只能一直长

	runTurns(p, 50)
	p.turnMu.Lock()
	turns := len(p.windows[windowKey{session: "s1"}].turns)
	p.turnMu.Unlock()
	if turns > 10 {
		t.Errorf("窗口应有上界（间隔的两倍），实际 %d 轮", turns)
	}
	close(c.release)
}

func TestTurnExtractDisabled(t *testing.T) {
	c := &fakeComplete{}
	p := newPluginWithComplete(t, c, map[string]any{"turn_extract": false, "turn_extract_every": 5})
	t.Cleanup(p.Stop)

	runTurns(p, 20)
	time.Sleep(50 * time.Millisecond)
	if c.calls != 0 {
		t.Errorf("关闭定期提炼后不该有调用，实际 %d 次", c.calls)
	}
}

func TestStopCancelsInFlightExtract(t *testing.T) {
	c := newBlockingComplete()
	p := newPluginWithComplete(t, &fakeComplete{}, turnCfg(nil))
	p.mu.Lock()
	p.complete = c.fn
	p.mu.Unlock()

	runTurns(p, 5)
	<-c.started

	done := make(chan struct{})
	go func() { p.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop 应取消在途调用并有界返回，不能等整轮提炼跑完")
	}
}

func TestPluginImplementsTurnObserverAndStoppable(t *testing.T) {
	var _ plugin.TurnObserver = New()
	var _ plugin.Stoppable = New()
}

// noticeRecorder 收集插件写出的会话注记。
type noticeRecorder struct {
	mu    sync.Mutex
	items []string
	sess  []string
	tags  []string
}

func (n *noticeRecorder) fn(ctx context.Context, sessionID, text string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.items = append(n.items, text)
	n.sess = append(n.sess, sessionID)
	n.tags = append(n.tags, plugin.ScopeFrom(ctx).Write)
	return nil
}

func (n *noticeRecorder) snapshot() ([]string, []string, []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string{}, n.items...), append([]string{}, n.sess...), append([]string{}, n.tags...)
}

// newPluginWithNotice 建一个装了注记出口的插件。
func newPluginWithNotice(t *testing.T, c *fakeComplete, n *noticeRecorder, cfg map[string]any) *Plugin {
	t.Helper()
	p := New()
	ictx := plugin.InitContext{StateDir: t.TempDir(), Complete: c.fn, Notice: n.fn}
	if err := p.Init(ictx, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	return p
}

func TestTurnExtractPostsNotice(t *testing.T) {
	c := &fakeComplete{replies: []string{
		`{"memories":[{"name":"构建命令","description":"go build","type":"事实","content":"构建：go build ./cmd/wen","mode":"create"}],"mentioned":[]}`,
	}}
	n := &noticeRecorder{}
	p := newPluginWithNotice(t, c, n, turnCfg(nil))

	runTurns(p, 5)
	waitForNotice(t, n, 1)

	items, sess, _ := n.snapshot()
	// 提炼跑在轮次收尾之后，不留痕迹的话模型自动记了什么就只有日志知道
	if !strings.Contains(items[0], "新增") || !strings.Contains(items[0], "事实/构建命令") {
		t.Errorf("注记应说清记了什么：%q", items[0])
	}
	if sess[0] != "s1" {
		t.Errorf("注记应落在触发提炼的那个会话上，实际 %q", sess[0])
	}
}

func TestTurnExtractNoticeCarriesScope(t *testing.T) {
	c := &fakeComplete{replies: []string{
		`{"memories":[{"name":"里侧的事","description":"摘要","type":"事实","content":"正文","mode":"create"}],"mentioned":[]}`,
	}}
	n := &noticeRecorder{}
	p := newPluginWithNotice(t, c, n, turnCfg(nil))

	ctx := plugin.WithScope(context.Background(), plugin.Scope{Write: "inner", Read: []string{"inner"}})
	base := time.Now()
	for i := range 5 {
		p.OnTurnEnd(ctx, chat(i, base.Add(time.Duration(i)*time.Minute)))
	}
	waitForNotice(t, n, 1)

	// 「在人格 A 的库里记了什么」这件事本身也属于人格 A
	if _, _, tags := n.snapshot(); tags[0] != "inner" {
		t.Errorf("注记应带上提炼所属的可见域，实际 %q", tags[0])
	}
}

func TestTurnExtractNoNoticeWhenNothingChanged(t *testing.T) {
	c := &fakeComplete{replies: []string{`{"memories":[],"mentioned":[]}`}}
	n := &noticeRecorder{}
	p := newPluginWithNotice(t, c, n, turnCfg(nil))

	runTurns(p, 5)
	time.Sleep(200 * time.Millisecond)
	if items, _, _ := n.snapshot(); len(items) != 0 {
		t.Errorf("没记下任何东西时不该打扰：%v", items)
	}
}

// waitForNotice 等到收满 n 条注记。
func waitForNotice(t *testing.T, rec *noticeRecorder, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if items, _, _ := rec.snapshot(); len(items) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等不到 %d 条注记", n)
}

// 提炼窗口要跨重启延续。
//
// 这是本次修复的回归测试：窗口原本只在内存里，重启归零，于是重启比「提炼间隔」
// 更频繁的人，定期提炼一次都不会发生。这里模拟「攒了 4 轮 → 重启 → 再来 1 轮」，
// 第 5 轮应当触发提炼。
func TestWindowSurvivesRestart(t *testing.T) {
	stateDir := t.TempDir()

	p1 := New()
	if err := p1.Init(plugin.InitContext{StateDir: stateDir, Complete: (&fakeComplete{}).fn},
		turnCfg(nil)); err != nil {
		t.Fatal(err)
	}
	runTurns(p1, 4)
	p1.Stop() // Stop 会等在途的写盘结束

	// 新进程：同一个状态目录，重新装回缓冲
	c := newBlockingComplete()
	p2 := New()
	if err := p2.Init(plugin.InitContext{StateDir: stateDir, Complete: c.fn}, turnCfg(nil)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p2.Stop)

	p2.turnMu.Lock()
	got := len(p2.windows[windowKey{session: "s1"}].turns)
	p2.turnMu.Unlock()
	if got != 4 {
		t.Fatalf("重启后窗口里有 %d 轮，应接着上次的 4 轮", got)
	}

	runTurns(p2, 1) // 第 5 轮，应到点
	select {
	case <-c.started:
	case <-time.After(2 * time.Second):
		t.Fatal("重启后再来一轮就该到间隔，却没有触发提炼")
	}
	close(c.release)
}

// 落盘的窗口要保住内容与可见域，否则提炼时会读错库、写错库。
func TestWindowStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	end := time.Now().Truncate(time.Second)
	windows := map[windowKey]*window{
		{session: "s1", tag: "inner"}: {
			scope:   plugin.Scope{Write: "inner", Read: []string{"inner"}},
			turns:   []windowTurn{{user: "问", reply: "答"}},
			bytes:   6,
			lastEnd: end,
		},
	}
	saveWindowState("", dir, windows, dayMarks{lastSweep: end, lastTimeline: end})

	got, marks := loadWindowState("", dir)
	w := got[windowKey{session: "s1", tag: "inner"}]
	if w == nil {
		t.Fatal("窗口未恢复")
	}
	if len(w.turns) != 1 || w.turns[0].user != "问" || w.turns[0].reply != "答" {
		t.Errorf("对话内容不对: %+v", w.turns)
	}
	if w.scope.Write != "inner" || len(w.scope.Read) != 1 || w.scope.Read[0] != "inner" {
		t.Errorf("可见域丢失: %+v", w.scope)
	}
	if w.bytes != 6 {
		t.Errorf("字节数 = %d, want 6（够不够格提炼靠它判断）", w.bytes)
	}
	if !marks.lastSweep.Equal(end) {
		t.Errorf("上次清扫日期 = %v, want %v", marks.lastSweep, end)
	}
	if !marks.lastTimeline.Equal(end) {
		t.Errorf("上次日切日期 = %v, want %v", marks.lastTimeline, end)
	}
}

// 重新配置（SetConfig 会再走一次 Init）不该把内存里刚攒的几轮退回盘上的旧值。
func TestReinitKeepsInMemoryWindow(t *testing.T) {
	stateDir := t.TempDir()
	ictx := plugin.InitContext{StateDir: stateDir, Complete: (&fakeComplete{}).fn}

	p := New()
	if err := p.Init(ictx, turnCfg(nil)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	runTurns(p, 3)

	// 改个无关配置，触发重新 Init
	if err := p.Init(ictx, turnCfg(map[string]any{"max_extract": 3})); err != nil {
		t.Fatal(err)
	}
	p.turnMu.Lock()
	got := len(p.windows[windowKey{session: "s1"}].turns)
	p.turnMu.Unlock()
	if got != 3 {
		t.Errorf("重新配置后窗口里有 %d 轮，应保持内存中的 3 轮", got)
	}
}

// 没有可用的持久化目录时退化成纯内存缓冲，不报错也不乱写文件。
func TestWindowNoStateDirDegrades(t *testing.T) {
	saveWindowState("", "", map[windowKey]*window{}, dayMarks{})
	if got, _ := loadWindowState("", ""); got != nil {
		t.Error("无目录时不该返回窗口")
	}
}
