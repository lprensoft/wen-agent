package heartbeat

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wen/internal/cue"
	"wen/internal/plugin"
)

func drainCues() { cue.Take(time.Now().Add(24 * time.Hour)) }

func postCue(text string) {
	cue.Post(cue.Cue{Source: "test", Key: text, Text: text, Expire: time.Now().Add(time.Hour)})
}

// 有待说的理由时下一拍提前到最快间隔；固定节奏不提前；暂停压过一切。
func TestNextBeatPulledForwardByCue(t *testing.T) {
	drainCues()
	defer drainCues()
	p, _ := newInited(t, noTurn, map[string]any{
		"interval_minutes": 60, "min_minutes": 5, "max_minutes": 120,
	})

	p.mu.Lock()
	p.lastBeat = time.Now()
	base := p.nextBeatLocked()
	p.mu.Unlock()

	postCue("下雨了")
	p.mu.Lock()
	pulled := p.nextBeatLocked()
	p.mu.Unlock()
	if !pulled.Before(base) {
		t.Errorf("有理由待说时下一拍应提前: base=%v pulled=%v", base, pulled)
	}
	if got := pulled.Sub(p.lastBeat); got != 5*time.Minute {
		t.Errorf("应提前到最快间隔，得到 %v", got)
	}

	// 暂停仍然压过提前：睡着的人不为一场雨叫醒
	p.mu.Lock()
	p.pausedUntil = time.Now().Add(8 * time.Hour)
	paused := p.nextBeatLocked()
	p.pausedUntil = time.Time{}
	p.mu.Unlock()
	if paused.Sub(p.lastBeat) < 8*time.Hour {
		t.Errorf("暂停期间不该被理由叫醒: %v", paused)
	}
	drainCues()

	// 固定节奏雷打不动
	q, _ := newInited(t, noTurn, map[string]any{
		"interval_minutes": 60, "min_minutes": 5, "max_minutes": 120, "dynamic": false,
	})
	postCue("下雨了")
	q.mu.Lock()
	q.lastBeat = time.Now()
	fixed := q.nextBeatLocked()
	q.mu.Unlock()
	if got := fixed.Sub(q.lastBeat); got != 60*time.Minute {
		t.Errorf("固定节奏不该提前: %v", got)
	}
}

func turnCueCtx(write string, read []string) context.Context {
	ctx := plugin.WithTurnOrigin(context.Background(), "heartbeat")
	return plugin.WithScope(ctx, plugin.Scope{Write: write, Read: read})
}

// 别的人格域的理由留在队列里，但不能把这一人格的心跳反复提前叫醒。
func TestNextBeatIgnoresOtherDomainCuesForPull(t *testing.T) {
	drainCues()
	defer drainCues()
	p, _ := newInited(t, noTurn, map[string]any{
		"interval_minutes": 60, "min_minutes": 5, "max_minutes": 120,
	})
	p.takeCues(turnCueCtx("outer", []string{"outer"}), "") // 记住上一次心跳的可读域
	p.mu.Lock()
	p.lastBeat = time.Now()
	base := p.nextBeatLocked()
	p.mu.Unlock()

	cue.Post(cue.Cue{Source: "agenda", Key: "inner", Domain: "inner", Text: "里", Expire: time.Now().Add(time.Hour)})
	p.mu.Lock()
	notPulled := p.nextBeatLocked()
	p.mu.Unlock()
	if notPulled.Before(base) {
		t.Errorf("别的域的理由不该把这一轮提前: base=%v got=%v", base, notPulled)
	}

	cue.Post(cue.Cue{Source: "agenda", Key: "outer", Domain: "outer", Text: "表", Expire: time.Now().Add(time.Hour)})
	p.mu.Lock()
	pulled := p.nextBeatLocked()
	p.mu.Unlock()
	if !pulled.Before(base) {
		t.Errorf("本域的理由应提前下一拍: base=%v got=%v", base, pulled)
	}
}

// 用户从表人格切到里人格后，调度器要按新人格判断提前与否：
// 里人格的提醒不能因为缓存还停在表人格而错过提前唤醒。
func TestPersonaSwitchRefreshesPullScope(t *testing.T) {
	drainCues()
	defer drainCues()
	p, store := newInited(t, noTurn, map[string]any{
		"interval_minutes": 60, "min_minutes": 5, "max_minutes": 120,
	})
	sess, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetLastActive(sess.ID, time.Now().Add(-time.Minute))
	p.takeCues(turnCueCtx("outer", []string{"outer"}), sess.ID) // 上一次心跳属于表人格
	p.mu.Lock()
	p.lastBeat = time.Now()
	base := p.nextBeatLocked()
	p.mu.Unlock()

	cue.Post(cue.Cue{Source: "agenda", Key: "inner", Domain: "inner", Text: "里", Expire: time.Now().Add(15 * time.Minute)})
	p.mu.Lock()
	notPulled := p.nextBeatLocked()
	p.mu.Unlock()
	if notPulled.Before(base) {
		t.Fatalf("切换前不该按里人格的理由提前: base=%v got=%v", base, notPulled)
	}

	ctx := plugin.WithScope(context.Background(), plugin.Scope{Write: "inner", Read: []string{"outer", "inner"}})
	p.OnTurnEnd(ctx, plugin.TurnEndEvent{SessionID: sess.ID, Interactive: true, EndedAt: time.Now()})

	p.mu.Lock()
	pulled := p.nextBeatLocked()
	p.mu.Unlock()
	if !pulled.Before(base) {
		t.Errorf("切到里人格后，里人格的理由应提前下一拍: base=%v got=%v", base, pulled)
	}
	if got := pulled.Sub(p.lastBeat); got != 5*time.Minute {
		t.Errorf("应提前到最快间隔，得到 %v", got)
	}
}

// 并发会话按开始时间决定落点、按结束顺序回调：先开始后结束的表人格会话
// 不能把后开始先结束的里人格会话的可见域盖掉。
func TestConcurrentSessionsKeepOwnPullScope(t *testing.T) {
	drainCues()
	defer drainCues()
	p, store := newInited(t, noTurn, map[string]any{
		"interval_minutes": 60, "min_minutes": 5, "max_minutes": 120,
	})
	a, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	startA, startB := now.Add(-20*time.Minute), now.Add(-10*time.Minute)
	_ = store.SetLastActive(a.ID, startA)
	_ = store.SetLastActive(b.ID, startB)

	// B（里人格）后开始、先结束；A（表人格）先开始、后结束。
	p.OnTurnEnd(plugin.WithScope(context.Background(), plugin.Scope{Write: "inner", Read: []string{"outer", "inner"}}),
		plugin.TurnEndEvent{SessionID: b.ID, Interactive: true, StartedAt: startB, EndedAt: startB.Add(time.Minute)})
	p.OnTurnEnd(plugin.WithScope(context.Background(), plugin.Scope{Write: "outer", Read: []string{"outer"}}),
		plugin.TurnEndEvent{SessionID: a.ID, Interactive: true, StartedAt: startA, EndedAt: startA.Add(10 * time.Minute)})

	p.mu.Lock()
	p.lastBeat = now
	base := p.nextBeatLocked()
	p.mu.Unlock()
	cue.Post(cue.Cue{Source: "agenda", Key: "inner", Domain: "inner", Text: "里", Expire: now.Add(15 * time.Minute)})
	p.mu.Lock()
	pulled := p.nextBeatLocked()
	p.mu.Unlock()
	if !pulled.Before(base) {
		t.Fatalf("落点是后开始的里人格会话，应被它的理由提前: base=%v got=%v", base, pulled)
	}
	if got := pulled.Sub(now); got != 5*time.Minute {
		t.Errorf("应提前到最快间隔，得到 %v", got)
	}
}

// 真人轮次的可见域在 TurnPrompt 阶段就记下：用户生成中途点停止时不会再走
// OnTurnEnd，不能等到那时才更新缓存。
func TestTurnPromptRecordsScopeBeforeTurnEnds(t *testing.T) {
	drainCues()
	defer drainCues()
	p, store := newInited(t, noTurn, map[string]any{
		"interval_minutes": 60, "min_minutes": 5, "max_minutes": 120,
	})
	sess, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetLastActive(sess.ID, time.Now().Add(-time.Minute))

	// 先把这个会话缓存成表人格：里人格的提醒此刻不该提前心跳。
	p.takeCues(turnCueCtx("outer", []string{"outer"}), sess.ID)
	p.mu.Lock()
	p.lastBeat = time.Now()
	base := p.nextBeatLocked()
	p.mu.Unlock()
	cue.Post(cue.Cue{Source: "agenda", Key: "inner", Domain: "inner", Text: "里", Expire: time.Now().Add(15 * time.Minute)})
	p.mu.Lock()
	notPulled := p.nextBeatLocked()
	p.mu.Unlock()
	if notPulled.Before(base) {
		t.Fatalf("切换前里人格的理由不该提前: base=%v got=%v", base, notPulled)
	}

	ctx := plugin.WithInteractive(plugin.WithScope(context.Background(),
		plugin.Scope{Write: "inner", Read: []string{"outer", "inner"}}))
	if _, err := p.TurnPrompt(ctx, plugin.TurnEvent{SessionID: sess.ID}); err != nil {
		t.Fatal(err)
	}
	// 模拟生成中途取消：此后不调用 OnTurnEnd。

	p.mu.Lock()
	pulled := p.nextBeatLocked()
	p.mu.Unlock()
	if !pulled.Before(base) {
		t.Fatalf("取消的真人轮次也应把可见域换成里人格: base=%v got=%v", base, pulled)
	}
	if got := pulled.Sub(p.lastBeat); got != 5*time.Minute {
		t.Errorf("应提前到最快间隔，得到 %v", got)
	}
}

// 目标会话换到另一个已有缓存的会话时，该会话自身的可见域没变，循环同样要醒来：
// 否则公告板上的理由要等常规间隔才被看见，可能已经过期。
func TestPendingCueWakesWhenTargetSessionChanges(t *testing.T) {
	drainCues()
	defer drainCues()
	p, store := newInited(t, noTurn, map[string]any{
		"interval_minutes": 60, "min_minutes": 5, "max_minutes": 120,
	})
	sess, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetLastActive(sess.ID, time.Now().Add(-time.Minute))
	ctx := plugin.WithScope(plugin.WithTurnOrigin(context.Background(), "lark_bot"),
		plugin.Scope{Write: "outer", Read: []string{"outer"}})
	ev := plugin.TurnEndEvent{SessionID: sess.ID, Origin: "lark_bot", Interactive: true, EndedAt: time.Now()}

	p.OnTurnEnd(ctx, ev) // 第一次记录这个会话的可见域
	for {
		select {
		case <-p.wake:
			continue
		default:
		}
		break
	}
	cue.Post(cue.Cue{Source: "agenda", Key: "outer", Domain: "outer", Text: "表", Expire: time.Now().Add(15 * time.Minute)})
	p.OnTurnEnd(ctx, ev) // 可见域没变，但理由在等：必须有唤醒

	select {
	case <-p.wake:
	default:
		t.Fatal("公告板有待发理由时，真人轮次收尾应叫醒循环重算目标会话")
	}
}

// 心跳撞上会话忙时不能把临期提醒推到最快间隔之后：回滚起点、短重试，
// 真人轮次收尾（会话锁释放）后立刻再试。
func TestBusyBeatRetriesSoonWhileCuePending(t *testing.T) {
	drainCues()
	defer drainCues()
	busy := true
	turn := func(context.Context, string, string) (string, error) {
		if busy {
			return "", plugin.ErrSessionBusy
		}
		return "ok", nil
	}
	p, store := newInited(t, turn, map[string]any{
		"interval_minutes": 60, "min_minutes": 5, "max_minutes": 120,
	})
	sess, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetLastActive(sess.ID, time.Now().Add(-time.Minute))

	now := time.Now()
	prev := now.Add(-5 * time.Minute) // 最快间隔已到，这一拍本该现在开口
	p.mu.Lock()
	p.lastBeat = prev
	p.mu.Unlock()
	cue.Post(cue.Cue{Source: "agenda", Key: "soon", Domain: "", Text: "快到期", Expire: now.Add(2 * time.Minute)})

	if err := p.beatAt(context.Background(), now); err != nil {
		t.Fatalf("会话忙不该报错 = %v", err)
	}
	p.mu.Lock()
	lb, retry, next := p.lastBeat, p.beatRetryAt, p.nextBeatLocked()
	retrySess := p.beatRetrySession
	p.mu.Unlock()
	if !lb.Equal(prev) {
		t.Fatalf("会话忙应回滚 lastBeat：%v → %v", prev, lb)
	}
	if !retry.Equal(now.Add(busyRetryDelay)) || retrySess != sess.ID {
		t.Fatalf("应安排短重试并记下会话：%v / %q", retry, retrySess)
	}
	if !next.Equal(retry) {
		t.Fatalf("下一拍应是重试时刻而不是最快间隔之后：next=%v retry=%v", next, retry)
	}

	// OnTurnEnd 仍在轮次锁内：不能在这里清掉等待，否则重试会再撞一次忙。
	p.OnTurnEnd(plugin.WithScope(plugin.WithTurnOrigin(context.Background(), "lark_bot"), plugin.Scope{}),
		plugin.TurnEndEvent{SessionID: sess.ID, Origin: "lark_bot", Interactive: true, EndedAt: now.Add(time.Second)})
	p.mu.Lock()
	retry = p.beatRetryAt
	p.mu.Unlock()
	if retry.IsZero() {
		t.Fatal("轮次锁未释放前不该清掉重试等待")
	}

	// 锁真正释放后才清掉等待，下一拍立刻到期。
	p.OnSessionIdle(context.Background(), sess.ID)
	p.mu.Lock()
	retry, next = p.beatRetryAt, p.nextBeatLocked()
	p.mu.Unlock()
	if !retry.IsZero() {
		t.Fatalf("会话空闲后应清掉重试等待：%v", retry)
	}
	if next.After(now.Add(time.Second)) {
		t.Fatalf("会话空闲后应立刻重试：next=%v", next)
	}

	busy = false
	if err := p.beatAt(context.Background(), now.Add(time.Second)); err != nil {
		t.Fatalf("重试 = %v", err)
	}
	p.mu.Lock()
	retry, lb = p.beatRetryAt, p.lastBeat
	p.mu.Unlock()
	if !retry.IsZero() || !lb.Equal(now.Add(time.Second)) {
		t.Fatalf("成功后不该再留重试：retry=%v lastBeat=%v", retry, lb)
	}
}

// 目标会话读不到的提醒不该触发忙重试：回滚起点却又不采用退避，会让下一拍
// 停在已到期的时刻，循环反复撞忙、反复写盘。
func TestBusyBeatIgnoresOtherDomainCues(t *testing.T) {
	drainCues()
	defer drainCues()
	turn := func(context.Context, string, string) (string, error) { return "", plugin.ErrSessionBusy }
	p, store := newInited(t, turn, map[string]any{
		"interval_minutes": 60, "min_minutes": 5, "max_minutes": 120,
	})
	sess, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetLastActive(sess.ID, time.Now().Add(-time.Minute))
	p.takeCues(turnCueCtx("outer", []string{"outer"}), sess.ID)

	now := time.Now()
	p.mu.Lock()
	p.lastBeat = now.Add(-61 * time.Minute) // 常规心跳已到期
	p.mu.Unlock()
	cue.Post(cue.Cue{Source: "agenda", Key: "inner", Domain: "inner", Text: "里", Expire: now.Add(time.Hour)})

	if err := p.beatAt(context.Background(), now); err != nil {
		t.Fatalf("会话忙不该报错 = %v", err)
	}
	p.mu.Lock()
	lb, retry, next := p.lastBeat, p.beatRetryAt, p.nextBeatLocked()
	p.mu.Unlock()
	if !lb.Equal(now) {
		t.Fatalf("读不到的理由不该回滚 lastBeat：%v", lb)
	}
	if !retry.IsZero() {
		t.Fatalf("读不到的理由不该安排重试：%v", retry)
	}
	if next.Before(now) {
		t.Fatalf("下一拍不应停在已到期的时刻：%v", next)
	}
}

// 模型重设间隔会推进倒计时，此前为撞忙安排的重试必须失效，不能盖掉新节奏。
func TestResetClockDropsBusyRetry(t *testing.T) {
	drainCues()
	defer drainCues()
	p, store := newInited(t, noTurn, map[string]any{
		"interval_minutes": 60, "min_minutes": 5, "max_minutes": 120,
	})
	sess, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetLastActive(sess.ID, time.Now().Add(-time.Minute))
	p.takeCues(turnCueCtx("outer", []string{"outer"}), sess.ID)
	cue.Post(cue.Cue{Source: "agenda", Key: "outer", Domain: "outer", Text: "表", Expire: time.Now().Add(time.Hour)})

	now := time.Now()
	p.mu.Lock()
	p.lastBeat = now.Add(-5 * time.Minute)
	p.beatRetryAt = now.Add(busyRetryDelay)
	p.beatRetrySession = sess.ID
	p.cur = 45 * time.Minute
	p.resetClockLocked(now)
	next := p.nextBeatLocked()
	retry := p.beatRetryAt
	p.mu.Unlock()
	if !retry.IsZero() {
		t.Fatalf("重设倒计时应清掉旧重试：%v", retry)
	}
	if !next.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("下一拍应按新倒计时与理由提前，而不是旧重试时刻：%v", next)
	}
}

// 判忙之后、登记重试之前会话就解锁了：通知会先到，不能因此把重试留到 30 秒后。
func TestBusyRetryIsImmediateWhenUnlockRaced(t *testing.T) {
	drainCues()
	defer drainCues()
	var (
		p   *Plugin
		sid string
	)
	turn := func(context.Context, string, string) (string, error) {
		p.OnSessionIdle(context.Background(), sid) // 解锁插在判忙与结果处理之间
		return "", plugin.ErrSessionBusy
	}
	p, store := newInited(t, turn, map[string]any{
		"interval_minutes": 60, "min_minutes": 5, "max_minutes": 120,
	})
	sess, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	sid = sess.ID
	_ = store.SetLastActive(sess.ID, time.Now().Add(-time.Minute))

	now := time.Now()
	p.mu.Lock()
	p.lastBeat = now.Add(-5 * time.Minute)
	p.mu.Unlock()
	cue.Post(cue.Cue{Source: "agenda", Key: "soon", Domain: "", Text: "快到期", Expire: now.Add(12 * time.Second)})

	if err := p.beatAt(context.Background(), now); err != nil {
		t.Fatalf("会话忙不该报错 = %v", err)
	}
	p.mu.Lock()
	retry, retrySess, next := p.beatRetryAt, p.beatRetrySession, p.nextBeatLocked()
	p.mu.Unlock()
	if !retry.Equal(now) || retrySess != sid {
		t.Fatalf("尝试期间已解锁，应立即重试：retry=%v sess=%q", retry, retrySess)
	}
	if next.After(now) {
		t.Fatalf("下一拍应立刻到期：%v", next)
	}
}

// 撞忙结果回来前倒计时已被重设：旧尝试不能再写回退避、覆盖新节奏。
func TestStaleBusyAttemptDoesNotWriteRetry(t *testing.T) {
	drainCues()
	defer drainCues()
	now := time.Now()
	var p *Plugin
	turn := func(context.Context, string, string) (string, error) {
		p.mu.Lock()
		p.cur = 45 * time.Minute
		p.resetClockLocked(now.Add(time.Millisecond)) // 另一个并发操作重设了倒计时
		p.mu.Unlock()
		return "", plugin.ErrSessionBusy
	}
	p, store := newInited(t, turn, map[string]any{
		"interval_minutes": 60, "min_minutes": 5, "max_minutes": 120,
	})
	sess, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetLastActive(sess.ID, time.Now().Add(-time.Minute))
	p.takeCues(turnCueCtx("outer", []string{"outer"}), sess.ID)
	cue.Post(cue.Cue{Source: "agenda", Key: "outer", Domain: "outer", Text: "表", Expire: now.Add(time.Hour)})

	p.mu.Lock()
	p.lastBeat = now.Add(-5 * time.Minute)
	p.mu.Unlock()
	if err := p.beatAt(context.Background(), now); err != nil {
		t.Fatalf("会话忙不该报错 = %v", err)
	}
	p.mu.Lock()
	lb, retry, next := p.lastBeat, p.beatRetryAt, p.nextBeatLocked()
	p.mu.Unlock()
	if !retry.IsZero() {
		t.Fatalf("旧尝试不该写回退避：%v", retry)
	}
	if !lb.Equal(now.Add(time.Millisecond)) {
		t.Fatalf("旧尝试不该回滚新的倒计时起点：%v", lb)
	}
	if want := lb.Add(5 * time.Minute); !next.Equal(want) {
		t.Fatalf("下一拍应按新倒计时与理由提前：next=%v want=%v", next, want)
	}
}

// 目标会话已经换人时，旧会话的退避不能拖住新目标。
func TestRetryOnlyAppliesToItsSession(t *testing.T) {
	drainCues()
	defer drainCues()
	p, store := newInited(t, noTurn, map[string]any{
		"interval_minutes": 60, "min_minutes": 5, "max_minutes": 120,
	})
	a, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_ = store.SetLastActive(a.ID, now.Add(-20*time.Minute))
	_ = store.SetLastActive(b.ID, now.Add(-10*time.Minute)) // 目标换到 B
	cue.Post(cue.Cue{Source: "agenda", Key: "shared", Domain: "", Text: "共享提醒", Expire: now.Add(12 * time.Second)})

	p.mu.Lock()
	p.lastBeat = now.Add(-5 * time.Minute)
	p.beatRetryAt = now.Add(30 * time.Second)
	p.beatRetrySession = a.ID
	next := p.nextBeatLocked()
	p.mu.Unlock()
	if next.After(now) {
		t.Fatalf("新目标应立刻开口，不该沿用旧会话的退避：%v", next)
	}
}

// 轮次结束通知可能晚到：它带的 EndedAt 早于本次尝试，时间戳不前进，但仍是一次
// 倒计时重设。撞忙结果不能据此把起点回滚，跳过刚生效的最快间隔。
func TestDelayedTurnEndDoesNotRollbackBeat(t *testing.T) {
	drainCues()
	defer drainCues()
	now := time.Now()
	var (
		p   *Plugin
		sid string
	)
	turn := func(context.Context, string, string) (string, error) {
		p.OnTurnEnd(context.Background(), plugin.TurnEndEvent{
			SessionID: sid, Interactive: true, EndedAt: now.Add(-time.Millisecond),
		})
		return "", plugin.ErrSessionBusy
	}
	p, store := newInited(t, turn, map[string]any{
		"interval_minutes": 60, "min_minutes": 5, "max_minutes": 120,
	})
	sess, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	sid = sess.ID
	_ = store.SetLastActive(sess.ID, now.Add(-time.Minute))

	p.mu.Lock()
	p.lastBeat = now.Add(-5 * time.Minute)
	p.mu.Unlock()
	cue.Post(cue.Cue{Source: "agenda", Key: "soon", Domain: "", Text: "快到期", Expire: now.Add(time.Minute)})

	if err := p.beatAt(context.Background(), now); err != nil {
		t.Fatalf("会话忙不该报错 = %v", err)
	}
	p.mu.Lock()
	lb, retry, next := p.lastBeat, p.beatRetryAt, p.nextBeatLocked()
	p.mu.Unlock()
	if !lb.Equal(now) {
		t.Fatalf("延迟到达的结束通知不该让旧尝试回滚起点：%v", lb)
	}
	if !retry.IsZero() {
		t.Fatalf("倒计时已重设，不该登记旧重试：%v", retry)
	}
	if want := now.Add(5 * time.Minute); !next.Equal(want) {
		t.Fatalf("下一拍应落在最快间隔之后：next=%v want=%v", next, want)
	}
}

// 撞忙回滚只改内存会让重启后的下一拍被推晚：最终采用的起点要同步落盘。
func TestBusyRollbackPersistsSchedulingStart(t *testing.T) {
	drainCues()
	defer drainCues()
	turn := func(context.Context, string, string) (string, error) { return "", plugin.ErrSessionBusy }
	p, store := newInited(t, turn, map[string]any{
		"interval_minutes": 60, "min_minutes": 5, "max_minutes": 120,
	})
	sess, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetLastActive(sess.ID, time.Now().Add(-time.Minute))

	now := time.Now()
	prev := now.Add(-40 * time.Minute)
	p.mu.Lock()
	p.lastBeat = prev
	p.mu.Unlock()
	cue.Post(cue.Cue{Source: "agenda", Key: "soon", Domain: "", Text: "快到期", Expire: now.Add(time.Minute)})

	if err := p.beatAt(context.Background(), now); err != nil {
		t.Fatalf("会话忙不该报错 = %v", err)
	}
	p.mu.Lock()
	lb := p.lastBeat
	st := p.loadStateLocked()
	p.mu.Unlock()
	if !lb.Equal(prev) {
		t.Fatalf("内存起点应回滚：%v", lb)
	}
	if !st.LastBeat.Equal(prev) {
		t.Fatalf("落盘起点应与会话恢复后的调度起点一致：%v", st.LastBeat)
	}
}

// 暂停这类路径先取快照、解锁后才写盘；撞忙回滚可能在中间写入更新的状态。
// 状态写盘按快照序号提交，旧快照无论早到还是晚到都不能覆盖新状态。
func TestStaleStateSnapshotDoesNotOverwriteNewer(t *testing.T) {
	for _, newFirst := range []bool{false, true} {
		name := "旧快照后写"
		if newFirst {
			name = "新快照先写"
		}
		t.Run(name, func(t *testing.T) {
			p, _ := newInited(t, noTurn, map[string]any{
				"interval_minutes": 60, "min_minutes": 5, "max_minutes": 120,
			})
			now := time.Now()
			prev := now.Add(-40 * time.Minute)

			p.mu.Lock()
			p.lastBeat = now // 暂停操作先取到的快照
			oldDir, oldSnap := p.snapshotStateLocked()
			p.lastBeat = prev // 随后撞忙回滚写入的新状态
			newDir, newSnap := p.snapshotStateLocked()
			p.mu.Unlock()

			if newFirst {
				p.commitState(newDir, newSnap)
				p.commitState(oldDir, oldSnap)
			} else {
				p.commitState(oldDir, oldSnap)
				p.commitState(newDir, newSnap)
			}
			st := p.loadStateLocked()
			if !st.LastBeat.Equal(prev) {
				t.Fatalf("旧快照覆盖了新状态：lastBeat=%v want=%v", st.LastBeat, prev)
			}
		})
	}
}

type countingSessions struct {
	plugin.SessionQuery
	n atomic.Int32
}

func (c *countingSessions) LastActive() (string, time.Time, error) {
	c.n.Add(1)
	return c.SessionQuery.LastActive()
}

// 公告板为空时，算下一拍不该为了可见域去扫描全部会话文件。
func TestNextBeatSkipsSessionScanWithoutCues(t *testing.T) {
	drainCues()
	defer drainCues()
	p, store := newInited(t, noTurn, map[string]any{
		"interval_minutes": 60, "min_minutes": 5, "max_minutes": 120,
	})
	count := &countingSessions{SessionQuery: store}
	p.mu.Lock()
	p.sessions = count
	p.lastBeat = time.Now()
	p.nextBeatLocked()
	p.mu.Unlock()
	if got := count.n.Load(); got != 0 {
		t.Fatalf("没有待发理由时不该扫描会话：%d 次", got)
	}

	cue.Post(cue.Cue{Source: "agenda", Key: "x", Domain: "", Text: "有", Expire: time.Now().Add(time.Hour)})
	p.mu.Lock()
	p.nextBeatLocked()
	p.mu.Unlock()
	if got := count.n.Load(); got == 0 {
		t.Fatal("有待发理由时应查询目标会话的可见域")
	}
}

// 心跳在可见域裁决之后领取理由，取走即清；下一次没有理由时不再出现该段。
func TestTurnPromptCarriesCues(t *testing.T) {
	drainCues()
	defer drainCues()
	p, _ := newInited(t, noTurn, nil)

	postCue("你所在的杭州刚下起了小雨。")
	ctx := turnCueCtx("outer", []string{"outer"})
	text, err := p.TurnPrompt(ctx, plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "【值得开口的事】") || !strings.Contains(text, "刚下起了小雨") {
		t.Errorf("心跳提示应带上开口理由:\n%s", text)
	}
	p.settleCues(true)
	if cue.Pending(time.Now()) {
		t.Error("送达后公告板应已清空")
	}

	text, err = p.TurnPrompt(ctx, plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "值得开口的事") {
		t.Errorf("无理由时不该出现该段:\n%s", text)
	}
}

// 只领取本轮可读域的理由，其他域的留在公告板上等它自己那一轮。
func TestTurnPromptFiltersCuesByDomain(t *testing.T) {
	drainCues()
	defer drainCues()
	p, _ := newInited(t, noTurn, nil)
	cue.Post(cue.Cue{Source: "agenda", Key: "outer", Domain: "outer", Text: "表人格的约定", Expire: time.Now().Add(time.Hour)})
	cue.Post(cue.Cue{Source: "agenda", Key: "inner", Domain: "inner", Text: "里人格的约定", Expire: time.Now().Add(time.Hour)})

	outerText, err := p.TurnPrompt(turnCueCtx("outer", []string{"outer"}), plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outerText, "表人格的约定") || strings.Contains(outerText, "里人格的约定") {
		t.Fatalf("表人格的提示混入了里人格的理由:\n%s", outerText)
	}
	p.settleCues(true)

	innerText, err := p.TurnPrompt(turnCueCtx("inner", []string{"outer", "inner"}), plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(innerText, "里人格的约定") {
		t.Fatalf("里人格那一轮没拿到自己的理由:\n%s", innerText)
	}
	p.settleCues(true)
}

// 轮次失败时把已领取的理由放回公告板，等下一拍再说。
func TestSettleCuesRestoresOnFailure(t *testing.T) {
	drainCues()
	defer drainCues()
	p, _ := newInited(t, noTurn, nil)
	postCue("下雨了")
	if _, err := p.TurnPrompt(turnCueCtx("outer", []string{"outer"}), plugin.TurnEvent{}); err != nil {
		t.Fatal(err)
	}
	if cue.Pending(time.Now()) {
		t.Fatal("TurnPrompt 之后理由应已被领取")
	}
	p.settleCues(false)
	if !cue.Pending(time.Now()) {
		t.Error("轮次失败时理由应被放回公告板")
	}
}
