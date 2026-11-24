package usage

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// budgetPrice 是预算测试用的价格表：按模型名给两种币种与一种没定价的模型。
// 一百万输入 token 恰好 1.00 元，测试里的数字因此可以直接心算。
func budgetPrice(_, model string) (Price, bool) {
	switch model {
	case "cny", "cacheback":
		// cacheback 故意不给缓存价：命中缓存的那部分按输入价估算，金额偏高、标不精确
		return Price{Currency: "CNY", Input: 1, Output: 2}, true
	case "usd":
		return Price{Currency: "USD", Input: 1, Output: 2}, true
	}
	return Price{}, false
}

// costSettings 造一份带金额上限的设置。币种用小写写，顺带确认它会被归一化成大写。
func costSettings(tokens int64, cost float64, over string, warn int) Settings {
	s := limitSettings(tokens, over, warn)
	s.MonthlyCost = cost
	s.Currency = "cny"
	return s
}

// costBudget 造一份账本里带一条指定记录的预算。
func costBudget(t *testing.T, s Settings, e Entry, now time.Time) *Budget {
	t.Helper()
	l := mustOpen(t, t.TempDir())
	mustAppend(t, l, e)
	return NewBudget(s, l, budgetPrice, now)
}

// writeFailBudget 造一份「目录读得出来、当天分片写不进去」的预算。
//
// 做法是建一个正常的 usage 目录，再把当天日分片那条路径建成目录：scanMonth 会跳过
// 目录（见它的 de.IsDir()），首次扫描因此成功；Append 去写同名的目录必然失败，正好
// 只测「写失败」。不能反过来把 usage 本身建成普通文件——Ubuntu 上首次 ReadDir 得到
// ENOTDIR，NewBudget 会把 Degraded 标上，与「写失败」混在一起。
func writeFailBudget(t *testing.T, s Settings, now time.Time) *Budget {
	t.Helper()
	return NewBudget(s, writeFailLedger(t, now), budgetPrice, now)
}

// writeFailLedger 造一份「目录读得出来、day 那天的日分片写不进去」的账本。道理与
// writeFailBudget 相同，只是那一天由调用方指定：跨月测试要失败的往往是另一个月。
func writeFailLedger(t *testing.T, day time.Time) *Ledger {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "usage")
	failShard(t, dir, day)
	l, err := Open(dir) // Open 是惰性的：路径写不进去要等 Append 才发现
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// failShard 把 day 那一天的日分片建成目录：扫描跳过它，Append 一定写不进去。
func failShard(t *testing.T, dir string, day time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	shard := filepath.Join(dir, day.In(time.Local).Format(dayFormat)+".jsonl")
	if err := os.Mkdir(shard, 0o700); err != nil {
		t.Fatal(err)
	}
}

// 金额达限拦后台、skip_background 下前台照常：判据与 token 维度一样是硬保证。
func TestBudgetCostBlocksBackground(t *testing.T) {
	now := time.Now()
	// 1.00 CNY 的花费，上限 0.5：已超
	b := costBudget(t, costSettings(0, 0.5, OverSkipBackground, 80),
		Entry{TS: now, Kind: KindTurn, Provider: "p", Model: "cny", Prompt: 1_000_000}, now)

	st := b.Status(now)
	if !st.Enabled || !st.CostReady || !st.Exceeded {
		t.Fatalf("金额超限应判超限并报出可用的价格状态: %+v", st)
	}
	if !sameAmount(st.SpentCost, 1) || !sameAmount(st.LimitCost, 0.5) || st.Currency != "CNY" {
		t.Fatalf("金额与币种应照实报出: %+v", st)
	}
	if _, blocked := b.Blocked(now, false); !blocked {
		t.Fatal("skip_background 下后台轮次应被金额上限拦")
	}
	if _, blocked := b.Blocked(now, true); blocked {
		t.Fatal("skip_background 下前台轮次不该被拦")
	}

	// block_all 连前台一起停
	all := costBudget(t, costSettings(0, 0.5, OverBlockAll, 80),
		Entry{TS: now, Kind: KindTurn, Model: "cny", Prompt: 1_000_000}, now)
	if _, blocked := all.Blocked(now, true); !blocked {
		t.Fatal("block_all 下前台也应被金额上限拦")
	}
}

// 任一维度超限都拦：token 到限、金额到限、两者都到限，结果一样。
func TestBudgetEitherDimensionExceeds(t *testing.T) {
	now := time.Now()
	entry := Entry{TS: now, Kind: KindTurn, Model: "cny", Prompt: 1_000_000} // 1.00 CNY / 100 万 token
	cases := []struct {
		name string
		s    Settings
		want bool
	}{
		{"都不设限", limitSettings(0, OverBlockAll, 80), false},
		{"token 未到、金额未到", limitSettings(2_000_000, OverBlockAll, 80), false},
		{"token 到限", limitSettings(1_000_000, OverBlockAll, 80), true},
		{"金额到限", costSettings(0, 1.0, OverBlockAll, 80), true},
		{"两者都到限", costSettings(1_000_000, 1.0, OverBlockAll, 80), true},
	}
	for _, c := range cases {
		b := costBudget(t, c.s, entry, now)
		st := b.Status(now)
		if st.Exceeded != c.want {
			t.Errorf("%s: Exceeded = %v, want %v (%+v)", c.name, st.Exceeded, c.want, st)
		}
		if _, blocked := b.Blocked(now, false); blocked != c.want {
			t.Errorf("%s: blocked = %v, want %v", c.name, blocked, c.want)
		}
		if c.want && st.Warn {
			t.Errorf("%s: 已超限时不该仍只是「接近上限」: %+v", c.name, st)
		}
	}
}

// 没定价的 token 只进 token 上限，不进金额：把它们算成 0 元会让花费看起来比实际低。
func TestBudgetUnpricedTokensOnlyCountTokens(t *testing.T) {
	now := time.Now()
	b := costBudget(t, costSettings(2_000_000, 0.5, OverBlockAll, 80),
		Entry{TS: now, Kind: KindTurn, Model: "nope", Prompt: 1_000_000}, now)

	st := b.Status(now)
	if st.SpentTokens != 1_000_000 || !sameAmount(st.SpentCost, 0) {
		t.Fatalf("未定价的用量应只计 token: %+v", st)
	}
	if st.UnpricedTokens != 1_000_000 || !st.CostInexact || st.Exceeded {
		t.Fatalf("未定价应被报出且不触发金额上限: %+v", st)
	}
	// 再来一条：token 到限了，金额仍然是 0
	b.Observe(Entry{TS: now, Kind: KindTurn, Model: "nope", Prompt: 1_000_000})
	st = b.Status(now)
	if !st.Exceeded {
		t.Fatalf("token 到限应拦下（未定价不等于免费）: %+v", st)
	}
	if !sameAmount(st.SpentCost, 0) || st.UnpricedTokens != 2_000_000 {
		t.Fatalf("金额仍应是 0，未定价累计到 200 万: %+v", st)
	}
}

// 别的币种不进本币预算，但要标「金额不完整」：用户设的是 CNY 上限，USD 的花费
// 不能悄悄算成 0 元、也不能按魔幻汇率折进来。
func TestBudgetOtherCurrencyIsInexact(t *testing.T) {
	now := time.Now()
	b := costBudget(t, costSettings(0, 0.5, OverBlockAll, 80),
		Entry{TS: now, Kind: KindTurn, Model: "usd", Prompt: 1_000_000}, now)

	st := b.Status(now)
	if !sameAmount(st.SpentCost, 0) || st.Exceeded {
		t.Fatalf("别的币种不该计入本币预算: %+v", st)
	}
	if !st.CostInexact {
		t.Fatalf("有别的币种的金额时应标不精确: %+v", st)
	}
	if st.Currency != "CNY" {
		t.Fatalf("Currency = %q, want CNY（小写应被归一化）", st.Currency)
	}
}

// 没有价格表时金额上限不生效，而 token 上限照常：把「没有价格」当成降级会让
// 用户设的 token 上限也跟着失效。
func TestBudgetWithoutPriceTable(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	l := mustOpen(t, dir)
	mustAppend(t, l, Entry{TS: now, Kind: KindTurn, Model: "cny", Prompt: 1_000_000})

	b := NewBudget(costSettings(0, 0.25, OverBlockAll, 80), l, nil, now)
	st := b.Status(now)
	if st.CostReady || !st.Enabled || st.Exceeded || st.Degraded {
		t.Fatalf("没有价格表时金额上限应不生效，且不算降级: %+v", st)
	}
	// 「没有价格表」由 CostReady 交代，不再重复标 CostUnpriced
	if st.CostUnpriced {
		t.Fatalf("没有价格表时不该同时标 CostUnpriced: %+v", st)
	}
	if !sameAmount(st.SpentCost, 0) || st.UnpricedTokens != 1_000_000 {
		t.Fatalf("没有价格表时应全部记为未定价: %+v", st)
	}
	if _, blocked := b.Blocked(now, false); blocked {
		t.Fatal("金额上限未生效时不该拦")
	}

	// 同一份账本配上 token 上限：照常拦
	tb := NewBudget(limitSettings(1_000_000, OverBlockAll, 80), l, nil, now)
	if st := tb.Status(now); !st.Exceeded {
		t.Fatalf("token 上限应照常生效: %+v", st)
	}
}

// 跨月重扫金额：上个月的额度不再占着，新月份的金额按那份账本重新算。
func TestBudgetRollsOverMonthWithCost(t *testing.T) {
	nov := time.Date(2026, 11, 20, 12, 0, 0, 0, time.Local)
	dec := time.Date(2026, 12, 3, 9, 0, 0, 0, time.Local)
	l := mustOpen(t, t.TempDir())
	mustAppend(t, l, Entry{TS: nov, Kind: KindTurn, Model: "cny", Prompt: 1_000_000}) // 1.00
	mustAppend(t, l, Entry{TS: dec, Kind: KindTurn, Model: "cny", Prompt: 3_000_000}) // 3.00

	b := NewBudget(costSettings(0, 2, OverBlockAll, 80), l, budgetPrice, nov)
	st := b.Status(nov)
	if st.Month != "2026-11" || !sameAmount(st.SpentCost, 1) || st.Exceeded {
		t.Fatalf("11 月应只算 11 月的 1.00: %+v", st)
	}
	st = b.Status(dec)
	if st.Month != "2026-12" || !sameAmount(st.SpentCost, 3) || !st.Exceeded {
		t.Fatalf("12 月应重扫为 3.00 并超限: %+v", st)
	}
	// 回到上个月（旧请求拖到新月份才查）：不倒退状态，仍是 12 月那份 3.00，
	// 而不是把月份拨回 11 月、让一笔请求重新获准。
	if st := b.Status(nov); st.Month != "2026-12" || !sameAmount(st.SpentCost, 3) || !st.Exceeded {
		t.Fatalf("旧月份的时间不该倒退预算状态: %+v", st)
	}
}

// Observe 立刻累加金额，不必等下一次扫盘；缓存价回退也在这里标出。
func TestBudgetObserveAccumulatesCost(t *testing.T) {
	now := time.Now()
	b, _, _ := budgetAtLookup(t, costSettings(0, 2, OverBlockAll, 80), 0, now, budgetPrice)
	if _, blocked := b.Blocked(now, false); blocked {
		t.Fatal("还没花钱不该拦")
	}

	b.Observe(Entry{TS: now, Kind: KindTurn, Model: "cny", Prompt: 1_000_000})
	st := b.Status(now)
	if !sameAmount(st.SpentCost, 1) || st.Exceeded || st.CostInexact {
		t.Fatalf("Observe 后应立即累加金额: %+v", st)
	}

	// 缓存命中但没有缓存价：金额按输入价估算，标记不精确
	b.Observe(Entry{TS: now, Kind: KindTurn, Model: "cacheback", Prompt: 1_000_000, Cached: 200_000})
	st = b.Status(now)
	if !sameAmount(st.SpentCost, 2) || !st.Exceeded {
		t.Fatalf("第二条应把金额累加到 2.00 并超限: %+v", st)
	}
	if !st.CostInexact {
		t.Fatalf("缓存价回退应标不精确: %+v", st)
	}

	// 未定价的条目只累加 token
	b.Observe(Entry{TS: now, Kind: KindTurn, Model: "nope", Prompt: 500_000})
	st = b.Status(now)
	if !sameAmount(st.SpentCost, 2) || st.UnpricedTokens != 500_000 {
		t.Fatalf("未定价条目不该动金额: %+v", st)
	}
}

// 金额字段的 JSON 契约：不设金额上限时 limit_cost 整个省略；spent_cost 为 0 时也省略；
// cost_ready 恒在下发（前端靠它决定金额段显不显示）。
func TestBudgetStatusCostJSONContract(t *testing.T) {
	now := time.Now()
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

	noCost, _, _ := budgetAtLookup(t, limitSettings(2_000, OverSkipBackground, 80), 100, now, budgetPrice)
	m := decode(t, noCost.Status(now))
	if _, ok := m["limit_cost"]; ok {
		t.Errorf("不设金额上限时不该下发 limit_cost: %s", m["limit_cost"])
	}
	if ready, ok := m["cost_ready"]; !ok || string(ready) != "true" {
		t.Errorf("cost_ready 应恒在下发且为 true: %s / %v", ready, ok)
	}

	withCost, _, _ := budgetAtLookup(t, costSettings(0, 50, OverSkipBackground, 80), 0, now, budgetPrice)
	m = decode(t, withCost.Status(now))
	var limit float64
	if err := json.Unmarshal(m["limit_cost"], &limit); err != nil || !sameAmount(limit, 50) {
		t.Errorf("limit_cost = %s (%v), want 50", m["limit_cost"], err)
	}
	var currency string
	if err := json.Unmarshal(m["currency"], &currency); err != nil || currency != "CNY" {
		t.Errorf("currency = %s (%v), want CNY", m["currency"], err)
	}
}

// 价格表在运行中变了（模型设置里改单价）：Refresh 之后已用金额与拦截立刻按新价
// 算，而不是等跨月或一次故障重试。
func TestBudgetRefreshUsesNewPrice(t *testing.T) {
	now := time.Now()
	l := mustOpen(t, t.TempDir())
	mustAppend(t, l, Entry{TS: now, Kind: KindTurn, Provider: "p", Model: "cny", Prompt: 1_000_000})

	b := NewBudget(costSettings(0, 2, OverBlockAll, 80), l, budgetPrice, now)
	if st := b.Status(now); !sameAmount(st.SpentCost, 1) || st.Exceeded {
		t.Fatalf("按旧价应为 1.00 且未超限: %+v", st)
	}

	// 单价翻倍，然后 Refresh：本月已花按新价重算
	b.mu.Lock()
	b.costLookup = func(_, model string) (Price, bool) {
		if model == "cny" {
			return Price{Currency: "CNY", Input: 2, Output: 4}, true
		}
		return Price{}, false
	}
	b.mu.Unlock()
	b.Refresh(now)

	if st := b.Status(now); !sameAmount(st.SpentCost, 2) || !st.Exceeded {
		t.Fatalf("Refresh 后应按新价算成 2.00 并超限: %+v", st)
	}
	if _, blocked := b.Blocked(now, false); !blocked {
		t.Fatal("新价下已超限，后台应被拦")
	}
}

// 设了金额上限、价格表也在，但本月用到的模型一个都没定价：CostReady 仍为真，
// 另由 CostUnpriced 交代「金额上限其实没生效」，且不拦。
func TestBudgetCostUnpricedFlag(t *testing.T) {
	now := time.Now()
	priceless := func(_, _ string) (Price, bool) { return Price{}, false }
	b, _, _ := budgetAtLookup(t, costSettings(0, 1, OverBlockAll, 80), 1_000_000, now, priceless)

	st := b.Status(now)
	if !st.CostReady || !st.CostUnpriced {
		t.Fatalf("有价格表却没定价时 CostReady 与 CostUnpriced 都应报出: %+v", st)
	}
	if !sameAmount(st.SpentCost, 0) || st.Exceeded || st.Warn {
		t.Fatalf("金额恒为 0，不该超限或提醒: %+v", st)
	}
	if _, blocked := b.Blocked(now, false); blocked {
		t.Fatal("金额上限没生效时不该拦")
	}
}

// CostUnpriced 的判据是「一分都没折进预算币种」：本月既有已定价的本币用量、又有
// 未定价用量时，金额上限是在管着的，只是那个数不含未定价的部分——此时报
// 「金额上限未生效」会是假话，用户会以为钱的上限根本没在兜底。
func TestBudgetCostUnpricedRequiresZeroSpent(t *testing.T) {
	now := time.Now()
	l := mustOpen(t, t.TempDir())
	mustAppend(t, l, Entry{TS: now, Kind: KindTurn, Provider: "p", Model: "cny", Prompt: 1_000_000})
	mustAppend(t, l, Entry{TS: now, Kind: KindTurn, Provider: "p", Model: "nope", Prompt: 500_000})

	b := NewBudget(costSettings(0, 50, OverBlockAll, 80), l, budgetPrice, now)
	st := b.Status(now)
	if !st.CostReady {
		t.Fatalf("价格表在，CostReady 应为真: %+v", st)
	}
	if !sameAmount(st.SpentCost, 1) || st.CostUnpriced {
		t.Fatalf("有本币花费时不该标 CostUnpriced: cost=%v flag=%v", st.SpentCost, st.CostUnpriced)
	}
	if st.UnpricedTokens != 500_000 || !st.CostInexact {
		t.Fatalf("未定价那部分仍要说出来: %+v", st)
	}
	// 金额上限照常生效：1.00 < 50 只是没到限，不是「没在管」。
	if st.Exceeded || st.Warn {
		t.Fatalf("1.00 / 50.00 不该超限或提醒: %+v", st)
	}
}

// Record 与并发的 Refresh 不该把同一笔记两次，也不该漏记：追加与重扫共用同一个
// 原子边界（recordMu），扫描进不来的时候才能保证「扫到的归扫描、扫不到的归累加」。
func TestBudgetRecordConcurrentWithRefresh(t *testing.T) {
	// 固定日期：用 time.Now() 加秒在月末会跨进下个月。
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.Local)
	l := mustOpen(t, t.TempDir())
	b := NewBudget(costSettings(0, 50, OverSkipBackground, 80), l, budgetPrice, now)

	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := b.Record(Entry{TS: now, Kind: KindTurn, Model: "cny", Prompt: 1_000}); err != nil {
				t.Errorf("Record: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			b.Refresh(now)
		}()
	}
	wg.Wait()

	sum, err := l.SummaryCostOnMonth(monthOf(now), GroupByKind, budgetPrice)
	if err != nil {
		t.Fatal(err)
	}
	st := b.Status(now)
	if want := int64(sum.Total.Prompt + sum.Total.Completion); st.SpentTokens != want {
		t.Errorf("已用 token = %d，账本合计 %d（重复或漏记）", st.SpentTokens, want)
	}
	if want := sum.Total.amountsUnits["CNY"]; costUnits(st.SpentCost) != want {
		t.Errorf("已用金额 = %v（%d 纳单位），账本合计 %d 纳单位",
			st.SpentCost, costUnits(st.SpentCost), want)
	}
	// 每条 1,000 token、共 100 条：合计必须是整 10 万，不是「差不多」。
	if st.SpentTokens != n*1_000 {
		t.Errorf("已用 token = %d，期望 %d", st.SpentTokens, n*1_000)
	}
}

// 账本写不进去（当天分片那条路径被一个目录占住）也照样累加已用：钱已经花掉了，
// 预算要按实际花费拦；只是状态里得说得出「这个统计不完整」。
func TestBudgetRecordWriteFailureStillCounts(t *testing.T) {
	now := time.Now()
	b := writeFailBudget(t, costSettings(0, 50, OverSkipBackground, 80), now)

	if err := b.Record(Entry{TS: now, Kind: KindTurn, Model: "cny", Prompt: 1_000_000}); err == nil {
		t.Fatal("写不进去时 Record 应报错")
	}
	st := b.Status(now)
	if !st.WriteFailed {
		t.Fatalf("写失败应标 WriteFailed: %+v", st)
	}
	if st.SpentTokens != 1_000_000 || !sameAmount(st.SpentCost, 1) {
		t.Fatalf("写失败的那一笔也要计入已用: %+v", st)
	}
	// 它不并进 Degraded：并进去会让预算 fail-open，正好给了一次绕过上限的机会。
	// 这个账本目录读起来还是个「空目录」，扫描并不算失败，Degraded 因此为假——
	// 正好证明这两个标记是分开的。
	if st.Degraded {
		t.Fatalf("写失败不该让判定降级（那会 fail-open）: %+v", st)
	}

	// 一笔成功的记账不该把标记清掉：之前那笔确实没落盘，统计仍是不完整的。
	good := mustOpen(t, t.TempDir())
	b.mu.Lock()
	b.ledger = good
	b.mu.Unlock()
	if err := b.Record(Entry{TS: now, Kind: KindTurn, Model: "cny", Prompt: 1_000}); err != nil {
		t.Fatal(err)
	}
	if st := b.Status(now); !st.WriteFailed {
		t.Fatalf("成功的那一笔不该清掉 WriteFailed: %+v", st)
	}
	if st := b.Status(now); st.SpentTokens != 1_001_000 {
		t.Fatalf("两笔都该计入: %+v", st)
	}
}

// 写失败的那一笔在重扫之后仍要算、仍要拦：Refresh（保存模型、同月重扫）会把 spent
// 换成磁盘合计，而磁盘上偏偏没有它。pending 就是为这一刻留的——重扫载入磁盘那一份
// 之后逐条重放，失败的那笔才不会消失、下一次请求才不会被重新放行。
func TestBudgetWriteFailureSurvivesRefresh(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.Local)
	b := writeFailBudget(t, limitSettings(10, OverBlockAll, 80), now)

	if err := b.Record(Entry{TS: now, Kind: KindTurn, Prompt: 10}); err == nil {
		t.Fatal("写不进去时 Record 应报错")
	}
	// 账本是空的（分片写不进去），能拦住的只有刚记下的那一笔。
	st := b.Status(now)
	if st.SpentTokens != 10 || !st.Exceeded || !st.WriteFailed {
		t.Fatalf("写失败的那笔应计入并拦住: %+v", st)
	}
	if st.Degraded {
		t.Fatalf("当天分片写不进去不算扫描失败（扫描跳过目录）: %+v", st)
	}
	if _, blocked := b.Blocked(now, true); !blocked {
		t.Fatal("到限应拦住")
	}

	b.Refresh(now)
	st = b.Status(now)
	if st.SpentTokens != 10 || !st.Exceeded || !st.WriteFailed {
		t.Fatalf("Refresh 之后仍应按 10/10 拦住: %+v", st)
	}
	if _, blocked := b.Blocked(now, true); !blocked {
		t.Fatal("Refresh 之后仍应拦住")
	}
	// 再扫一次也不该把它数两遍
	b.Refresh(now)
	if st := b.Status(now); st.SpentTokens != 10 {
		t.Fatalf("重放不该重复计数: %+v", st)
	}

	// 跨月：旧月份的失败记录不再占着新月份
	if st := b.Status(now.AddDate(0, 1, 0)); st.SpentTokens != 0 || st.Exceeded {
		t.Fatalf("跨月后旧月份的失败记录不该影响新月份: %+v", st)
	}

	// 正常账本（没有 pending）：重扫也不会把已经落盘的记录数两遍
	good := mustOpen(t, t.TempDir())
	mustAppend(t, good, Entry{TS: now, Kind: KindTurn, Prompt: 10})
	nb := NewBudget(limitSettings(10, OverBlockAll, 80), good, budgetPrice, now)
	nb.Refresh(now)
	nb.Refresh(now)
	if st := nb.Status(now); st.SpentTokens != 10 || !st.Exceeded || st.WriteFailed {
		t.Fatalf("正常账本重扫不该重复计数: %+v", st)
	}
}

// 请求在月底通过检查、次月才返回用量：这时 b.month 还停在上个月，记录按新月份落盘
// 失败时也必须保下新月份的用量。不先对齐月份的话，这一笔既不在重扫的范围里、也会被
// observeLocked 的月份判断跳过，下一次检查显示 0 并放行。
func TestBudgetRecordNextMonthWriteFailureKeepsUsage(t *testing.T) {
	nov := time.Date(2026, 11, 20, 12, 0, 0, 0, time.Local)
	dec := time.Date(2026, 12, 3, 9, 0, 0, 0, time.Local)
	jan := time.Date(2027, 1, 5, 9, 0, 0, 0, time.Local)

	// 失败夹具的目标月份是新月份：11-20 的分片写不进去也不影响这个测试。
	b := NewBudget(limitSettings(10, OverBlockAll, 80), writeFailLedger(t, dec), budgetPrice, nov)

	if err := b.Record(Entry{TS: dec, Kind: KindTurn, Prompt: 10}); err == nil {
		t.Fatal("写不进去时 Record 应报错")
	}
	st := b.Status(dec)
	if st.Month != "2026-12" || st.SpentTokens != 10 || !st.Exceeded || !st.WriteFailed {
		t.Fatalf("新月份写失败的那笔也要计入并拦住: %+v", st)
	}
	if st.Degraded {
		t.Fatalf("当天分片写不进去不算扫描失败: %+v", st)
	}
	if _, blocked := b.Blocked(dec, true); !blocked {
		t.Fatal("到限应拦住")
	}

	// 跨月对齐之后它进了 pending，重扫因此不会把它换回磁盘合计
	b.Refresh(dec)
	if st := b.Status(dec); st.SpentTokens != 10 || !st.Exceeded || !st.WriteFailed {
		t.Fatalf("Refresh 之后仍应按 10/10 拦住: %+v", st)
	}

	// 再下个月：旧月份的失败记录与标记都不跟过来
	st = b.Status(jan)
	if st.Month != "2027-01" || st.WriteFailed || st.SpentTokens != 0 || st.Exceeded {
		t.Fatalf("旧月份的写失败不该跟到新月份: %+v", st)
	}
}

// 旧月份的状态查询不能把预算月份拨回去：月底发出的请求拖到新月份才拿到锁时，
// 它必须看到当前月（新月份）的状态；倒退会把新月份的 pending/writeFailed 清掉、
// 已用量归零，下一笔请求于是重新获准。
func TestBudgetStatusOldMonthDoesNotRollBack(t *testing.T) {
	nov := time.Date(2026, 11, 20, 12, 0, 0, 0, time.Local)
	dec := time.Date(2026, 12, 3, 9, 0, 0, 0, time.Local)

	b := NewBudget(limitSettings(10, OverBlockAll, 80), writeFailLedger(t, dec), budgetPrice, dec)
	if err := b.Record(Entry{TS: dec, Kind: KindTurn, Prompt: 10}); err == nil {
		t.Fatal("新月份首笔写不进去时 Record 应报错")
	}

	// 旧时间的查询先到：看到的仍是 12 月那份快照，而不是被拨回 11 月。
	if st := b.Status(nov); st.Month != "2026-12" || st.SpentTokens != 10 || !st.Exceeded {
		t.Fatalf("旧月份的时间应看到当前月的状态: %+v", st)
	}
	// 再看当前月：用量、超限与写失败标记都还在。
	st := b.Status(dec)
	if st.Month != "2026-12" || st.SpentTokens != 10 || !st.Exceeded || !st.WriteFailed {
		t.Fatalf("旧查询不该清掉新月份的用量与写失败标记: %+v", st)
	}
	b.mu.Lock()
	pending := len(b.pending)
	b.mu.Unlock()
	if pending != 1 {
		t.Fatalf("旧查询不该清掉新月份的 pending: %d", pending)
	}
}

// WriteFailed 按月归属：上个月的写失败不该让新月份一直报「统计不完整」，也不该让
// 旧月份的 pending 继续占着额度；新月份自己再失败时仍要报出来。
func TestBudgetWriteFailedBelongsToMonth(t *testing.T) {
	apr := time.Date(2026, 4, 15, 12, 0, 0, 0, time.Local)
	may := time.Date(2026, 5, 2, 9, 0, 0, 0, time.Local)

	// 两个月各自的分片都写成目录：哪个月动笔都失败。
	dir := filepath.Join(t.TempDir(), "usage")
	failShard(t, dir, apr)
	failShard(t, dir, may)
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := NewBudget(limitSettings(10, OverBlockAll, 80), l, budgetPrice, apr)

	if err := b.Record(Entry{TS: apr, Kind: KindTurn, Prompt: 10}); err == nil {
		t.Fatal("4 月分片写不进去时 Record 应报错")
	}
	if st := b.Status(apr); !st.WriteFailed || st.SpentTokens != 10 || !st.Exceeded {
		t.Fatalf("4 月的写失败应在 4 月报出: %+v", st)
	}

	// 跨月：标记与 pending 一起清掉，已用数按新月份的磁盘算
	if st := b.Status(may); st.WriteFailed || st.SpentTokens != 0 || st.Month != "2026-05" {
		t.Fatalf("5 月不该继承 4 月的写失败: %+v", st)
	}

	// 5 月自己再失败：这一次要报出来
	if err := b.Record(Entry{TS: may, Kind: KindTurn, Prompt: 10}); err == nil {
		t.Fatal("5 月分片写不进去时 Record 应报错")
	}
	if st := b.Status(may); !st.WriteFailed || st.SpentTokens != 10 {
		t.Fatalf("5 月自己的写失败应报出: %+v", st)
	}
}

// 提醒阈值按 128 位比较：月限额一亿两千万、阈值八成时 limitUnits*80 是 9.6e18，
// 超过 int64 的 9.22e18，溢出之后花一个纳单位也会判成「已到阈值」。
func TestBudgetWarnRatioDoesNotOverflow(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.Local)
	b, _, _ := budgetAtLookup(t, costSettings(0, 120_000_000, OverBlockAll, 80), 0, now, budgetPrice)

	// 金额在纳单位里按元换算：80% 的阈值是 96,000,000 元。
	cases := []struct {
		amount float64
		want   bool
	}{
		{1, false},          // 1.00 元，离阈值还很远
		{95_999_999, false}, // 差一元到不了阈值
		{96_000_000, true},  // 恰好 80%
	}
	for _, c := range cases {
		b.mu.Lock()
		b.spentCostUnits = map[string]int64{"CNY": costUnits(c.amount)}
		b.mu.Unlock()
		st := b.Status(now)
		if st.Exceeded {
			t.Fatalf("%v 元未到上限却判超限: %+v", c.amount, st)
		}
		if st.Warn != c.want {
			t.Errorf("%v 元：Warn = %v, want %v", c.amount, st.Warn, c.want)
		}
	}

	// warn_percent=0 是「一花就提醒」，不是「还没花就提醒」。
	b.SetSettings(costSettings(0, 120_000_000, OverBlockAll, 0))
	b.mu.Lock()
	b.spentCostUnits = map[string]int64{"CNY": costUnits(1)}
	b.mu.Unlock()
	if st := b.Status(now); !st.Warn {
		t.Fatalf("warn_percent=0 时一花钱就该提醒: %+v", st)
	}

	// token 那一侧同样不能靠溢出的乘积判：限额装不下时一个 token 也不该提醒
	tb, _, _ := budgetAtLookup(t, limitSettings(120_000_000_000_000_000, OverBlockAll, 80), 0, now, budgetPrice)
	tb.mu.Lock()
	tb.spent = 1
	tb.mu.Unlock()
	if st := tb.Status(now); st.Warn || st.Exceeded {
		t.Fatalf("装不下的乘积不该让一个 token 触发提醒: %+v", st)
	}
}

// 金额恰好到限必须拦住。10 笔各 0.1 元浮点累加是 0.9999999999999999，界面上早已
// 显示 1.00 / 1.00；按纳单位整数判定才与看到的数字一致。
func TestBudgetCostExactAtLimit(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.Local)
	// budgetPrice 里 cny 的输入价是 1.00 / 百万 token，10 万 token 恰好 0.1 元。
	entry := func() Entry {
		return Entry{TS: now, Kind: KindTurn, Model: "cny", Prompt: 100_000}
	}

	// 增量路径：一笔一笔记，第 10 笔之后到限
	b, _, _ := budgetAtLookup(t, costSettings(0, 1, OverBlockAll, 80), 0, now, budgetPrice)
	for i := 0; i < 10; i++ {
		if err := b.Record(entry()); err != nil {
			t.Fatal(err)
		}
	}
	st := b.Status(now)
	if !sameAmount(st.SpentCost, 1) || !st.Exceeded {
		t.Fatalf("10 笔各 0.1 元应恰好到限: %+v", st)
	}
	if _, blocked := b.Blocked(now, true); !blocked {
		t.Fatal("恰好到限应拦住")
	}

	// 重扫路径：同样的 10 笔从账本里读回来，判定必须一致
	l := mustOpen(t, t.TempDir())
	for i := 0; i < 10; i++ {
		mustAppend(t, l, entry())
	}
	rescanned := NewBudget(costSettings(0, 1, OverBlockAll, 80), l, budgetPrice, now)
	if st := rescanned.Status(now); !sameAmount(st.SpentCost, 1) || !st.Exceeded {
		t.Fatalf("重扫路径同样应判到限: %+v", st)
	}

	// 差一笔就不拦：整数判定也不能反过来多拦
	fewer := NewBudget(costSettings(0, 1, OverBlockAll, 80), mustOpen(t, t.TempDir()), budgetPrice, now)
	for i := 0; i < 9; i++ {
		mustAppend(t, fewer.ledger, entry())
	}
	fewer.Refresh(now)
	if st := fewer.Status(now); !sameAmount(st.SpentCost, 0.9) || st.Exceeded {
		t.Fatalf("9 笔（0.90 / 1.00）不该到限: %+v", st)
	}
}

// 实时记账与重扫必须用同一精度、同一逐笔舍入。0.1 元/百万 token 的三笔 33,334
// 恰好压在 0.01 上限上：先浮点合计再舍入是 0.0100002（拦截），而按微单位逐笔
// 舍入是 0.009999（放行）——同一批记录两条路径给出两个答案，上限也就时灵时不灵。
func TestBudgetCostSamePrecisionOnRecordAndRescan(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.Local)
	lookup := func(_, model string) (Price, bool) {
		if model == "tiny" {
			return Price{Currency: "CNY", Input: 0.1, Output: 0}, true
		}
		return Price{}, false
	}
	entry := Entry{TS: now, Kind: KindTurn, Model: "tiny", Prompt: 33_334} // 0.0033334 元

	// 实时路径：逐笔 3,333,400 纳单位，三笔 10,000,200，越过 0.01（10,000,000）
	b, _, _ := budgetAtLookup(t, costSettings(0, 0.01, OverBlockAll, 80), 0, now, lookup)
	for i := 0; i < 3; i++ {
		if err := b.Record(entry); err != nil {
			t.Fatal(err)
		}
	}
	st := b.Status(now)
	if !st.Exceeded || !sameAmount(st.SpentCost, 0.0100002) {
		t.Fatalf("实时路径应算成 0.0100002 并到限: %+v", st)
	}

	// 重扫路径：同样的三笔从账本读回来，金额与判定都不变
	b.Refresh(now)
	st = b.Status(now)
	if !st.Exceeded || !sameAmount(st.SpentCost, 0.0100002) {
		t.Fatalf("重扫路径应与实时一致: %+v", st)
	}
	if _, blocked := b.Blocked(now, false); !blocked {
		t.Fatal("重扫之后仍应拦住")
	}
}

// 预算 CNY、只有 USD 花费：ForeignCost 说出「上限看不见这笔钱」，与「未定价」区分，
// 也不误报超限。
func TestBudgetForeignCostFlag(t *testing.T) {
	now := time.Now()
	b := costBudget(t, costSettings(0, 0.5, OverBlockAll, 80),
		Entry{TS: now, Kind: KindTurn, Model: "usd", Prompt: 1_000_000}, now)

	st := b.Status(now)
	if !st.ForeignCost || st.CostUnpriced {
		t.Fatalf("币种不匹配应标 ForeignCost 而不是 CostUnpriced: %+v", st)
	}
	if !st.CostInexact || !sameAmount(st.SpentCost, 0) || st.Exceeded {
		t.Fatalf("别的币种不计入本币上限，也不该算超限: %+v", st)
	}

	// 不设金额上限时不存在「上限看不见这笔钱」，不必报
	plain := costBudget(t, limitSettings(0, OverBlockAll, 80),
		Entry{TS: now, Kind: KindTurn, Model: "usd", Prompt: 1_000_000}, now)
	if st := plain.Status(now); st.ForeignCost {
		t.Fatalf("不设金额上限时不该报 ForeignCost: %+v", st)
	}
}

// 坏分片只让合计偏少，不把预算标成不可靠：能读的部分照常算、照常拦。
func TestBudgetSkippedFilesStillEnforces(t *testing.T) {
	old := maxLineBytes
	maxLineBytes = 512
	t.Cleanup(func() { maxLineBytes = old })

	now := time.Now()
	l := mustOpen(t, t.TempDir())
	mustAppend(t, l, Entry{TS: now, Kind: KindTurn, Model: "cny", Prompt: 1_000_000})

	// 另造一个同月但不同日的分片，里面放一行超长记录：整份分片读不出来。
	loc := now.In(time.Local)
	otherDay := time.Date(loc.Year(), loc.Month(), 1, 9, 0, 0, 0, time.Local)
	if otherDay.Day() == loc.Day() {
		otherDay = time.Date(loc.Year(), loc.Month(), 2, 9, 0, 0, 0, time.Local)
	}
	huge := lineJSON(t, Entry{TS: otherDay, Kind: KindTurn, Model: strings.Repeat("x", 4*1024)})
	name := filepath.Join(l.dir, otherDay.Format(dayFormat)+".jsonl")
	if err := os.WriteFile(name, []byte(huge+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	b := NewBudget(costSettings(0, 1, OverBlockAll, 80), l, budgetPrice, now)
	st := b.Status(now)
	if st.SkippedFiles != 1 || st.Degraded {
		t.Fatalf("坏分片应单独报出且不算降级: %+v", st)
	}
	if st.SpentTokens != 1_000_000 || !sameAmount(st.SpentCost, 1) {
		t.Fatalf("能读的部分应照常计入: %+v", st)
	}
	// 按可读部分判定：1.00 / 1.00 已到上限，照常拦
	if _, blocked := b.Blocked(now, false); !blocked {
		t.Fatal("坏分片不该让拦截失效")
	}

	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["skipped_files"]) != "1" {
		t.Errorf("skipped_files = %s, want 1", m["skipped_files"])
	}
}

// 新字段的 JSON 契约：没有就省略，免得界面把 false 当成「确实没问题」以外的意思。
func TestBudgetStatusExtraFieldsOmitted(t *testing.T) {
	now := time.Now()
	b, _, _ := budgetAtLookup(t, limitSettings(100, OverBlockAll, 80), 10, now, budgetPrice)
	raw, err := json.Marshal(b.Status(now))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"cost_unpriced", "foreign_cost", "skipped_files"} {
		if _, ok := m[key]; ok {
			t.Errorf("没有对应情况时不该下发 %s: %s", key, m[key])
		}
	}
}

// 账本探活失败是可恢复的降级：一次成功的重扫（Refresh）或一次成功的记账就清掉；
// 设置侧的降级不清，只能由 MarkReady 清。
func TestBudgetLedgerDegradedRecovers(t *testing.T) {
	now := time.Now()
	b, _, _ := budgetAt(t, limitSettings(100, OverBlockAll, 80), 0, now)

	b.MarkLedgerDegraded()
	if st := b.Status(now); !st.Degraded {
		t.Fatalf("探活失败应标降级: %+v", st)
	}
	b.Refresh(now)
	if st := b.Status(now); st.Degraded {
		t.Fatalf("重扫成功后账本侧的降级应清除: %+v", st)
	}

	// 记账成功（Append 已经成功才会走到 Observe）同样能恢复
	b.MarkLedgerDegraded()
	b.Observe(Entry{TS: now, Kind: KindTurn, Model: "cny", Prompt: 1_000})
	if st := b.Status(now); st.Degraded {
		t.Fatalf("记账成功后账本侧的降级应清除: %+v", st)
	}

	// 设置侧的降级不会被重扫抹掉
	b.MarkDegraded()
	b.Refresh(now)
	if st := b.Status(now); !st.Degraded {
		t.Fatalf("设置侧的降级只能由 MarkReady 清: %+v", st)
	}
	b.MarkReady()
	if st := b.Status(now); st.Degraded {
		t.Fatalf("MarkReady 后应恢复: %+v", st)
	}
}

// costWarn 也遵守「零用量不提醒」：warn_percent=0 是「一花就提醒」，不是「还没花就提醒」。
func TestBudgetCostWarnBounds(t *testing.T) {
	now := time.Now()
	b, _, _ := budgetAtLookup(t, costSettings(0, 1, OverBlockAll, 0), 0, now, budgetPrice)
	if st := b.Status(now); st.Warn || st.Exceeded {
		t.Fatalf("还没花钱时 warn=0 不该提醒: %+v", st)
	}
	b.Observe(Entry{TS: now, Kind: KindTurn, Model: "cny", Prompt: 1_000})
	if st := b.Status(now); !st.Warn || st.Exceeded {
		t.Fatalf("warn=0 应一花就提醒: %+v", st)
	}
}

// 累计金额溢出不能翻成负数放行。每百万 5e9 元的两笔各一百万 token，合计 1e10 元，
// 纳单位合计 1e19 超过 int64 的 9.22e18：直接相加会翻负，「这个月花了多少」与拦截
// 判定一起反过来。实时记账、重扫与新建预算三条路径都必须判超限。
func TestBudgetCostAccumulationSaturates(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.Local)
	hugePrice := func(_, model string) (Price, bool) {
		if model != "huge" {
			return Price{}, false
		}
		return Price{Currency: "CNY", Input: 5e9}, true
	}
	entry := Entry{TS: now, Kind: KindTurn, Model: "huge", Prompt: 1_000_000}
	// 饱和之后合计落在 MaxInt64 纳单位上，即约 9.223e9 元。
	const wantAmount = float64(math.MaxInt64) / float64(costUnitsPerUnit)

	check := func(t *testing.T, what string, st BudgetStatus) {
		t.Helper()
		if !st.Exceeded {
			t.Fatalf("%s：溢出成负数后判成了未超限: %+v", what, st)
		}
		if st.SpentCost <= 0 || math.Abs(st.SpentCost-wantAmount) > 1 {
			t.Fatalf("%s：已用金额应饱和在约 %.3e，得到 %v", what, wantAmount, st.SpentCost)
		}
		if st.SpentTokens != 2_000_000 {
			t.Fatalf("%s：token 合计应照实报出: %+v", what, st)
		}
	}

	// 实时记账：Record 一路追加 + 累加
	l := mustOpen(t, t.TempDir())
	b := NewBudget(costSettings(0, 8e9, OverBlockAll, 80), l, hugePrice, now)
	for i := 0; i < 2; i++ {
		if err := b.Record(entry); err != nil {
			t.Fatal(err)
		}
	}
	check(t, "实时记账", b.Status(now))

	// 重扫：同一个月的两笔从账本里读回来，整数合计同样要饱和
	b.Refresh(now)
	check(t, "重扫", b.Status(now))

	// 冷启动直接扫账本
	fresh := NewBudget(costSettings(0, 8e9, OverBlockAll, 80), l, hugePrice, now)
	check(t, "冷启动", fresh.Status(now))
}

// costUnits 对坏值与超量程的值饱和：NaN 与负数归 0，Inf 与装不下的金额归 MaxInt64。
// 反过来（归 0 或溢出成负数）会让「花了 1e10 元」读成「一分没花」。
func TestCostUnitsSaturates(t *testing.T) {
	cases := []struct {
		name   string
		amount float64
		want   int64
	}{
		{"NaN", math.NaN(), 0},
		{"负数", -1, 0},
		{"零", 0, 0},
		{"正常", 1.5, 1_500_000_000},
		{"正无穷", math.Inf(1), math.MaxInt64},
		{"远超量程", 1e300, math.MaxInt64},
		{"刚好超量程", 9.3e9, math.MaxInt64},
		{"量程之内", 9e9, 9_000_000_000_000_000_000},
	}
	for _, c := range cases {
		if got := costUnits(c.amount); got != c.want {
			t.Errorf("%s: costUnits(%v) = %d, want %d", c.name, c.amount, got, c.want)
		}
	}
}

// satAdd 只会往「远超上限」那一侧饱和，绝不翻负。
func TestSatAdd(t *testing.T) {
	cases := []struct {
		a, b, want int64
	}{
		{0, 0, 0},
		{5, 3, 8},
		{math.MaxInt64 - 1, 5, math.MaxInt64},
		{math.MaxInt64 / 2, math.MaxInt64/2 + 1, math.MaxInt64},
		{math.MaxInt64, 0, math.MaxInt64},
		{7, -3, 7}, // 负的 b 不算，也不会减回去
	}
	for _, c := range cases {
		if got := satAdd(c.a, c.b); got != c.want {
			t.Errorf("satAdd(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
