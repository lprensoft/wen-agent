package usage

import (
	"errors"
	"math"
	"math/bits"
	"strings"
	"sync"
	"time"
)

// ErrBudgetExceeded 表示这一轮没有发出模型请求：本月的用量预算已经用尽。
//
// 它与「提供商报错」不同：没有钱被花掉，也没有东西坏了。发起方据此可以把这次
// 尝试算作已经过去（心跳不回滚 lastBeat，按正常间隔等下一次），而不是立刻重试
// ——立刻重试只会把每一次请求都变成一次被拦截的尝试。
var ErrBudgetExceeded = errors.New("usage: budget exceeded")

// BudgetStatus 是一次预算判定的快照。它同时给两条路径用：拦截时随错误交给发起方，
// 平时供状态输出展示。
//
// LimitTokens 与 LimitCost 都为 0 表示不设限（Enabled=false），此时 Warn 与 Exceeded
// 恒为假。Degraded 表示这次判定建立在不可靠的数据上（账本读不出来、设置没读到），此时
// 一律不拦：预算失效不该把用户关在自己的程序外面。
type BudgetStatus struct {
	Month       string `json:"month"`
	Enabled     bool   `json:"enabled"`
	Warn        bool   `json:"warn"`
	Exceeded    bool   `json:"exceeded"`
	LimitTokens int64  `json:"limit_tokens,omitempty"`
	SpentTokens int64  `json:"spent_tokens"`
	// 金额维度。LimitCost 为 0 表示不设金额上限；CostReady 为假表示这次运行没有价格表，
	// 金额上限因此不生效（token 上限照常）——它与 Degraded 是两件事：没有价格不等于
	// 数据不可靠，token 那一路仍然算得准。
	LimitCost float64 `json:"limit_cost,omitempty"`
	SpentCost float64 `json:"spent_cost,omitempty"`
	// Currency 是预算的记账币种（大写）。金额只按它累加，别的币种另记并置 CostInexact。
	Currency string `json:"currency,omitempty"`
	// CostReady 表示价格表可用；为假时 SpentCost 恒为 0，只有 token 上限生效。
	CostReady bool `json:"cost_ready"`
	// CostInexact 表示金额是估算：缓存价没填（按输入价回退）、有未定价用量、有别的
	// 币种的金额没计进本币预算，或金额超出可表示范围被截断。这几种都会让 SpentCost
	// 与实际账单不一致。
	CostInexact    bool  `json:"cost_inexact,omitempty"`
	UnpricedTokens int64 `json:"unpriced_tokens,omitempty"`
	// CostUnpriced 表示设了金额上限、本月却一分都没折进来，只因有用量没有价格。
	// 它与 CostReady 不同：价格表在（CostReady 为真），但里面没有本月用到的那些模型，
	// 金额上限因此静默不生效——只报 CostReady 会让人以为钱的上限在兜底。
	CostUnpriced bool `json:"cost_unpriced,omitempty"`
	// ForeignCost 表示有别的币种的金额没计进预算币种。用户设的是 CNY 上限、账单是
	// USD 时，金额上限看不见那部分花费；与「未定价」不是一回事，文案要分开说。
	ForeignCost bool   `json:"foreign_cost,omitempty"`
	WarnPercent int    `json:"warn_percent"`
	Over        string `json:"over"`
	Degraded    bool   `json:"degraded,omitempty"`
	// SkippedFiles 是读不出来的账本分片数。它不等于 Degraded：能读的那部分照常算、
	// 照常拦截，只是合计可能偏少——放宽拦截会让一个坏分片变成完全不设防。
	SkippedFiles int `json:"skipped_files,omitempty"`
	// WriteFailed 表示当前自然月里至少有一条记录没能写进账本。它同样不并进 Degraded：
	// 已观测到的花费仍然可信、仍然照常拦截，只是本月统计会偏少——放宽拦截正好给了
	// 一个绕过上限的办法。
	WriteFailed bool `json:"write_failed,omitempty"`
}

// Budget 把 usage.json 里的预算设置配上账本，回答「这个月还能不能花」。
//
// token 与金额两个维度都是硬保证：钱花出去了就回不来，拦住下一笔请求是唯一还来得及
// 的动作。任一维度到限就拦，判定不区分是哪个维度——对发起方来说结果一样。
//
// 金额按预算币种累加，别的币种另存不动（不折算：汇率是另一件要联网、会过时的事），
// 它们在状态里以 CostInexact 交代「这个数不含那部分」。
//
// 月是自然月，按条目落盘的 Day 归属（TotalsOnMonth），不是 MonthBounds 那种绝对
// 区间：跨月回拨的城市里同一个当地月会分成两段，区间只盖得住第一段。
//
// 计数不只是「查询时扫盘」：每记一条账就地累加（Record），账本只在启动、跨月与
// 扫描失败后的重试时各扫一次。请求路径上不该为了一个数字读一遍当月的全部分片。
//
// 追加、就地累加与重扫共用一把 recordMu：账本追加是一条记录的唯一落点，扫描与
// 累加必须看到同一个先后顺序，否则同一条记录会被扫描与累加各数一次（见 Record）。
type Budget struct {
	// recordMu 串行化「追加 + 累加」与「扫描 + 累加」这两条路径。锁序固定为
	// recordMu → mu：持 recordMu 的一侧可以再取 mu，反之不行。
	recordMu sync.Mutex
	mu       sync.Mutex
	settings Settings
	ledger   *Ledger
	// costLookup 是价格表（提供商, 模型 → 单价）。为 nil 表示这次运行没有价格表：
	// token 上限照常，金额上限不生效（见 BudgetStatus.CostReady）。
	costLookup PriceLookup
	month      string
	spent      int64
	// spentCostUnits 是当月各币种的金额（键统一大写），单位是纳单位（1e-9）。
	// 用整数累加而不是浮点：10 笔各 0.1 元浮点加起来是 0.9999999999999999，
	// 界面上是 1.00 / 1.00 却判不出「到限」。不折算、不合并别的币种：见 Budget 的说明。
	spentCostUnits map[string]int64
	// pending 是「已计入预算、却没写进账本」的记录（见 Record 的失败路径）。重扫会把
	// spent 换成磁盘合计，而这几笔磁盘上没有；不留下它们，一次 Refresh（保存模型、
	// 跨月后重扫同一个月）就会把失败的那笔忘掉，下一次请求重新获准。只保留当前
	// 自然月：旧月份的失败记录不影响新月份，跨月切换时由 rollLocked 与 Record 清空。
	pending []Entry
	// unpricedTokens 是没查到价格、或价格没写币种的 token 数。它们计进 token 上限，
	// 但不计进金额——把没定价的用量算成 0 元，只会让「这个月花了多少」比实际低。
	unpricedTokens int64
	// costInexact 是「金额为估算」的累积标记：至少一条用了缓存价回退、没查到价格，
	// 或金额被截断（超出可表示范围）。
	costInexact bool
	// 两个降级来源分开记：账本读失败可能只是瞬时故障（杀毒软件占住目录），
	// 下一次重扫成功就恢复；设置读失败不会因为账本能读而变好，只能由调用方
	// 确认原因消失后清掉（MarkReady）。
	scanFailed bool
	degraded   bool
	// ledgerDegraded 是「启动时账本探活失败」那一档：目录写不进去，已用数虽然从
	// 账本读得到、新账却记不下来。它随一次成功的重扫或一次成功的记账（Append 已经
	// 成功，Record 才会往下走）清掉——数字重新可信就该恢复拦截。
	ledgerDegraded bool
	// skippedFiles 是最近一次扫描里读不出来的分片数。坏分片不降低可信度，只是让
	// 合计偏少，所以它单独报出而不是并进 Degraded。
	skippedFiles int
	// writeFailed 表示当前自然月里至少有一条记录没落盘。它不随一次成功的 Append 清除，
	// 也不随重扫清除：那笔记录压根不在账本里，重扫读回来的合计仍然缺它。它按自然月
	// 归属——只在当前月的记录写失败时置位，跨月切换（rollLocked 与 Record）时与 pending
	// 一起清空：上个月不完整不等于这个月不完整。
	writeFailed bool
	// lastScan 是最近一次扫描账本的时刻。扫描失败后不再整月作废：Status 在
	// rescanRetry 之后再试一次，账本读得回来就当场恢复（见 Status）。它只用于重试
	// 判定，不再参与「这条记过没有」——那件事由 recordMu 的互斥保证。
	lastScan time.Time
}

// rescanRetry 是扫描失败后的重试间隔。账本读不出来多半是瞬时故障（Windows 上
// 杀毒软件独占目录、外接盘短暂掉线），而失败期间预算一律不拦——整月不再重试的话，
// 用户设的上限会在一次故障之后悄悄失效到下次重启。
const rescanRetry = 30 * time.Second

// NewBudget 建立一份预算判定：按 now 定出当前自然月，从账本载入已用 token 与金额。
// lookup 是查价用的价格表，为 nil 表示没有价格（金额上限不生效，token 照常）。
// 账本为 nil 或扫描失败时标记降级（不拦）。
func NewBudget(settings Settings, ledger *Ledger, lookup PriceLookup, now time.Time) *Budget {
	s := normalize(settings)
	degraded := false
	if ValidateSettings(s) != nil {
		// 正常运行里设置都经过 LoadSettings 的校验；真拿到非法值时按「不设限」处理：
		// 一份写错的预算不该把所有人挡在门外，更不该让服务起不来。但状态里要看得出
		// 这是「写坏了退回默认」而不是「用户本来就没设限」——否则他会以为自己设的
		// 限额在生效，实际一分没限。
		s = DefaultSettings()
		degraded = true
	}
	b := &Budget{settings: s, ledger: ledger, costLookup: lookup, month: monthOf(now), degraded: degraded}
	b.mu.Lock()
	b.rescanLocked(now)
	b.mu.Unlock()
	return b
}

// SetSettings 换一份预算设置（第 5 步的热更新用）。归一化后校验，非法值原样报错、
// 旧设置保持不变——半份配置比不更新更糟。
func (b *Budget) SetSettings(s Settings) error {
	s = normalize(s)
	if err := ValidateSettings(s); err != nil {
		return err
	}
	b.mu.Lock()
	b.settings = s
	b.mu.Unlock()
	return nil
}

// MarkDegraded 标记这次判定不可靠（调用方读设置失败时用）：此后不拦。
func (b *Budget) MarkDegraded() {
	b.mu.Lock()
	b.degraded = true
	b.mu.Unlock()
}

// MarkReady 清除设置侧的降级标记。调用方确认设置又能读了之后再调。
func (b *Budget) MarkReady() {
	b.mu.Lock()
	b.degraded = false
	b.mu.Unlock()
}

// MarkLedgerDegraded 标记「账本探活失败」：目录写不进去，新账记不下来而界面上
// 看不出这一点。与 MarkDegraded 分开是因为它能自愈——一次成功的重扫或一次成功的
// 记账就把它清掉（见 rescanLocked、Record）。
func (b *Budget) MarkLedgerDegraded() {
	b.mu.Lock()
	b.ledgerDegraded = true
	b.mu.Unlock()
}

// Refresh 按当前的账本与价格表重新扫一遍本月用量。
//
// 价格表在运行中可以变（模型设置里改单价）：本月已用金额是按旧价算的，保存模型
// 之后调一次，状态与拦截立刻按新价算，不必等跨月或一次故障重试。只重扫账本与
// 金额，设置侧的降级标记不动——账本能读并不代表设置读得出来（MarkReady 管那个）。
func (b *Budget) Refresh(now time.Time) {
	b.recordMu.Lock()
	defer b.recordMu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	if monthOf(now) > b.month {
		b.rollLocked(now) // 跨月时它自己会按新月份重扫
		return
	}
	// 同一月，或 now 比预算旧（时钟倒退）：只重扫当前月，月份不跟着倒退。
	b.rescanLocked(now)
}

// errNoLedger 是「有预算但没有账本」时的写入失败。它不致命：花费仍然计入本月已用
// （见 Record），只是账本上没有对应的行。
var errNoLedger = errors.New("usage: ledger unavailable")

// Record 先把一条记录追加进账本，再把它累加进当月已用。
//
// 追加与累加必须在同一个原子边界里（recordMu）：扫描是「把当月分片整段读一遍」，
// 两者一旦交错，同一笔账就会被扫描和这里各数一次。持 recordMu 期间扫描进不来，
// 于是每条记录只有一条路径能数它——扫到的由扫描数，扫不到的由这里数。
//
// 追加失败也照样累加：钱已经花掉了，只是账本上少了一行。此时置 WriteFailed 让
// 状态里看得出统计不完整，但仍按已观测到的花费拦截——写不进去不该变成不设防。
// 累加的同时把这一笔留在 pending 里，之后的重扫会逐条重放（见 rescanLocked），
// 否则一次 Refresh 就会把它换回磁盘合计、下一次请求重新获准。
// 账本为 nil 时同理，返回一个可识别的错误供调用方记日志。
//
// 记录的月份可能比 b.month 新：请求在月底通过检查、次月才返回用量，这时 b.month
// 还停在上个月（下一次 Status 才会跨月）。先把月份对齐到这条记录再写——不对齐的话，
// 这一笔落盘后既不在当月的重扫范围里、也会被 observeLocked 的月份判断跳过，下一次
// 检查显示 0 并放行。反方向（迟到的上月记录）只写盘、不进这个月的账（见 pendingWrite）。
func (b *Budget) Record(e Entry) error {
	b.recordMu.Lock()
	defer b.recordMu.Unlock()
	if m := entryMonth(e); m > b.month {
		ts := e.TS
		if ts.IsZero() {
			ts = time.Now()
		}
		b.mu.Lock()
		b.month = m
		// 旧月份的失败记录与标记都归旧月份：新月份从磁盘重新算起（与 rollLocked 同）。
		b.pending = nil
		b.writeFailed = false
		// 重扫必须在 Append 之前：它把新月份磁盘上已有的记录读进 spent，而这一笔还没
		// 落盘，此后只由 observe 或 pendingWrite 数一次。反过来先 Append 再重扫，成功的
		// 那一笔会被扫描与累加各数一次。
		b.rescanLocked(ts)
		b.mu.Unlock()
	}
	if b.ledger == nil {
		b.pendingWrite(e)
		return errNoLedger
	}
	if err := b.ledger.Append(e); err != nil {
		b.pendingWrite(e)
		return err
	}
	// 追加成功：目录可写了，探活失败那一档降级可以清掉。
	b.mu.Lock()
	b.ledgerDegraded = false
	b.mu.Unlock()
	b.observe(e)
	return nil
}

// pendingWrite 累加一条没能落盘的记录，并把它留在 pending 里供重扫重放。
// 调用方需持有 recordMu。
//
// 两个动作必须在同一把 mu 里完成：累加与「记下要重放的那一笔」之间只要隔开一次
// 重扫（例如 Status 在 30 秒后的重试），那条记录就会一头被重扫抹掉、一头还没进
// pending。写失败不清 ledgerDegraded，也不因为下一条成功而清掉 writeFailed——
// 这一笔确实不在账本里，重扫读回来的合计仍然缺它。
//
// 只认当前自然月的记录：迟到的上月记录既不是这个月的花费，写没写进去也与这个月的
// 统计完不完整无关，因此它不进 pending、也不置 writeFailed（月份的判断口径与
// observeLocked 一致，改一处就要改另一处）。
func (b *Budget) pendingWrite(e Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if entryMonth(e) != b.month {
		return
	}
	b.observeLocked(e)
	b.writeFailed = true
	b.pending = append(b.pending, e)
}

// Observe 把一条已经落盘的记录累加进当月已用，不碰账本。
//
// 正常记账走 Record——那条路径同时负责追加与互斥；这里留给已经自己写过账本的
// 调用方与测试。按 Entry.Day 判归属，与 TotalsOnMonth 同一口径；Day 为空
// （调用方自己构造、没等 Append 补）时按 TS 的本机时区算。
func (b *Budget) Observe(e Entry) {
	b.recordMu.Lock()
	defer b.recordMu.Unlock()
	b.mu.Lock()
	// 走到这里说明那笔账已经落了盘：目录可写了，探活失败那一档降级可以清掉。
	b.ledgerDegraded = false
	b.mu.Unlock()
	b.observe(e)
}

// observe 把一条记录累加进当月已用。调用方需持有 recordMu。
func (b *Budget) observe(e Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.observeLocked(e)
}

// observeLocked 把一条记录累加进当月已用。调用方需持有 b.mu。
//
// 拆出这个「已持锁」的版本是为了重扫能重放 pending（见 rescanLocked）：那条路径
// 已经持有 mu，再走 observe 就是自锁。查价放在锁内做——价格表是别处维护的只读
// 快照，重放时逐条查一遍与实时记账走同一段代码，口径才一致。
//
// 只认当前月的条目：别的月份的迟到记录不混进这个月的账。
//
// 金额在这一步就按价格表算好：请求路径上不能为了几个数重读一遍当月的分片。查不到
// 价格（或价格没写币种）的 token 记进 unpricedTokens 而不是 0 元——「没定价」与
// 「免费」是两件相反的事。
func (b *Budget) observeLocked(e Entry) {
	tokens := int64(e.Prompt + e.Completion)
	if tokens <= 0 {
		return // Append 已拒绝负数；这里再挡一次，一个坏值不该把整月抹平
	}
	if entryMonth(e) != b.month {
		return
	}
	var (
		price  Price
		priced bool
	)
	if b.costLookup != nil {
		price, priced = b.costLookup(e.Provider, e.Model)
	}
	b.spent += tokens
	currency := strings.ToUpper(strings.TrimSpace(price.Currency))
	if !priced || currency == "" {
		b.unpricedTokens += tokens
		b.costInexact = true
		return
	}
	amount, exact, overflow := price.Cost(Totals{
		Prompt: e.Prompt, Completion: e.Completion, Cached: e.Cached, CacheWrite: e.CacheWrite,
	})
	// 金额被截断时这个数同样只能当估算看：BudgetStatus 没有单独的「已截断」字段，
	// 先用 CostInexact 把「别拿它当准数」这条提示带出去。
	if !exact || overflow {
		b.costInexact = true
	}
	if b.spentCostUnits == nil {
		b.spentCostUnits = make(map[string]int64, 1)
	}
	// 饱和相加：两笔各 5e9 元的合法花费合起来超过纳单位量程，溢出成负数会让
	// 「这个月花了多少」与拦截判定一起反过来（见 satAdd）。
	b.spentCostUnits[currency] = satAdd(b.spentCostUnits[currency], costUnits(amount))
}

// Status 返回预算当前月份的快照。跨进新月份时先按新月份重扫账本再算；
// 上一次扫描失败时，隔 rescanRetry 再试一次。
//
// now 可能比预算的月份旧（月底发出的请求拖到新月份才走到这里）：旧时间不倒退状态
// （见 rollLocked），返回的仍是预算当前月份的快照，而不是那个历史月。查询历史月
// 要看账本报告（ReportOnMonth 那一族），不是这里。
//
// 它持 recordMu：重扫与「追加 + 累加」必须互斥，否则扫描读到的记录可能同时被
// Record 数一遍（见 Record）。这条路径只在状态查询与请求闸门上走，代价可接受。
func (b *Budget) Status(now time.Time) BudgetStatus {
	b.recordMu.Lock()
	defer b.recordMu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked(now)
	// 扫描失败不该让整月都按「数据不可靠」放过：那时候预算恒不拦，而它正是最需要
	// 上限的时候。只在失败后重试——好的账本不必在请求路径上反复读盘。
	if b.scanFailed && b.ledger != nil && now.Sub(b.lastScan) >= rescanRetry {
		b.rescanLocked(now)
	}
	return b.statusLocked()
}

// Blocked 判定这一轮该不该被拦下，并返回判定所用的快照。
//
// 拦截的两种例外：降级时一律不拦（数据不可靠，宁可放过）；前台且在 skip_background
// 策略下不拦——那一条策略的意思是「后台先停，人还能继续用」。
func (b *Budget) Blocked(now time.Time, interactive bool) (BudgetStatus, bool) {
	st := b.Status(now)
	if st.Degraded || !st.Exceeded {
		return st, false
	}
	if interactive && st.Over == OverSkipBackground {
		return st, false
	}
	return st, true
}

// statusLocked 按当前设置与已用数拍一份快照。调用方需持有 b.mu。
//
// 两个维度各自判定，任一到达阈值就提醒、任一到达上限就超限：对发起方来说「为什么
// 被拦」不重要，重要的是别再花。token 与金额的判据都要求已用为正——warn_percent
// 设成 0 的意思是「一花就提醒」，不是「还没花就提醒」。
func (b *Budget) statusLocked() BudgetStatus {
	s := b.settings
	currency := strings.ToUpper(strings.TrimSpace(s.Currency))
	ready := b.costLookup != nil
	// 金额一律用纳单位整数判定：浮点累加出来的 0.9999999999999999 会让「恰好到限」
	// 判不出来，而界面上那个数已经显示成 1.00 / 1.00 了。实时记账与重扫都用同一精度
	// 逐笔舍入（见 costUnitsPerUnit），两条路径因此不会给出两个答案。
	units := b.spentCostUnits[currency]
	limitUnits := costUnits(s.MonthlyCost)
	spentCost := costUnitsToAmount(units)
	st := BudgetStatus{
		Month:          b.month,
		Enabled:        s.MonthlyTokens > 0 || s.MonthlyCost > 0,
		LimitTokens:    s.MonthlyTokens,
		SpentTokens:    b.spent,
		LimitCost:      s.MonthlyCost,
		SpentCost:      spentCost,
		Currency:       currency,
		CostReady:      ready,
		CostInexact:    b.costInexact || b.unpricedTokens > 0,
		UnpricedTokens: b.unpricedTokens,
		// 设了金额上限、本月却一分都没折进预算币种：钱的上限其实没在兜底。有价格表
		// 却没花到本币时，原因只可能是「用到的模型没定价」或「花在别的币种上」，
		// 两者分开报，免得用户以为填了价格就万事大吉。没有价格表那一档由 CostReady
		// 交代，不在这里重复。
		CostUnpriced: ready && s.MonthlyCost > 0 && units == 0 && b.unpricedTokens > 0,
		WarnPercent:  s.WarnPercent,
		Over:         s.Over,
		Degraded:     b.degraded || b.scanFailed || b.ledgerDegraded,
		SkippedFiles: b.skippedFiles,
		WriteFailed:  b.writeFailed,
	}
	// 别的币种的金额没计进本币预算：这个数因此低于实际花费，得让看的人知道。
	for cur, other := range b.spentCostUnits {
		if cur != currency && other > 0 {
			st.CostInexact = true
			st.ForeignCost = s.MonthlyCost > 0
		}
	}
	tokenLimited := s.MonthlyTokens > 0
	// 没有价格表时金额恒为 0，按它判定只会得出「还绰绰有余」这种假话：金额上限
	// 干脆不生效，状态里由 CostReady=false 交代。上限折成 0 纳单位（填了一个
	// 小到不足一纳单位的数）同样按「没有金额上限」处理：那不是用户想要的拦截。
	costLimited := s.MonthlyCost > 0 && ready && limitUnits > 0
	st.Exceeded = (tokenLimited && b.spent >= s.MonthlyTokens) ||
		(costLimited && units >= limitUnits)
	tokenWarn := tokenLimited && ratioAtLeast(b.spent, s.MonthlyTokens, s.WarnPercent)
	costWarn := costLimited && ratioAtLeast(units, limitUnits, s.WarnPercent)
	st.Warn = !st.Exceeded && (tokenWarn || costWarn)
	return st
}

// ratioAtLeast 判定 value 是否到了 limit 的 percent%。
//
// 不能直接写 value*100 >= limit*percent：两个乘积都可能装不进 int64。月限额一亿
// 两千万、阈值八成时 limit*percent 是 9.6e18，超过 int64 的 9.22e18，溢出之后
// 符号翻负，花一个纳单位也会判成「已到阈值」。两个乘积都按 128 位算，先比高 64 位、
// 再比低 64 位，边界（恰好等于）因此判得准。
//
// value 为 0 时一律不提醒：warn_percent=0 的意思是「一花就提醒」，不是「还没花就
// 提醒」。percent 为 0 且 value>0 时恒为真（同样的「一花就提醒」）。
func ratioAtLeast(value, limit int64, percent int) bool {
	if limit <= 0 || value <= 0 {
		return false
	}
	hi, lo := bits.Mul64(uint64(value), 100)
	hi2, lo2 := bits.Mul64(uint64(limit), uint64(percent))
	return hi > hi2 || (hi == hi2 && lo >= lo2)
}

// costUnitsPerUnit 是金额的精度：一纳单位（1e-9）。
//
// 分只有两位，但单价是「每百万 token」的量级，一笔几万 token 的花费常常远小于
// 一分；而拦截的判据是「恰好到限也拦」。精度必须细到让实时逐笔记账与重扫逐笔累加
// 落在同一个整数上：0.1 元/百万 token、一笔 33,334 token 是 0.0033334，按微单位
// 舍入会变成三笔 0.009999（放行），先浮点合计再舍入却是 0.0100002（拦截）——同一批
// 记录两条路径给出两个答案。1e-9 下这一笔正好是 3,333,400 单位，三笔 10,000,200，
// 与上限 0.01（10,000,000）的比较在哪儿都一致。int64 在 1e-9 下仍能表示约九十二亿
// 单位的金额，远超任何预算。
const costUnitsPerUnit = 1_000_000_000

// costUnits 把金额换成纳单位整数。单价、逐笔金额与合计都在这个精度上对齐。
//
// 它必须饱和而不是溢出：价格是用户手填的，两个离谱的合法值（每百万 5e9，上限 8e9）
// 各折一次就超过 int64 的纳单位量程，溢出之后符号翻负——「花了 1e10 元」于是变成
// 「一分没花」，实时与重扫都放行。饱和到 MaxInt64 的后果是「远超任何上限」，那正是
// 这个数想表达的事。NaN 与负数同理归 0：坏值不该把整月的花费抹掉或抵掉。
func costUnits(amount float64) int64 {
	if math.IsNaN(amount) || amount <= 0 {
		return 0
	}
	units := amount * costUnitsPerUnit
	if math.IsInf(units, 0) || units >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	// 舍入之后仍可能跨过 2^63（float64 表示不了 MaxInt64，只能表示 2^63）：
	// 那一步转换在 Go 里是实现相关的，可能给出负数，所以舍入后再挡一次。
	r := math.Round(units)
	if r >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(r)
}

// satAdd 是纳单位的饱和加法：两笔金额相加只会往大里错，绝不能翻成负数。
//
// b<=0 直接返回 a：金额单位算出来不会是负数，这一支只是不给反方向留溢出。
// a > MaxInt64-b 时返回 MaxInt64——判成「远超上限」是安全的那一侧，放行才是危险的。
func satAdd(a, b int64) int64 {
	if b <= 0 {
		return a
	}
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// costUnitsToAmount 把纳单位整数换回给人看的金额。
func costUnitsToAmount(units int64) float64 {
	return float64(units) / costUnitsPerUnit
}

// rollLocked 在 now 跨进新的自然月时重置已用数并按新月份重扫。调用方需持有 b.mu。
//
// 只前进、不后退：m <= b.month 直接返回。now 可能来自月底发出的旧请求（新月份已经
// Record 过之后才拿到锁），把 b.month 拨回旧月会清掉新月的 pending 与 writeFailed、
// 已用量归零——下一笔请求于是重新获准。旧时间只能看到当前状态，不能倒退它。
func (b *Budget) rollLocked(now time.Time) {
	m := monthOf(now)
	if m <= b.month {
		return
	}
	b.month = m
	// 旧月份的失败记录与标记都与新月份无关：重放它们只会把上个月的花费算进这个月，
	// 而 WriteFailed 说的是「这个月的统计不完整」。两者同生命周期，一起清。
	b.pending = nil
	b.writeFailed = false
	b.rescanLocked(now)
}

// rescanLocked 按当前月份重新读一遍账本，并记下这次扫描的时刻。调用方需持有 b.mu
// （或独占 b）。
//
// 走成本合计而不是 token 合计：一次扫描要同时给出 token、金额、未定价与精确性，
// 两套扫描口径会让同一段区间里的数字对不上。lookup 为 nil 时全部记成未定价。
//
// 重扫成功只清「扫描失败」与「探活失败」这两档：它们的理由是「已用数不可信」，
// 数字重新读得回来就该恢复拦截。设置读不出来是另一回事，账本能读不代表它好了。
//
// 坏分片（sum.Total.SkippedFiles）不给降级：能读的部分是真数据，按它判定不会
// 多拦；放宽拦截反而会让一个坏分片把整月变成不设防。这个数字单独报出去。
//
// 载入磁盘合计之后还要重放 pending（写失败但已计入的记录）：重扫的意义是「按磁盘
// 重来」，而磁盘上偏偏没有那几笔；不重放，Refresh 就成了一次丢失花费的捷径。
// 扫描失败时停在降级状态上，pending 不进这个月的数——那一档本来就不拦。
func (b *Budget) rescanLocked(now time.Time) {
	b.lastScan = now
	b.spent = 0
	b.spentCostUnits = nil
	b.unpricedTokens = 0
	b.costInexact = false
	b.skippedFiles = 0
	if b.ledger == nil {
		b.scanFailed = true
		return
	}
	sum, err := b.ledger.SummaryCostOnMonth(b.month, GroupByKind, b.costLookup)
	if err != nil {
		b.scanFailed = true
		return
	}
	b.scanFailed = false
	b.ledgerDegraded = false
	b.spent = int64(sum.Total.Prompt + sum.Total.Completion)
	b.unpricedTokens = int64(sum.Total.UnpricedTokens)
	b.skippedFiles = sum.Total.SkippedFiles
	b.costInexact = sum.Total.Inexact || sum.Total.Overflow || b.unpricedTokens > 0
	// 金额优先用逐笔舍入后的整数合计：实时记账也是逐笔累加的，两条路径在同一精度上
	// 相加才能对同一批记录给出同一个答案（见 costUnitsPerUnit）。只有手工构造的
	// CostTotals（没有这个包内字段）才回落到浮点合计再舍入。
	if len(sum.Total.amountsUnits) > 0 {
		b.spentCostUnits = make(map[string]int64, len(sum.Total.amountsUnits))
		for cur, units := range sum.Total.amountsUnits {
			// 币种由用户填在价格表里，大小写不该决定它算不算同一个桶。
			// 同一币种可能被大小写拆成两个键，合并时同样要饱和（见 satAdd）。
			b.spentCostUnits[strings.ToUpper(cur)] = satAdd(b.spentCostUnits[strings.ToUpper(cur)], units)
		}
	} else {
		for cur, amount := range sum.Total.Amounts {
			if b.spentCostUnits == nil {
				b.spentCostUnits = make(map[string]int64, len(sum.Total.Amounts))
			}
			b.spentCostUnits[strings.ToUpper(cur)] = satAdd(b.spentCostUnits[strings.ToUpper(cur)], costUnits(amount))
		}
	}
	// 磁盘合计里没有那些「写失败但已计入预算」的记录，逐条重放回去（见 pending）。
	// 不重放的话，一次 Refresh 就会让失败的那笔消失、下一次请求重新获准。
	for _, e := range b.pending {
		b.observeLocked(e)
	}
}

// monthOf 返回 now 所在的自然月（本机时区）。墙上的月份才是预算的月份：
// 用户不会有「UTC 的下个月」这个概念。
func monthOf(now time.Time) string {
	return now.In(time.Local).Format(monthFormat)
}

// entryMonth 返回一条记录归属的自然月，口径与 TotalsOnMonth 一致。
func entryMonth(e Entry) string {
	if d := e.Day; len(d) >= len(monthFormat) {
		return d[:len(monthFormat)]
	}
	ts := e.TS
	if ts.IsZero() {
		ts = time.Now()
	}
	return monthOf(ts)
}
