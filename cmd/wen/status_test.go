package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"wen/internal/config"
	"wen/internal/i18n"
	"wen/internal/usage"
)

// statusTestConfig 造一个临时配置目录的配置视图；这一行只读 BaseDir，不必真写
// config.yaml。
func statusTestConfig(t *testing.T) *config.Config {
	t.Helper()
	oldLang := uiLang
	uiLang = i18n.LangZH
	t.Cleanup(func() { uiLang = oldLang })
	return &config.Config{BaseDir: t.TempDir()}
}

// 没有账本、没有记录时返回空串：wen status 不该为此多打一行空行。
func TestStatusUsageLineEmpty(t *testing.T) {
	cfg := statusTestConfig(t)
	if got := statusUsageLine(cfg, time.Now()); got != "" {
		t.Errorf("没有记录时应返回空串，得到 %q", got)
	}
}

// 有记录、没预算：只报本月合计，单位写在行里。
func TestStatusUsageLineWithoutBudget(t *testing.T) {
	cfg := statusTestConfig(t)
	now := time.Now()
	ledger, err := usage.Open(usageDirFor(cfg, runtimeOverrides{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Append(usage.Entry{TS: now, Kind: usage.KindTurn, Prompt: 1_000, Completion: 100}); err != nil {
		t.Fatal(err)
	}

	// 没有价格表时全部算未定价，这部分要单独说出来——金额是 0 不代表一分没花
	want := "本月用量  1 次 · 输入 1,000 · 输出 100 tokens · 未定价 1,100 tokens"
	if got := statusUsageLine(cfg, now); got != want {
		t.Errorf("用量行 = %q\nwant %q", got, want)
	}
}

// 有预算时追加预算段，并按状态加后缀；超限的后缀按 over 区分拦截范围。
func TestStatusUsageLineWithBudget(t *testing.T) {
	cfg := statusTestConfig(t)
	now := time.Now()
	ledger, err := usage.Open(usageDirFor(cfg, runtimeOverrides{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Append(usage.Entry{TS: now, Kind: usage.KindTurn, Prompt: 1_000, Completion: 100}); err != nil {
		t.Fatal(err)
	}
	settingsPath := usageSettingsFor(cfg, runtimeOverrides{})
	// 没有价格表：这个用量全部算未定价，行里要带出这部分 token
	base := "本月用量  1 次 · 输入 1,000 · 输出 100 tokens · 未定价 1,100 tokens"

	// 1,100 / 2,000 = 55.0%，未到 80% 阈值
	if err := usage.SaveSettings(settingsPath, usage.Settings{
		MonthlyTokens: 2_000, WarnPercent: 80, Over: usage.OverSkipBackground,
	}); err != nil {
		t.Fatal(err)
	}
	want := base + " · 本月预算 1,100 / 2,000 tokens（55.0%）"
	if got := statusUsageLine(cfg, now); got != want {
		t.Errorf("预算行 = %q\nwant %q", got, want)
	}

	// 上限压到已用之下：要标明已达上限，且只说后台
	if err := usage.SaveSettings(settingsPath, usage.Settings{
		MonthlyTokens: 1_000, WarnPercent: 80, Over: usage.OverSkipBackground,
	}); err != nil {
		t.Fatal(err)
	}
	want = base + " · 本月预算 1,100 / 1,000 tokens（110.0%）（已达上限，后台任务已暂停）"
	if got := statusUsageLine(cfg, now); got != want {
		t.Errorf("超限行 = %q\nwant %q", got, want)
	}

	// block_all 连前台一起停：后缀不能只说「后台任务」
	if err := usage.SaveSettings(settingsPath, usage.Settings{
		MonthlyTokens: 1_000, WarnPercent: 80, Over: usage.OverBlockAll,
	}); err != nil {
		t.Fatal(err)
	}
	want = base + " · 本月预算 1,100 / 1,000 tokens（110.0%）（已达上限，所有请求已暂停）"
	if got := statusUsageLine(cfg, now); got != want {
		t.Errorf("block_all 超限行 = %q\nwant %q", got, want)
	}

	// 阈值降到 50% 时 55% 就是接近上限
	if err := usage.SaveSettings(settingsPath, usage.Settings{
		MonthlyTokens: 2_000, WarnPercent: 50, Over: usage.OverSkipBackground,
	}); err != nil {
		t.Fatal(err)
	}
	want = base + " · 本月预算 1,100 / 2,000 tokens（55.0%）（接近上限）"
	if got := statusUsageLine(cfg, now); got != want {
		t.Errorf("接近上限行 = %q\nwant %q", got, want)
	}
}

// 没设上限但账本写不进去：这一行本来整段省略，这时要留一句——默认配置没有上限，
// 把「合计偏少」一起省掉，用户就永远看不到它；不设限且记账成功时仍然什么都不说。
func TestStatusBudgetSuffixWriteFailedOnly(t *testing.T) {
	cfg := statusTestConfig(t)
	if got := statusBudgetSuffix(usage.BudgetStatus{}); got != "" {
		t.Errorf("不设限且记账成功时不该有预算段: %q", got)
	}
	got := statusBudgetSuffix(usage.BudgetStatus{WriteFailed: true})
	for _, want := range []string{"本月预算", "未设置", "账本写入失败"} {
		if !strings.Contains(got, want) {
			t.Errorf("无预算但记账失败时缺少 %q: %q", want, got)
		}
	}
	// 有记录时接在用量行之后，没有记录时也要单独成行（不能被「0 次」一起藏掉）。
	now := time.Now()
	ledger, err := usage.Open(usageDirFor(cfg, runtimeOverrides{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Append(usage.Entry{TS: now, Kind: usage.KindTurn, Prompt: 1_000}); err != nil {
		t.Fatal(err)
	}
	if got := statusUsageLine(cfg, now); !strings.Contains(got, "本月用量") || strings.Contains(got, "未设置") {
		t.Errorf("账本写得进去时不该出现记账失败提示: %q", got)
	}

	uiLang = i18n.LangEN
	if got := statusBudgetSuffix(usage.BudgetStatus{WriteFailed: true}); hasCJK(got) {
		t.Errorf("英文预算段里有汉字: %q", got)
	}
}

// 本月还没花、但设了预算：预算段照出，不能被「0 条」一起藏掉——藏掉会让人
// 以为限额没生效。
func TestStatusUsageLineBudgetWithoutRecords(t *testing.T) {
	cfg := statusTestConfig(t)
	now := time.Now()
	if err := usage.SaveSettings(usageSettingsFor(cfg, runtimeOverrides{}), usage.Settings{
		MonthlyTokens: 2_000_000, WarnPercent: 80, Over: usage.OverSkipBackground,
	}); err != nil {
		t.Fatal(err)
	}
	got := statusUsageLine(cfg, now)
	for _, want := range []string{"本月用量", "暂无记录", "本月预算", "0 / 2,000,000 tokens（0.0%）"} {
		if !strings.Contains(got, want) {
			t.Errorf("用量行缺少 %q: %q", want, got)
		}
	}
}

// usage.json 坏了：不能按「不设限」把预算整段藏掉，要写「不可用」，且不给任何
// 数字（读不出来的账本报出来的比例会被当成真实用量）。
func TestStatusUsageLineBrokenSettings(t *testing.T) {
	cfg := statusTestConfig(t)
	if err := os.WriteFile(usageSettingsFor(cfg, runtimeOverrides{}), []byte("{ 不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := statusUsageLine(cfg, time.Now())
	if !strings.Contains(got, "本月预算不可用") {
		t.Errorf("坏设置时应报预算不可用: %q", got)
	}
	if !strings.Contains(got, "（账本或设置读不出来）") {
		t.Errorf("应说明不可用的原因: %q", got)
	}
	if strings.ContainsAny(got, "/%0123456789") {
		t.Errorf("不可用时不该出现数字或比例: %q", got)
	}
}

// 英文用量行：没有汉字，也不出现「1 requests」这种单复数硬拼。
func TestStatusUsageLineEnglish(t *testing.T) {
	cfg := statusTestConfig(t)
	uiLang = i18n.LangEN
	now := time.Now()
	ledger, err := usage.Open(usageDirFor(cfg, runtimeOverrides{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Append(usage.Entry{TS: now, Kind: usage.KindTurn, Prompt: 1_000, Completion: 100}); err != nil {
		t.Fatal(err)
	}

	got := statusUsageLine(cfg, now)
	if hasCJK(got) {
		t.Errorf("英文用量行里有汉字: %q", got)
	}
	for _, want := range []string{"This month", "requests: 1", "input 1,000", "output 100 tokens"} {
		if !strings.Contains(got, want) {
			t.Errorf("英文用量行缺少 %q: %q", want, got)
		}
	}
	if strings.Contains(got, "1 requests") {
		t.Errorf("英文里出现了单复数错误: %q", got)
	}
}
