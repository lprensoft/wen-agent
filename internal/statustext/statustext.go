// Package statustext 把状态快照渲染成给人看的文本。
//
// /status 有三处输出：Web UI、QQ、微信。数据早已同源（都取 plugin.StatusInfo /
// GET /api/status），但措辞此前各写一份，两个 IM 插件里是逐字节相同的复制品——
// 改一处忘一处，输出就会悄悄分叉。Go 侧因此统一到这里；Web UI 在浏览器里另有
// 一份实现（app.js 的 runStatus），改动措辞时两边要一起动。
package statustext

import (
	"fmt"
	"strings"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/usage"
)

// Render 渲染完整的状态文本。sessionID 为空时不展示会话 ID 那一行。
//
// 版面刻意压得紧：模型与思考深度同行，上下文窗口并进会话那一行——
// 窗口大小单独占一行时，读的人还得自己拿它和用量做除法，而占用比例本来就在旁边。
func Render(lang string, info plugin.StatusInfo, sessionID string) string {
	lines := []string{
		header(lang, info.Version),
		i18n.Tf(lang, "status.model", "模型：%s / %s · 思考深度 %s", info.Provider, info.Model, info.Thinking),
		sessionLine(lang, info),
	}
	if info.HasSession {
		if line := CacheLine(lang, info); line != "" {
			lines = append(lines, line)
		}
		if sessionID != "" {
			// 会话 ID 便于用 read_session / read_archive 定位这次对话
			lines = append(lines, i18n.Tf(lang, "status.session_id", "会话 ID：%s", sessionID))
		}
	}
	if line := BudgetLine(lang, info); line != "" {
		lines = append(lines, line)
	}
	lines = append(lines, info.PluginLines...) // 插件贡献的状态行，按注册顺序
	return strings.Join(lines, "\n")
}

// header 是首行：带上程序版本号。
func header(lang, version string) string {
	if version == "" {
		return i18n.T(lang, "status.header.plain", "📊 Agent 状态")
	}
	return i18n.Tf(lang, "status.header", "📊 Wen Agent %s", version)
}

// sessionLine 把「用了多少 / 一共多少 / 占多少」放进一行。
// 实测值不加标注，估算值前缀「约」——区别只在这一个字上，不必再写「（估算）」。
func sessionLine(lang string, info plugin.StatusInfo) string {
	if !info.HasSession {
		return i18n.Tf(lang, "status.session.none", "当前会话：无（上下文窗口 %s tokens）", Comma(info.ContextLength))
	}
	used, prefix := info.MeasuredTokens, ""
	if used < 0 {
		used, prefix = info.EstTokens, i18n.T(lang, "status.approx", "约 ")
	}
	return i18n.Tf(lang, "status.session", "当前会话：%d 条消息，%s%s / %s tokens（占用 %s%%）",
		info.MessageCount, prefix, Comma(used), Comma(info.ContextLength), Pct(used, info.ContextLength))
}

// CacheLine 给出提示词缓存那一行，本轮没用上缓存时返回空串。
func CacheLine(lang string, info plugin.StatusInfo) string {
	if info.CachedTokens <= 0 && info.CacheWriteTokens <= 0 {
		return ""
	}
	s := i18n.Tf(lang, "status.cache", "提示词缓存：命中 %s", Comma(info.CachedTokens))
	if info.CacheWriteTokens > 0 {
		s += i18n.Tf(lang, "status.cache.write", " / 写入 %s", Comma(info.CacheWriteTokens))
	}
	s += " tokens"
	if info.PromptTokens > 0 {
		// MeasuredTokens 含输出，不能拿来当分母
		s += i18n.Tf(lang, "status.cache.share", "（占本轮输入 %s%%）", Pct(info.CachedTokens, info.PromptTokens))
	}
	return s
}

// BudgetLine 给出本月用量预算那一行；没有预算、或既不设限也不降级也不记账失败时
// 返回空串。不设限但记账失败时给单独一行（见下）。
//
// 阈值（Warn / Exceeded / Degraded）与口径都由 BudgetInfo 给，这里只负责排版：
// 自己重算等于在界面上引入第二套判定，两边迟早会对不上。
//
// token 与金额两段都在时用「 · 」并列，任一维度的限额为 0 就整段省略——设了钱
// 的上限却没设 token 上限是正常配置，不该显示「0 / 0 tokens」。
func BudgetLine(lang string, info plugin.StatusInfo) string {
	b := info.Budget
	if b == nil {
		return ""
	}
	if b.Degraded {
		// 降级时不报数字：账本或设置读不出来，报出来的比例只会被当成真实用量。
		return i18n.T(lang, "status.budget.degraded", "⚠️ 本月用量预算不可用（账本或设置读不出来）")
	}
	// 没设上限时本来不占版面，但账本写不进去是另一件事：即便不设限，本月合计
	// 也可能偏少。它不该被「没有预算就没有预算行」一起藏掉，也不该塞进预算行冒充。
	if !b.Enabled {
		if !b.WriteFailed {
			return ""
		}
		return i18n.T(lang, "status.budget.writeFailedOnly", "⚠️ 本月用量账本写入失败，统计可能不完整")
	}
	var parts []string
	if b.LimitTokens > 0 {
		parts = append(parts, i18n.Tf(lang, "status.budget.tokens", "%s / %s tokens（%s%%）",
			Comma64(b.SpentTokens), Comma64(b.LimitTokens), Pct64(b.SpentTokens, b.LimitTokens)))
	}
	if cost := costDetail(lang, b); cost != "" {
		parts = append(parts, cost)
	}
	line := i18n.T(lang, "status.budget.head", "本月用量预算：") + strings.Join(parts, " · ")
	// 设了金额上限却没有价格表：那一段永远不会生效，必须说出来，否则用户会以为
	// 钱的上限在兜底，实际只有 token 那一路管着。
	if b.LimitCost > 0 && !b.CostReady {
		line += i18n.T(lang, "status.budget.noPrice", "（缺少价格，金额上限未生效）")
	} else if b.LimitCost > 0 && b.CostUnpriced {
		// 价格表在，但本月用到的模型一个都没定价：同样等于没有金额上限。
		line += i18n.T(lang, "status.budget.costUnpriced", "（本月有用量没有价格，金额上限未生效）")
	}
	// 别的币种的金额没计进本币上限：不说明的话，「还差得远」是个假象。
	if b.LimitCost > 0 && b.ForeignCost {
		line += i18n.Tf(lang, "status.budget.foreign", "（有其他币种的花费未计入 %s 上限）", b.Currency)
	}
	// 坏分片让合计偏少，但不改变拦截：说出来即可，别让用户把偏低的数字当成真实用量。
	if b.SkippedFiles > 0 {
		line += i18n.Tf(lang, "status.budget.skipped", "（%d 个账本分片读不出来，合计可能偏少）", b.SkippedFiles)
	}
	// 记账失败同样只让统计偏少，不改变拦截：说出来即可。它不并进 Degraded——
	// 那会 fail-open，而 token 上限正该继续按已观测到的花费拦着。
	if b.WriteFailed {
		line += i18n.T(lang, "status.budget.writeFailed", "（账本写入失败，本月统计可能不完整）")
	}
	// 超限的拦截范围取决于 over：block_all 连前台一起停，只说「后台」是低报。
	switch {
	case b.Exceeded && b.Over == usage.OverBlockAll:
		return "⛔ " + line + i18n.T(lang, "status.budget.exceededAll", "（已达上限，所有请求已暂停）")
	case b.Exceeded:
		return "⛔ " + line + i18n.T(lang, "status.budget.exceeded", "（已达上限，后台任务已暂停）")
	case b.Warn:
		return "⚠️ " + line + i18n.T(lang, "status.budget.warn", "（接近上限）")
	}
	return "💰 " + line
}

// costDetail 给出预算行里的金额段（含「这个数不准」的说明）。
//
// 没有价格表、或既没设金额上限也没花过钱时返回空串：前者由 BudgetLine 的 noPrice
// 说明，后者没有可看的数字。
func costDetail(lang string, b *plugin.BudgetInfo) string {
	if !b.CostReady || (b.LimitCost <= 0 && b.SpentCost <= 0) {
		return ""
	}
	amount := Money(b.SpentCost)
	if b.LimitCost > 0 {
		amount += " / " + Money(b.LimitCost)
	}
	if cur := strings.TrimSpace(b.Currency); cur != "" {
		amount = cur + " " + amount
	}
	// 未定价的用量与缓存价回退都会让这个数字偏离账单，二选一说一句就够。花了 0
	// 元时这两句没有意义——那时该由 CostUnpriced / ForeignCost 解释为什么是 0。
	// 缓存价那一档不说「偏高」：读缓存通常更便宜、写缓存有时更贵，方向因提供商而异。
	switch {
	case b.SpentCost > 0 && b.UnpricedTokens > 0:
		amount += i18n.Tf(lang, "status.budget.unpriced", "（含 %s tokens 未定价，金额为估算）",
			Comma64(b.UnpricedTokens))
	case b.SpentCost > 0 && b.CostInexact:
		amount += i18n.T(lang, "status.budget.inexact", "（金额为估算，可能偏高或偏低）")
	}
	return amount
}

// Money 把金额排成两位小数。单价是每百万 token 的价，账单上的数到分就够了；
// 更细的小数只是把浮点噪声显示得更精确。
func Money(v float64) string {
	return fmt.Sprintf("%.2f", v)
}

// Comma 加千位分隔符，与 Web UI 的 toLocaleString 显示一致。
func Comma(n int) string {
	return Comma64(int64(n))
}

// Comma64 是 Comma 的 int64 版本：预算的已用数与上限都是 int64。
func Comma64(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

// Pct 取一位小数。token 占用本就是个粗略量（还带着估算值），
// 两位小数只是把噪声显示得更精确。
func Pct(used, total int) string {
	return Pct64(int64(used), int64(total))
}

// Pct64 是 Pct 的 int64 版本，供预算这类大数用。
//
// 整数半上取一位小数，不走 "%.1f"：后者按二进制浮点值舍入，5,000 / 2,000,000
// 这种恰好落在 .25 处的比例会被舍成 0.2，而 Web 端的 toFixed(1) 给 0.3——同一个
// 数字在终端与网页上不一样，是一种只在截图里才看得见的别扭。
func Pct64(used, total int64) string {
	if total <= 0 || used <= 0 {
		return "0.0"
	}
	tenths := (used*1000 + total/2) / total
	return fmt.Sprintf("%d.%d", tenths/10, tenths%10)
}
