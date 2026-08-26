// Package textclip 提供插件注入文本的预算截断。
//
// 角色设定、台词样例这类配置文本会随提示词每轮全额重发且不参与预算裁剪，
// 必须有硬上限；截断的方式又因内容而异——设定砍掉一半仍然能用，样例被拦腰
// 砍断却是一份坏范本，只能整段丢弃。roleplay 与 dual_persona 都需要这两种
// 截断，各写一份迟早分叉（internal/statustext 补过这一课），故收在这里。
package textclip

import "wen/internal/prompt"

import "strings"

// Clip 按字节上限截断，不切断 UTF-8 字符。
// 截断并注明，而不是整段丢弃——设定被砍掉一半仍然能扮演，整段没了就不能。
func Clip(lang, s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	note := prompt.T(lang, "prompt.core.textclip.clipped", "…（设定过长已截断）")
	if limit <= len(note) {
		return ""
	}
	cut := limit - len(note)
	for cut > 0 && !isBoundary(s, cut) {
		cut--
	}
	if cut == 0 {
		return ""
	}
	return s[:cut] + note
}

// ClipPair 让两段文本共用一个字节预算，第一段优先。
func ClipPair(lang, first, second string, limit int) (string, string) {
	if limit <= 0 || len(first)+len(second) <= limit {
		return first, second
	}
	first = Clip(lang, first, limit)
	return first, Clip(lang, second, limit-len(first))
}

// ClipSegments 按空行分段、整段丢弃地截断——台词样例被拦腰砍断本身就是一份
// 坏范本，教出来的就是断句。段落都塞不下时宁可整个不要，不做字节级截断。
// 调用方须先把 \r\n 归一成 \n，否则认不出空行。
func ClipSegments(lang, s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	note := prompt.T(lang, "prompt.core.textclip.segments_dropped", "\n\n（其余样例过长，已略去）")
	budget := limit - len(note)
	if budget <= 0 {
		return ""
	}
	// 在预算内找最后一个空行边界。按字节切 s[:budget] 是安全的：
	// "\n" 不会是多字节字符的组成部分，LastIndex 只认完整的空行。
	cut := strings.LastIndex(s[:budget], "\n\n")
	if cut <= 0 {
		return ""
	}
	return strings.TrimRight(s[:cut], "\n") + note
}

// isBoundary 判断字节下标是否落在 UTF-8 字符边界上。
func isBoundary(s string, i int) bool {
	return i >= len(s) || s[i]&0xC0 != 0x80
}
