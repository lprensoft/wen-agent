package statustext

import (
	"testing"
	"unicode"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/usage"
)

func hanIn(s string) string {
	var out []rune
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			out = append(out, r)
		}
	}
	return string(out)
}

// 状态命令的措辞三处输出共用（Web UI 另有一份在浏览器里），漏一句就会在英文用户的
// QQ 或微信里冒出一行中文。逐条键有没有译文是覆盖测试的事，这里查的是拼出来的整段。
func TestEnglishStatusHasNoChinese(t *testing.T) {
	info := plugin.StatusInfo{
		Version: "v0.0.0", Provider: "p", Model: "m", Thinking: "high",
		ContextLength: 200000, HasSession: true, MessageCount: 12,
		MeasuredTokens: 3000, EstTokens: 2500,
		CachedTokens: 1000, CacheWriteTokens: 200, PromptTokens: 2000,
		Budget: &plugin.BudgetInfo{
			Enabled: true, LimitTokens: 2000000, SpentTokens: 1300000, Warn: true,
			// 金额段与它的三条说明各有自己的键，必须逐个走到
			LimitCost: 50, SpentCost: 1.23, Currency: "CNY", CostReady: true,
			CostInexact: true, UnpricedTokens: 4000,
		},
	}
	// 降级那一句自成一行，措辞与其它三种不同，单独扫一遍
	degraded := info
	degraded.Budget = &plugin.BudgetInfo{Enabled: true, LimitTokens: 2000000, Degraded: true}
	// block_all 的超限后缀另有一个键，也要有英文
	blockedAll := info
	blockedAll.Budget = &plugin.BudgetInfo{
		Enabled: true, LimitTokens: 2000000, SpentTokens: 2100000,
		Exceeded: true, Over: usage.OverBlockAll,
	}
	// 缓存价回退与「没有价格表」各有一句说明，也要有英文
	inexact := info
	inexact.Budget = &plugin.BudgetInfo{
		Enabled: true, LimitCost: 50, SpentCost: 1.23, Currency: "CNY",
		CostReady: true, CostInexact: true,
	}
	noPrice := info
	noPrice.Budget = &plugin.BudgetInfo{Enabled: true, LimitCost: 50, Currency: "CNY"}
	// 未定价、别的币种与坏分片各有一句，也要有英文
	costUnpriced := info
	costUnpriced.Budget = &plugin.BudgetInfo{Enabled: true, LimitCost: 50, Currency: "CNY",
		CostReady: true, CostUnpriced: true, UnpricedTokens: 4000}
	foreign := info
	foreign.Budget = &plugin.BudgetInfo{Enabled: true, LimitCost: 50, Currency: "CNY",
		CostReady: true, ForeignCost: true}
	skipped := info
	skipped.Budget = &plugin.BudgetInfo{Enabled: true, LimitTokens: 2000000, SpentTokens: 1300000,
		SkippedFiles: 2}
	// 记账失败另有一句，也要有英文
	writeFailed := info
	writeFailed.Budget = &plugin.BudgetInfo{Enabled: true, LimitTokens: 2000000, SpentTokens: 1300000,
		WriteFailed: true}
	// 没设上限但记账失败时单列一行，这一档也要有英文
	writeFailedOnly := info
	writeFailedOnly.Budget = &plugin.BudgetInfo{WriteFailed: true}
	for _, s := range []string{
		Render(i18n.LangEN, info, "s1"),
		Render(i18n.LangEN, plugin.StatusInfo{Version: "v0.0.0", Provider: "p", Model: "m", Thinking: "high"}, ""),
		Render(i18n.LangEN, degraded, ""),
		Render(i18n.LangEN, blockedAll, ""),
		CacheLine(i18n.LangEN, info),
		BudgetLine(i18n.LangEN, info),
		BudgetLine(i18n.LangEN, degraded),
		BudgetLine(i18n.LangEN, blockedAll),
		BudgetLine(i18n.LangEN, inexact),
		BudgetLine(i18n.LangEN, noPrice),
		BudgetLine(i18n.LangEN, costUnpriced),
		BudgetLine(i18n.LangEN, foreign),
		BudgetLine(i18n.LangEN, skipped),
		BudgetLine(i18n.LangEN, writeFailed),
		BudgetLine(i18n.LangEN, writeFailedOnly),
	} {
		if han := hanIn(s); han != "" {
			t.Errorf("英文状态输出里有中文 %q:\n%s", han, s)
		}
	}
}
