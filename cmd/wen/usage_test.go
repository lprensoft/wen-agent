package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wen/internal/config"
	"wen/internal/i18n"
	"wen/internal/usage"
)

// usageTestSetup 造一个临时配置目录（只有 lang: zh）与它的账本，并把界面语言
// 定成中文——按键断言的期望值写的是中文原文。
func usageTestSetup(t *testing.T) (*config.Config, *usage.Ledger) {
	t.Helper()
	oldLang := uiLang
	uiLang = i18n.LangZH
	t.Cleanup(func() { uiLang = oldLang })

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("lang: zh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := usage.Open(usageDirFor(cfg, runtimeOverrides{}))
	if err != nil {
		t.Fatal(err)
	}
	return cfg, ledger
}

func appendUsage(t *testing.T, l *usage.Ledger, e usage.Entry) {
	t.Helper()
	if err := l.Append(e); err != nil {
		t.Fatal(err)
	}
}

func saveUsageSettings(t *testing.T, cfg *config.Config, s usage.Settings) {
	t.Helper()
	if err := usage.SaveSettings(usageSettingsFor(cfg, runtimeOverrides{}), s); err != nil {
		t.Fatal(err)
	}
}

// 月视图：按来源分组、合计、预算行，以及上个月的记录不该混进来。
func TestUsageReportMonth(t *testing.T) {
	cfg, ledger := usageTestSetup(t)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.Local)
	at := func(day int, hour int) time.Time {
		return time.Date(2026, 11, day, hour, 0, 0, 0, time.Local)
	}
	appendUsage(t, ledger, usage.Entry{TS: at(5, 9), Kind: usage.KindTurn, Prompt: 800_000, Completion: 50_000, Cached: 600_000})
	appendUsage(t, ledger, usage.Entry{TS: at(20, 9), Kind: usage.KindTurn, Prompt: 400_000, Completion: 50_000, Cached: 30_000})
	appendUsage(t, ledger, usage.Entry{TS: at(20, 9), Kind: usage.KindHelper, Origin: "heartbeat", Prompt: 10_000})
	// 上个月：月视图不该算它
	appendUsage(t, ledger, usage.Entry{TS: time.Date(2026, 10, 31, 23, 0, 0, 0, time.Local), Kind: usage.KindTurn, Prompt: 999_999})
	saveUsageSettings(t, cfg, usage.Settings{MonthlyTokens: 2_000_000, WarnPercent: 80, Over: usage.OverSkipBackground})

	got, err := usageReport(cfg, usageOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Wen Agent 用量",
		"2026-11（本月）",
		"3 次请求",
		"输入 1,210,000",
		"输出 100,000",
		"缓存命中 630,000（52.1%）",
		"1,310,000 / 2,000,000 tokens（65.5%）",
		"按来源",
		"网页前台",
		"heartbeat",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("报告里缺少 %q:\n%s", want, got)
		}
	}
	// 10 月那条的 token 数不该出现
	if strings.Contains(got, "999,999") {
		t.Errorf("上个月的记录混进了月视图:\n%s", got)
	}
	if strings.Contains(got, "接近上限") || strings.Contains(got, "已达上限") {
		t.Errorf("65.5%% 未到阈值，不该有阈值后缀:\n%s", got)
	}
}

// --days 逐日合并：窗口外的记录与更早的日期都不该出现。
func TestUsageReportRecentDays(t *testing.T) {
	cfg, ledger := usageTestSetup(t)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.Local)
	appendUsage(t, ledger, usage.Entry{TS: now, Kind: usage.KindTurn, Prompt: 1_000, Completion: 100})
	appendUsage(t, ledger, usage.Entry{TS: time.Date(2026, 11, 19, 23, 0, 0, 0, time.Local), Kind: usage.KindTurn, Prompt: 2_000, Completion: 200})
	appendUsage(t, ledger, usage.Entry{TS: time.Date(2026, 11, 18, 10, 0, 0, 0, time.Local), Kind: usage.KindTurn, Prompt: 4_000, Completion: 400})

	got, err := usageReport(cfg, usageOptions{Days: 2, By: usage.GroupByDay}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"最近 2 天", "2 次请求", "输入 3,000", "按日期", "2026-11-20", "2026-11-19"} {
		if !strings.Contains(got, want) {
			t.Errorf("报告里缺少 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "2026-11-18") || strings.Contains(got, "6,000") {
		t.Errorf("窗口外的记录混进了最近 2 天:\n%s", got)
	}
}

// 超限与接近上限：阈值来自 usage.json，后缀由状态决定。
func TestUsageReportBudgetThresholds(t *testing.T) {
	cfg, ledger := usageTestSetup(t)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.Local)
	appendUsage(t, ledger, usage.Entry{TS: now, Kind: usage.KindTurn, Prompt: 1_700_000})

	saveUsageSettings(t, cfg, usage.Settings{MonthlyTokens: 2_000_000, WarnPercent: 80, Over: usage.OverSkipBackground})
	got, err := usageReport(cfg, usageOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "（接近上限）") {
		t.Errorf("85%% 应报接近上限:\n%s", got)
	}

	saveUsageSettings(t, cfg, usage.Settings{MonthlyTokens: 1_000_000, WarnPercent: 80, Over: usage.OverSkipBackground})
	got, err = usageReport(cfg, usageOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "（已达上限，后台任务已暂停）") {
		t.Errorf("超限应报已达上限:\n%s", got)
	}
}

// 降级：账本或设置读不出来时不报任何数字，只说一句不可用。
func TestFormatUsageBudget(t *testing.T) {
	oldLang := uiLang
	uiLang = i18n.LangZH
	defer func() { uiLang = oldLang }()

	cases := []struct {
		name string
		st   usage.BudgetStatus
		want string
	}{
		{"不设限", usage.BudgetStatus{}, ""},
		{"正常", usage.BudgetStatus{Enabled: true, LimitTokens: 2_000_000, SpentTokens: 1_300_000}, "1,300,000 / 2,000,000 tokens（65.0%）"},
		{"接近", usage.BudgetStatus{Enabled: true, LimitTokens: 2_000_000, SpentTokens: 1_700_000, Warn: true}, "1,700,000 / 2,000,000 tokens（85.0%）（接近上限）"},
		{
			"超限·后台",
			usage.BudgetStatus{Enabled: true, LimitTokens: 2_000_000, SpentTokens: 2_100_000, Exceeded: true, Over: usage.OverSkipBackground},
			"2,100,000 / 2,000,000 tokens（105.0%）（已达上限，后台任务已暂停）",
		},
		{
			"超限·全部",
			usage.BudgetStatus{Enabled: true, LimitTokens: 2_000_000, SpentTokens: 2_100_000, Exceeded: true, Over: usage.OverBlockAll},
			"2,100,000 / 2,000,000 tokens（105.0%）（已达上限，所有请求已暂停）",
		},
		{"降级", usage.BudgetStatus{Enabled: true, LimitTokens: 2_000_000, Degraded: true}, "不可用（账本或设置读不出来）"},
		// 设置读不出来时退回不设限：Enabled 是 false，但降级本身就要报出来
		{"降级且不设限", usage.BudgetStatus{Degraded: true}, "不可用（账本或设置读不出来）"},
	}
	for _, c := range cases {
		got := formatUsageBudget(c.st)
		if got != c.want {
			t.Errorf("%s: %q\nwant %q", c.name, got, c.want)
		}
		if c.st.Degraded && (strings.Contains(got, "/") || strings.Contains(got, "%")) {
			t.Errorf("%s: 降级时不该出现比例: %q", c.name, got)
		}
	}
}

// 预算设置读不出来：报告照出，预算行写「不可用」（不是「不设限」），并说明原因。
func TestUsageReportBrokenSettings(t *testing.T) {
	cfg, ledger := usageTestSetup(t)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.Local)
	appendUsage(t, ledger, usage.Entry{TS: now, Kind: usage.KindTurn, Prompt: 1_000, Completion: 100})
	if err := os.WriteFile(usageSettingsFor(cfg, runtimeOverrides{}), []byte("{ 不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := usageReport(cfg, usageOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"预算设置读取失败", "本月预算", "不可用（账本或设置读不出来）"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "tokens（") {
		t.Errorf("设置读不出来时不该报预算比例:\n%s", got)
	}
}

// 没有记录时不报错，只打一行。
func TestUsageReportEmpty(t *testing.T) {
	cfg, _ := usageTestSetup(t)
	got, err := usageReport(cfg, usageOptions{}, time.Date(2026, 11, 20, 10, 0, 0, 0, time.Local))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "这段时间还没有用量记录") {
		t.Errorf("空账本应打一行提示:\n%s", got)
	}
	if strings.Contains(got, "按来源") || strings.Contains(got, "合计") {
		t.Errorf("空账本不该有合计与分组:\n%s", got)
	}
}

// 参数校验：未知维度、非法天数、互斥参数都报错，不静默退回默认。
func TestUsageReportBadOptions(t *testing.T) {
	cfg, _ := usageTestSetup(t)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.Local)
	cases := []struct {
		name string
		opts usageOptions
		want string
	}{
		{"未知维度", usageOptions{By: "week"}, "未知的分组维度"},
		{"负天数", usageOptions{Days: -1}, "--days 必须是正整数"},
		{"天数过大", usageOptions{Days: maxUsageDays + 1}, "--days 不能超过"},
		{"月与天数互斥", usageOptions{Month: true, Days: 7}, "--month 与 --days 不能同时使用"},
	}
	for _, c := range cases {
		_, err := usageReport(cfg, c.opts, now)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v，期望包含 %q", c.name, err, c.want)
		}
	}
}

// 分组键的译名：前台、内部调用与三种轮次类型都有可读的名字。
func TestUsageKeyLabel(t *testing.T) {
	oldLang := uiLang
	uiLang = i18n.LangZH
	defer func() { uiLang = oldLang }()

	cases := []struct {
		by, key, want string
	}{
		{usage.GroupBySource, "foreground", "网页前台"},
		{usage.GroupBySource, "internal", "内部调用"},
		{usage.GroupBySource, "heartbeat", "heartbeat"},
		{usage.GroupByKind, usage.KindTurn, "对话"},
		{usage.GroupByKind, usage.KindCompact, "压缩"},
		{usage.GroupByKind, usage.KindHelper, "辅助"},
		{usage.GroupByDay, "2026-11-20", "2026-11-20"},
		{usage.GroupByModel, "unknown", "未知"},
	}
	for _, c := range cases {
		if got := usageKeyLabel(c.by, c.key); got != c.want {
			t.Errorf("usageKeyLabel(%s, %s) = %q, want %q", c.by, c.key, got, c.want)
		}
	}
}

// 帮助里列出的 --by 取值必须与实现认的维度一致（两个语言各查一遍）：写漏一个
// provider_model，用户就会以为按模型分组这条路走不通。
func TestUsageHelpByValuesMatchImplementation(t *testing.T) {
	oldLang := uiLang
	defer func() { uiLang = oldLang }()

	all := []string{
		usage.GroupByDay, usage.GroupBySource, usage.GroupByModel,
		usage.GroupByKind, usage.GroupByProvider, usage.GroupByProviderModel,
	}
	for _, lang := range []string{i18n.LangZH, i18n.LangEN} {
		uiLang = lang
		cmd := lookup("usage")
		if cmd == nil {
			t.Fatal("命令表里没有 usage")
		}
		const marker = "--by "
		i := strings.Index(cmd.usage, marker)
		if i < 0 {
			t.Fatalf("%s: 用法里没有 %q: %s", lang, marker, cmd.usage)
		}
		raw := strings.TrimRight(strings.TrimSpace(cmd.usage[i+len(marker):]), "]")
		seen := map[string]bool{}
		for _, v := range strings.Split(raw, "|") {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			if !usageGroupByValid(v) {
				t.Errorf("%s: 帮助里列了实现不认的维度 %q", lang, v)
			}
			seen[v] = true
		}
		for _, dim := range all {
			if !seen[dim] {
				t.Errorf("%s: 用法里缺少 %q", lang, dim)
			}
			if !strings.Contains(cmd.detail, dim) {
				t.Errorf("%s: 详细说明里缺少 %q", lang, dim)
			}
		}
	}
}

// setUsageLocal 把 time.Local 换成 loc，测试结束还原。测试都不并行，换全局变量安全。
func setUsageLocal(t *testing.T, loc *time.Location) {
	t.Helper()
	old := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = old })
}

// --days 的合计只盖窗口，预算行按自然月：两套口径并排时标签必须写明「本月」，
// 数字也只按本月算（上月那条不进合计，也不进预算）。
func TestUsageReportDaysWithMonthBudget(t *testing.T) {
	cfg, ledger := usageTestSetup(t)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.Local)
	// 窗口内一条
	appendUsage(t, ledger, usage.Entry{TS: now, Kind: usage.KindTurn, Prompt: 1_000, Completion: 100})
	// 窗口外（上个月）一条：只有预算按月会看见它
	appendUsage(t, ledger, usage.Entry{TS: time.Date(2026, 10, 31, 23, 0, 0, 0, time.Local), Kind: usage.KindTurn, Prompt: 9_000, Completion: 900})
	saveUsageSettings(t, cfg, usage.Settings{MonthlyTokens: 2_000_000, WarnPercent: 80, Over: usage.OverSkipBackground})

	got, err := usageReport(cfg, usageOptions{Days: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"最近 1 天",
		"1 次请求",
		"输入 1,000",
		"输出 100",
		"本月预算",
		"1,100 / 2,000,000 tokens（0.1%）",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("报告里缺少 %q:\n%s", want, got)
		}
	}
	// 上个月那条 9,900 既不进窗口合计，也不进本月预算
	for _, bad := range []string{"9,900", "11,000"} {
		if strings.Contains(got, bad) {
			t.Errorf("上个月的记录混进了 %q:\n%s", bad, got)
		}
	}
}

// Apia 的 2011-12-30 在墙上不存在。往回数日子要按日历走、并去重，不能因为
// AddDate 归一化回前一天就把同一天读两遍（那会让合计双计）。
func TestMergeRecentDaysAcrossSkippedDay(t *testing.T) {
	apia, err := time.LoadLocation("Pacific/Apia")
	if err != nil {
		t.Fatalf("加载 Pacific/Apia: %v（本机缺 IANA 库时应由 time/tzdata 兜住）", err)
	}
	setUsageLocal(t, apia)
	_, ledger := usageTestSetup(t)
	now := time.Date(2012, 1, 1, 10, 0, 0, 0, apia)
	appendUsage(t, ledger, usage.Entry{TS: now, Kind: usage.KindTurn, Prompt: 1_000, Completion: 100})
	appendUsage(t, ledger, usage.Entry{TS: time.Date(2011, 12, 31, 9, 0, 0, 0, apia), Kind: usage.KindTurn, Prompt: 2_000, Completion: 200})

	labels := recentDayLabels(now, 3)
	wantLabels := []string{"2012-01-01", "2011-12-31", "2011-12-30"}
	if len(labels) != len(wantLabels) {
		t.Fatalf("日期列表 = %v，want %v", labels, wantLabels)
	}
	seen := map[string]bool{}
	for i, d := range labels {
		if d != wantLabels[i] {
			t.Errorf("第 %d 天 = %s，want %s（%v）", i, d, wantLabels[i], labels)
		}
		if seen[d] {
			t.Errorf("日期 %s 被读了两次: %v", d, labels)
		}
		seen[d] = true
	}

	// 跳过的 12-30 读到 0 条是正确的；12-31 只该计一次。
	sum, err := mergeRecentDays(ledger, usage.GroupByDay, nil, now, 3)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Total.Requests != 2 || sum.Total.Prompt != 3_000 || sum.Total.Completion != 300 {
		t.Fatalf("跳日处合计被双计: %+v", sum.Total)
	}
}

// 英文报告不得漏出汉字，也不能出现「1 requests」这种单复数硬拼。
func TestUsageReportEnglish(t *testing.T) {
	cfg, ledger := usageTestSetup(t)
	uiLang = i18n.LangEN
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.Local)
	appendUsage(t, ledger, usage.Entry{TS: now, Kind: usage.KindTurn, Prompt: 1_000, Completion: 100})

	got, err := usageReport(cfg, usageOptions{Days: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if hasCJK(got) {
		t.Errorf("英文报告里有汉字:\n%s", got)
	}
	for _, want := range []string{"Wen Agent usage", "Range", "requests: 1", "1-day window", "By source", "Web"} {
		if !strings.Contains(got, want) {
			t.Errorf("英文报告里缺少 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "1 requests") {
		t.Errorf("英文里出现了单复数错误:\n%s", got)
	}
}
