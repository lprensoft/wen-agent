package statustext

import (
	"strings"
	"testing"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/usage"
)

func base() plugin.StatusInfo {
	return plugin.StatusInfo{
		Version: "v0.1.3", Provider: "deepseek", Model: "deepseek-v4-flash",
		Thinking: "high", ContextLength: 1000000, MeasuredTokens: -1,
	}
}

func TestComma(t *testing.T) {
	cases := map[int]string{131072: "131,072", 999: "999", 0: "0", 1000000: "1,000,000", -4567: "-4,567"}
	for in, want := range cases {
		if got := Comma(in); got != want {
			t.Errorf("Comma(%d) = %s, want %s", in, got, want)
		}
	}
}

func TestPct(t *testing.T) {
	if got := Pct(1234, 131072); got != "0.9" {
		t.Errorf("Pct = %s, want 0.9", got)
	}
	if got := Pct(1, 0); got != "0.0" {
		t.Errorf("除零 = %s, want 0.0", got)
	}
}

// 取一位小数按整数半上，与 Web 端 toFixed(1) 一致：5,000 / 2,000,000 是 0.25%，
// 两边都该显示 0.3，而不是浮点舍入的 0.2。
func TestPct64(t *testing.T) {
	cases := []struct {
		used, total int64
		want        string
	}{
		{5000, 2_000_000, "0.3"},
		{1_300_000, 2_000_000, "65.0"},
		{2_100_000, 2_000_000, "105.0"},
		{0, 2_000_000, "0.0"},
		{1, 0, "0.0"},
	}
	for _, c := range cases {
		if got := Pct64(c.used, c.total); got != c.want {
			t.Errorf("Pct64(%d, %d) = %s, want %s", c.used, c.total, got, c.want)
		}
	}
}

// 模型与思考深度同一行，上下文窗口并进会话那一行。
func TestRenderMergesLines(t *testing.T) {
	info := base()
	info.HasSession = true
	info.MessageCount = 12
	info.MeasuredTokens = 45231

	got := Render("", info, "sess-1")
	lines := strings.Split(got, "\n")

	if lines[0] != "📊 Wen Agent v0.1.3" {
		t.Errorf("首行 = %q", lines[0])
	}
	if lines[1] != "模型：deepseek / deepseek-v4-flash · 思考深度 high" {
		t.Errorf("模型行 = %q", lines[1])
	}
	if lines[2] != "当前会话：12 条消息，45,231 / 1,000,000 tokens（占用 4.5%）" {
		t.Errorf("会话行 = %q", lines[2])
	}
	if lines[3] != "会话 ID：sess-1" {
		t.Errorf("会话 ID 行 = %q", lines[3])
	}
	if len(lines) != 4 {
		t.Errorf("共 %d 行，超出预期的 4 行：\n%s", len(lines), got)
	}
	// 上下文窗口不再单独占一行
	for _, l := range lines {
		if strings.HasPrefix(l, "上下文窗口") {
			t.Errorf("上下文窗口仍单独成行: %q", l)
		}
	}
}

// 没有实测值时用估算值，靠一个「约」字区分，不再另写「（估算）」。
func TestRenderEstimated(t *testing.T) {
	info := base()
	info.HasSession = true
	info.MessageCount = 3
	info.MeasuredTokens = -1
	info.EstTokens = 2048

	line := strings.Split(Render("", info, ""), "\n")[2]
	if line != "当前会话：3 条消息，约 2,048 / 1,000,000 tokens（占用 0.2%）" {
		t.Errorf("估算行 = %q", line)
	}
	if strings.Contains(line, "估算") {
		t.Errorf("「约」已经表达了估算，不必再写一次: %q", line)
	}
}

// 无会话时仍要能看到上下文窗口——它是模型配置的一部分，不随会话消失。
func TestRenderWithoutSession(t *testing.T) {
	got := Render("", base(), "")
	if !strings.Contains(got, "当前会话：无（上下文窗口 1,000,000 tokens）") {
		t.Errorf("无会话时的输出 = %q", got)
	}
	if strings.Contains(got, "会话 ID") {
		t.Error("无会话时不该出现会话 ID")
	}
}

func TestCacheLine(t *testing.T) {
	info := base()
	if CacheLine("", info) != "" {
		t.Error("没用上缓存时应返回空串")
	}
	info.CachedTokens = 30000
	info.CacheWriteTokens = 5000
	info.PromptTokens = 45000
	want := "提示词缓存：命中 30,000 / 写入 5,000 tokens（占本轮输入 66.7%）"
	if got := CacheLine("", info); got != want {
		t.Errorf("缓存行 = %q\nwant %q", got, want)
	}
}

// 插件状态行接在会话行之后，按传入顺序原样输出，核心不解释其内容。
func TestRenderAppendsPluginLines(t *testing.T) {
	info := base()
	info.PluginLines = []string{"💓 心跳：…", "🧠 记忆：…"}
	lines := strings.Split(Render("", info, ""), "\n")
	if lines[len(lines)-2] != "💓 心跳：…" || lines[len(lines)-1] != "🧠 记忆：…" {
		t.Errorf("插件行未按序附在末尾:\n%s", strings.Join(lines, "\n"))
	}
}

// 预算行四种状态：正常、接近上限、已达上限、降级。阈值由 BudgetInfo 给，
// 这里只确认排版与后缀。
func TestBudgetLine(t *testing.T) {
	info := base()
	if got := BudgetLine("", info); got != "" {
		t.Errorf("没有预算时不该有预算行: %q", got)
	}
	info.Budget = &plugin.BudgetInfo{Enabled: false, LimitTokens: 0}
	if got := BudgetLine("", info); got != "" {
		t.Errorf("不设限且不降级时不该有预算行: %q", got)
	}

	const body = "本月用量预算：1,300,000 / 2,000,000 tokens（65.0%）"
	cases := []struct {
		name string
		info plugin.BudgetInfo
		want string
	}{
		{"正常", plugin.BudgetInfo{Enabled: true, LimitTokens: 2_000_000, SpentTokens: 1_300_000}, "💰 " + body},
		{"接近", plugin.BudgetInfo{Enabled: true, LimitTokens: 2_000_000, SpentTokens: 1_300_000, Warn: true}, "⚠️ " + body + "（接近上限）"},
		{"超限·后台", plugin.BudgetInfo{Enabled: true, LimitTokens: 2_000_000, SpentTokens: 1_300_000, Exceeded: true, Over: usage.OverSkipBackground}, "⛔ " + body + "（已达上限，后台任务已暂停）"},
		{"超限·全部", plugin.BudgetInfo{Enabled: true, LimitTokens: 2_000_000, SpentTokens: 1_300_000, Exceeded: true, Over: usage.OverBlockAll}, "⛔ " + body + "（已达上限，所有请求已暂停）"},
		{"降级", plugin.BudgetInfo{Enabled: true, LimitTokens: 2_000_000, Degraded: true}, "⚠️ 本月用量预算不可用（账本或设置读不出来）"},
	}
	for _, c := range cases {
		info.Budget = &c.info
		if got := BudgetLine("", info); got != c.want {
			t.Errorf("%s: 预算行 = %q\nwant %q", c.name, got, c.want)
		}
	}
}

// 没设上限但账本写不进去：预算行本来不出现，这时要单独给一行——默认配置下没有
// 上限，把「合计可能偏少」一起藏掉，用户就永远看不到它。
func TestBudgetLineWriteFailedOnly(t *testing.T) {
	info := base()
	info.Budget = &plugin.BudgetInfo{WriteFailed: true}
	const want = "⚠️ 本月用量账本写入失败，统计可能不完整"
	if got := BudgetLine("", info); got != want {
		t.Errorf("无预算但记账失败 = %q\nwant %q", got, want)
	}
	if han := hanIn(BudgetLine(i18n.LangEN, info)); han != "" {
		t.Errorf("英文行里有中文 %q", han)
	}
}

// 预算行的金额段：有价格就并列金额（含「这个数不准」的说明），没价格就只说
// 「金额上限未生效」——设了钱的上限却看不见它生效，比不设更糟。
func TestBudgetLineCost(t *testing.T) {
	const head = "本月用量预算："
	cases := []struct {
		name string
		info plugin.BudgetInfo
		want string
	}{
		{
			"只有金额上限",
			plugin.BudgetInfo{Enabled: true, LimitCost: 50, SpentCost: 1.23, Currency: "CNY", CostReady: true},
			"💰 " + head + "CNY 1.23 / 50.00",
		},
		{
			"token 与金额并列",
			plugin.BudgetInfo{
				Enabled: true, LimitTokens: 2_000_000, SpentTokens: 1_300_000,
				LimitCost: 50, SpentCost: 1.23, Currency: "CNY", CostReady: true,
			},
			"💰 " + head + "1,300,000 / 2,000,000 tokens（65.0%） · CNY 1.23 / 50.00",
		},
		{
			"只花了钱没设金额上限",
			plugin.BudgetInfo{Enabled: true, LimitTokens: 2_000_000, SpentCost: 1.5, Currency: "CNY", CostReady: true},
			"💰 " + head + "0 / 2,000,000 tokens（0.0%） · CNY 1.50",
		},
		{
			"有未定价用量",
			plugin.BudgetInfo{
				Enabled: true, LimitCost: 50, SpentCost: 1.23, Currency: "CNY",
				CostReady: true, CostInexact: true, UnpricedTokens: 4_000,
			},
			"💰 " + head + "CNY 1.23 / 50.00（含 4,000 tokens 未定价，金额为估算）",
		},
		{
			"缓存价回退",
			plugin.BudgetInfo{
				Enabled: true, LimitCost: 50, SpentCost: 1.23, Currency: "CNY",
				CostReady: true, CostInexact: true,
			},
			"💰 " + head + "CNY 1.23 / 50.00（金额为估算，可能偏高或偏低）",
		},
		{
			"没有价格表但设了金额上限",
			plugin.BudgetInfo{Enabled: true, LimitCost: 50, Currency: "CNY"},
			"💰 " + head + "（缺少价格，金额上限未生效）",
		},
		{
			"价格表在但本月用到的模型没定价",
			plugin.BudgetInfo{
				Enabled: true, LimitCost: 50, Currency: "CNY", CostReady: true,
				CostUnpriced: true, UnpricedTokens: 1_000,
			},
			"💰 " + head + "CNY 0.00 / 50.00（本月有用量没有价格，金额上限未生效）",
		},
		{
			"只有别的币种的花费",
			plugin.BudgetInfo{Enabled: true, LimitCost: 50, Currency: "CNY", CostReady: true, ForeignCost: true},
			"💰 " + head + "CNY 0.00 / 50.00（有其他币种的花费未计入 CNY 上限）",
		},
		{
			"有坏分片",
			plugin.BudgetInfo{Enabled: true, LimitTokens: 2_000_000, SpentTokens: 1_300_000, SkippedFiles: 2},
			"💰 " + head + "1,300,000 / 2,000,000 tokens（65.0%）（2 个账本分片读不出来，合计可能偏少）",
		},
		{
			// 记账失败只让统计偏少，不改变拦截：说出来即可，也不能悄悄藏掉。
			"账本写入失败",
			plugin.BudgetInfo{Enabled: true, LimitTokens: 2_000_000, SpentTokens: 1_300_000, WriteFailed: true},
			"💰 " + head + "1,300,000 / 2,000,000 tokens（65.0%）（账本写入失败，本月统计可能不完整）",
		},
		{
			"超限时金额同样报出",
			plugin.BudgetInfo{
				Enabled: true, LimitCost: 1, SpentCost: 2.5, Currency: "CNY",
				CostReady: true, Exceeded: true, Over: usage.OverSkipBackground,
			},
			"⛔ " + head + "CNY 2.50 / 1.00（已达上限，后台任务已暂停）",
		},
	}
	for _, c := range cases {
		info := base()
		info.Budget = &c.info
		if got := BudgetLine("", info); got != c.want {
			t.Errorf("%s: 预算行 = %q\nwant %q", c.name, got, c.want)
		}
	}
}

// 本月既有已定价的本币用量、又有未定价用量：金额上限在管着（CostUnpriced 为假），
// 这时候再报「金额上限未生效」就是假话——只需要说明这个数里有一部分没定价。
func TestBudgetLineCostUnpricedWithSpent(t *testing.T) {
	info := base()
	info.Budget = &plugin.BudgetInfo{
		Enabled: true, LimitCost: 50, SpentCost: 1.23, Currency: "CNY",
		CostReady: true, CostInexact: true, UnpricedTokens: 4_000,
	}
	got := BudgetLine("", info)
	if strings.Contains(got, "金额上限未生效") {
		t.Errorf("有本币花费时不该说金额上限未生效: %q", got)
	}
	if !strings.Contains(got, "（含 4,000 tokens 未定价，金额为估算）") {
		t.Errorf("应说明未定价的那部分: %q", got)
	}
}

// Money 两位小数：金额只在分这一位上可读，更多小数只是把浮点噪声显示得更精确。
func TestMoney(t *testing.T) {
	cases := map[float64]string{0: "0.00", 1: "1.00", 1.234: "1.23", 12.345: "12.35", 1234.5: "1234.50"}
	for in, want := range cases {
		if got := Money(in); got != want {
			t.Errorf("Money(%v) = %q, want %q", in, got, want)
		}
	}
}

// 预算行在会话/缓存之后、插件行之前，且与会话无关：没有会话也照常出现。
func TestRenderPlacesBudgetLine(t *testing.T) {
	info := base()
	info.HasSession = true
	info.MessageCount = 12
	info.MeasuredTokens = 45231
	info.Budget = &plugin.BudgetInfo{Enabled: true, LimitTokens: 2_000_000, SpentTokens: 1_300_000}
	info.PluginLines = []string{"💓 心跳：…"}

	lines := strings.Split(Render("", info, "sess-1"), "\n")
	if len(lines) != 6 {
		t.Fatalf("共 %d 行，超出预期的 6 行：\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.HasPrefix(lines[4], "💰 本月用量预算：") {
		t.Errorf("预算行位置不对: %q", lines[4])
	}
	if lines[5] != "💓 心跳：…" {
		t.Errorf("插件行应在预算行之后: %q", lines[5])
	}

	// 无会话时插件此前会整段消失，预算不该跟着消失
	noSession := base()
	noSession.Budget = info.Budget
	if !strings.Contains(Render("", noSession, ""), "本月用量预算") {
		t.Errorf("无会话时也应输出预算行:\n%s", Render("", noSession, ""))
	}
}
