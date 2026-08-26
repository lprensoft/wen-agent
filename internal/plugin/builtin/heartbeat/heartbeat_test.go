package heartbeat

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
	"wen/internal/session"
)

// mustStore 打开一个会话存储，供直接构造 InitContext 的用例使用。
func mustStore(t *testing.T, dir string) *session.Store {
	t.Helper()
	s, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newInited(t *testing.T, runTurn plugin.RunTurnFunc, cfg map[string]any) (*Plugin, *session.Store) {
	t.Helper()
	sessDir := t.TempDir()
	store, err := session.NewStore(sessDir)
	if err != nil {
		t.Fatal(err)
	}
	p := New()
	ictx := plugin.InitContext{
		StateDir:   t.TempDir(),
		Sessions:   store,
		RunTurn:    runTurn,
		NewSession: func() (string, error) { m, err := store.Create(); return m.ID, err },
	}
	if err := p.Init(ictx, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	// Init 起的后台循环立刻停掉：本包的用例都是直接调内部方法来驱动的，循环留着跑
	// 只会和它们抢同一份状态。它第一轮什么时候被调度是不确定的——本地通常在这里就
	// 算好了下一次到期时刻然后 park 住，CI 机器忙时会晚一步，那时用例已经把「早就
	// 到期」的状态塞了进去，于是循环立刻多跑一次，断言的计数就多了一。
	// 要测循环本身的话另起一个不停的 helper，别把这里改回去。
	p.Stop()
	return p, store
}

func noTurn(context.Context, string, string) (string, error) { return "", nil }

// 间隔配置必须满足 最快 ≤ 基础 ≤ 最慢。
func TestInitValidatesIntervals(t *testing.T) {
	p := New()
	err := p.Init(plugin.InitContext{StateDir: t.TempDir(), Sessions: mustStore(t, t.TempDir()), RunTurn: noTurn},
		map[string]any{"interval_minutes": 3, "min_minutes": 5, "max_minutes": 120})
	if err == nil {
		t.Fatal("基础间隔小于最快间隔应报错")
	}
}

// Init 可重入：重复 Init 后旧循环被停掉，Stop 幂等。
func TestInitReentrant(t *testing.T) {
	p, store := newInited(t, noTurn, nil)
	ictx := plugin.InitContext{
		StateDir: t.TempDir(), Sessions: mustStore(t, t.TempDir()), RunTurn: noTurn,
		NewSession: func() (string, error) { m, err := store.Create(); return m.ID, err },
	}
	if err := p.Init(ictx, map[string]any{"interval_minutes": 10}); err != nil {
		t.Fatal(err)
	}
	p.Stop()
	p.Stop() // 幂等
}

// pickSession 挑最近活跃的会话；旧会话没有 LastActiveAt 时回落创建时间；没有会话则新建。
func TestPickSession(t *testing.T) {
	p, store := newInited(t, noTurn, nil)

	old, _ := store.Create()
	time.Sleep(1100 * time.Millisecond) // 会话 ID 精确到秒，隔开保证新会话排后
	fresh, _ := store.Create()

	// 都没有 LastActiveAt：回落创建时间挑后建的，但「上次对话时间」仍是未知
	sid, active, err := p.pickSession()
	if err != nil {
		t.Fatal(err)
	}
	if sid != fresh.ID {
		t.Fatalf("应挑最近创建的 %s，得到 %s", fresh.ID, sid)
	}
	if !active.IsZero() {
		t.Fatalf("没有 LastActiveAt 时不该拿创建时间充数，得到 %v", active)
	}

	// 老会话被真人使用过后应反超，并带回该会话的真人对话时间
	used := time.Now().Add(time.Hour).Round(time.Second)
	_ = store.SetLastActive(old.ID, used)
	sid, active, err = p.pickSession()
	if err != nil {
		t.Fatal(err)
	}
	if sid != old.ID {
		t.Fatalf("应挑最近活跃的 %s，得到 %s", old.ID, sid)
	}
	if !active.Equal(used) {
		t.Fatalf("应带回该会话的真人对话时间 %v，得到 %v", used, active)
	}
}

// 没有任何会话时心跳自己新建一个。
func TestPickSessionCreates(t *testing.T) {
	p, store := newInited(t, noTurn, nil)
	sid, active, err := p.pickSession()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Get(sid); err != nil {
		t.Fatalf("新建的会话应存在: %v", err)
	}
	if !active.IsZero() {
		t.Fatalf("刚建的会话没有「上次对话」可言，得到 %v", active)
	}
}

// 心跳轮次带一次性输入标记。
func TestBeatMarksEphemeral(t *testing.T) {
	var sawEphemeral bool
	turn := func(ctx context.Context, _, _ string) (string, error) {
		sawEphemeral = plugin.IsEphemeralInput(ctx)
		return "ok", nil
	}
	p, store := newInited(t, turn, nil)
	_, _ = store.Create()
	p.beat(context.Background())
	if !sawEphemeral {
		t.Fatal("心跳轮次应标记为一次性输入")
	}
}

// beat 在会话忙时静默跳过，不算错误。
func TestBeatSkipsBusy(t *testing.T) {
	called := 0
	busyTurn := func(context.Context, string, string) (string, error) {
		called++
		return "", plugin.ErrSessionBusy
	}
	p, store := newInited(t, busyTurn, nil)
	_, _ = store.Create()
	p.beat(context.Background())
	if called != 1 {
		t.Fatalf("RunTurn 应被调用一次，实际 %d", called)
	}
}

// 空闲衰减：超过一个间隔没有真人交互时放缓 1.5 倍，到最慢间隔封顶；动态关闭时不衰减。
func TestMaybeDecay(t *testing.T) {
	p := &Plugin{dynamic: true, minIv: 5 * time.Minute, maxIv: 60 * time.Minute, cur: 30 * time.Minute}
	p.lastActive = time.Now().Add(-time.Hour)

	p.maybeDecay()
	if p.cur != 45*time.Minute {
		t.Fatalf("衰减后 cur = %v，期望 45m", p.cur)
	}
	p.maybeDecay()
	if p.cur != 60*time.Minute {
		t.Fatalf("衰减应封顶在最慢间隔，得到 %v", p.cur)
	}

	// 刚聊过天：不衰减
	p.cur = 30 * time.Minute
	p.lastActive = time.Now()
	p.maybeDecay()
	if p.cur != 30*time.Minute {
		t.Fatalf("活跃期内不该衰减，得到 %v", p.cur)
	}

	// 动态关闭：不衰减
	p.dynamic = false
	p.lastActive = time.Now().Add(-time.Hour)
	p.maybeDecay()
	if p.cur != 30*time.Minute {
		t.Fatalf("动态关闭时不该衰减，得到 %v", p.cur)
	}

	// 暂停期间：不衰减——这段安静是模型自己定的，不是「没人想聊」的证据
	p.dynamic = true
	p.pausedUntil = time.Now().Add(time.Hour)
	p.maybeDecay()
	if p.cur != 30*time.Minute {
		t.Fatalf("暂停期间不该衰减，得到 %v", p.cur)
	}

	// 暂停到期后：又是自然安静，衰减照常恢复
	p.pausedUntil = time.Now().Add(-time.Minute)
	p.maybeDecay()
	if p.cur != 45*time.Minute {
		t.Fatalf("暂停到期后应恢复衰减，得到 %v", p.cur)
	}
}

// setTool 造一个能直接调用的工具与它所属的插件。
func setTool(t *testing.T, cur time.Duration, dynamic bool) (*Plugin, plugin.Tool) {
	t.Helper()
	p := &Plugin{
		dynamic: dynamic, minIv: 5 * time.Minute, maxIv: 120 * time.Minute, cur: cur,
		stateDir: t.TempDir(), wake: make(chan struct{}, 1),
	}
	return p, p.Tools()[0]
}

func callSet(t *testing.T, tl plugin.Tool, minutes int, reason string) (string, error) {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"minutes": minutes, "reason": reason})
	return tl.Execute(context.Background(), args)
}

// 模型自己定节奏：报几分钟就是几分钟，并且倒计时从此刻重算。
func TestSetIntervalTool(t *testing.T) {
	p, tl := setTool(t, 30*time.Minute, true)
	before := time.Now()

	out, err := callSet(t, tl, 90, "她说去忙了")
	if err != nil {
		t.Fatal(err)
	}
	if p.cur != 90*time.Minute {
		t.Fatalf("cur = %v，期望 90m", p.cur)
	}
	if !p.adjusted {
		t.Fatal("模型定过的节奏要标记为 adjusted，否则重启就丢了")
	}
	if p.lastBeat.Before(before) {
		t.Fatal("倒计时应从此刻重算")
	}
	if !strings.Contains(out, "1 小时 30 分钟") {
		t.Errorf("回执应说明实际生效的间隔: %q", out)
	}
	select {
	case <-p.wake:
	default:
		t.Fatal("改完间隔要唤醒循环重算下次心跳时刻")
	}
}

// 超出配置范围时限幅，并且要把规则告诉模型——否则它只会换个数字再试一次。
func TestSetIntervalToolClamps(t *testing.T) {
	p, tl := setTool(t, 30*time.Minute, true)
	out, err := callSet(t, tl, 1, "话题正热")
	if err != nil {
		t.Fatal(err)
	}
	if p.cur != 5*time.Minute {
		t.Fatalf("低于最快间隔应限幅到 5m，得到 %v", p.cur)
	}
	if !strings.Contains(out, "1 分钟") || !strings.Contains(out, "收到") {
		t.Errorf("限幅生效时要说明原报值与范围: %q", out)
	}

	p2, tl2 := setTool(t, 30*time.Minute, true)
	if _, err := callSet(t, tl2, 10000, "她要出差一周"); err != nil {
		t.Fatal(err)
	}
	if p2.cur != 120*time.Minute {
		t.Fatalf("高于最慢间隔应限幅到 120m，得到 %v", p2.cur)
	}
}

// 关掉动态心跳时工具要拒绝，并说明为什么——固定节奏就是「固定」的意思。
func TestSetIntervalToolRefusedWhenStatic(t *testing.T) {
	p, tl := setTool(t, 30*time.Minute, false)
	_, err := callSet(t, tl, 10, "话题正热")
	if err == nil {
		t.Fatal("固定节奏下应拒绝调整")
	}
	if !strings.Contains(err.Error(), "动态心跳") {
		t.Errorf("拒绝时要指明怎么才能放开: %v", err)
	}
	if p.cur != 30*time.Minute {
		t.Fatalf("拒绝之后不该改动间隔: %v", p.cur)
	}
}

func TestSetIntervalToolRejectsBadInput(t *testing.T) {
	_, tl := setTool(t, 30*time.Minute, true)
	for _, m := range []int{0, -5} {
		if _, err := callSet(t, tl, m, "随便"); err == nil {
			t.Errorf("%d 分钟应被拒绝", m)
		}
	}
	if _, err := tl.Execute(context.Background(), []byte("{")); err == nil {
		t.Error("坏参数应报错")
	}
}

// 工具返回时状态必须已经落盘。写盘若另起 goroutine，就会脱离插件的生命周期——
// 停掉插件之后才写，甚至在测试的临时目录被清理之后才写（CI 上表现为
// 「TempDir RemoveAll cleanup: directory not empty」，且只在 Linux 上偶发）。
func TestSetIntervalToolSavesBeforeReturning(t *testing.T) {
	p, tl := setTool(t, 30*time.Minute, true)
	if _, err := callSet(t, tl, 15, "跟进一下"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(p.stateDir, "state.json"))
	if err != nil {
		t.Fatalf("工具返回时状态还没落盘: %v", err)
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("状态文件解析失败: %v", err)
	}
	if st.IntervalSeconds != int(15*time.Minute/time.Second) {
		t.Errorf("落盘的间隔 = %d 秒，期望 900", st.IntervalSeconds)
	}
	if !st.Adjusted {
		t.Error("落盘时应带上 adjusted")
	}
}

// 真人交互的轮次结束后心跳时钟重置：下一次心跳从这一刻起重新计时，聊天途中不会被心跳打断。
func TestTurnEndResetsClock(t *testing.T) {
	p, _ := newInited(t, noTurn, map[string]any{"interval_minutes": 30, "dynamic": false})

	p.mu.Lock()
	p.lastBeat = time.Now().Add(-29 * time.Minute) // 差一分钟就要心跳了
	p.mu.Unlock()

	end := time.Now()
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{Interactive: true, StartedAt: end.Add(-time.Minute), EndedAt: end})

	p.mu.Lock()
	beat, active := p.lastBeat, p.lastActive
	p.mu.Unlock()
	if !beat.Equal(end) {
		t.Fatalf("真人轮次结束后心跳时钟应重置到 %v，得到 %v", end, beat)
	}
	if !active.Equal(end) {
		t.Fatalf("活跃时间应刷新到 %v，得到 %v", end, active)
	}
	select { // 循环挂在旧的到期时刻上，必须被叫醒重算
	case <-p.wake:
	default:
		t.Fatal("重置心跳时钟后应唤醒循环重算下一次心跳时刻")
	}
}

// 后台轮次（心跳自己、定时任务等）不重置心跳时钟，否则背景活动会把心跳无限推后。
func TestTurnEndBackgroundKeepsClock(t *testing.T) {
	p, _ := newInited(t, noTurn, map[string]any{"dynamic": false})
	before := time.Now().Add(-20 * time.Minute)
	p.mu.Lock()
	p.lastBeat = before
	p.mu.Unlock()

	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{Origin: "scheduler", EndedAt: time.Now()})
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{EndedAt: time.Now()}) // 非交互

	p.mu.Lock()
	beat := p.lastBeat
	p.mu.Unlock()
	if !beat.Equal(before) {
		t.Fatalf("后台轮次不该重置心跳时钟，得到 %v", beat)
	}
}

// 时钟只前推不回拨：时间戳缺失或落后于当前起点时保持原值。
func TestResetClockNeverRewinds(t *testing.T) {
	now := time.Now()
	p := &Plugin{lastBeat: now, wake: make(chan struct{}, 1)}

	p.resetClockLocked(now.Add(-time.Hour))
	if !p.lastBeat.Equal(now) {
		t.Fatalf("过去的时间戳不该回拨时钟，得到 %v", p.lastBeat)
	}

	p.lastBeat = time.Time{}
	p.resetClockLocked(time.Time{}) // 零值当作「现在」
	if p.lastBeat.IsZero() {
		t.Fatal("零值时间戳应回落到当前时刻")
	}
}

// earnRhythm 让插件处在「动态判定已经把节奏调整成 iv」的状态并落盘，
// 模拟聊了一阵之后的样子。
func earnRhythm(t *testing.T, p *Plugin, iv time.Duration) {
	t.Helper()
	p.mu.Lock()
	p.cur = iv
	p.adjusted = true
	dir, st := p.snapshotStateLocked()
	p.mu.Unlock()
	persistState(dir, st)
}

func curOf(t *testing.T, p *Plugin) time.Duration {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur
}

// 动态判定挣来的节奏在重启后生效并按新配置限幅。
func TestIntervalPersistence(t *testing.T) {
	stateDir := t.TempDir()
	sessDir := t.TempDir()
	ictx := plugin.InitContext{StateDir: stateDir, Sessions: mustStore(t, sessDir), RunTurn: noTurn}

	p := New()
	if err := p.Init(ictx, nil); err != nil {
		t.Fatal(err)
	}
	earnRhythm(t, p, 10*time.Minute)
	p.Stop()

	p2 := New()
	if err := p2.Init(ictx, nil); err != nil {
		t.Fatal(err)
	}
	defer p2.Stop()
	if cur := curOf(t, p2); cur != 10*time.Minute {
		t.Fatalf("重启后应恢复持久化间隔 10m，得到 %v", cur)
	}
}

// 关掉动态心跳后，间隔必须跟随基础间隔。
//
// 回归：状态文件只存了间隔，没存「它是怎么来的」。静态模式下 cur 恒等于 base，
// 于是落盘的是一份「当时的基础间隔」的副本；下次 Init 又把这份副本读回来盖住新配
// 的值——用户在设置页把基础间隔从 30 改成 45，心跳照旧 30 分钟一次，且永远改不动。
func TestStaticIntervalFollowsBase(t *testing.T) {
	ictx := plugin.InitContext{
		StateDir: t.TempDir(), Sessions: mustStore(t, t.TempDir()), RunTurn: noTurn,
	}
	p := New()
	if err := p.Init(ictx, map[string]any{"interval_minutes": 30, "dynamic": false}); err != nil {
		t.Fatal(err)
	}
	p.Stop()

	if err := p.Init(ictx, map[string]any{"interval_minutes": 45, "dynamic": false}); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	if cur := curOf(t, p); cur != 45*time.Minute {
		t.Errorf("静态心跳应跟随基础间隔：cur=%v，want 45m", cur)
	}
}

// 开着动态心跳时，改基础间隔不得抹掉已经调整出来的节奏。
func TestDynamicRhythmSurvivesBaseChange(t *testing.T) {
	ictx := plugin.InitContext{
		StateDir: t.TempDir(), Sessions: mustStore(t, t.TempDir()), RunTurn: noTurn,
	}
	p := New()
	if err := p.Init(ictx, map[string]any{"interval_minutes": 30, "dynamic": true}); err != nil {
		t.Fatal(err)
	}
	earnRhythm(t, p, 60*time.Minute)
	p.Stop()

	if err := p.Init(ictx, map[string]any{"interval_minutes": 45, "dynamic": true}); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	if cur := curOf(t, p); cur != 60*time.Minute {
		t.Errorf("动态节奏应当保住：cur=%v，want 60m", cur)
	}
}

// 开着动态心跳但一次都没调整过时，间隔同样跟随基础间隔——
// 那个持久化值不是节奏，只是上一次基础间隔的副本，没什么可保的。
func TestUnadjustedIntervalFollowsBase(t *testing.T) {
	ictx := plugin.InitContext{
		StateDir: t.TempDir(), Sessions: mustStore(t, t.TempDir()), RunTurn: noTurn,
	}
	p := New()
	if err := p.Init(ictx, map[string]any{"interval_minutes": 30, "dynamic": true}); err != nil {
		t.Fatal(err)
	}
	p.Stop()

	if err := p.Init(ictx, map[string]any{"interval_minutes": 45, "dynamic": true}); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	if cur := curOf(t, p); cur != 45*time.Minute {
		t.Errorf("没挣来过节奏时应跟随基础间隔：cur=%v，want 45m", cur)
	}
}

// 关掉动态心跳会把已有节奏丢掉，而不是存着等下次打开时复活：
// 关掉期间跑的是基础间隔，再打开时该从那里继续。
func TestDisablingDynamicDropsRhythm(t *testing.T) {
	ictx := plugin.InitContext{
		StateDir: t.TempDir(), Sessions: mustStore(t, t.TempDir()), RunTurn: noTurn,
	}
	p := New()
	if err := p.Init(ictx, map[string]any{"interval_minutes": 30, "dynamic": true}); err != nil {
		t.Fatal(err)
	}
	earnRhythm(t, p, 60*time.Minute)
	p.Stop()

	if err := p.Init(ictx, map[string]any{"interval_minutes": 30, "dynamic": false}); err != nil {
		t.Fatal(err)
	}
	p.Stop()
	if cur := curOf(t, p); cur != 30*time.Minute {
		t.Fatalf("关掉动态后应跟随基础间隔：cur=%v", cur)
	}

	if err := p.Init(ictx, map[string]any{"interval_minutes": 30, "dynamic": true}); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	if cur := curOf(t, p); cur != 30*time.Minute {
		t.Errorf("重新打开动态后不该复活旧节奏：cur=%v，want 30m", cur)
	}
}

// 心跳提示词末尾附上距上次真人对话的时长；时间未知或不足一分钟时不附。
func TestGapNote(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		lastActive time.Time
		want       string
	}{
		{"未知不附", time.Time{}, "提示词"},
		{"不足一分钟不附", now.Add(-30 * time.Second), "提示词"},
		{"分钟", now.Add(-20 * time.Minute), "提示词\n\n【距上次对话】20 分钟"},
		{"跨天附冷唤醒提示", now.Add(-30 * time.Hour), "提示词\n\n【距上次对话】1 天 6 小时\n" + coldWakeNoteZH},
	}
	for _, c := range cases {
		if got := gapNote("", "提示词", c.lastActive, now, 30*time.Minute); got != c.want {
			t.Errorf("%s: gapNote = %q，期望 %q", c.name, got, c.want)
		}
	}
}

// 心跳实际发出的输入带上了时长注记，且仍是一次性输入。
func TestBeatSendsGap(t *testing.T) {
	var sent string
	turn := func(_ context.Context, _, input string) (string, error) {
		sent = input
		return "ok", nil
	}
	p, store := newInited(t, turn, map[string]any{"prompt": "心跳内容"})
	m, _ := store.Create()
	_ = store.SetLastActive(m.ID, time.Now().Add(-2*time.Hour))

	p.beat(context.Background())
	want := "心跳内容\n\n【距上次对话】2 小时"
	if sent != want {
		t.Fatalf("心跳输入 = %q，期望 %q", sent, want)
	}
}

// 重启后倒计时接着上次走，而不是从零重算。
//
// 这是「重启比心跳间隔更频繁时，心跳一次都不会触发」那个毛病的回归测试：
// 起点若每次 Init 都取 now，下一次心跳就被无限推迟。
func TestLastBeatSurvivesRestart(t *testing.T) {
	stateDir := t.TempDir()
	sessDir := t.TempDir()
	ictx := plugin.InitContext{StateDir: stateDir, Sessions: mustStore(t, sessDir), RunTurn: noTurn}

	p := New()
	if err := p.Init(ictx, map[string]any{"interval_minutes": 60, "dynamic": false}); err != nil {
		t.Fatal(err)
	}
	// 假装 40 分钟前心跳过一次
	beatAt := time.Now().Add(-40 * time.Minute)
	p.mu.Lock()
	p.lastBeat = beatAt
	dir, st := p.snapshotStateLocked()
	p.mu.Unlock()
	persistState(dir, st)
	p.Stop()

	p2 := New()
	if err := p2.Init(ictx, map[string]any{"interval_minutes": 60, "dynamic": false}); err != nil {
		t.Fatal(err)
	}
	defer p2.Stop()
	p2.mu.Lock()
	got := p2.lastBeat
	p2.mu.Unlock()

	if diff := got.Sub(beatAt); diff < -time.Minute || diff > time.Minute {
		t.Errorf("重启后倒计时起点 = %v，应接着上次的 %v（相差 %v）", got, beatAt, diff)
	}
	// 还剩约 20 分钟，不该立刻心跳
	if left := time.Until(got.Add(60 * time.Minute)); left < 19*time.Minute {
		t.Errorf("下次心跳只剩 %v，应还有约 20 分钟", left)
	}
}

// 关机太久导致「早就该心跳了」时补一次，但要等过宽限期，
// 免得服务刚起来的那一秒角色就抢先开口。
func TestOverdueBeatWaitsForGrace(t *testing.T) {
	p := &Plugin{cur: 60 * time.Minute}
	now := time.Now()

	got := p.resumeLastBeat(now.Add(-5*time.Hour), time.Time{}, now)
	next := got.Add(p.cur)
	if d := next.Sub(now); d < startupGrace-time.Second || d > startupGrace+time.Second {
		t.Errorf("过期后下次心跳在 %v 后，应正好是宽限期 %v", d, startupGrace)
	}
}

// 真人对话会重置心跳时钟，这个时刻记在会话元数据里，
// 因此重启后要认它——否则刚聊完就重启，会比预期更早心跳。
func TestResumeUsesLaterOfBeatAndActive(t *testing.T) {
	p := &Plugin{cur: 60 * time.Minute}
	now := time.Now()
	beat := now.Add(-50 * time.Minute)
	active := now.Add(-10 * time.Minute) // 聊完才 10 分钟

	got := p.resumeLastBeat(beat, active, now)
	if !got.Equal(active) {
		t.Errorf("起点 = %v，应取较晚的上次对话时刻 %v", got, active)
	}
}

// 首次启用没有任何先前状态，应等满一个完整间隔，而不是被宽限期规则拽成立刻心跳。
func TestFreshInstallWaitsFullInterval(t *testing.T) {
	p := &Plugin{cur: 60 * time.Minute}
	now := time.Now()

	got := p.resumeLastBeat(time.Time{}, time.Time{}, now)
	if !got.Equal(now) {
		t.Errorf("首次启用的起点 = %v，应为当前时刻 %v", got, now)
	}
}

// 间隔一律取整到分钟：三个间隔配置项的单位都是分钟，亚分钟的精度用户表达不出来。
func TestNormalizeRoundsToMinutes(t *testing.T) {
	p := &Plugin{minIv: 5 * time.Minute, maxIv: 120 * time.Minute}
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{16*time.Minute + 52*time.Second + 500*time.Millisecond, 17 * time.Minute},
		{37*time.Minute + 58*time.Second + 125*time.Millisecond, 38 * time.Minute},
		{7*time.Minute + 30*time.Second, 8 * time.Minute}, // 半分钟向上取
		{30 * time.Minute, 30 * time.Minute},
		{time.Second, 5 * time.Minute},         // 取整后小于下限，限幅
		{300 * time.Minute, 120 * time.Minute}, // 超过上限，限幅
	}
	for _, c := range cases {
		if got := p.normalize(c.in); got != c.want {
			t.Errorf("normalize(%v) = %v，期望 %v", c.in, got, c.want)
		}
	}
}

// 空闲衰减每次乘 1.5，而 Duration 是纳秒精度的整数：不取整的话
// 5m → 7m30s → 11m15s → 16m52.5s → 37m58.125s，越衰减越"精确"，
// 最后落到一个谁也没配过的数上。这是实际跑出来的回归。
func TestDecayStaysOnWholeMinutes(t *testing.T) {
	p := &Plugin{dynamic: true, minIv: 5 * time.Minute, maxIv: 120 * time.Minute, cur: 5 * time.Minute}
	p.lastActive = time.Now().Add(-24 * time.Hour)

	seen := []time.Duration{p.cur}
	for range 20 {
		p.maybeDecay()
		if p.cur%time.Minute != 0 {
			t.Fatalf("衰减出了不足一分钟的零头：%v（路径 %v）", p.cur, seen)
		}
		seen = append(seen, p.cur)
	}
	if p.cur != p.maxIv {
		t.Fatalf("衰减最终应封顶在最慢间隔，得到 %v", p.cur)
	}
}

// 见人恢复常态：真人轮次把衰减或调慢出来的节奏拉回基础值并落盘；
// 比基础值快的设定保留——热聊中调快的节奏不该被一条新消息打回去。
func TestTurnEndRestoresBaseline(t *testing.T) {
	p, _ := newInited(t, noTurn, map[string]any{"interval_minutes": 30})

	p.mu.Lock()
	p.cur, p.adjusted = 90*time.Minute, true
	p.mu.Unlock()
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{Interactive: true, EndedAt: time.Now()})
	p.mu.Lock()
	cur, adjusted := p.cur, p.adjusted
	st := p.loadStateLocked()
	p.mu.Unlock()
	if cur != 30*time.Minute || adjusted {
		t.Fatalf("真人轮次后应恢复基础节奏，得到 %v adjusted=%v", cur, adjusted)
	}
	if st.IntervalSeconds != int(30*time.Minute/time.Second) || st.Adjusted {
		t.Fatalf("恢复基础节奏要落盘，state=%+v", st)
	}

	p.mu.Lock()
	p.cur, p.adjusted = 10*time.Minute, true
	p.mu.Unlock()
	p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{Interactive: true, EndedAt: time.Now()})
	p.mu.Lock()
	cur, adjusted = p.cur, p.adjusted
	p.mu.Unlock()
	if cur != 10*time.Minute || !adjusted {
		t.Fatalf("快于基础值的设定不该被打回，得到 %v adjusted=%v", cur, adjusted)
	}
}

// 暂停到点后的第一拍是「睡醒」：节奏恢复基础值（快慢都不保留），暂停记录清除。
func TestBeatWakesFromPauseToBaseline(t *testing.T) {
	p, _ := newInited(t, noTurn, map[string]any{"interval_minutes": 30})
	p.mu.Lock()
	p.cur, p.adjusted = 15*time.Minute, true
	p.pausedUntil, p.pausedAt = time.Now().Add(-time.Minute), time.Now().Add(-8*time.Hour)
	p.mu.Unlock()

	p.beat(context.Background())

	p.mu.Lock()
	cur, adjusted, paused := p.cur, p.adjusted, p.pausedUntil
	p.mu.Unlock()
	if cur != 30*time.Minute || adjusted {
		t.Fatalf("睡醒应恢复基础节奏，得到 %v adjusted=%v", cur, adjusted)
	}
	if !paused.IsZero() {
		t.Fatal("睡醒后暂停记录应清除")
	}
}

// 停机期间暂停到点：重启等同睡醒，节奏回基础值，不让睡前的节奏越过暂停期存活。
func TestInitWakesFromExpiredPause(t *testing.T) {
	store := mustStore(t, t.TempDir())
	stateDir := t.TempDir()
	ictx := plugin.InitContext{
		StateDir: stateDir, Sessions: store, RunTurn: noTurn,
		NewSession: func() (string, error) { m, err := store.Create(); return m.ID, err },
	}
	persistState(stateDir, state{
		IntervalSeconds: int(90 * time.Minute / time.Second),
		Adjusted:        true,
		LastBeat:        time.Now().Add(-9 * time.Hour),
		PausedUntil:     time.Now().Add(-time.Hour),
		PausedAt:        time.Now().Add(-9 * time.Hour),
	})

	p := New()
	if err := p.Init(ictx, map[string]any{"interval_minutes": 30}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	p.Stop()

	p.mu.Lock()
	cur, adjusted, paused := p.cur, p.adjusted, p.pausedUntil
	p.mu.Unlock()
	if cur != 30*time.Minute || adjusted {
		t.Fatalf("停机期间暂停到点，重启应恢复基础节奏，得到 %v adjusted=%v", cur, adjusted)
	}
	if !paused.IsZero() {
		t.Fatal("已到点的暂停不该复活")
	}
}

// TurnPrompt 亮出当前节奏；暂停时改报暂停；固定节奏不注入（调整会被拒绝，
// 亮出来只会诱导模型去调）。
func TestTurnPrompt(t *testing.T) {
	p, _ := newInited(t, noTurn, map[string]any{"interval_minutes": 30})
	out, err := p.TurnPrompt(context.Background(), plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "30 分钟") || !strings.Contains(out, "set_heartbeat_interval") {
		t.Fatalf("应亮出当前节奏与调整途径: %q", out)
	}

	p.mu.Lock()
	p.pausedUntil = time.Now().Add(2 * time.Hour)
	p.mu.Unlock()
	out, _ = p.TurnPrompt(context.Background(), plugin.TurnEvent{})
	if !strings.Contains(out, "暂停") {
		t.Fatalf("暂停期间应报暂停: %q", out)
	}

	ps, _ := newInited(t, noTurn, map[string]any{"interval_minutes": 30, "dynamic": false})
	out, _ = ps.TurnPrompt(context.Background(), plugin.TurnEvent{})
	if out != "" {
		t.Fatalf("固定节奏不该注入: %q", out)
	}
}
