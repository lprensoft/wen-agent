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

// Comma 加千位分隔符，与 Web UI 的 toLocaleString 显示一致。
func Comma(n int) string {
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
	if total <= 0 {
		return "0.0"
	}
	return fmt.Sprintf("%.1f", float64(used)/float64(total)*100)
}
