package usage

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// budgetAt 造一份带账本的预算：先在账本里记下 spent 个 prompt token，再按 now 建预算。
func budgetAt(t *testing.T, s Settings, spent int, now time.Time) (*Budget, *Ledger, string) {
	t.Helper()
	return budgetAtLookup(t, s, spent, now, nil)
}

// budgetAtLookup 与 budgetAt 相同，但可以带上价格表。
func budgetAtLookup(t *testing.T, s Settings, spent int, now time.Time, lookup PriceLookup) (*Budget, *Ledger, string) {
	t.Helper()
	dir := t.TempDir()
	l := mustOpen(t, dir)
	if spent > 0 {
		mustAppend(t, l, Entry{TS: now, Kind: KindTurn, Prompt: spent})
	}
	return NewBudget(s, l, lookup, now), l, dir
}

func limitSettings(tokens int64, over string, warn int) Settings {
	s := DefaultSettings()
	s.MonthlyTokens = tokens
	s.Over = over
	s.WarnPercent = warn
	return s
}

// 限额为 0 就是不设限：花多少都不拦、不提醒。
func TestBudgetNoLimitNeverBlocks(t *testing.T) {
	now := time.Now()
	b, _, _ := budgetAt(t, DefaultSettings(), 10_000, now)
	st := b.Status(now)
	if st.Enabled || st.Warn || st.Exceeded {
		t.Fatalf("不设限时不该有提醒或拦截: %+v", st)
	}
	if _, blocked := b.Blocked(now, false); blocked {
		t.Fatal("不设限时后台也不该被拦")
	}
	if st.SpentTokens != 10_000 {
		t.Errorf("已用 token 应照实报出: %+v", st)
	}
}

func TestBudgetWarnThreshold(t *testing.T) {
	now := time.Now()
	s := limitSettings(1000, OverSkipBackground, 80)

	// 未到阈值：79%
	b, _, _ := budgetAt(t, s, 790, now)
	if st := b.Status(now); st.Warn || st.Exceeded {
		t.Fatalf("79%% 不该提醒: %+v", st)
	}
	// 到达阈值：80%
	b, _, _ = budgetAt(t, s, 800, now)
	st := b.Status(now)
	if !st.Warn || st.Exceeded {
		t.Fatalf("80%% 应提醒但未超限: %+v", st)
	}
	if _, blocked := b.Blocked(now, false); blocked {
		t.Fatal("只提醒不该拦")
	}
}

func TestBudgetExceeded(t *testing.T) {
	now := time.Now()
	s := limitSettings(1000, OverSkipBackground, 80)

	// 差一个 token：未超
	b, _, _ := budgetAt(t, s, 999, now)
	if st := b.Status(now); st.Exceeded {
		t.Fatalf("999/1000 不该算超限: %+v", st)
	}
	// 恰好到达：超限（>=），此时不再是「提醒」
	b, _, _ = budgetAt(t, s, 1000, now)
	st := b.Status(now)
	if !st.Exceeded || st.Warn {
		t.Fatalf("到达限额应判超限且不再只是提醒: %+v", st)
	}
	// 超过
	b, _, _ = budgetAt(t, s, 1500, now)
	if st := b.Status(now); !st.Exceeded {
		t.Fatalf("1500/1000 应超限: %+v", st)
	}
}

// skip_background：后台拦下，前台照常。
func TestBudgetSkipBackground(t *testing.T) {
	now := time.Now()
	b, _, _ := budgetAt(t, limitSettings(100, OverSkipBackground, 80), 100, now)

	if _, blocked := b.Blocked(now, false); !blocked {
		t.Fatal("skip_background 下后台轮次应被拦")
	}
	if _, blocked := b.Blocked(now, true); blocked {
		t.Fatal("skip_background 下前台轮次不该被拦")
	}
}

// block_all：前台也拦。
func TestBudgetBlockAll(t *testing.T) {
	now := time.Now()
	b, _, _ := budgetAt(t, limitSettings(100, OverBlockAll, 80), 200, now)

	for _, interactive := range []bool{false, true} {
		st, blocked := b.Blocked(now, interactive)
		if !blocked {
			t.Fatalf("block_all 下 interactive=%v 也应被拦", interactive)
		}
		if st.LimitTokens != 100 || st.SpentTokens != 200 {
			t.Errorf("快照应带上限额与已用: %+v", st)
		}
	}
}

// 记录刚落盘，预算就该知道：请求路径上不能等下一次扫盘。
func TestBudgetObserveTakesEffectImmediately(t *testing.T) {
	now := time.Now()
	b, _, _ := budgetAt(t, limitSettings(100, OverBlockAll, 80), 0, now)
	if _, blocked := b.Blocked(now, true); blocked {
		t.Fatal("还没花钱不该拦")
	}
	b.Observe(Entry{TS: now, Kind: KindTurn, Prompt: 60, Completion: 40})
	if st := b.Status(now); st.SpentTokens != 100 || !st.Exceeded {
		t.Fatalf("Observe 后应立即生效: %+v", st)
	}
	if _, blocked := b.Blocked(now, true); !blocked {
		t.Fatal("Observe 到限额后应拦下")
	}
}

// 别的月份的迟到记录不能混进当月。
func TestBudgetObserveIgnoresOtherMonths(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.Local)
	b, _, _ := budgetAt(t, limitSettings(1000, OverBlockAll, 80), 0, now)

	// Day 明确写着上个月：即使 TS 是现在也不计入
	b.Observe(Entry{TS: now, Day: "2026-10-31", Kind: KindTurn, Prompt: 500})
	if got := b.Status(now).SpentTokens; got != 0 {
		t.Fatalf("上个月的条目不该累加，得到 %d", got)
	}
	// Day 为空时按 TS 判归属：本月条目照常计入
	b.Observe(Entry{TS: now, Kind: KindTurn, Prompt: 30, Completion: 20})
	if got := b.Status(now).SpentTokens; got != 50 {
		t.Fatalf("本月条目应累加，得到 %d", got)
	}
}

// 跨月时重置并重扫：上个月的额度不再占着，新月份账本里的记录立刻算进来。
func TestBudgetRollsOverMonth(t *testing.T) {
	nov := time.Date(2026, 11, 20, 12, 0, 0, 0, time.Local)
	dir := t.TempDir()
	l := mustOpen(t, dir)
	mustAppend(t, l, Entry{TS: nov.Add(-48 * time.Hour), Kind: KindTurn, Prompt: 900}) // 11-18
	mustAppend(t, l, Entry{TS: time.Date(2026, 12, 2, 9, 0, 0, 0, time.Local), Kind: KindTurn, Prompt: 300})

	b := NewBudget(limitSettings(1000, OverBlockAll, 80), l, nil, nov)
	if st := b.Status(nov); st.SpentTokens != 900 || !st.Warn {
		t.Fatalf("11 月应只算 11 月的 900: %+v", st)
	}

	dec := time.Date(2026, 12, 3, 9, 0, 0, 0, time.Local)
	if st := b.Status(dec); st.Month != "2026-12" || st.SpentTokens != 300 {
		t.Fatalf("12 月应重扫为 300: %+v", st)
	}
	// 回到上个月（月底发出的旧请求拖到新月份才查）：不倒退预算状态，看到的仍是
	// 12 月那份快照，而不是把月份拨回 11 月、清掉 12 月已经记下的数。
	if st := b.Status(nov); st.Month != "2026-12" || st.SpentTokens != 300 {
		t.Fatalf("旧月份的时间不该倒退预算状态: %+v", st)
	}
}

// 账本为 nil 或扫描失败：标记降级，一律不拦——防呆优先于硬保证。
func TestBudgetDegradedNeverBlocks(t *testing.T) {
	now := time.Now()

	// 没有账本
	noLedger := NewBudget(limitSettings(1, OverBlockAll, 80), nil, nil, now)
	if got := noLedger.Status(now); !got.Degraded {
		t.Fatalf("没有账本应标降级: %+v", got)
	}
	if _, blocked := noLedger.Blocked(now, false); blocked {
		t.Fatal("降级时不该拦")
	}

	// 扫描失败：目录名里带 NUL，任何平台上都打不开，等同于一次真实的读盘错误
	l := mustOpen(t, "bad\x00dir")
	b := NewBudget(limitSettings(1, OverBlockAll, 80), l, nil, now)
	if got := b.Status(now); !got.Degraded {
		t.Fatalf("账本读不出来应标降级: %+v", got)
	}
	if _, blocked := b.Blocked(now, true); blocked {
		t.Fatal("降级时前台也不该拦")
	}
	// 扫描侧的降级是可恢复的：换成能读的账本重扫一次就清了
	b.ledger = mustOpen(t, t.TempDir())
	b.mu.Lock()
	b.rescanLocked(now)
	b.mu.Unlock()
	if got := b.Status(now); got.Degraded {
		t.Fatalf("重扫成功后不该再标降级: %+v", got)
	}
}

// 设置读不出来是另一类降级：它不会因为账本能读而变好，只能由调用方确认后清除。
func TestBudgetMarkDegradedAndReady(t *testing.T) {
	now := time.Now()
	b, _, _ := budgetAt(t, limitSettings(1, OverBlockAll, 80), 1, now)

	b.MarkDegraded()
	st := b.Status(now)
	if !st.Degraded || !st.Exceeded {
		t.Fatalf("设置降级后状态应同时报出超限事实: %+v", st)
	}
	if _, blocked := b.Blocked(now, true); blocked {
		t.Fatal("设置降级时不该拦")
	}

	b.MarkReady()
	if st := b.Status(now); st.Degraded {
		t.Fatalf("MarkReady 后不该再降级: %+v", st)
	}
	if _, blocked := b.Blocked(now, true); !blocked {
		t.Fatal("恢复后应重新按超限拦截")
	}
}

// 非法的热更新保留旧设置，不会把一份能用的预算换成半份。
func TestBudgetSetSettingsKeepsOldOnInvalid(t *testing.T) {
	now := time.Now()
	b, _, _ := budgetAt(t, limitSettings(100, OverBlockAll, 80), 100, now)

	bad := limitSettings(0, "whatever", 80) // 未知的 over
	if err := b.SetSettings(bad); err == nil {
		t.Fatal("非法的 over 应报错")
	}
	if st := b.Status(now); st.LimitTokens != 100 || st.Over != OverBlockAll || !st.Exceeded {
		t.Fatalf("失败的热更新不该改动旧设置: %+v", st)
	}

	// 合法更新立即生效
	if err := b.SetSettings(limitSettings(0, OverSkipBackground, 50)); err != nil {
		t.Fatal(err)
	}
	if st := b.Status(now); st.Enabled || st.LimitTokens != 0 || st.Exceeded {
		t.Fatalf("合法更新应生效: %+v", st)
	}
}

// 设置侧的降级不会被一次成功的重扫抹掉：账本能读不代表设置好了。
func TestBudgetRescanKeepsSettingsDegraded(t *testing.T) {
	now := time.Now()
	b, _, _ := budgetAt(t, limitSettings(100, OverBlockAll, 80), 0, now)
	b.MarkDegraded()
	if st := b.Status(now); !st.Degraded {
		t.Fatalf("应保持降级: %+v", st)
	}
	// 跨月重扫成功也不清
	if st := b.Status(now.AddDate(0, 1, 0)); !st.Degraded {
		t.Fatalf("重扫成功不该清掉降级: %+v", st)
	}
}

// 扫描失败后不是整月作废：隔 rescanRetry 再试一次，账本读得回来就当场恢复。
func TestBudgetRescansAfterScanFailure(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.Local)
	l := mustOpen(t, t.TempDir())
	mustAppend(t, l, Entry{TS: now, Kind: KindTurn, Prompt: 400})
	b := NewBudget(limitSettings(1000, OverBlockAll, 80), l, nil, now)
	if st := b.Status(now); st.SpentTokens != 400 || st.Degraded {
		t.Fatalf("开跑时应读到 400: %+v", st)
	}

	// 账本暂时读不出来（目录被占住、盘掉线）：模拟一次失败的扫描
	b.mu.Lock()
	b.ledger = nil
	b.rescanLocked(now)
	b.mu.Unlock()

	// 账本回来了（换成一份有数据的新账本），但重试间隔还没到：仍是降级、已用为 0
	back := mustOpen(t, t.TempDir())
	mustAppend(t, back, Entry{TS: now, Kind: KindTurn, Prompt: 700})
	b.mu.Lock()
	b.ledger = back
	b.mu.Unlock()
	early := now.Add(10 * time.Second)
	st := b.Status(early)
	if !st.Degraded || st.SpentTokens != 0 {
		t.Fatalf("重试间隔之前不该重扫: %+v", st)
	}
	for _, interactive := range []bool{false, true} {
		if _, blocked := b.Blocked(early, interactive); blocked {
			t.Fatalf("降级期间 interactive=%v 不该拦", interactive)
		}
	}

	// 过了重试间隔：自动重扫，已用恢复、降级清除
	late := now.Add(rescanRetry + time.Second)
	st = b.Status(late)
	if st.Degraded || st.SpentTokens != 700 {
		t.Fatalf("重试后应恢复: %+v", st)
	}
	if _, blocked := b.Blocked(late, true); blocked {
		t.Fatal("700/1000 未超限，恢复后也不该拦")
	}
}

// warn_percent 的两端：0 的意思是「一花就提醒」（零用量不该亮），100 要等真到限额
// ——而那时判据已经是 Exceeded，Warn 不再成立。
func TestBudgetWarnBounds(t *testing.T) {
	now := time.Now()
	cases := []struct {
		warn  int
		spent int
		want  bool
	}{
		{0, 0, false},
		{0, 1, true},
		{100, 999, false},
		{100, 1000, false},
	}
	for _, c := range cases {
		b, _, _ := budgetAt(t, limitSettings(1000, OverBlockAll, c.warn), c.spent, now)
		if got := b.Status(now).Warn; got != c.want {
			t.Errorf("warn=%d spent=%d: Warn=%v，期望 %v", c.warn, c.spent, got, c.want)
		}
	}
}

// 非法设置退回默认值时必须标降级：那是「写坏了」，与「本来就没设限」不是一回事，
// 状态里分得出来，用户才会去看那份文件。
func TestBudgetInvalidSettingsDegrade(t *testing.T) {
	now := time.Now()
	b, _, _ := budgetAt(t, limitSettings(1000, "whatever", 80), 5, now)
	st := b.Status(now)
	if !st.Degraded || st.Enabled || st.Exceeded {
		t.Fatalf("非法设置应退回不设限并标降级: %+v", st)
	}
}

// 错误值可被 errors.Is 认出，发起方据此区分「预算用尽」与普通失败。
func TestBudgetErrorIdentity(t *testing.T) {
	if !errors.Is(ErrBudgetExceeded, ErrBudgetExceeded) {
		t.Fatal("ErrBudgetExceeded 应可自匹配")
	}
}

// /api/status 的契约：不设限时 limit_tokens 整个省略（前端据此判断「没有限额」，
// 也避免 0 被当成一个上限）；设了限就必须是一个正数，enabled 与它同时出现。
func TestBudgetStatusJSONContract(t *testing.T) {
	decode := func(t *testing.T, st BudgetStatus) map[string]json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	now := time.Now()
	noLimit, _, _ := budgetAt(t, DefaultSettings(), 500, now)
	m := decode(t, noLimit.Status(now))
	if _, ok := m["limit_tokens"]; ok {
		t.Errorf("不设限时不该下发 limit_tokens: %s", m["limit_tokens"])
	}

	limited, _, _ := budgetAt(t, limitSettings(2_000, OverSkipBackground, 80), 1_100, now)
	m = decode(t, limited.Status(now))
	var limit int64
	if err := json.Unmarshal(m["limit_tokens"], &limit); err != nil {
		t.Fatalf("设限时 limit_tokens 应存在且是数字: %v", err)
	}
	if limit != 2_000 {
		t.Errorf("limit_tokens = %d, want 2000", limit)
	}
	var enabled bool
	if err := json.Unmarshal(m["enabled"], &enabled); err != nil || !enabled {
		t.Errorf("设限时 enabled 应为 true: %v / %v", enabled, err)
	}
	var spent int64
	if err := json.Unmarshal(m["spent_tokens"], &spent); err != nil || spent != 1_100 {
		t.Errorf("spent_tokens = %d (%v), want 1100", spent, err)
	}
}
