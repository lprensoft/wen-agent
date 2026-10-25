package wechatbot

import (
	"regexp"
	"strings"
	"unicode"
)

// filterMarkdown 去掉微信渲染不了、会原样露出符号的几种 markdown 写法，规则与官方
// 包（2.1.3 起的 markdown-filter）一致：
//
//   - 包着中日韩文字的斜体与粗斜体（*字*、***字***、_字_、___字___）去掉标记、留下文字；
//     包着西文的照留
//   - 五级、六级标题去掉 # 号
//   - 图片 ![alt](url) 整个去掉
//
// 代码块、行内代码、表格、分隔线与 **粗体** 原样保留。官方是边生成边过滤的流式
// 状态机，我们发的是整段文本，按行处理即可；代码块内一律不动。
func filterMarkdown(s string) string {
	lines := strings.Split(s, "\n")
	inFence := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		lines[i] = filterLine(line)
	}
	return strings.Join(lines, "\n")
}

var (
	deepHeadingRE = regexp.MustCompile(`^(\s{0,3})#{5,6}\s+`)
	imageRE       = regexp.MustCompile(`!\[[^\]\n]*\]\([^)\n]*\)`)
	boldItalicRE  = regexp.MustCompile(`(\*\*\*|___)([^*_\n]+?)(\*\*\*|___)`)
	// 单个 * 或 _：两侧不能紧挨同一个符号，否则那是 ** 粗体或 ___ 的一部分
	italicStarRE  = regexp.MustCompile(`(^|[^*])\*([^*\n]+?)\*([^*]|$)`)
	italicUnderRE = regexp.MustCompile(`(^|[^_\w])_([^_\n]+?)_([^_\w]|$)`)
)

func filterLine(line string) string {
	line = deepHeadingRE.ReplaceAllString(line, "$1")
	// 行内代码原样保留：按反引号切开，只处理代码以外的部分
	parts := strings.Split(line, "`")
	for i := 0; i < len(parts); i += 2 {
		p := imageRE.ReplaceAllString(parts[i], "")
		p = boldItalicRE.ReplaceAllStringFunc(p, func(m string) string {
			sub := boldItalicRE.FindStringSubmatch(m)
			if sub[1] == sub[3] && hasCJK(sub[2]) {
				return sub[2]
			}
			return m
		})
		p = stripItalic(p, italicStarRE)
		p = stripItalic(p, italicUnderRE)
		parts[i] = p
	}
	return strings.Join(parts, "`")
}

func stripItalic(s string, re *regexp.Regexp) string {
	return re.ReplaceAllStringFunc(s, func(m string) string {
		sub := re.FindStringSubmatch(m)
		if !hasCJK(sub[2]) {
			return m
		}
		return sub[1] + sub[2] + sub[3]
	})
}

func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			return true
		}
	}
	return false
}
