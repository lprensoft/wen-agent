package stylecheck

import (
	"regexp"
	"strings"

	"wen/internal/i18n"
)

// 「互动演绎」的记号在各语言里不是同一个：中文用【】，英文用一对星号。
//
// 这不是这里自己定的，是 roleplay 的 [互动演绎] 定的——那段提示词告诉模型用哪个
// 记号，这里只是照着找。两处必须对得上，否则演绎占比会一直是 0，而「少用修饰」
// 那几条规则会连演绎的笔调一起罚。cmd/wen 的 TestActingMarkerMatchesPrompt
// 盯着这条对应关系。
//
// 中文的记号是全角【】，与星号互不干扰；英文的星号则要先避开 **加粗**——
// `**bold**` 里面正好含着一对能匹配 `*…*` 的星号。

// bracketPattern 匹配一段【】演绎。不跨段落：没闭合的【会把后面整段吞掉。
var bracketPattern = regexp.MustCompile(`【[^】\n]*】`)

// asteriskPattern 匹配一段 *…* 演绎。同样不跨行。
var asteriskPattern = regexp.MustCompile(`\*[^*\n]+\*`)

// actingSpans 返回文本里全部演绎段的字节区间。
func actingSpans(lang, text string) [][]int {
	if isZH(lang) {
		return bracketPattern.FindAllStringIndex(text, -1)
	}
	// 先把 **加粗** 抹成等长的空格再找：抹掉的是记号本身，字节位置不变，
	// 因此找到的区间可以直接用在原文上。
	return asteriskPattern.FindAllStringIndex(blankBold(text), -1)
}

// isZH 判断某门语言的演绎记号是不是【】。空语言按中文算——它是本项目的源语言。
func isZH(lang string) bool { return lang == i18n.LangZH || lang == "" }

// blankBold 把 **加粗** 整段换成等长的空格。
func blankBold(s string) string {
	locs := mdBold.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s
	}
	b := []byte(s)
	for _, loc := range locs {
		for i := loc[0]; i < loc[1]; i++ {
			b[i] = ' '
		}
	}
	return string(b)
}

// cutSpans 把给定区间从文本里挖掉，各换成 sep。
func cutSpans(s string, spans [][]int, sep string) string {
	if len(spans) == 0 {
		return s
	}
	var b strings.Builder
	prev := 0
	for _, sp := range spans {
		b.WriteString(s[prev:sp[0]])
		b.WriteString(sep)
		prev = sp[1]
	}
	b.WriteString(s[prev:])
	return b.String()
}

// inSpans 判断某个字节位置是否落在演绎段内。spans 按位置递增，直接顺序扫。
func inSpans(spans [][]int, pos int) bool {
	for _, sp := range spans {
		if pos < sp[0] {
			return false
		}
		if pos < sp[1] {
			return true
		}
	}
	return false
}
