package usage

import (
	"math"
	"sort"
	"strings"
)

// priceUnit 是单价的计价基数：价格写的是「每百万 token」。
const priceUnit = 1e6

// Price 是某个模型的单价，单位统一为「每百万 token」。价格表由用户在设置页里填，
// 因此它只出现在 models.json 的模型条目上（见 modelcfg），账本本身不猜任何价格：
// 单价是随时间与人变的外部事实，写死在代码里迟早是错的。
//
// 缓存两项用指针：0 是合法价（比如缓存写入不额外收费），必须与「没填」区分——
// 没填时按输入价估算并标记为不精确，填了 0 就按 0 算。
type Price struct {
	Currency   string   `json:"currency"`
	Input      float64  `json:"input"`  // 每百万 token 的输入（未命中缓存）价
	Output     float64  `json:"output"` // 每百万 token 的输出价
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
}

// PriceLookup 按提供商与模型取单价。找不到（没配价格）返回 false，调用方据此计
// unpriced token——「未定价」与「价格为 0」是两件相反的事。
type PriceLookup func(provider, model string) (Price, bool)

// Cost 返回一组同价用量的估算金额、它是否精确，以及金额是否因放不下而被截断。
//
// 口径与 llm.Usage 的归一口径一致：Totals.Prompt 是总输入，其中 Cached（命中缓存）
// 与 CacheWrite（Anthropic 的缓存写入）各自单独计价，剩下的才是按 Input 计的部分。
// 金额 = (未命中 * Input + 命中 * 读价 + 写入 * 写价 + Completion * Output) / 1e6。
//
// 每一段都是先除后乘（tokens * (price / 1e6)）而不是先乘后除：单价可以是接近
// float64 上限的有限值（见 modelcfg.validatePrice），tokens*price 的中间量会先
// 溢出成 +Inf，而最终金额本来还表示得出来。真到终值也放不下时饱和到 MaxFloat64
// 并报 overflow——+Inf/NaN 写不进 JSON，会让整份用量报告的响应体空掉。
//
// overflow 与 exact 是两件事：exact 只说「缓存价有没有回退」，overflow 只说
// 「金额被截断了」。把它们并成一个标记会让界面上「缓存价未填」的提示变成一句
// 假话——单笔金额明明就是用填好的价格算的，只是这个数大得放不下。
//
// 缓存价没填时按 Input 估算：读缓存通常比普通输入便宜，写缓存有时反而更贵，所以
// 这个数可能偏高也可能偏低，哪一个方向都不该说死。它不能当账单用，因此 exact 报
// false，由调用方在界面上说明「估算」。Currency 为空表示这个模型压根没定价，返回
// (0, false, false)，调用方把它算进 unpriced token。
func (p Price) Cost(t Totals) (amount float64, exact, overflow bool) {
	if strings.TrimSpace(p.Currency) == "" {
		return 0, false, false
	}
	// 扣成负数说明这条记录本身不自洽（Prompt 比两项缓存之和小）。按 0 算：宁可少算
	// 一次异常记录，也不让负数把同一段区间里的其他花费抵掉。
	nonCached := t.Prompt - t.Cached - t.CacheWrite
	if nonCached < 0 {
		nonCached = 0
	}
	exact = true
	read := p.Input
	if p.CacheRead != nil {
		read = *p.CacheRead
	} else if t.Cached > 0 {
		exact = false
	}
	write := p.Input
	if p.CacheWrite != nil {
		write = *p.CacheWrite
	} else if t.CacheWrite > 0 {
		exact = false
	}
	amount = float64(nonCached)*(p.Input/priceUnit) + float64(t.Cached)*(read/priceUnit) +
		float64(t.CacheWrite)*(write/priceUnit) + float64(t.Completion)*(p.Output/priceUnit)
	// 极端单价乘极端 token 数仍可能越过 float64（终值本身就是 Inf），NaN 则来自
	// 0*Inf 这类组合。两者都写不进 JSON：饱和到 MaxFloat64，并明说这个数被截断了。
	// 这里保留 exact：它回答的是「缓存价有没有回退」，与「这个数放不下」无关。
	// 丢掉它会让一笔明明用填好的价格算出来的金额被说成「缓存价未填」。
	if math.IsInf(amount, 0) || math.IsNaN(amount) {
		return math.MaxFloat64, exact, true
	}
	return amount, exact, false
}

// clampAmount 把不能编码成 JSON 的浮点金额（±Inf / NaN）饱和到 MaxFloat64，
// 并报出这次是否真的发生了截断。
//
// Price.Cost 已经饱和过一次，但累加仍会溢出：两笔 1e308 量级的估算相加就是 +Inf。
// 它会顺着 Amounts 进 JSON，让 writeJSON 编码失败——状态码已经是 200、响应体却
// 是空的，一次「数字大得离谱」于是把整张报表变成空白。饱和值虽然离谱，但至少
// 能读、能编码，界面上看得出这组数据不正常。
//
// 返回的 bool 让调用方把「截断」这个原因传出去：金额显示成 MaxFloat64 时，报表
// 必须说清它是被截断的，而不是让人以为这就是真实花费。
func clampAmount(amount float64) (float64, bool) {
	if math.IsInf(amount, 0) || math.IsNaN(amount) {
		return math.MaxFloat64, true
	}
	return amount, false
}

// CostTotals 是合计加上按币种拆开的金额。
//
// 金额按币种分开存而不是折成一个数：汇率是另一件要联网、会过时、还得让用户相信的
// 事，而用户填的价格本来就可能来自不同地区的账单（订阅送的额度、本地与海外两种
// 计费）。界面上并列几行比一个悄悄换算过的总数诚实。
type CostTotals struct {
	Totals
	Amounts        map[string]float64 // 币种 → 金额；一个都没定价时为 nil
	UnpricedTokens int                // 没查到价格或价格没写币种的 token 数（Prompt+Completion）
	Inexact        bool               // 至少一条用了缓存价回退：金额是估算，可能偏高或偏低
	// Overflow 表示金额合计里至少有一笔被截断过（±Inf/NaN 饱和到 MaxFloat64）。
	// 它与 Inexact 分开：金额被截断时「缓存价未填」那句话是假的，界面要说的是
	// 「这个数放不下」，而不是「估算不准」。两者可以同时为真。
	Overflow bool

	// amountsUnits 是 Amounts 按 costUnits 逐笔舍入后的整数合计（币种 → 纳单位）。
	// 预算判定必须用它而不是先浮点累加再舍入：两条路径（实时逐笔记账、重扫整月）
	// 只有在同一精度上逐笔舍入才能给出同一个答案，否则同一批记录会出现「实时放行、
	// 重扫拦截」。它不对外：界面要的是浮点金额（Amounts），这里只服务预算。
	amountsUnits map[string]int64
}

// Add 把 o 合并进来。map 懒建，零值 CostTotals 可以直接用。浮点金额的累加也饱和
// （见 clampAmount）：一笔溢出写不进 JSON，不该让整份报告变成空响应体。截断过就
// 置 Overflow——两笔各自可表示的 1e308 相加，合计是假的 MaxFloat64，报表得说出来。
func (c *CostTotals) Add(o CostTotals) {
	c.Requests += o.Requests
	c.Prompt += o.Prompt
	c.Completion += o.Completion
	c.Cached += o.Cached
	c.CacheWrite += o.CacheWrite
	c.SkippedFiles += o.SkippedFiles
	for cur, amount := range o.Amounts {
		if c.Amounts == nil {
			c.Amounts = make(map[string]float64, len(o.Amounts))
		}
		sum, clamped := clampAmount(c.Amounts[cur] + amount)
		c.Amounts[cur] = sum
		if clamped {
			c.Overflow = true
		}
	}
	// 整数合计与浮点金额同步累加：前者是预算判定的口径，后者给界面读。
	// 用饱和加法：整月合计超过纳单位量程时溢出会翻成负数，等于把「花了远超上限」
	// 读成「一分没花」（见 satAdd）。
	for cur, units := range o.amountsUnits {
		if c.amountsUnits == nil {
			c.amountsUnits = make(map[string]int64, len(o.amountsUnits))
		}
		c.amountsUnits[cur] = satAdd(c.amountsUnits[cur], units)
	}
	c.UnpricedTokens += o.UnpricedTokens
	c.Inexact = c.Inexact || o.Inexact
	c.Overflow = c.Overflow || o.Overflow
}

// GroupCost 是一个分组键下的成本合计。
type GroupCost struct {
	Key string
	CostTotals
}

// CostSummary 是全量成本加上按某个维度拆分的分组，分组按键字典序。
type CostSummary struct {
	Total  CostTotals
	Groups []GroupCost
}

// CostReport 是一次月度扫描给出的合计与三个维度的分组（日 / 来源 / 提供商与模型）。
//
// 报告页原本按三个维度各扫一遍、合计再扫一遍：同一份账本被读四次，每条记录查四次价。
// 用量的金额是现算的，账本一大这件事就直接落在刷新时间上；而四次扫描里只要有一次
// 碰上正在写的分片，四块数字还会来自四个略有差异的瞬间。这里一次扫完，四块数字出自
// 同一份读结果。
type CostReport struct {
	Total    CostTotals
	Daily    []GroupCost
	BySource []GroupCost
	ByModel  []GroupCost
}

// ReportCostOnMonth 扫一遍 month 那个月的分片，同时算出合计与三个维度的分组。
//
// 每条记录只查一次价（costOf 只调一次）：合计与三份分组都从同一份结果累加，所以
// 合计恒等于任一分组维度之和，金额也只有一次四舍五入，不会因为扫了几遍而对不上。
// SkippedFiles 只填合计：同一个坏分片会落进多个分组，按组计数没有意义。
func (l *Ledger) ReportCostOnMonth(month string, lookup PriceLookup) (CostReport, error) {
	keys, err := groupKeyFuncs(GroupByDay, GroupBySource, GroupByProviderModel)
	if err != nil {
		return CostReport{}, err
	}
	dayOf, sourceOf, modelOf := keys[0], keys[1], keys[2]

	var (
		r      CostReport
		day    = map[string]*CostTotals{}
		source = map[string]*CostTotals{}
		model  = map[string]*CostTotals{}
	)
	skipped, err := l.scanMonth(month, func(e Entry) {
		// 一条记录只算一次：合计与三个分组用的是同一份金额。
		one := costOf(e, lookup)
		r.Total.Add(one)
		addGroup(day, dayOf(e), one)
		addGroup(source, sourceOf(e), one)
		addGroup(model, modelOf(e), one)
	})
	if err != nil {
		return CostReport{}, err
	}
	r.Total.SkippedFiles = skipped
	r.Daily = sortedGroups(day)
	r.BySource = sortedGroups(source)
	r.ByModel = sortedGroups(model)
	return r, nil
}

// groupKeyFuncs 一次取出多个维度的分组键函数。维度名是写死的常量，出错只可能是
// 调用方传了别的东西——照样报出去，不静默退回某个维度。
func groupKeyFuncs(groupBys ...string) ([]func(Entry) string, error) {
	fns := make([]func(Entry) string, len(groupBys))
	for i, g := range groupBys {
		fn, err := groupKeyFunc(g)
		if err != nil {
			return nil, err
		}
		fns[i] = fn
	}
	return fns, nil
}

// addGroup 把一份已经算好的成本累加进某个分组。map 懒建，键在该维度里第一次出现。
func addGroup(groups map[string]*CostTotals, key string, one CostTotals) {
	g, ok := groups[key]
	if !ok {
		g = &CostTotals{}
		groups[key] = g
	}
	g.Add(one)
}

// sortedGroups 把分组收成按键字典序排列的切片，空维度给空切片而不是 nil。
func sortedGroups(groups map[string]*CostTotals) []GroupCost {
	out := make([]GroupCost, 0, len(groups))
	for k, g := range groups {
		out = append(out, GroupCost{Key: k, CostTotals: *g})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// SummaryCostOnMonth 与 SummaryOnMonth 同形，但每条记录先查价再累加金额。
//
// 扫描、损坏行与跳过分片的口径全部复用 scanMonth：一份账本在同一段区间上不该因为
// 「看 token」还是「看钱」而数出两套数字。lookup 为 nil 时全部计为 unpriced，不 panic
// ——调用方还没有价格表时仍该拿得到 token 合计。
func (l *Ledger) SummaryCostOnMonth(month, groupBy string, lookup PriceLookup) (CostSummary, error) {
	s, err := newCostSummarizer(groupBy, lookup)
	if err != nil {
		return CostSummary{}, err
	}
	skipped, err := l.scanMonth(month, s.add)
	if err != nil {
		return CostSummary{}, err
	}
	return s.result(skipped), nil
}

// SummaryCostOnDay 与 SummaryOnDay 同形，但每条记录先查价再累加金额。
// 筛法是落盘 Day（见 TotalsOnDay 的说明），与按月的成本合计同源。
func (l *Ledger) SummaryCostOnDay(day, groupBy string, lookup PriceLookup) (CostSummary, error) {
	s, err := newCostSummarizer(groupBy, lookup)
	if err != nil {
		return CostSummary{}, err
	}
	skipped, err := l.scanDay(day, s.add)
	if err != nil {
		return CostSummary{}, err
	}
	return s.result(skipped), nil
}

// costSummarizer 一边累进成本合计与分组。分组键复用 groupKeyFunc：同一个维度在
// token 视图与金额视图里必须是同一个键，否则两个页面上的分组对不上。
type costSummarizer struct {
	keyOf  func(Entry) string
	lookup PriceLookup
	total  CostTotals
	groups map[string]*CostTotals
}

func newCostSummarizer(groupBy string, lookup PriceLookup) (*costSummarizer, error) {
	keyOf, err := groupKeyFunc(groupBy)
	if err != nil {
		return nil, err
	}
	return &costSummarizer{keyOf: keyOf, lookup: lookup, groups: map[string]*CostTotals{}}, nil
}

func (s *costSummarizer) add(e Entry) {
	// 一条记录的归属只算一次，合计与分组用同一份结果累加：查价与四舍五入都只有一处，
	// 合计因此恒等于各分组之和。
	one := costOf(e, s.lookup)
	s.total.Add(one)
	addGroup(s.groups, s.keyOf(e), one)
}

// result 收口成分组按键字典序排列的 CostSummary。skipped 只填在合计上。
func (s *costSummarizer) result(skipped int) CostSummary {
	s.total.SkippedFiles = skipped
	return CostSummary{Total: s.total, Groups: sortedGroups(s.groups)}
}

// costOf 把一条记录算成只含一次的 CostTotals。
//
// 查不到价格或价格没写币种都算 unpriced：token 照常计入合计，只是不折算成钱——
// 把这些 token 也算成 0 元，会让「这个月花了多少」看起来比实际低，而用户恰恰是
// 为了知道这个才打开这张表。
func costOf(e Entry, lookup PriceLookup) CostTotals {
	var c CostTotals
	c.Requests = 1
	c.Prompt = e.Prompt
	c.Completion = e.Completion
	c.Cached = e.Cached
	c.CacheWrite = e.CacheWrite

	var (
		p  Price
		ok bool
	)
	if lookup != nil {
		p, ok = lookup(e.Provider, e.Model)
	}
	// 币种大小写不决定它是不是同一个桶：价格表与预算设置是两个人手填的，
	// "cny" 与 "CNY" 必须落进同一行，否则重扫后的金额在预算里查不到。
	currency := strings.ToUpper(strings.TrimSpace(p.Currency))
	if !ok || currency == "" {
		c.UnpricedTokens = e.Prompt + e.Completion
		return c
	}
	amount, exact, overflow := p.Cost(c.Totals)
	if !exact {
		c.Inexact = true
	}
	amount, clamped := clampAmount(amount)
	// 截断与「缓存价回退」分开记：前者说这个数放不下，后者说这个数估得不准。
	c.Overflow = overflow || clamped
	c.Amounts = map[string]float64{currency: amount}
	// 逐笔记下舍入后的整数：合计只在累加时发生，不再对浮点总数二次舍入。
	c.amountsUnits = map[string]int64{currency: costUnits(amount)}
	return c
}
