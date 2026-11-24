package server

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"wen/internal/config"
	"wen/internal/modelcfg"
	"wen/internal/usage"
)

// usageTestModels 造一份带价格表的模型配置：p1 下 mA 用 CNY、mB 用 USD，其余模型
// （以及 p1/mC）没定价——报告里该看到「有金额」「另一个币种」「未定价」三档。
func usageTestModels(t *testing.T) *modelcfg.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	doc := `{
  "version": 1,
  "providers": [
    {
      "name": "p1",
      "type": "openai_compat",
      "base_url": "https://api.example.com",
      "api_key": "sk-test",
      "models": [
        {"id": "mA", "price": {"currency": "CNY", "input": 1, "output": 2}},
        {"id": "mB", "price": {"currency": "USD", "input": 1, "output": 1}},
        {"id": "mC"}
      ]
    }
  ],
  "current": {"provider": "p1", "model": "mA"}
}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := modelcfg.NewStore("", path, &config.Config{})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

// usageTestServer 造一台只接了用量那几样东西的 Server：临时账本、临时设置路径、
// 带价格的模型配置，以及一份跟着它们走的预算。
func usageTestServer(t *testing.T) (*Server, *usage.Ledger, string) {
	t.Helper()
	dir := t.TempDir()
	ledger, err := usage.Open(filepath.Join(dir, "usage"))
	if err != nil {
		t.Fatal(err)
	}
	models := usageTestModels(t)
	settingsPath := filepath.Join(dir, "usage.json")
	settings, err := usage.LoadSettings(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		models:        models,
		usage:         ledger,
		usageSettings: settingsPath,
		usageBudget:   usage.NewBudget(settings, ledger, models.Price, time.Now()),
	}
	return s, ledger, settingsPath
}

// appendUsageEntry 往账本里记一条（TS 决定它落在哪一天）。
func appendUsageEntry(t *testing.T, l *usage.Ledger, e usage.Entry) {
	t.Helper()
	if err := l.Append(e); err != nil {
		t.Fatal(err)
	}
}

func getUsage(t *testing.T, s *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.usageReport(rec, httptest.NewRequest(http.MethodGet, "/api/usage"+query, nil))
	return rec
}

func decodeUsage(t *testing.T, rec *httptest.ResponseRecorder) usageReportView {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var v usageReportView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("解析响应: %v\n%s", err, rec.Body.String())
	}
	return v
}

// GET 的完整形状：合计、按日/来源/提供商与模型的分组、多币种、未定价。
func TestUsageReportHandler(t *testing.T) {
	s, ledger, settingsPath := usageTestServer(t)
	day1 := time.Date(2026, 4, 1, 9, 0, 0, 0, time.Local)
	appendUsageEntry(t, ledger, usage.Entry{TS: day1, Kind: usage.KindTurn,
		Provider: "p1", Model: "mA", Prompt: 1_000_000})
	appendUsageEntry(t, ledger, usage.Entry{TS: day1.Add(time.Hour), Kind: usage.KindCompact,
		Origin: "scheduler", Provider: "p1", Model: "mB", Prompt: 1_000_000})
	appendUsageEntry(t, ledger, usage.Entry{TS: time.Date(2026, 4, 2, 9, 0, 0, 0, time.Local),
		Kind: usage.KindTurn, Provider: "p9", Model: "mZ", Prompt: 10})

	// 设一份金额上限，报告里的 budget 才有东西可看。预算对象是运行中那一份，
	// 走与保存接口相同的热更新路径。
	if err := usage.SaveSettings(settingsPath, usage.Settings{
		MonthlyTokens: 2_000_000, MonthlyCost: 50, Currency: "CNY", WarnPercent: 80, Over: usage.OverSkipBackground,
	}); err != nil {
		t.Fatal(err)
	}
	settings, err := usage.LoadSettings(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.usageBudget.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	v := decodeUsage(t, getUsage(t, s, "?month=2026-04"))
	if v.Month != "2026-04" || v.Degraded {
		t.Fatalf("month/degraded = %q/%v", v.Month, v.Degraded)
	}
	if v.SettingsError != "" || v.Settings.Currency != "CNY" || v.Settings.MonthlyCost != 50 {
		t.Fatalf("settings = %+v err=%q", v.Settings, v.SettingsError)
	}
	if v.Budget == nil || v.Budget.LimitCost != 50 || !v.Budget.CostReady || v.Budget.Currency != "CNY" {
		t.Fatalf("budget = %+v", v.Budget)
	}

	tot := v.Totals
	if tot.Requests != 3 || tot.Prompt != 2_000_010 || tot.Completion != 0 {
		t.Fatalf("totals = %+v", tot)
	}
	if len(tot.Costs) != 2 || tot.Costs["CNY"] != 1 || tot.Costs["USD"] != 1 {
		t.Fatalf("costs = %v，期望 CNY 1 / USD 1", tot.Costs)
	}
	if tot.UnpricedTokens != 10 || tot.Inexact {
		t.Fatalf("未定价应为 10 且金额精确: %+v", tot)
	}

	// 按日：两行，键就是落盘日期
	if len(v.Daily) != 2 || v.Daily[0].Key != "2026-04-01" || v.Daily[1].Key != "2026-04-02" {
		t.Fatalf("daily = %+v", v.Daily)
	}
	if v.Daily[0].Requests != 2 || v.Daily[0].Costs["CNY"] != 1 || v.Daily[0].Costs["USD"] != 1 {
		t.Fatalf("daily[0] = %+v", v.Daily[0])
	}
	// 按来源：前台 + 定时任务各一行
	if len(v.BySource) != 2 || v.BySource[0].Key != "foreground" || v.BySource[1].Key != "scheduler" {
		t.Fatalf("by_source = %+v", v.BySource)
	}
	// 按提供商与模型：价格挂在模型上，键是查价用的那一对
	keys := make([]string, 0, len(v.ByModel))
	for _, it := range v.ByModel {
		keys = append(keys, it.Key)
	}
	if strings.Join(keys, "|") != "p1 / mA|p1 / mB|p9 / mZ" {
		t.Fatalf("by_model keys = %v", keys)
	}
	if v.ByModel[2].UnpricedTokens != 10 || len(v.ByModel[2].Costs) != 0 {
		t.Fatalf("未定价的模型不该有金额: %+v", v.ByModel[2])
	}
	// 分组里永远不带 skipped_files：同一个坏分片会落进多个分组。
	if strings.Contains(recBody(t, s, "?month=2026-04"), "skipped_files") {
		t.Errorf("没有坏分片时不该下发 skipped_files")
	}

	// 默认月份是本机当前月份：换成别的月份查，数字应当为空
	other := decodeUsage(t, getUsage(t, s, "?month=2026-03"))
	if other.Totals.Requests != 0 || len(other.Daily) != 0 {
		t.Fatalf("别的月份应当没有记录: %+v", other)
	}
}

// recBody 返回一次 GET 的原始响应体，供「某个字段不该出现」这类断言用。
func recBody(t *testing.T, s *Server, query string) string {
	t.Helper()
	return getUsage(t, s, query).Body.String()
}

// 单价是有限但极大的值（1e308）时，两个 token 的金额仍是有限的 2e302。历史缺陷：
// 先乘后除让中间量溢出成 +Inf，它顺着 costs 进了 JSON，writeJSON 编码失败——状态码
// 200、响应体却是空的。这里把真实处理器跑一遍，断言响应体非空且金额有限。
func TestUsageReportExtremePrice(t *testing.T) {
	dir := t.TempDir()
	ledger, err := usage.Open(filepath.Join(dir, "usage"))
	if err != nil {
		t.Fatal(err)
	}
	modelsPath := filepath.Join(dir, "models.json")
	doc := `{
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
}`
	if err := os.WriteFile(modelsPath, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	// 载入时也要过一遍价格校验：有限的大单价合法，不该被拒。
	models, err := modelcfg.NewStore("", modelsPath, &config.Config{})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	s := &Server{models: models, usage: ledger, usageSettings: filepath.Join(dir, "usage.json")}

	appendUsageEntry(t, ledger, usage.Entry{TS: time.Date(2026, 4, 1, 9, 0, 0, 0, time.Local),
		Kind: usage.KindTurn, Provider: "p1", Model: "mHuge", Prompt: 2})

	rec := getUsage(t, s, "?month=2026-04")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() == 0 {
		t.Fatal("响应体是空的：Inf 进了 costs，编码失败")
	}
	v := decodeUsage(t, rec)
	got, ok := v.Totals.Costs["CNY"]
	if !ok || math.IsInf(got, 0) || math.IsNaN(got) || got <= 1e300 {
		t.Fatalf("costs.CNY = %v（存在=%v），期望约 2e302 的有限值", got, ok)
	}
	if v.Totals.Inexact {
		t.Fatalf("金额本就精确，不该标 inexact: %+v", v.Totals)
	}
	// 分组行走的是同一份金额，也要能编码
	if len(v.ByModel) != 1 || math.IsInf(v.ByModel[0].Costs["CNY"], 0) {
		t.Fatalf("by_model = %+v", v.ByModel)
	}
}

// 两笔各自可表示、合计溢出：金额被截断到 MaxFloat64，报表必须标 overflow。历史缺陷：
// 合计被饱和了，Inexact 却仍是 false，报表看不出金额已经不可信；而 CLI 与界面若只
// 看 Inexact，还会把「截断」说成「缓存价未填」。这里跑一遍真实处理器。
func TestUsageReportAmountOverflow(t *testing.T) {
	dir := t.TempDir()
	ledger, err := usage.Open(filepath.Join(dir, "usage"))
	if err != nil {
		t.Fatal(err)
	}
	modelsPath := filepath.Join(dir, "models.json")
	doc := `{
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
}`
	if err := os.WriteFile(modelsPath, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := modelcfg.NewStore("", modelsPath, &config.Config{})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	s := &Server{models: models, usage: ledger, usageSettings: filepath.Join(dir, "usage.json")}

	// 每笔 1e6 token × 1e308/百万 = 1e308；两笔同币种相加就是 +Inf。
	day := time.Date(2026, 4, 1, 9, 0, 0, 0, time.Local)
	for i := 0; i < 2; i++ {
		appendUsageEntry(t, ledger, usage.Entry{TS: day.Add(time.Duration(i) * time.Minute),
			Kind: usage.KindTurn, Provider: "p1", Model: "mHuge", Prompt: 1_000_000})
	}

	rec := getUsage(t, s, "?month=2026-04")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() == 0 {
		t.Fatal("响应体是空的：溢出金额没被饱和，编码失败")
	}
	v := decodeUsage(t, rec)
	got := v.Totals.Costs["CNY"]
	if math.IsInf(got, 0) || math.IsNaN(got) || got != math.MaxFloat64 {
		t.Fatalf("costs.CNY = %v，期望被截断后的 MaxFloat64", got)
	}
	if !v.Totals.Overflow {
		t.Fatalf("合计溢出被截断，totals.overflow 应当为真: %+v", v.Totals)
	}
	if v.Totals.Inexact {
		t.Fatalf("截断不等于缓存价回退，inexact 不该被置上: %+v", v.Totals)
	}
	// 分组那一行走的是同一份金额，也必须能编码并标出截断。
	if len(v.ByModel) != 1 || !v.ByModel[0].Overflow {
		t.Fatalf("by_model = %+v，期望标出 overflow", v.ByModel)
	}
}

// 月份非法：400，不能把 "../x" 放进账本目录的筛选前缀里。
func TestUsageReportBadMonth(t *testing.T) {
	s, _, _ := usageTestServer(t)
	for _, month := range []string{"2026-4", "../x", "2026-13", "2026-04-01"} {
		rec := getUsage(t, s, "?month="+month)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("month=%q: status = %d, want 400", month, rec.Code)
		}
	}
}

// 没有账本：空报告 + degraded，字段齐全但不 panic。
func TestUsageReportWithoutLedger(t *testing.T) {
	s := &Server{}
	v := decodeUsage(t, getUsage(t, s, "?month=2026-04"))
	if !v.Degraded {
		t.Fatal("没有账本时应标降级")
	}
	if v.Totals.Requests != 0 || len(v.Daily) != 0 || len(v.BySource) != 0 || len(v.ByModel) != 0 {
		t.Fatalf("空报告的分组应为空数组: %+v", v)
	}
	if v.Budget != nil {
		t.Fatalf("没有预算判定时不该下发 budget: %+v", v.Budget)
	}
	if v.Settings.Currency != "CNY" {
		t.Fatalf("没有设置文件时应给默认值: %+v", v.Settings)
	}
	// 空数组而不是 null：界面按数组画表
	if !strings.Contains(recBody(t, s, "?month=2026-04"), `"daily":[]`) {
		t.Error("空报告里 daily 应是空数组")
	}
}

// 设置文件坏了：报告照出，但要说出来（settings 给默认值 + settings_error + degraded）。
func TestUsageReportBrokenSettings(t *testing.T) {
	s, _, settingsPath := usageTestServer(t)
	if err := os.WriteFile(settingsPath, []byte("{ 不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	v := decodeUsage(t, getUsage(t, s, "?month=2026-04"))
	if v.SettingsError == "" || !v.Degraded {
		t.Fatalf("坏设置应报出来: %+v", v)
	}
	if v.Settings != usage.DefaultSettings() {
		t.Fatalf("坏设置应退回默认值: %+v", v.Settings)
	}
}

// 坏分片只跳过不报错，但合计会因此偏少——报告要把它报出来，否则用户会把偏低的
// 数字当成真实用量。用一行超过账本单行上限（16MB）的记录造一个读不完的分片。
func TestUsageReportSkippedFiles(t *testing.T) {
	s, ledger, _ := usageTestServer(t)
	appendUsageEntry(t, ledger, usage.Entry{TS: time.Date(2026, 4, 1, 9, 0, 0, 0, time.Local),
		Kind: usage.KindTurn, Provider: "p1", Model: "mA", Prompt: 1_000_000})

	huge := `{"ts":"2026-04-05T09:00:00Z","kind":"turn","model":"` + strings.Repeat("x", 17<<20) + `"}`
	if err := os.WriteFile(filepath.Join(filepath.Dir(s.usageSettings), "usage", "2026-04-05.jsonl"),
		[]byte(huge+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	v := decodeUsage(t, getUsage(t, s, "?month=2026-04"))
	if v.Totals.SkippedFiles != 1 {
		t.Fatalf("SkippedFiles = %d, want 1: %+v", v.Totals.SkippedFiles, v.Totals)
	}
	// 好分片照常计入
	if v.Totals.Requests != 1 || v.Totals.Costs["CNY"] != 1 {
		t.Fatalf("坏分片不该影响别的分片: %+v", v.Totals)
	}
	for _, it := range v.Daily {
		if it.SkippedFiles != 0 {
			t.Fatalf("分组里不该带 skipped_files: %+v", it)
		}
	}
}

// PUT 设置：落盘、归一化、热更新到运行中的预算。
func TestSaveUsageSettings(t *testing.T) {
	s, _, settingsPath := usageTestServer(t)
	now := time.Now()

	body := `{"monthly_tokens":2000000,"monthly_cost":0.5,"currency":"cny","warn_percent":80,"over":"block_all"}`
	rec := httptest.NewRecorder()
	s.saveUsageSettings(rec, httptest.NewRequest(http.MethodPut, "/api/usage/settings", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// 落盘的是归一化后的值（币种大写）
	saved, err := usage.LoadSettings(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Currency != "CNY" || saved.MonthlyCost != 0.5 || saved.Over != usage.OverBlockAll {
		t.Fatalf("落盘的设置 = %+v", saved)
	}
	// 热更新：不用重启就能生效（金额上限与拦截策略都换了）
	st := s.usageBudget.Status(now)
	if st.LimitCost != 0.5 || st.Over != usage.OverBlockAll || !st.Enabled || st.Currency != "CNY" {
		t.Fatalf("预算应热更新: %+v", st)
	}
	var resp struct {
		Settings usage.Settings     `json:"settings"`
		Budget   usage.BudgetStatus `json:"budget"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Budget.LimitCost != 0.5 || resp.Settings.Currency != "CNY" {
		t.Fatalf("响应应带上最新预算与设置: %+v", resp)
	}
}

// PUT 非法值：400，磁盘与运行中的预算都不动（半份配置比不更新更糟）。
func TestSaveUsageSettingsInvalid(t *testing.T) {
	s, _, settingsPath := usageTestServer(t)
	before := s.usageBudget.Status(time.Now())

	body := `{"monthly_tokens":-1,"warn_percent":80,"over":"block_all"}`
	rec := httptest.NewRecorder()
	s.saveUsageSettings(rec, httptest.NewRequest(http.MethodPut, "/api/usage/settings", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
		t.Fatalf("非法的设置不该落盘: %v", err)
	}
	after := s.usageBudget.Status(time.Now())
	if after.LimitTokens != before.LimitTokens || after.LimitCost != before.LimitCost ||
		after.Exceeded != before.Exceeded || after.Over != before.Over {
		t.Fatalf("非法热更新不该改动预算: before=%+v after=%+v", before, after)
	}
}

// 没有设置路径（评测装配形态）：保存直接拒绝，而不是写到一个空路径上。
func TestSaveUsageSettingsWithoutPath(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.saveUsageSettings(rec, httptest.NewRequest(http.MethodPut, "/api/usage/settings", strings.NewReader(`{}`)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// PUT 的严格解析：拼错的键与 null 一律 400（静默忽略会让人以为限额设上了），
// 缺省字段保持原值（漏一项就退回 0 等于把限额悄悄清掉）。
func TestSaveUsageSettingsStrictBody(t *testing.T) {
	s, _, settingsPath := usageTestServer(t)
	if err := usage.SaveSettings(settingsPath, usage.Settings{
		MonthlyTokens: 1_000, MonthlyCost: 2, Currency: "CNY", WarnPercent: 55, Over: usage.OverBlockAll,
	}); err != nil {
		t.Fatal(err)
	}
	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.saveUsageSettings(rec, httptest.NewRequest(http.MethodPut, "/api/usage/settings", strings.NewReader(body)))
		return rec
	}

	for _, body := range []string{
		`{"monthly_token":1}`,    // 拼错的键
		`{"monthly_cost":null}`,  // null 与「没写」意思可能相反
		`{"over":null}`,          // 字符串字段同理
		`{"monthly_tokens":"1"}`, // 类型不对
	} {
		if rec := put(body); rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400 (%s)", body, rec.Code, rec.Body.String())
		}
	}
	// 坏请求不该动磁盘
	if saved, err := usage.LoadSettings(settingsPath); err != nil ||
		saved.MonthlyTokens != 1_000 || saved.WarnPercent != 55 || saved.Over != usage.OverBlockAll {
		t.Fatalf("坏请求改动了设置: %+v err=%v", saved, err)
	}

	// 只提交一项：其余保持原值
	if rec := put(`{"monthly_tokens":3000}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	saved, err := usage.LoadSettings(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.MonthlyTokens != 3_000 || saved.MonthlyCost != 2 || saved.Currency != "CNY" ||
		saved.WarnPercent != 55 || saved.Over != usage.OverBlockAll {
		t.Fatalf("缺省字段应保持原值: %+v", saved)
	}
}

// 预算只挂在当前自然月上：查历史月份时 budget 仍报本月，并用 budget_month_matches
// 明确告知它不属于这份报表。
func TestUsageReportBudgetMonth(t *testing.T) {
	s, _, _ := usageTestServer(t)
	now := time.Now().In(time.Local).Format(usageMonthFormat)

	v := decodeUsage(t, getUsage(t, s, ""))
	if v.Month != now || v.Budget == nil || v.Budget.Month != now || !v.BudgetMonthMatches {
		t.Fatalf("本月报表的预算应匹配: month=%q budget=%+v matches=%v", v.Month, v.Budget, v.BudgetMonthMatches)
	}
	old := decodeUsage(t, getUsage(t, s, "?month=2000-01"))
	if old.Budget == nil || old.Budget.Month != now || old.BudgetMonthMatches {
		t.Fatalf("历史月份的预算仍应是本月且明确标为不匹配: %+v matches=%v", old.Budget, old.BudgetMonthMatches)
	}
	if !strings.Contains(getUsage(t, s, "?month=2000-01").Body.String(), `"budget_month_matches":false`) {
		t.Error("budget_month_matches 应恒在下发")
	}
}

// 设置读得出来、账本也接着：一次 GET 就该清掉启动时留下的设置侧降级，
// 否则一次读失败会把限额挡到进程结束。
func TestUsageReportClearsSettingsDegrade(t *testing.T) {
	s, _, _ := usageTestServer(t)
	s.usageBudget.MarkDegraded()
	if st := s.usageBudget.Status(time.Now()); !st.Degraded {
		t.Fatalf("应处于降级: %+v", st)
	}
	v := decodeUsage(t, getUsage(t, s, ""))
	if v.SettingsError != "" || v.Budget == nil || v.Budget.Degraded {
		t.Fatalf("设置读得出来时不该再降级: settings_err=%q budget=%+v", v.SettingsError, v.Budget)
	}
}

// 磁盘上的 usage.json 被别处改过、却没走保存接口：页面上的设置是新值，运行中的
// 预算仍是旧值——这一点必须说出来，否则改完上限的人会以为它已经生效。
func TestUsageReportSettingsStale(t *testing.T) {
	s, _, settingsPath := usageTestServer(t)

	// 一开始两边一致（都是默认值）
	if v := decodeUsage(t, getUsage(t, s, "")); v.SettingsStale {
		t.Fatalf("默认状态下不该标过期: %+v", v.Settings)
	}

	// 只改盘、不热更新：stale 应为真
	if err := usage.SaveSettings(settingsPath, usage.Settings{
		MonthlyTokens: 2_000_000, MonthlyCost: 50, Currency: "CNY", WarnPercent: 80, Over: usage.OverSkipBackground,
	}); err != nil {
		t.Fatal(err)
	}
	v := decodeUsage(t, getUsage(t, s, ""))
	if !v.SettingsStale {
		t.Fatalf("磁盘设置改了而预算没跟着换，应标 settings_stale: %+v", v)
	}
	if v.Budget == nil || v.Budget.LimitCost != 0 {
		t.Fatalf("运行中的预算应仍是旧值: %+v", v.Budget)
	}

	// 走保存接口一次：热更新生效，stale 应清掉
	body := `{"monthly_tokens":2000000,"monthly_cost":50,"currency":"CNY","warn_percent":80,"over":"skip_background"}`
	rec := httptest.NewRecorder()
	s.saveUsageSettings(rec, httptest.NewRequest(http.MethodPut, "/api/usage/settings", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if v := decodeUsage(t, getUsage(t, s, "")); v.SettingsStale {
		t.Fatalf("保存接口生效后不该再标过期: %+v", v.Budget)
	}
}

// 页面标着 settings_stale 时点保存：表单里的值与加载时的那份相同，前端一个字段都没
// 改，发的是空对象。服务端必须以磁盘上的现有值为底稿合并并热更新——否则「点保存或
// 重启后生效」是假话，运行中的预算继续按旧值（往往就是不限额）判。
func TestSaveUsageSettingsEmptyBodyAppliesDisk(t *testing.T) {
	s, _, settingsPath := usageTestServer(t)
	if v := decodeUsage(t, getUsage(t, s, "")); v.SettingsStale {
		t.Fatalf("起始状态不该标过期: %+v", v.Settings)
	}

	// 手改盘、不热更新：页面加载到的 settings 就是这份，而预算还是默认（不限额）。
	if err := usage.SaveSettings(settingsPath, usage.Settings{
		MonthlyTokens: 2_000_000, MonthlyCost: 50, Currency: "CNY", WarnPercent: 80, Over: usage.OverSkipBackground,
	}); err != nil {
		t.Fatal(err)
	}
	if st := s.usageBudget.Status(time.Now()); st.LimitTokens != 0 || st.Enabled {
		t.Fatalf("此时运行中的预算应仍是旧值: %+v", st)
	}

	rec := httptest.NewRecorder()
	s.saveUsageSettings(rec, httptest.NewRequest(http.MethodPut, "/api/usage/settings", strings.NewReader(`{}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Settings usage.Settings     `json:"settings"`
		Budget   usage.BudgetStatus `json:"budget"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Settings.MonthlyTokens != 2_000_000 || resp.Settings.MonthlyCost != 50 || resp.Budget.LimitTokens != 2_000_000 {
		t.Fatalf("空对象应把磁盘上的配置应用起来: settings=%+v budget=%+v", resp.Settings, resp.Budget)
	}
	if st := s.usageBudget.Status(time.Now()); st.LimitTokens != 2_000_000 || st.LimitCost != 50 || !st.Enabled {
		t.Fatalf("运行中的预算应与磁盘同步: %+v", st)
	}
	if v := decodeUsage(t, getUsage(t, s, "")); v.SettingsStale {
		t.Fatalf("应用之后不该再标过期: %+v", v.Budget)
	}
}

// 磁盘上的 usage.json 在启动时就已经坏了：GET 报 settings_error、settings 退回默认值，
// 而 settings_stale 为假（默认值恰好与预算默认值一致），界面上看不出任何异常。用户点
// 保存时一个字段都没改，前端因此发的是空对象——服务端必须以默认值为底稿整份重写，
// 把文件修好并清掉降级；否则这一次点击只重新取一遍数据，文件还是坏的，预算一直降级。
func TestSaveUsageSettingsEmptyBodyRepairsBrokenFile(t *testing.T) {
	s, _, settingsPath := usageTestServer(t)
	if err := os.WriteFile(settingsPath, []byte("{ 不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 复现问题前提：报错但不 stale，所以界面这一侧只能靠 settings_error 决定强制保存。
	before := decodeUsage(t, getUsage(t, s, ""))
	if before.SettingsError == "" || !before.Degraded {
		t.Fatalf("坏文件应报出 settings_error: %+v", before)
	}
	if before.SettingsStale {
		t.Fatalf("默认值与预算默认值一致，此时不该标 stale: %+v", before.Settings)
	}

	rec := httptest.NewRecorder()
	s.saveUsageSettings(rec, httptest.NewRequest(http.MethodPut, "/api/usage/settings", strings.NewReader(`{}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Settings usage.Settings     `json:"settings"`
		Budget   usage.BudgetStatus `json:"budget"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Settings != usage.DefaultSettings() {
		t.Fatalf("响应应是默认设置: %+v", resp.Settings)
	}
	if resp.Budget.LimitTokens != 0 || resp.Budget.LimitCost != 0 || resp.Budget.Enabled {
		t.Fatalf("默认设置对应的预算是不设限: %+v", resp.Budget)
	}

	// 文件被写成了合法的一份默认设置：不再是坏 JSON，读出来没有错误。
	saved, err := usage.LoadSettings(settingsPath)
	if err != nil {
		t.Fatalf("保存后文件应当能读: %v", err)
	}
	if saved != usage.DefaultSettings() {
		t.Fatalf("修复后的文件应是默认设置: %+v", saved)
	}
	after := decodeUsage(t, getUsage(t, s, ""))
	if after.SettingsError != "" || after.Degraded {
		t.Fatalf("修复后不该再报设置错误或降级: %+v", after)
	}
	if after.Budget == nil || after.Budget.Degraded {
		t.Fatalf("预算应已清掉设置侧降级: %+v", after.Budget)
	}
}

// 用量是现扫账本算出来的，缓存下来只会让刚花掉的钱在刷新后仍不出现。
func TestUsageReportNoStore(t *testing.T) {
	s, _, _ := usageTestServer(t)
	rec := getUsage(t, s, "")
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	// 早退分支（没有账本）也要带上同一份缓存头
	empty := getUsage(t, &Server{}, "")
	if got := empty.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("空报告 Cache-Control = %q, want no-store", got)
	}
}

// 磁盘上的 usage.json 是更新版本写的：不合并、不保存，文件一个字节都不能动——
// 否则新版本才有的字段（以及用户那里的限额）会被整份覆盖抹掉。
func TestSaveUsageSettingsRefusesNewerVersion(t *testing.T) {
	s, _, settingsPath := usageTestServer(t)
	body := `{"version": 2, "monthly_tokens": 10, "future_limit": 5}`
	if err := os.WriteFile(settingsPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/usage/settings", strings.NewReader(`{"warn_percent": 90}`))
	s.saveUsageSettings(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409，body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("应带上错误文案: %s", rec.Body.String())
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != body {
		t.Fatalf("更新的版本写的文件不许改写:\n%s", raw)
	}
}

// 两个并发保存各只设一项：整段「读底稿 → 合并 → 落盘」持锁串行，后写入的一方必须
// 读到前一方已写的值，最终两项都在——否则界面报两次成功，上限却只剩一个。
func TestSaveUsageSettingsSerializesConcurrentWrites(t *testing.T) {
	s, _, settingsPath := usageTestServer(t)

	put := func(body string) error {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/usage/settings", strings.NewReader(body))
		s.saveUsageSettings(rec, req)
		if rec.Code != http.StatusOK {
			return fmt.Errorf("status %d: %s", rec.Code, rec.Body.String())
		}
		return nil
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs[0] = put(`{"monthly_tokens": 1234}`)
	}()
	go func() {
		defer wg.Done()
		<-start
		errs[1] = put(`{"monthly_cost": 6.5}`)
	}()
	close(start)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	got, err := usage.LoadSettings(settingsPath)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got.MonthlyTokens != 1234 || got.MonthlyCost != 6.5 {
		t.Fatalf("两次保存应当互相叠加，got %+v", got)
	}
}
