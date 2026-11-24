package usage

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fptr(v float64) *float64 { return &v }

// sameAmount 比较金额。价格是小数，直接 == 会在十进制与浮点的转换上翻车；
// 金额量级在 1e-6 上下，用绝对误差比相对误差稳。
func sameAmount(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestPriceCost(t *testing.T) {
	cases := []struct {
		name   string
		price  Price
		totals Totals
		amount float64
		exact  bool
	}{
		{
			name:   "没有用量",
			price:  Price{Currency: "USD", Input: 2, Output: 8},
			totals: Totals{},
			amount: 0,
			exact:  true,
		},
		{
			name:   "没有缓存价但也没用到缓存",
			price:  Price{Currency: "USD", Input: 2, Output: 8},
			totals: Totals{Prompt: 1000, Completion: 500},
			amount: (1000*2 + 500*8) / 1e6,
			exact:  true, // 没用到缓存就不存在回退，金额仍然是准的
		},
		{
			name: "缓存读价与写价齐全",
			price: Price{Currency: "USD", Input: 2, Output: 8,
				CacheRead: fptr(0.2), CacheWrite: fptr(2.5)},
			totals: Totals{Prompt: 1000, Completion: 500, Cached: 400, CacheWrite: 100},
			// 未命中 500 个按输入价，命中 400 个按读价，写入 100 个按写价
			amount: (500*2 + 400*0.2 + 100*2.5 + 500*8) / 1e6,
			exact:  true,
		},
		{
			name:   "缓存读价缺失按输入价回退",
			price:  Price{Currency: "USD", Input: 2, Output: 8},
			totals: Totals{Prompt: 1000, Completion: 0, Cached: 400},
			amount: (600*2 + 400*2) / 1e6,
			exact:  false, // 金额偏高，账单上的数比它低
		},
		{
			name:   "缓存写价缺失按输入价回退",
			price:  Price{Currency: "USD", Input: 2, Output: 8, CacheRead: fptr(0.2)},
			totals: Totals{Prompt: 1000, CacheWrite: 100},
			amount: (900*2 + 100*2) / 1e6,
			exact:  false,
		},
		{
			name:   "缓存价显式为零",
			price:  Price{Currency: "USD", Input: 2, Output: 8, CacheRead: fptr(0), CacheWrite: fptr(0)},
			totals: Totals{Prompt: 1000, Completion: 0, Cached: 400, CacheWrite: 100},
			amount: (500 * 2) / 1e6,
			exact:  true, // 指针非 nil 就是「填过」，0 是有效价而不是缺省
		},
		{
			name:   "币种为空表示未定价",
			price:  Price{Input: 2, Output: 8},
			totals: Totals{Prompt: 100, Completion: 50},
			amount: 0,
			exact:  false,
		},
		{
			name:   "币种只有空白也算未定价",
			price:  Price{Currency: "  ", Input: 2, Output: 8},
			totals: Totals{Prompt: 100, Completion: 50},
			amount: 0,
			exact:  false,
		},
		{
			name: "缓存计数超过总输入时未命中按零算",
			price: Price{Currency: "USD", Input: 2, Output: 8,
				CacheRead: fptr(0.1), CacheWrite: fptr(1.25)},
			totals: Totals{Prompt: 100, Cached: 60, CacheWrite: 60},
			// 记录本身不自洽（60+60 > 100），未命中夹到 0，不让负数抵掉别的花费
			amount: (60*0.1 + 60*1.25) / 1e6,
			exact:  true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			amount, exact, overflow := c.price.Cost(c.totals)
			if !sameAmount(amount, c.amount) {
				t.Fatalf("amount = %v, want %v", amount, c.amount)
			}
			if exact != c.exact {
				t.Fatalf("exact = %v, want %v", exact, c.exact)
			}
			if overflow {
				t.Fatalf("这些用例金额都放得下，不该标 overflow")
			}
		})
	}
}

// 单价允许是接近 float64 上限的有限值：先乘后除时 tokens*price 的中间量先溢出成
// +Inf，而最终金额本来还表示得出来。先除后乘把中间量压在终值上；真到终值也放不下
// 时饱和到 MaxFloat64 并标成截断——Inf 写不进 JSON，会让整份用量报告变成空响应体。
func TestPriceCostSaturates(t *testing.T) {
	p := Price{Currency: "USD", Input: 1e308, Output: 1e308}
	amount, exact, overflow := p.Cost(Totals{Prompt: 2})
	if math.IsInf(amount, 0) || math.IsNaN(amount) {
		t.Fatalf("2 token × 1e308/百万 = %v，不应当是 Inf/NaN", amount)
	}
	if amount <= 1e300 || !exact || overflow {
		t.Fatalf("金额 = %v（exact=%v overflow=%v），期望约 2e302、精确且未截断", amount, exact, overflow)
	}
	// 这个量级上绝对值容差没有意义，按相对误差比。
	if want := 2e302; math.Abs(amount-want)/want > 1e-9 {
		t.Fatalf("金额 = %v，期望约 %v", amount, want)
	}

	// 极端 token 数乘极端单价：终值也超出 float64，饱和到 MaxFloat64 并报截断。
	// exact 要保留：这里没有缓存用量，金额用到的就是填好的输入/输出价。把它一起丢掉，
	// 界面与 CLI 就会把「这个数放不下」说成「缓存价未填」。
	amount, exact, overflow = p.Cost(Totals{Prompt: math.MaxInt64})
	if amount != math.MaxFloat64 || !exact || !overflow {
		t.Fatalf("溢出时应饱和到 MaxFloat64、保留 exact 并标 overflow: amount=%v exact=%v overflow=%v",
			amount, exact, overflow)
	}

	// 具体一笔：200 万输入、单价 1e308/百万、无缓存用量。金额 2e308 放不下，但用的
	// 就是填好的输入价——溢出与「缓存价没填」无关，exact 必须为真。
	amount, exact, overflow = p.Cost(Totals{Prompt: 2_000_000})
	if amount != math.MaxFloat64 || !exact || !overflow {
		t.Fatalf("单笔溢出不该牵连 exact: amount=%v exact=%v overflow=%v", amount, exact, overflow)
	}
	// 反过来，缓存读价没填、又有缓存命中用量时，两件事同时成立：金额被截断，且它是
	// 按输入价估出来的。两个标记各说各的，谁都不该被对方吞掉。
	amount, exact, overflow = p.Cost(Totals{Prompt: 2_000_000, Cached: 1})
	if amount != math.MaxFloat64 || exact || !overflow {
		t.Fatalf("溢出 + 缓存价回退应同时成立: amount=%v exact=%v overflow=%v", amount, exact, overflow)
	}

	// 缓存读/写价同样先除后乘：每一段各自都不该先溢出。
	cached := Price{Currency: "USD", Input: 0, Output: 0, CacheRead: fptr(1e308), CacheWrite: fptr(1e308)}
	amount, exact, overflow = cached.Cost(Totals{Prompt: 4, Cached: 2, CacheWrite: 2})
	if math.IsInf(amount, 0) || amount <= 1e300 || !exact || overflow {
		t.Fatalf("缓存段金额 = %v exact=%v overflow=%v，期望约 4e302、精确且未截断", amount, exact, overflow)
	}
	// 1e308 量级的两段合起来仍然有限：溢出只在终值真的放不下时才报。
	if got, _, overflow := p.Cost(Totals{Prompt: 1_000_000}); math.IsInf(got, 0) || overflow {
		t.Fatalf("1e308 单价 × 1e6 token = %v（overflow=%v），终值有限不该截断", got, overflow)
	}
}

// 累加也会溢出：两笔各自有界的估算相加就是 +Inf，它会顺着 Amounts 进 JSON，让编码
// 失败、响应体空掉。浮点累加因此同样饱和，并把「截断过」记进 Overflow——合计虽然
// 还是有限值，但它已经不是真实花费了，报表必须说得出原因。
func TestCostTotalsAddClamps(t *testing.T) {
	for _, start := range []float64{math.Inf(1), math.Inf(-1), math.NaN()} {
		c := CostTotals{Amounts: map[string]float64{"CNY": start}}
		c.Add(CostTotals{Amounts: map[string]float64{"CNY": 1}})
		if got := c.Amounts["CNY"]; got != math.MaxFloat64 {
			t.Fatalf("%v + 1 = %v，want MaxFloat64", start, got)
		}
		if !c.Overflow {
			t.Fatalf("%v + 1 被截断了，Overflow 应当为真", start)
		}
	}

	// 两笔各有界、相加溢出
	c := CostTotals{Amounts: map[string]float64{"CNY": 1e308}}
	c.Add(CostTotals{Amounts: map[string]float64{"CNY": 1e308}})
	if got := c.Amounts["CNY"]; got != math.MaxFloat64 {
		t.Fatalf("1e308 + 1e308 = %v，want MaxFloat64", got)
	}
	if !c.Overflow {
		t.Fatalf("1e308 + 1e308 溢出被截断，Overflow 应当为真")
	}
	// 截断与「缓存价回退」是两件事：这里没有任何金额估算，Inexact 不该被顺手置上。
	if c.Inexact {
		t.Fatalf("截断不该把 Inexact 一起置上: %+v", c)
	}

	// Overflow 会随合并传下去：分组之和里有一笔溢出，合计也要知道。
	c = CostTotals{Amounts: map[string]float64{"CNY": 1}, Overflow: true}
	c.Add(CostTotals{})
	if !c.Overflow {
		t.Fatal("已有的 Overflow 不该被合并抹掉")
	}
	c = CostTotals{}
	c.Add(CostTotals{Amounts: map[string]float64{"CNY": 2}, Overflow: true})
	if !c.Overflow {
		t.Fatal("合并进来的一方带 Overflow 时应当继承")
	}

	// 正常金额照旧按浮点累加，且不标截断
	c = CostTotals{Amounts: map[string]float64{"CNY": 1.5}}
	c.Add(CostTotals{Amounts: map[string]float64{"CNY": 2.25}})
	if got := c.Amounts["CNY"]; !sameAmount(got, 3.75) {
		t.Fatalf("1.5 + 2.25 = %v，want 3.75", got)
	}
	if c.Overflow || c.Inexact {
		t.Fatalf("正常累加不该标记截断或估算: %+v", c)
	}
}

func TestCostTotalsAdd(t *testing.T) {
	a := CostTotals{
		Totals:         Totals{Requests: 2, Prompt: 10, Completion: 3, Cached: 1, CacheWrite: 2, SkippedFiles: 1},
		Amounts:        map[string]float64{"USD": 1.5},
		UnpricedTokens: 7,
	}
	b := CostTotals{
		Totals:         Totals{Requests: 3, Prompt: 20, Completion: 4},
		Amounts:        map[string]float64{"USD": 2.25, "CNY": 10},
		UnpricedTokens: 5,
		Inexact:        true,
	}
	a.Add(b)

	want := CostTotals{
		Totals:         Totals{Requests: 5, Prompt: 30, Completion: 7, Cached: 1, CacheWrite: 2, SkippedFiles: 1},
		Amounts:        map[string]float64{"USD": 3.75, "CNY": 10},
		UnpricedTokens: 12,
		Inexact:        true,
	}
	if a.Requests != want.Requests || a.Prompt != want.Prompt || a.Completion != want.Completion ||
		a.Cached != want.Cached || a.CacheWrite != want.CacheWrite || a.SkippedFiles != want.SkippedFiles {
		t.Fatalf("totals = %+v, want %+v", a.Totals, want.Totals)
	}
	if len(a.Amounts) != len(want.Amounts) {
		t.Fatalf("amounts = %v, want %v", a.Amounts, want.Amounts)
	}
	for cur, amount := range want.Amounts {
		if !sameAmount(a.Amounts[cur], amount) {
			t.Fatalf("amounts[%q] = %v, want %v", cur, a.Amounts[cur], amount)
		}
	}
	if a.UnpricedTokens != want.UnpricedTokens || a.Inexact != want.Inexact {
		t.Fatalf("unpriced/inexact = %d/%v, want %d/%v", a.UnpricedTokens, a.Inexact, want.UnpricedTokens, want.Inexact)
	}
	// 合并是单向的：被加的那份不该被改到。
	if !sameAmount(b.Amounts["USD"], 2.25) || b.Requests != 3 {
		t.Fatalf("被合并的一方被改动了: %+v", b)
	}

	// 零值也能直接 Add，map 懒建。
	var empty CostTotals
	empty.Add(b)
	if len(empty.Amounts) != 2 || empty.Requests != 3 {
		t.Fatalf("零值 Add = %+v", empty)
	}
}

// costFixture 造一份跨两天、含多币种 / 未定价 / 一个坏分片的四月账本。
func costFixture(t *testing.T) *Ledger {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "usage")
	l := mustOpen(t, dir)

	day1 := time.Date(2026, 4, 1, 9, 0, 0, 0, time.Local)
	day2 := time.Date(2026, 4, 2, 9, 0, 0, 0, time.Local)
	for _, e := range []Entry{
		{TS: day1, Kind: KindTurn, Origin: "", Provider: "p1", Model: "mA",
			Prompt: 1_000_000, Completion: 500_000, Cached: 200_000, CacheWrite: 100_000},
		{TS: day1.Add(time.Hour), Kind: KindCompact, Origin: "scheduler", Provider: "p2", Model: "mB",
			Prompt: 300_000, Completion: 100_000, Cached: 100_000},
		{TS: day2, Kind: KindTurn, Origin: "scheduler", Provider: "p3", Model: "mC",
			Prompt: 10, Completion: 5},
		{TS: day2.Add(time.Hour), Kind: KindTurn, Origin: "", Prompt: 20},
	} {
		mustAppend(t, l, e)
	}
	return l
}

// costLookup 是测试用的价格表：p1/mA 有完整缓存价（USD），p2/mB 只有输入输出价
// （CNY），p3/mC 与空提供商都没有价格。
func costLookup(provider, model string) (Price, bool) {
	switch provider + "/" + model {
	case "p1/mA":
		return Price{Currency: "USD", Input: 2, Output: 8, CacheRead: fptr(0.2), CacheWrite: fptr(2.5)}, true
	case "p2/mB":
		return Price{Currency: "CNY", Input: 1, Output: 2}, true
	}
	return Price{}, false
}

func TestSummaryCostOnMonth(t *testing.T) {
	l := costFixture(t)

	// 2026-04-03 放一条超长行：整份分片读不完，应当只记一次跳过，不影响别的分片。
	old := maxLineBytes
	maxLineBytes = 512
	t.Cleanup(func() { maxLineBytes = old })
	huge := lineJSON(t, Entry{TS: time.Date(2026, 4, 3, 9, 0, 0, 0, time.Local), Kind: KindTurn,
		Model: strings.Repeat("x", 4*1024)})
	if err := os.WriteFile(filepath.Join(l.dir, "2026-04-03.jsonl"), []byte(huge+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := l.SummaryCostOnMonth("2026-04", GroupBySource, costLookup)
	if err != nil {
		t.Fatalf("SummaryCostOnMonth: %v", err)
	}

	wantTotal := Totals{Requests: 4, Prompt: 1_300_030, Completion: 600_005,
		Cached: 300_000, CacheWrite: 100_000, SkippedFiles: 1}
	if s.Total.Totals != wantTotal {
		t.Fatalf("Total = %+v, want %+v", s.Total.Totals, wantTotal)
	}
	if len(s.Total.Amounts) != 2 ||
		!sameAmount(s.Total.Amounts["USD"], 5.69) || !sameAmount(s.Total.Amounts["CNY"], 0.5) {
		t.Fatalf("Amounts = %v, want USD 5.69 / CNY 0.5", s.Total.Amounts)
	}
	// 两个未定价的模型各一条：10+5 与 20+0
	if s.Total.UnpricedTokens != 35 {
		t.Fatalf("UnpricedTokens = %d, want 35", s.Total.UnpricedTokens)
	}
	if !s.Total.Inexact {
		t.Fatal("p2/mB 用了缓存价回退，合计应当标记 Inexact")
	}

	if len(s.Groups) != 2 || s.Groups[0].Key != "foreground" || s.Groups[1].Key != "scheduler" {
		t.Fatalf("groups = %+v, want foreground / scheduler", s.Groups)
	}
	fg, sg := s.Groups[0].CostTotals, s.Groups[1].CostTotals
	if fg.Requests != 2 || fg.Prompt != 1_000_020 || fg.Completion != 500_000 ||
		!sameAmount(fg.Amounts["USD"], 5.69) || fg.UnpricedTokens != 20 || fg.Inexact {
		t.Fatalf("foreground = %+v", fg)
	}
	if sg.Requests != 2 || sg.Prompt != 300_010 || sg.Completion != 100_005 ||
		!sameAmount(sg.Amounts["CNY"], 0.5) || sg.UnpricedTokens != 15 || !sg.Inexact {
		t.Fatalf("scheduler = %+v", sg)
	}
	// 跳过数只填合计：同一个坏文件会落进多个分组，按组计数没有意义。
	for _, g := range s.Groups {
		if g.SkippedFiles != 0 {
			t.Fatalf("分组里不该填 SkippedFiles: %+v", g)
		}
	}

	// 按「提供商 / 模型」分组：键正好是查价用的那一对，空值归 unknown。
	pm, err := l.SummaryCostOnMonth("2026-04", GroupByProviderModel, costLookup)
	if err != nil {
		t.Fatalf("SummaryCostOnMonth(provider_model): %v", err)
	}
	wantKeys := []string{"p1 / mA", "p2 / mB", "p3 / mC", "unknown / unknown"}
	if len(pm.Groups) != len(wantKeys) {
		t.Fatalf("groups = %+v, want %v", pm.Groups, wantKeys)
	}
	for i, k := range wantKeys {
		if pm.Groups[i].Key != k {
			t.Fatalf("group[%d].Key = %q, want %q", i, pm.Groups[i].Key, k)
		}
	}
	if !sameAmount(pm.Groups[0].Amounts["USD"], 5.69) || !sameAmount(pm.Groups[1].Amounts["CNY"], 0.5) {
		t.Fatalf("provider_model 金额 = %+v", pm.Groups)
	}
	if pm.Groups[2].UnpricedTokens != 15 || pm.Groups[3].UnpricedTokens != 20 {
		t.Fatalf("provider_model 未定价 = %+v", pm.Groups)
	}

	// 未知维度与非法月份照旧报错，口径与 token 视图一致。
	if _, err := l.SummaryCostOnMonth("2026-04", "week", costLookup); err == nil {
		t.Fatal("未知 groupBy 应当报错")
	}
	if _, err := l.SummaryCostOnMonth("../x", GroupBySource, costLookup); err == nil {
		t.Fatal("非法 month 应当报错")
	}
}

// lookup 为 nil 时不能 panic，也要给出可解释的结果：全部算作未定价。
func TestSummaryCostOnMonthNilLookup(t *testing.T) {
	l := costFixture(t)
	s, err := l.SummaryCostOnMonth("2026-04", GroupBySource, nil)
	if err != nil {
		t.Fatalf("SummaryCostOnMonth(nil): %v", err)
	}
	if s.Total.Amounts != nil || s.Total.Inexact {
		t.Fatalf("没有价格表时不该有金额或 Inexact: %+v", s.Total)
	}
	if s.Total.UnpricedTokens != s.Total.Prompt+s.Total.Completion {
		t.Fatalf("UnpricedTokens = %d, want %d", s.Total.UnpricedTokens, s.Total.Prompt+s.Total.Completion)
	}
	if s.Total.UnpricedTokens != 1_900_035 {
		t.Fatalf("UnpricedTokens = %d, want 1900035", s.Total.UnpricedTokens)
	}
	for _, g := range s.Groups {
		if g.Amounts != nil || g.UnpricedTokens != g.Prompt+g.Completion {
			t.Fatalf("分组 %q = %+v", g.Key, g.CostTotals)
		}
	}
}

func TestSummaryCostOnDay(t *testing.T) {
	l := costFixture(t)

	s, err := l.SummaryCostOnDay("2026-04-02", GroupBySource, costLookup)
	if err != nil {
		t.Fatalf("SummaryCostOnDay: %v", err)
	}
	wantTotal := Totals{Requests: 2, Prompt: 30, Completion: 5}
	if s.Total.Totals != wantTotal {
		t.Fatalf("Total = %+v, want %+v（另一天的记录不该混进来）", s.Total.Totals, wantTotal)
	}
	if s.Total.Amounts != nil || s.Total.UnpricedTokens != 35 || s.Total.Inexact {
		t.Fatalf("Total = %+v", s.Total)
	}

	// 前一天的付费记录不能漏进来：这天的两组都只有未定价 token。
	if len(s.Groups) != 2 || s.Groups[0].Key != "foreground" || s.Groups[1].Key != "scheduler" {
		t.Fatalf("groups = %+v", s.Groups)
	}
	if s.Groups[0].UnpricedTokens != 20 || s.Groups[1].UnpricedTokens != 15 {
		t.Fatalf("groups = %+v", s.Groups)
	}
	if s.Total.SkippedFiles != 0 {
		t.Fatalf("SkippedFiles = %d, want 0（只读这一天的分片）", s.Total.SkippedFiles)
	}

	if _, err := l.SummaryCostOnDay("2026-02-30", GroupBySource, costLookup); err == nil {
		t.Fatal("非法 day 应当报错")
	}
}

// 价格的两项缓存价必须是「不填就不出现、填 0 就出现」：前者表示按输入价估算，
// 后者是一个有效的零价，两者不能长得一样。
func TestPriceJSONShape(t *testing.T) {
	raw, err := json.Marshal(Price{Currency: "USD", Input: 1, Output: 2})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "cache_read") || strings.Contains(string(raw), "cache_write") {
		t.Fatalf("没填的缓存价不该落盘: %s", raw)
	}

	raw, err = json.Marshal(Price{Currency: "USD", Input: 1, Output: 2, CacheRead: fptr(0)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"cache_read":0`) {
		t.Fatalf("显式的零缓存价应当出现: %s", raw)
	}
	if strings.Contains(string(raw), "cache_write") {
		t.Fatalf("没填的缓存写价不该跟着出现: %s", raw)
	}
}

// 价格表里手填的小写币种必须与预算设置的大写币种落进同一个桶，否则重扫后的
// 金额在预算判定里查不到，金额上限就悄悄失效了。
func TestCostCurrencyNormalized(t *testing.T) {
	l := mustOpen(t, filepath.Join(t.TempDir(), "usage"))
	ts := time.Date(2026, 11, 3, 10, 0, 0, 0, time.Local)
	mustAppend(t, l, Entry{TS: ts, Kind: KindTurn, Provider: "p", Model: "m", Prompt: 1_000_000})

	sum, err := l.SummaryCostOnMonth("2026-11", GroupBySource, func(_, _ string) (Price, bool) {
		return Price{Currency: "cny", Input: 2, Output: 0}, true
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sum.Total.Amounts["cny"]; ok {
		t.Fatalf("小写币种不该单独成桶: %v", sum.Total.Amounts)
	}
	if !sameAmount(sum.Total.Amounts["CNY"], 2) {
		t.Fatalf("Amounts = %v, want CNY 2", sum.Total.Amounts)
	}
}

// 两笔同币种、各自可表示（1e308）的金额在合计里溢出：Amounts 仍是有限值（饱和），
// 但 Overflow 必须为真，且 Inexact 不被顺手置上——「截断」不是「缓存价回退」。
func TestSummaryCostOverflowFlag(t *testing.T) {
	l := mustOpen(t, filepath.Join(t.TempDir(), "usage"))
	ts := time.Date(2026, 11, 3, 10, 0, 0, 0, time.Local)
	for i := 0; i < 2; i++ {
		origin := ""
		if i == 1 {
			origin = "scheduler" // 两个来源各一笔，分组里都不该溢出
		}
		mustAppend(t, l, Entry{TS: ts.Add(time.Duration(i) * time.Minute), Kind: KindTurn,
			Origin: origin, Provider: "p", Model: "m", Prompt: 1_000_000})
	}
	huge := func(_, _ string) (Price, bool) {
		return Price{Currency: "CNY", Input: 1e308, Output: 1e308}, true
	}
	sum, err := l.SummaryCostOnMonth("2026-11", GroupBySource, huge)
	if err != nil {
		t.Fatal(err)
	}
	got := sum.Total.Amounts["CNY"]
	if math.IsInf(got, 0) || math.IsNaN(got) || got != math.MaxFloat64 {
		t.Fatalf("两笔 1e308 的合计 = %v，want 有限的 MaxFloat64", got)
	}
	if !sum.Total.Overflow {
		t.Fatalf("合计溢出被截断，Overflow 应当为真: %+v", sum.Total)
	}
	if sum.Total.Inexact {
		t.Fatalf("金额被截断不等于缓存价回退，Inexact 不该被置上: %+v", sum.Total)
	}
	// 单笔金额本身没有溢出：分成两个来源分组时各自都不该标截断。
	for _, g := range sum.Groups {
		if g.Overflow {
			t.Fatalf("分组 %q 的单笔金额没溢出: %+v", g.Key, g)
		}
	}
}

// sameCostTotals 比两份成本合计，金额按浮点容差比（见 sameAmount）。
func sameCostTotals(a, b CostTotals) bool {
	if a.Totals != b.Totals || a.UnpricedTokens != b.UnpricedTokens ||
		a.Inexact != b.Inexact || a.Overflow != b.Overflow || len(a.Amounts) != len(b.Amounts) {
		return false
	}
	for cur, v := range a.Amounts {
		w, ok := b.Amounts[cur]
		if !ok || !sameAmount(v, w) {
			return false
		}
	}
	return true
}

// 报告页要一次扫描给出合计与三个维度的分组：合计等于各分组之和，且与按维度单独
// 汇总的结果逐组一致——同一份账本不该因为扫了几遍而数出两套数字。
func TestReportCostOnMonth(t *testing.T) {
	l := costFixture(t)
	// 一个读不完的分片：跳过数只填合计，分组里不该出现。
	old := maxLineBytes
	maxLineBytes = 512
	t.Cleanup(func() { maxLineBytes = old })
	huge := lineJSON(t, Entry{TS: time.Date(2026, 4, 3, 9, 0, 0, 0, time.Local), Kind: KindTurn,
		Model: strings.Repeat("x", 4*1024)})
	if err := os.WriteFile(filepath.Join(l.dir, "2026-04-03.jsonl"), []byte(huge+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 查价次数就是金额计算次数：一次扫描里每条记录只能算一次。
	calls := 0
	lookup := func(provider, model string) (Price, bool) {
		calls++
		return costLookup(provider, model)
	}
	rep, err := l.ReportCostOnMonth("2026-04", lookup)
	if err != nil {
		t.Fatalf("ReportCostOnMonth: %v", err)
	}
	if calls != 4 {
		t.Fatalf("查价次数 = %d, want 4（四条记录各算一次）", calls)
	}
	if rep.Total.SkippedFiles != 1 {
		t.Fatalf("SkippedFiles = %d, want 1", rep.Total.SkippedFiles)
	}
	// 合计本身要能对上既有口径：4 条记录、两币种、35 个未定价 token。
	if rep.Total.Requests != 4 || rep.Total.Prompt != 1_300_030 ||
		!sameAmount(rep.Total.Amounts["USD"], 5.69) || !sameAmount(rep.Total.Amounts["CNY"], 0.5) ||
		rep.Total.UnpricedTokens != 35 || !rep.Total.Inexact {
		t.Fatalf("Total = %+v", rep.Total)
	}

	dims := []struct {
		by     string
		groups []GroupCost
	}{
		{GroupByDay, rep.Daily},
		{GroupBySource, rep.BySource},
		{GroupByProviderModel, rep.ByModel},
	}
	total := rep.Total
	total.SkippedFiles = 0 // 跳过数只属于合计
	for _, dim := range dims {
		// 合计 = 这一维度各分组之和
		var sum CostTotals
		for _, g := range dim.groups {
			sum.Add(g.CostTotals)
		}
		if !sameCostTotals(sum, total) {
			t.Fatalf("%s 分组之和 = %+v, want %+v", dim.by, sum, total)
		}
		// 与按这个维度单独汇总的结果逐组一致
		want, err := l.SummaryCostOnMonth("2026-04", dim.by, costLookup)
		if err != nil {
			t.Fatalf("SummaryCostOnMonth(%s): %v", dim.by, err)
		}
		if len(want.Groups) != len(dim.groups) {
			t.Fatalf("%s 分组数 = %d, want %d", dim.by, len(dim.groups), len(want.Groups))
		}
		for i := range want.Groups {
			got := dim.groups[i]
			if got.Key != want.Groups[i].Key || !sameCostTotals(got.CostTotals, want.Groups[i].CostTotals) {
				t.Fatalf("%s 第 %d 组 = %q %+v, want %q %+v", dim.by, i, got.Key, got.CostTotals,
					want.Groups[i].Key, want.Groups[i].CostTotals)
			}
			if got.SkippedFiles != 0 {
				t.Fatalf("分组里不该填 SkippedFiles: %+v", got)
			}
		}
	}

	// 三个维度的键各自按键字典序
	if len(rep.Daily) != 2 || rep.Daily[0].Key != "2026-04-01" || rep.Daily[1].Key != "2026-04-02" {
		t.Fatalf("Daily = %+v", rep.Daily)
	}
	modelKeys := make([]string, 0, len(rep.ByModel))
	for _, g := range rep.ByModel {
		modelKeys = append(modelKeys, g.Key)
	}
	if strings.Join(modelKeys, "|") != "p1 / mA|p2 / mB|p3 / mC|unknown / unknown" {
		t.Fatalf("ByModel keys = %v", modelKeys)
	}
}

// 没有记录时给空切片而不是 nil：界面按数组画表。非法月份与未知维度照旧报错。
func TestReportCostOnMonthEmpty(t *testing.T) {
	l := costFixture(t)
	rep, err := l.ReportCostOnMonth("2026-05", nil)
	if err != nil {
		t.Fatalf("ReportCostOnMonth: %v", err)
	}
	if rep.Total.Requests != 0 || rep.Daily == nil || rep.BySource == nil || rep.ByModel == nil {
		t.Fatalf("空月份应给零值与空切片: %+v", rep)
	}
	if _, err := l.ReportCostOnMonth("../x", nil); err == nil {
		t.Fatal("非法 month 应当报错")
	}
}
