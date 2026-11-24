package main

import (
	"flag"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"wen/internal/config"
	"wen/internal/modelcfg"
	"wen/internal/statustext"
	"wen/internal/usage"
)

// maxUsageDays 是 --days 的上限。十年足够回答「最近花了多少」，而它挡住的是
// 一个手抖多敲两个零之后要开着几千个文件等半天的命令。
const maxUsageDays = 3660

// dayFormat 与账本分片名同形。这里只用来拼「最近 N 天」里每一天的日期。
const dayFormat = "2006-01-02"

// usageOptions 是 wen usage 的视图选项。Days 为 0 表示月视图。
type usageOptions struct {
	Month bool // --month：显式要月视图（默认也是它），与 --days 互斥
	Days  int  // --days N：最近 N 天（含今天），逐日读取后在本地合并
	By    string
}

// runUsage 解析参数并打印用量汇总。
//
// 账本直接按文件读：服务在跑也照读，不经过它的接口——用量是只追加的旁路数据，
// 没有「谁拥有它」的问题，而为只读视图拉起一个后端只会让它依赖服务是否应答。
func runUsage(args []string) error {
	fs := flag.NewFlagSet("usage", flag.ExitOnError)
	configPath := fs.String("c", "", tr("cli.flag.config", "配置文件路径（默认 ./config.yaml 或 ~/.wen/config.yaml）"))
	month := fs.Bool("month", false, tr("cli.usage.flag.month", "看当前自然月（默认）"))
	days := fs.Int("days", 0, tr("cli.usage.flag.days", "改为最近 N 天（含今天）"))
	by := fs.String("by", usage.GroupBySource, tr("cli.usage.flag.by", "分组维度：day、source、model、kind、provider、provider_model"))
	_ = fs.Parse(args)

	// 显式给 0 与「没给」在 flag 里长得一样（默认值都是 0），所以要看它有没有被设过：
	// --days 0 是写错，不该悄悄当成月视图。
	daysSet := false
	fs.Visit(func(f *flag.Flag) { daysSet = daysSet || f.Name == "days" })
	if daysSet && *days == 0 {
		return fmt.Errorf("%s", tr("cli.usage.err.days_positive", "--days 必须是正整数"))
	}

	path := config.ResolvePath(*configPath)
	cfg, err := config.Load(path)
	if err != nil {
		return fmt.Errorf(tr("cli.err.load_config", "加载配置失败: %w"), err)
	}
	adoptLang(cfg)

	report, err := usageReport(cfg, usageOptions{Month: *month, Days: *days, By: *by}, time.Now())
	if err != nil {
		return err
	}
	fmt.Print(report)
	return nil
}

// usageReport 生成整段报告文本。参数解析与打印留给 runUsage，好让这一层能直接测。
func usageReport(cfg *config.Config, opts usageOptions, now time.Time) (string, error) {
	if opts.Days < 0 {
		return "", fmt.Errorf("%s", tr("cli.usage.err.days_positive", "--days 必须是正整数"))
	}
	if opts.Days > maxUsageDays {
		return "", fmt.Errorf(tr("cli.usage.err.days_max", "--days 不能超过 %d 天"), maxUsageDays)
	}
	if opts.Days > 0 && opts.Month {
		return "", fmt.Errorf("%s", tr("cli.usage.err.month_days", "--month 与 --days 不能同时使用"))
	}
	by := opts.By
	if by == "" {
		by = usage.GroupBySource
	}
	if !usageGroupByValid(by) {
		return "", fmt.Errorf(tr("cli.usage.err.by", "未知的分组维度 %q（可用：day、source、model、kind、provider、provider_model）"), by)
	}

	ledger, err := usage.Open(usageDirFor(cfg, runtimeOverrides{}))
	if err != nil {
		return "", err
	}
	// 价格表来自模型配置：读不出来就只报 token，并在报告里明说一句——把金额
	// 静默留空，用户会以为这个月一分钱没花。
	lookup, priceErr := usagePriceLookup(cfg)

	var (
		sum  usage.CostSummary
		span string
	)
	month := now.In(time.Local).Format("2006-01")
	if opts.Days > 0 {
		span = trf("cli.usage.range.days", "最近 %d 天", opts.Days)
		sum, err = mergeRecentDays(ledger, by, lookup, now, opts.Days)
	} else {
		// 月视图按自然月，日视图逐日——都不是绝对区间：跨午夜回拨的城市里同一个
		// 当地日期在时间轴上分成两段，区间查询只能盖住第一段（见 usage.DayBounds）。
		span = trf("cli.usage.range.month", "%s（本月）", month)
		sum, err = ledger.SummaryCostOnMonth(month, by, lookup)
	}
	if err != nil {
		return "", err
	}

	rows := []usageRow{{text: tr("cli.usage.title", "Wen Agent 用量")}}
	rows = append(rows, usageRow{label: tr("cli.usage.range", "范围"), text: span})
	if priceErr != nil {
		rows = append(rows, usageRow{text: trf("cli.usage.price_failed", "价格表读取失败，报告不含金额：%v", priceErr)})
	}
	if sum.Total.Requests == 0 {
		rows = append(rows, usageRow{text: tr("cli.usage.empty", "这段时间还没有用量记录")})
	} else {
		rows = append(rows, usageRow{label: tr("cli.usage.total", "合计"), text: usageCountsLine(sum.Total)})
	}

	// 预算按自然月，与上面的区间口径不同：标签写明「本月」，别让它被读成「这段时间」。
	// 设置读不出来时也仍出这一行（写「不可用」），不静默。
	budgetDetail, budgetWarn := usageBudgetDetail(cfg, ledger, lookup, now)
	if budgetDetail != "" {
		rows = append(rows, usageRow{label: tr("cli.usage.budget", "本月预算"), text: budgetDetail})
	}
	if budgetWarn != "" {
		rows = append(rows, usageRow{text: budgetWarn})
	}

	if sum.Total.Requests > 0 {
		rows = append(rows, usageRow{text: usageGroupHeading(by)})
		for _, g := range sum.Groups {
			rows = append(rows, usageRow{label: "  " + usageKeyLabel(by, g.Key), text: usageCountsLine(g.CostTotals)})
		}
	}
	// 坏掉的分片只跳过不报错，但合计会因此偏少——不说出来，用户会把它当成真实数字。
	if sum.Total.SkippedFiles > 0 {
		rows = append(rows, usageRow{text: trf("cli.usage.skipped", "有 %d 个账本分片读不出来，合计可能偏少", sum.Total.SkippedFiles)})
	}
	return renderUsageRows(rows), nil
}

// usageRow 是报告里的一行：label 非空时按显示宽度与其它行对齐，为空时整行原样输出
// （标题、分组标题、警告这类不分栏的行）。
type usageRow struct {
	label string
	text  string
}

// renderUsageRows 把各行排成文本。
//
// 不用 text/tabwriter：它按 rune 数而不是显示宽度补空格，中文标签（每个字两列）
// 与英文标签混排时后半列永远对不齐。这里的宽度由 displayWidth 数，中英都成立。
func renderUsageRows(rows []usageRow) string {
	width := 0
	for _, r := range rows {
		if w := displayWidth(r.label); w > width {
			width = w
		}
	}
	var b strings.Builder
	for _, r := range rows {
		if r.label == "" {
			b.WriteString(r.text)
			b.WriteByte('\n')
			continue
		}
		b.WriteString(padLabel(r.label, width))
		b.WriteString("  ")
		b.WriteString(r.text)
		b.WriteByte('\n')
	}
	return b.String()
}

// padLabel 用空格把一段标签补到 width 列。表头与分组行共用它，中文按两列算，
// 因此「范围」「合计」「本月预算」与分组名在同一列上收尾。
func padLabel(label string, width int) string {
	if pad := width - displayWidth(label); pad > 0 {
		return label + strings.Repeat(" ", pad)
	}
	return label
}

// displayWidth 数一段文字的终端显示宽度：CJK 文字与全角标点占两列，其余占一列。
// 只做到「够排版」为止，不追求完整的 East Asian Width 表。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		if isWideRune(r) {
			w += 2
			continue
		}
		w++
	}
	return w
}

// isWideRune 判断一个字符是不是双列宽。覆盖界面文案里会出现的那几档：
// 汉字、假名、谚文、CJK 标点，以及全角字母数字与符号。
func isWideRune(r rune) bool {
	switch {
	case unicode.Is(unicode.Han, r),
		unicode.Is(unicode.Hangul, r),
		unicode.Is(unicode.Hiragana, r),
		unicode.Is(unicode.Katakana, r):
		return true
	case r >= 0x3000 && r <= 0x303F: // CJK 标点（、。「」等）
		return true
	case r >= 0xFF00 && r <= 0xFF60: // 全角字母数字与标点
		return true
	case r >= 0xFFE0 && r <= 0xFFE6: // 全角货币与符号
		return true
	}
	return false
}

// usageCounts 把一段合计排成一行人读的短语。
func usageCounts(t usage.Totals) string {
	if t.Cached > 0 {
		return trf("cli.usage.counts.cached", "%d 次请求 · 输入 %s · 输出 %s · 缓存命中 %s（%s%%）",
			t.Requests, statustext.Comma(t.Prompt), statustext.Comma(t.Completion),
			statustext.Comma(t.Cached), statustext.Pct(t.Cached, t.Prompt))
	}
	return trf("cli.usage.counts", "%d 次请求 · 输入 %s · 输出 %s",
		t.Requests, statustext.Comma(t.Prompt), statustext.Comma(t.Completion))
}

// usageCountsLine 是合计/分组行：token 短语后面按需追加金额、未定价与估算标记。
func usageCountsLine(c usage.CostTotals) string {
	return usageCounts(c.Totals) + usageCostSuffix(c)
}

// usageCostSuffix 给出金额那一段，没有可说的就返回空串。
//
// 多币种并列而不是折成一个数：价格表里的单价可能来自不同地区的账单，汇率要联网、
// 会过时，并列几行至少不会说谎。未定价、「缓存价回退」与「金额被截断」各自都要标
// 出来，它们都会让这个数字偏离账单，只是原因不同。
func usageCostSuffix(c usage.CostTotals) string {
	var parts []string
	if len(c.Amounts) > 0 {
		curs := make([]string, 0, len(c.Amounts))
		for cur := range c.Amounts {
			curs = append(curs, cur)
		}
		sort.Strings(curs)
		amounts := make([]string, 0, len(curs))
		for _, cur := range curs {
			amounts = append(amounts, cur+" "+statustext.Money(c.Amounts[cur]))
		}
		parts = append(parts, trf("cli.usage.cost.estimate", "估算 %s", strings.Join(amounts, " · ")))
	}
	if c.UnpricedTokens > 0 {
		parts = append(parts, trf("cli.usage.cost.unpriced", "未定价 %s tokens", statustext.Comma(c.UnpricedTokens)))
	}
	if c.Inexact {
		parts = append(parts, tr("cli.usage.cost.inexact", "缓存价未填，按输入价估算"))
	}
	// 金额被截断是另一个原因：它说的不是「估算不准」，而是「这个数放不下」。两者
	// 可以同时成立，但溢出必须单独出现——不然用户会照着「缓存价未填」去改价格，
	// 而价格其实填了，只是金额大得超出了 float64。
	if c.Overflow {
		parts = append(parts, tr("cli.usage.cost.overflow", "金额超出可表示范围，已截断"))
	}
	if len(parts) == 0 {
		return ""
	}
	return " · " + strings.Join(parts, " · ")
}

// usagePriceLookup 用模型配置里的价格表给用量折算金额。
//
// 读不出来就返回 nil：报告只报 token，由调用方明说「不含金额」。金额是旁路信息，
// 一份读不出来的 models.json 不该让整个用量报告消失。
func usagePriceLookup(cfg *config.Config) (usage.PriceLookup, error) {
	store, err := modelcfg.NewStore(uiLang, filepath.Join(cfg.BaseDir, "models.json"), cfg)
	if err != nil {
		return nil, err
	}
	return store.Price, nil
}

// usageBudgetDetail 给出预算行的内容与（可能有）的一行警告。
//
// 只在设了限、数据不可靠或记账失败时给内容：不设限是默认状态，每次都在报告里留
// 一行「本月预算：不设限」只会让人以为自己在哪儿开过它。
func usageBudgetDetail(cfg *config.Config, ledger *usage.Ledger, lookup usage.PriceLookup, now time.Time) (detail, warn string) {
	settings, err := usage.LoadSettings(usageSettingsFor(cfg, runtimeOverrides{}))
	budget := usage.NewBudget(settings, ledger, lookup, now)
	if err != nil {
		// 读不出来就标降级：退回默认值（不设限）只是为了让程序继续跑，报告里
		// 必须说「不可用」，否则用户会以为自己设的上限在生效。
		budget.MarkDegraded()
		warn = trf("cli.usage.budget.settings_failed", "预算设置读取失败：%v", err)
	}
	return formatUsageBudget(budget.Status(now)), warn
}

// formatUsageBudget 把一份预算快照排成预算行的内容；不设限、不降级也不记账失败时
// 返回空串（默认配置就是不设限，每次留一行「不设限」只会让人以为在哪儿开过它）。
//
// token 与金额两段并列，任一段的限额为 0 就整段省略：只设钱的上限是正常配置，
// 不该显示「0 / 0 tokens」。没设上限但账本写不进去时仍出一行：合计偏少与上限定没定
// 无关，藏起来就会让人把偏低的数字当成真实用量。
func formatUsageBudget(st usage.BudgetStatus) string {
	if !st.Enabled && !st.Degraded && !st.WriteFailed {
		return ""
	}
	// 降级时不报数字：账本或设置读不出来，任何比例都只会被当成真实用量。
	if st.Degraded {
		return tr("cli.usage.budget.degraded", "不可用（账本或设置读不出来）")
	}
	if !st.Enabled {
		return tr("cli.usage.budget.write_failed_only", "未设置（账本写入失败，统计可能不完整）")
	}
	var parts []string
	if st.LimitTokens > 0 {
		parts = append(parts, trf("cli.usage.budget.line", "%s / %s tokens（%s%%）",
			statustext.Comma64(st.SpentTokens), statustext.Comma64(st.LimitTokens),
			statustext.Pct64(st.SpentTokens, st.LimitTokens)))
	}
	if cost := budgetCostPart(st); cost != "" {
		parts = append(parts, cost)
	}
	detail := strings.Join(parts, " · ")
	// 设了金额上限却没有价格表：那一段永远不会生效，必须说出来。
	if st.LimitCost > 0 && !st.CostReady {
		detail += tr("cli.usage.budget.no_price", "（缺少价格，金额上限未生效）")
	} else if st.LimitCost > 0 && st.CostUnpriced {
		// 价格表在，本月用到的模型却一个都没定价：同样等于没有金额上限。
		detail += tr("cli.usage.budget.unpriced_usage", "（本月有用量没有价格，金额上限未生效）")
	}
	// 别的币种不计入本币上限；坏分片让合计偏少。两者都会让上面的数字偏离真相。
	if st.LimitCost > 0 && st.ForeignCost {
		detail += trf("cli.usage.budget.foreign", "（有其他币种的花费未计入 %s 上限）", st.Currency)
	}
	if st.SkippedFiles > 0 {
		detail += trf("cli.usage.budget.skipped", "（%d 个账本分片读不出来，合计可能偏少）", st.SkippedFiles)
	}
	// 记账失败只让统计偏少，不改变拦截：说出来即可。
	if st.WriteFailed {
		detail += tr("cli.usage.budget.write_failed", "（账本写入失败，本月统计可能不完整）")
	}
	// 超限的拦截范围取决于 over：block_all 连前台一起停，只说「后台」是低报。
	switch {
	case st.Exceeded && st.Over == usage.OverBlockAll:
		detail += tr("cli.usage.budget.exceeded_all", "（已达上限，所有请求已暂停）")
	case st.Exceeded:
		detail += tr("cli.usage.budget.exceeded", "（已达上限，后台任务已暂停）")
	case st.Warn:
		detail += tr("cli.usage.budget.warn", "（接近上限）")
	}
	return detail
}

// budgetCostPart 给出预算行里的金额段，没有可说的就返回空串。
//
// 金额后面按需缀一句「这个数不准」，口径与 statustext 的 costDetail 完全一致：
// 未定价与缓存价回退二选一说一句就够，它们都会让金额偏离账单，只是原因不同。
// 花了 0 元时两句都不说——那时该由 CostUnpriced / ForeignCost 解释为什么是 0。
func budgetCostPart(st usage.BudgetStatus) string {
	if !st.CostReady || (st.LimitCost <= 0 && st.SpentCost <= 0) {
		return ""
	}
	amount := statustext.Money(st.SpentCost)
	if st.LimitCost > 0 {
		amount += " / " + statustext.Money(st.LimitCost)
	}
	if st.Currency != "" {
		amount = st.Currency + " " + amount
	}
	switch {
	case st.SpentCost > 0 && st.UnpricedTokens > 0:
		amount += trf("cli.usage.budget.unpriced", "（含 %s tokens 未定价，金额为估算）",
			statustext.Comma64(st.UnpricedTokens))
	case st.SpentCost > 0 && st.CostInexact:
		amount += tr("cli.usage.budget.inexact", "（金额为估算，可能偏高或偏低）")
	}
	return amount
}

// mergeRecentDays 逐日汇总最近 days 天（含今天），再按键合并。
//
// 不用 Summary 的绝对区间：跨午夜回拨的城市里同一个当地日期会分成两段，区间只盖
// 得住第一段。逐日按条目落盘的 Day 筛，最后在本地合并分组——分组口径仍然只有
// 账本那一份，金额也跟着同一份记录算，不另算一遍。
func mergeRecentDays(ledger *usage.Ledger, by string, lookup usage.PriceLookup, now time.Time, days int) (usage.CostSummary, error) {
	out := usage.CostSummary{Groups: []usage.GroupCost{}}
	groups := map[string]usage.CostTotals{}
	for _, day := range recentDayLabels(now, days) {
		s, err := ledger.SummaryCostOnDay(day, by, lookup)
		if err != nil {
			return usage.CostSummary{}, err
		}
		out.Total.Add(s.Total)
		for _, g := range s.Groups {
			t := groups[g.Key]
			t.Add(g.CostTotals)
			groups[g.Key] = t
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out.Groups = append(out.Groups, usage.GroupCost{Key: k, CostTotals: groups[k]})
	}
	return out, nil
}

// recentDayLabels 给出最近 days 天的当地日期（含今天），由近及远、不重复。
//
// 日期算术放在 UTC 里做，只取当地日期的年月日当输入：有的城市整段日历日被跳过
// （Pacific/Apia 的 2011-12-30），在当地时区里 AddDate 到那一天会归一化回前一天，
// 同一个日期因此被读两遍、合计双计。跳过的日期墙上本来就不存在，读到 0 条正是
// 想要的结果。map 是第二道保险：真正的去重规则在这里，而不在日历的边角行为上。
func recentDayLabels(now time.Time, days int) []string {
	y, m, d := now.In(time.Local).Date()
	out := make([]string, 0, days)
	seen := make(map[string]bool, days)
	for i := 0; i < days; i++ {
		// UTC 没有夏令时、没有跳日，日历减法在这里是干净的。
		day := time.Date(y, m, d-i, 0, 0, 0, 0, time.UTC).Format(dayFormat)
		if seen[day] {
			continue
		}
		seen[day] = true
		out = append(out, day)
	}
	return out
}

// usageGroupByValid 认下账本支持的分组维度。provider_model 收在这里是因为价格挂在
// 模型上：同一个模型名可能出现在两家提供商下、单价不同，按它分组金额才对得上。
func usageGroupByValid(by string) bool {
	switch by {
	case usage.GroupByDay, usage.GroupBySource, usage.GroupByModel, usage.GroupByKind,
		usage.GroupByProvider, usage.GroupByProviderModel:
		return true
	}
	return false
}

// usageGroupHeading 是分组那一段的标题。
func usageGroupHeading(by string) string {
	switch by {
	case usage.GroupByDay:
		return tr("cli.usage.group.day", "按日期")
	case usage.GroupByModel:
		return tr("cli.usage.group.model", "按模型")
	case usage.GroupByKind:
		return tr("cli.usage.group.kind", "按类型")
	case usage.GroupByProvider:
		return tr("cli.usage.group.provider", "按提供商")
	case usage.GroupByProviderModel:
		return tr("cli.usage.group.provider_model", "按提供商与模型")
	}
	return tr("cli.usage.group.source", "按来源")
}

// usageKeyLabel 把分组键翻成界面上的说法。插件名与日期原样输出——它们是用户自己
// 认得的东西，翻译只会让人对不上号。
func usageKeyLabel(by, key string) string {
	switch by {
	case usage.GroupBySource:
		switch key {
		case "foreground":
			return tr("cli.usage.source.foreground", "网页前台")
		case "internal":
			return tr("cli.usage.source.internal", "内部调用")
		}
	case usage.GroupByKind:
		switch key {
		case usage.KindTurn:
			return tr("cli.usage.kind.turn", "对话")
		case usage.KindCompact:
			return tr("cli.usage.kind.compact", "压缩")
		case usage.KindHelper:
			return tr("cli.usage.kind.helper", "辅助")
		}
	}
	if key == "unknown" {
		return tr("cli.usage.unknown", "未知")
	}
	return key
}
