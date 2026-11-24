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

// usageModelsDoc 是一份带价格表的 models.json：p1/mA 用 CNY、p1/mB 用 USD，
// p9/mZ 没有价格。金额与「未定价」「多币种」三档因此都能在报告里看到。
const usageModelsDoc = `{
  "version": 1,
  "providers": [
    {
      "name": "p1",
      "type": "openai_compat",
      "base_url": "https://api.example.com",
      "api_key": "sk-test",
      "models": [
        {"id": "mA", "price": {"currency": "CNY", "input": 1, "output": 2}},
        {"id": "mB", "price": {"currency": "USD", "input": 1, "output": 1}}
      ]
    }
  ],
  "current": {"provider": "p1", "model": "mA"}
}`

// writeUsageModels 在临时配置目录里放一份价格表（随后由 modelcfg 读出）。
func writeUsageModels(t *testing.T, cfg *config.Config, doc string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cfg.BaseDir, "models.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 月视图带金额：合计与分组都追加金额、未定价与估算标记，预算行并列金额段。
func TestUsageReportCost(t *testing.T) {
	cfg, ledger := usageTestSetup(t)
	writeUsageModels(t, cfg, usageModelsDoc)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.Local)
	at := func(day int) time.Time { return time.Date(2026, 11, day, 9, 0, 0, 0, time.Local) }

	// 100 万输入：CNY 1.00（命中 20 万缓存，价格表没填缓存价 → 按输入价估算）
	appendUsage(t, ledger, usage.Entry{TS: at(5), Kind: usage.KindTurn,
		Provider: "p1", Model: "mA", Prompt: 1_000_000, Cached: 200_000})
	// 另一家的另一种币种
	appendUsage(t, ledger, usage.Entry{TS: at(6), Kind: usage.KindTurn,
		Provider: "p1", Model: "mB", Prompt: 1_000_000})
	// 没定价的模型
	appendUsage(t, ledger, usage.Entry{TS: at(7), Kind: usage.KindHelper, Origin: "heartbeat",
		Provider: "p9", Model: "mZ", Prompt: 10})
	saveUsageSettings(t, cfg, usage.Settings{
		MonthlyTokens: 3_000_000, MonthlyCost: 5, Currency: "CNY", WarnPercent: 80, Over: usage.OverSkipBackground,
	})

	got, err := usageReport(cfg, usageOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"3 次请求",
		// 多币种并列，不折成一个数
		"估算 CNY 1.00 · USD 1.00",
		// 未定价的 token 不进金额，但要报出来
		"未定价 10 tokens",
		// 缓存价没填，金额是估算
		"缓存价未填，按输入价估算",
		"按来源",
		"heartbeat",
		// 预算行：token 段与金额段并列
		"2,000,010 / 3,000,000 tokens（66.7%） · CNY 1.00 / 5.00",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("报告里缺少 %q:\n%s", want, got)
		}
	}
	// 没有坏分片，也没有价格表读取失败
	if strings.Contains(got, "价格表读取失败") {
		t.Errorf("价格表读得出来时不该报错:\n%s", got)
	}
}

// 两笔各自可表示、合计溢出的金额：报告要单独说「已截断」，而不是说「缓存价未填」。
// 后者是另一件事，照着它去补缓存价既改不好这个数，也遮住了真正的原因。
func TestUsageReportCostOverflow(t *testing.T) {
	cfg, ledger := usageTestSetup(t)
	writeUsageModels(t, cfg, `{
  "version": 1,
  "providers": [
    {
      "name": "p1",
      "type": "openai_compat",
      "base_url": "https://api.example.com",
      "api_key": "sk-test",
      "models": [
        {"id": "mHuge", "price": {"currency": "CNY", "input": 1e308, "output": 1e308}}
      ]
    }
  ],
  "current": {"provider": "p1", "model": "mHuge"}
}`)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.Local)
	// 每笔 1e6 token × 1e308/百万 = 1e308；同币种两笔相加超出 float64。
	for i := 0; i < 2; i++ {
		appendUsage(t, ledger, usage.Entry{TS: now.Add(time.Duration(i) * time.Minute),
			Kind: usage.KindTurn, Provider: "p1", Model: "mHuge", Prompt: 1_000_000})
	}

	got, err := usageReport(cfg, usageOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "金额超出可表示范围，已截断") {
		t.Errorf("报告里缺少截断提示:\n%s", got)
	}
	if strings.Contains(got, "缓存价未填") {
		t.Errorf("金额被截断不是缓存价缺失，不该出现「缓存价未填」:\n%s", got)
	}
}

// 价格表读不出来：token 照常，报告里明说不含金额——静默留空会让人以为一分没花。
func TestUsageReportCostWithoutPriceTable(t *testing.T) {
	cfg, ledger := usageTestSetup(t)
	writeUsageModels(t, cfg, "{ 不是 JSON")
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.Local)
	appendUsage(t, ledger, usage.Entry{TS: now, Kind: usage.KindTurn,
		Provider: "p1", Model: "mA", Prompt: 1_000_000})

	got, err := usageReport(cfg, usageOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"价格表读取失败，报告不含金额", "1 次请求", "输入 1,000,000"} {
		if !strings.Contains(got, want) {
			t.Errorf("报告里缺少 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "估算") {
		t.Errorf("没有价格表时不该出现金额:\n%s", got)
	}
}

// --by provider_model：价格挂在模型上，按「提供商 / 模型」分组时金额才与价格表对得上。
func TestUsageReportProviderModelGroup(t *testing.T) {
	cfg, ledger := usageTestSetup(t)
	writeUsageModels(t, cfg, usageModelsDoc)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.Local)
	appendUsage(t, ledger, usage.Entry{TS: now, Kind: usage.KindTurn, Provider: "p1", Model: "mA", Prompt: 1_000_000})

	got, err := usageReport(cfg, usageOptions{By: usage.GroupByProviderModel}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"按提供商与模型", "p1 / mA", "估算 CNY 1.00"} {
		if !strings.Contains(got, want) {
			t.Errorf("报告里缺少 %q:\n%s", want, got)
		}
	}
}

// 标题与维度名都有译文，且 --by 的帮助文本认得第六个维度。
func TestUsageGroupHeadingProviderModel(t *testing.T) {
	old := uiLang
	uiLang = i18n.LangZH
	defer func() { uiLang = old }()
	if got := usageGroupHeading(usage.GroupByProviderModel); got != "按提供商与模型" {
		t.Errorf("heading = %q", got)
	}
	if !usageGroupByValid(usage.GroupByProviderModel) {
		t.Error("provider_model 应当是一个可用的分组维度")
	}
}

// 没设上限但账本写不进去：预算行本来整段省略，这时要留一句——默认配置没有上限，
// 把「合计偏少」一起省掉，用户就永远看不到它。
func TestFormatUsageBudgetWriteFailedOnly(t *testing.T) {
	oldLang := uiLang
	uiLang = i18n.LangZH
	defer func() { uiLang = oldLang }()

	// 既不设限也不记账失败：仍然是空的（不设限是默认状态，不该占版面）
	if got := formatUsageBudget(usage.BudgetStatus{}); got != "" {
		t.Errorf("不设限且记账成功时不该有预算行: %q", got)
	}
	if got := formatUsageBudget(usage.BudgetStatus{WriteFailed: true}); got != "未设置（账本写入失败，统计可能不完整）" {
		t.Errorf("无预算但记账失败 = %q", got)
	}
	uiLang = i18n.LangEN
	if got := formatUsageBudget(usage.BudgetStatus{WriteFailed: true}); hasCJK(got) {
		t.Errorf("英文预算行里有汉字: %q", got)
	}
}

// 预算行的金额段：token 段与金额段并列，限额为 0 的维度整段省略。
func TestFormatUsageBudgetCost(t *testing.T) {
	oldLang := uiLang
	uiLang = i18n.LangZH
	defer func() { uiLang = oldLang }()

	cases := []struct {
		name string
		st   usage.BudgetStatus
		want string
	}{
		{
			"只有金额上限",
			usage.BudgetStatus{Enabled: true, LimitCost: 50, SpentCost: 1.23, Currency: "CNY", CostReady: true},
			"CNY 1.23 / 50.00",
		},
		{
			"token 与金额并列",
			usage.BudgetStatus{
				Enabled: true, LimitTokens: 2_000_000, SpentTokens: 1_300_000,
				LimitCost: 50, SpentCost: 1.23, Currency: "CNY", CostReady: true,
			},
			"1,300,000 / 2,000,000 tokens（65.0%） · CNY 1.23 / 50.00",
		},
		{
			"只花了钱没设上限",
			usage.BudgetStatus{Enabled: true, LimitTokens: 2_000_000, SpentCost: 1.5, Currency: "CNY", CostReady: true},
			"0 / 2,000,000 tokens（0.0%） · CNY 1.50",
		},
		{
			"没有价格表但设了金额上限",
			usage.BudgetStatus{Enabled: true, LimitTokens: 1_000, SpentTokens: 10, LimitCost: 50, Currency: "CNY"},
			"10 / 1,000 tokens（1.0%）（缺少价格，金额上限未生效）",
		},
		{
			"金额超限",
			usage.BudgetStatus{
				Enabled: true, LimitCost: 1, SpentCost: 2.5, Currency: "CNY",
				CostReady: true, Exceeded: true, Over: usage.OverSkipBackground,
			},
			"CNY 2.50 / 1.00（已达上限，后台任务已暂停）",
		},
	}
	for _, c := range cases {
		if got := formatUsageBudget(c.st); got != c.want {
			t.Errorf("%s: %q\nwant %q", c.name, got, c.want)
		}
	}
}

// 英文报告不得漏出汉字：金额段与它的三条说明各有自己的键。
func TestUsageReportCostEnglish(t *testing.T) {
	cfg, ledger := usageTestSetup(t)
	oldLang := uiLang
	uiLang = i18n.LangEN
	defer func() { uiLang = oldLang }()
	writeUsageModels(t, cfg, usageModelsDoc)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.Local)
	appendUsage(t, ledger, usage.Entry{TS: now, Kind: usage.KindTurn,
		Provider: "p1", Model: "mA", Prompt: 1_000_000, Cached: 200_000})
	appendUsage(t, ledger, usage.Entry{TS: now, Kind: usage.KindTurn, Provider: "p9", Model: "mZ", Prompt: 10})
	saveUsageSettings(t, cfg, usage.Settings{
		MonthlyTokens: 3_000_000, MonthlyCost: 0.5, Currency: "CNY", WarnPercent: 80, Over: usage.OverSkipBackground,
	})

	got, err := usageReport(cfg, usageOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if hasCJK(got) {
		t.Errorf("英文报告里有汉字:\n%s", got)
	}
	for _, want := range []string{"estimated CNY 1.00", "unpriced tokens: 10", "cache prices not set", "tokens (33.3%)", "limit reached"} {
		if !strings.Contains(got, want) {
			t.Errorf("英文报告里缺少 %q:\n%s", want, got)
		}
	}
}

// wen status 的用量行也带上估算金额；预算段并列金额并在超限时说明。
func TestStatusUsageLineCost(t *testing.T) {
	cfg := statusTestConfig(t)
	writeUsageModels(t, cfg, usageModelsDoc)
	now := time.Now()

	ledger, err := usage.Open(usageDirFor(cfg, runtimeOverrides{}))
	if err != nil {
		t.Fatal(err)
	}
	entry := usage.Entry{TS: now, Kind: usage.KindTurn, Provider: "p1", Model: "mA", Prompt: 1_000_000}
	if err := ledger.Append(entry); err != nil {
		t.Fatal(err)
	}

	want := "本月用量  1 次 · 输入 1,000,000 · 输出 0 tokens · 估算 CNY 1.00"
	if got := statusUsageLine(cfg, now); got != want {
		t.Errorf("用量行 = %q\nwant %q", got, want)
	}

	// 金额上限 0.5：已花 1.00，超限；没有 token 上限，token 段整段省略
	if err := usage.SaveSettings(usageSettingsFor(cfg, runtimeOverrides{}), usage.Settings{
		MonthlyCost: 0.5, Currency: "CNY", WarnPercent: 80, Over: usage.OverSkipBackground,
	}); err != nil {
		t.Fatal(err)
	}
	want += " · 本月预算 CNY 1.00 / 0.50（已达上限，后台任务已暂停）"
	if got := statusUsageLine(cfg, now); got != want {
		t.Errorf("金额超限行 = %q\nwant %q", got, want)
	}

	// 金额上限放宽、token 上限设上：两段并列
	if err := usage.SaveSettings(usageSettingsFor(cfg, runtimeOverrides{}), usage.Settings{
		MonthlyTokens: 1_000_000, MonthlyCost: 5, Currency: "CNY", WarnPercent: 80, Over: usage.OverSkipBackground,
	}); err != nil {
		t.Fatal(err)
	}
	want = "本月用量  1 次 · 输入 1,000,000 · 输出 0 tokens · 估算 CNY 1.00" +
		" · 本月预算 1,000,000 / 1,000,000 tokens（100.0%） · CNY 1.00 / 5.00（已达上限，后台任务已暂停）"
	if got := statusUsageLine(cfg, now); got != want {
		t.Errorf("两段并列行 = %q\nwant %q", got, want)
	}
}

// 价格表坏了、又设了金额上限：status 行说明金额上限未生效，而不是假装它管着。
func TestStatusUsageLineCostWithoutPriceTable(t *testing.T) {
	cfg := statusTestConfig(t)
	writeUsageModels(t, cfg, "{ 不是 JSON")
	now := time.Now()
	ledger, err := usage.Open(usageDirFor(cfg, runtimeOverrides{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Append(usage.Entry{TS: now, Kind: usage.KindTurn, Prompt: 1_000}); err != nil {
		t.Fatal(err)
	}
	if err := usage.SaveSettings(usageSettingsFor(cfg, runtimeOverrides{}), usage.Settings{
		MonthlyCost: 5, Currency: "CNY", WarnPercent: 80, Over: usage.OverSkipBackground,
	}); err != nil {
		t.Fatal(err)
	}
	got := statusUsageLine(cfg, now)
	if !strings.Contains(got, "（缺少价格，金额上限未生效）") {
		t.Errorf("没有价格时应说明金额上限未生效: %q", got)
	}
	if strings.Contains(got, "估算") {
		t.Errorf("没有价格表时不该出现金额: %q", got)
	}
}

// 价格表在、本月用到的模型却没定价：status 行同样要说明金额上限没生效，
// 与「缺少价格表」分开说，因为要改的地方不同（一个是填价格，一个是修文件）。
func TestStatusUsageLineCostUnpriced(t *testing.T) {
	cfg := statusTestConfig(t)
	writeUsageModels(t, cfg, usageModelsDoc) // mZ 那一档没有价格
	now := time.Now()
	ledger, err := usage.Open(usageDirFor(cfg, runtimeOverrides{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Append(usage.Entry{TS: now, Kind: usage.KindTurn, Provider: "p9", Model: "mZ", Prompt: 1_000}); err != nil {
		t.Fatal(err)
	}
	if err := usage.SaveSettings(usageSettingsFor(cfg, runtimeOverrides{}), usage.Settings{
		MonthlyCost: 5, Currency: "CNY", WarnPercent: 80, Over: usage.OverSkipBackground,
	}); err != nil {
		t.Fatal(err)
	}
	got := statusUsageLine(cfg, now)
	for _, want := range []string{"CNY 0.00 / 5.00", "（本月有用量没有价格，金额上限未生效）",
		// 未定价的 token 也要在用量行上报出来，只报金额会让人以为一分没花
		"未定价 1,000 tokens"} {
		if !strings.Contains(got, want) {
			t.Errorf("状态行里缺少 %q: %q", want, got)
		}
	}
}

// 预算行的补充说明：未定价、别的币种、坏分片各有一句，且与「缺价格表」不混用。
func TestFormatUsageBudgetNotes(t *testing.T) {
	oldLang := uiLang
	uiLang = i18n.LangZH
	defer func() { uiLang = oldLang }()

	cases := []struct {
		name string
		st   usage.BudgetStatus
		want string
	}{
		{
			"有用量没定价",
			usage.BudgetStatus{Enabled: true, LimitCost: 5, Currency: "CNY", CostReady: true,
				CostUnpriced: true, UnpricedTokens: 1_000},
			"CNY 0.00 / 5.00（本月有用量没有价格，金额上限未生效）",
		},
		{
			"别的币种没计入",
			usage.BudgetStatus{Enabled: true, LimitCost: 5, Currency: "CNY", CostReady: true, ForeignCost: true},
			"CNY 0.00 / 5.00（有其他币种的花费未计入 CNY 上限）",
		},
		{
			"坏分片",
			usage.BudgetStatus{Enabled: true, LimitTokens: 1_000, SpentTokens: 10, SkippedFiles: 2},
			"10 / 1,000 tokens（1.0%）（2 个账本分片读不出来，合计可能偏少）",
		},
		{
			// 记账失败只让统计偏少，不改变拦截：说出来即可，不能悄悄藏掉。
			"账本写入失败",
			usage.BudgetStatus{Enabled: true, LimitTokens: 1_000, SpentTokens: 10, WriteFailed: true},
			"10 / 1,000 tokens（1.0%）（账本写入失败，本月统计可能不完整）",
		},
	}
	for _, c := range cases {
		if got := formatUsageBudget(c.st); got != c.want {
			t.Errorf("%s: %q\nwant %q", c.name, got, c.want)
		}
	}
}

// 预算行的金额段后面也要带上「这个数不准」的提示，口径与 statustext 一致：未定价
// 与缓存价回退二选一说一句，外币与坏分片各一句——只报一个 CNY 数字会让人把它当账单。
func TestFormatUsageBudgetInexactHints(t *testing.T) {
	oldLang := uiLang
	uiLang = i18n.LangZH
	defer func() { uiLang = oldLang }()

	cases := []struct {
		name string
		st   usage.BudgetStatus
		want string
	}{
		{
			"有未定价用量",
			usage.BudgetStatus{Enabled: true, LimitCost: 5, SpentCost: 1, Currency: "CNY",
				CostReady: true, CostInexact: true, UnpricedTokens: 4_000},
			"CNY 1.00 / 5.00（含 4,000 tokens 未定价，金额为估算）",
		},
		{
			"缓存价回退",
			usage.BudgetStatus{Enabled: true, LimitCost: 5, SpentCost: 1, Currency: "CNY",
				CostReady: true, CostInexact: true},
			"CNY 1.00 / 5.00（金额为估算，可能偏高或偏低）",
		},
		{
			"外币与坏分片",
			usage.BudgetStatus{Enabled: true, LimitCost: 5, SpentCost: 1, Currency: "CNY",
				CostReady: true, ForeignCost: true, SkippedFiles: 2},
			"CNY 1.00 / 5.00（有其他币种的花费未计入 CNY 上限）（2 个账本分片读不出来，合计可能偏少）",
		},
	}
	for _, c := range cases {
		if got := formatUsageBudget(c.st); got != c.want {
			t.Errorf("%s: %q\nwant %q", c.name, got, c.want)
		}
	}
}

// 已定价 + 未定价 + 外币 + 坏分片同时出现：两处预算段都要把这些提示带出来。
func TestBudgetHintsInStatusLine(t *testing.T) {
	cfg := statusTestConfig(t)
	writeUsageModels(t, cfg, usageModelsDoc) // p1/mA 用 CNY、p1/mB 用 USD、p9/mZ 没价
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.Local)
	at := func(day int) time.Time { return time.Date(2026, 11, day, 9, 0, 0, 0, time.Local) }

	l, err := usage.Open(usageDirFor(cfg, runtimeOverrides{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []usage.Entry{
		{TS: at(5), Kind: usage.KindTurn, Provider: "p1", Model: "mA", Prompt: 1_000_000},
		{TS: at(6), Kind: usage.KindTurn, Provider: "p1", Model: "mB", Prompt: 1_000_000},
		{TS: at(7), Kind: usage.KindHelper, Provider: "p9", Model: "mZ", Prompt: 1_000},
	} {
		if err := l.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	// 一个月内的坏分片：能读的部分照常算，合计偏少要说明。
	huge := `{"ts":"2026-11-19T09:00:00Z","kind":"turn","model":"` + strings.Repeat("x", 17<<20) + `"}`
	if err := os.WriteFile(filepath.Join(usageDirFor(cfg, runtimeOverrides{}), "2026-11-19.jsonl"),
		[]byte(huge+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	saveUsageSettings(t, cfg, usage.Settings{
		MonthlyCost: 50, Currency: "CNY", WarnPercent: 80, Over: usage.OverSkipBackground,
	})

	got := statusUsageLine(cfg, now)
	for _, want := range []string{
		"CNY 1.00 / 50.00（含 1,000 tokens 未定价，金额为估算）",
		"（有其他币种的花费未计入 CNY 上限）",
		"（1 个账本分片读不出来，合计可能偏少）",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("状态行里缺少 %q: %q", want, got)
		}
	}

	// 英文：同样三条各有译文，且不得漏出汉字。
	uiLang = i18n.LangEN
	en := formatUsageBudget(usage.BudgetStatus{
		Enabled: true, LimitCost: 5, SpentCost: 1, Currency: "CNY", CostReady: true,
		CostInexact: true, UnpricedTokens: 4_000, ForeignCost: true, SkippedFiles: 2,
		WriteFailed: true,
	})
	if hasCJK(en) {
		t.Errorf("英文预算行里有汉字: %q", en)
	}
	for _, want := range []string{"unpriced tokens", "other currencies", "unreadable ledger shards", "ledger write failed"} {
		if !strings.Contains(en, want) {
			t.Errorf("英文预算行里缺少 %q: %q", want, en)
		}
	}
}

// --days 逐日合并时金额、未定价与坏分片都要跟着合并，而不是只留最后一天的数字。
func TestUsageReportDaysCost(t *testing.T) {
	cfg, ledger := usageTestSetup(t)
	writeUsageModels(t, cfg, usageModelsDoc)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.Local)
	at := func(day int) time.Time { return time.Date(2026, 11, day, 9, 0, 0, 0, time.Local) }

	appendUsage(t, ledger, usage.Entry{TS: at(18), Kind: usage.KindTurn, Provider: "p1", Model: "mA", Prompt: 1_000_000})
	appendUsage(t, ledger, usage.Entry{TS: at(20), Kind: usage.KindTurn, Provider: "p1", Model: "mB", Prompt: 1_000_000})
	appendUsage(t, ledger, usage.Entry{TS: at(20), Kind: usage.KindHelper, Provider: "p9", Model: "mZ", Prompt: 10})
	// 一个读不出来的分片：跳过数也要跟着逐日合并进合计
	huge := `{"ts":"2026-11-19T09:00:00Z","kind":"turn","model":"` + strings.Repeat("x", 17<<20) + `"}`
	if err := os.WriteFile(filepath.Join(usageDirFor(cfg, runtimeOverrides{}), "2026-11-19.jsonl"),
		[]byte(huge+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 范围外的记录不该混进来
	appendUsage(t, ledger, usage.Entry{TS: time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local),
		Kind: usage.KindTurn, Provider: "p1", Model: "mA", Prompt: 9_000_000})

	lookup, err := usagePriceLookup(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := mergeRecentDays(ledger, usage.GroupByProviderModel, lookup, now, 3)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Total.Requests != 3 || len(sum.Total.Amounts) != 2 ||
		sum.Total.Amounts["CNY"] != 1 || sum.Total.Amounts["USD"] != 1 {
		t.Fatalf("合并后的金额不对: %+v", sum.Total)
	}
	if sum.Total.UnpricedTokens != 10 || sum.Total.SkippedFiles != 1 {
		t.Fatalf("未定价与跳过数应一并合并: %+v", sum.Total)
	}
	if len(sum.Groups) != 3 {
		t.Fatalf("分组应合并为三个: %+v", sum.Groups)
	}

	got, err := usageReport(cfg, usageOptions{Days: 3}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"最近 3 天", "3 次请求", "估算 CNY 1.00 · USD 1.00",
		"未定价 10 tokens", "有 1 个账本分片读不出来"} {
		if !strings.Contains(got, want) {
			t.Errorf("报告里缺少 %q:\n%s", want, got)
		}
	}
}
