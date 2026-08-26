package telegrambot

import (
	"strings"

	"wen/internal/i18n"
)

// Telegram 的 MarkdownV2 与通常的 markdown 不是一回事：加粗是单个 *，斜体是单个 _，
// 而 _*[]()~`>#+-=|{}.! 这十八个字符**在任何位置**都必须反斜杠转义，漏一个整条消息
// 就会被 400 拒收。所以不能把模型输出的 markdown 直接发过去，得翻译一遍。
//
// 翻译的原则是「宁可少认一种语法，不可多转一个字符」：认不出来的一律当字面量转义，
// 最坏结果只是少了个加粗；而转义漏了则是消息整条发不出去。真出错还有兜底——
// send 那边收到解析失败会对该用户降级成纯文本重发。

// mdv2Special 是 MarkdownV2 的全部保留字符。
const mdv2Special = "_*[]()~`>#+-=|{}.!"

// escapeMDV2 转义正文里的保留字符。
func escapeMDV2(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 16)
	for _, r := range s {
		if r < 128 && strings.ContainsRune(mdv2Special, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// escapeCode 转义代码块与行内代码里的内容：只有反引号和反斜杠需要处理。
func escapeCode(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, "`", "\\`")
}

// escapeURL 转义链接目标：只有右括号和反斜杠需要处理。
func escapeURL(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, ")", `\)`)
}

// hrLine 是分隔线的替代形态：--- 在 MarkdownV2 里要逐个转义，转出来是一串 \-，
// 不如直接换成画线字符（它不在保留字集合里）。
const hrLine = "──────────"

// toMarkdownV2 把模型输出的 markdown 翻成 Telegram 的 MarkdownV2。
func toMarkdownV2(lang, text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	inCode := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// 代码块围栏：开围栏可以带语言标注，闭围栏不带
		if strings.HasPrefix(trimmed, "```") {
			if !inCode {
				lang := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
				out = append(out, "```"+escapeCode(lang))
			} else {
				out = append(out, "```")
			}
			inCode = !inCode
			continue
		}
		if inCode {
			out = append(out, escapeCode(line))
			continue
		}

		out = append(out, convertLine(lang, line))
	}
	// 未闭合的代码块自己补上，否则 Telegram 会因为围栏不成对整条拒收
	if inCode {
		out = append(out, "```")
	}
	return strings.Join(out, "\n")
}

// convertLine 处理一个非代码块行的行级语法，再把余下内容交给行内转换。
func convertLine(lang, line string) string {
	trimmed := strings.TrimSpace(line)

	// 分隔线
	if isHR(trimmed) {
		return hrLine
	}

	// 标题：转成加粗。标题里的强调标记先剥掉，避免加粗套加粗——嵌套同类实体
	// 会被 Telegram 判为语法错误，而标题里再嵌一层格式本来也没什么意义。
	if level, rest, ok := splitHeading(trimmed); ok && level > 0 {
		rest = strings.TrimSpace(stripEmphasis(rest))
		if rest == "" {
			return ""
		}
		return "*" + escapeMDV2(rest) + "*"
	}

	// 引用：> 是 MarkdownV2 的引用标记，本身不转义
	if strings.HasPrefix(trimmed, ">") {
		body := strings.TrimPrefix(trimmed, ">")
		body = strings.TrimPrefix(body, " ")
		return ">" + convertInline(lang, body)
	}

	// 无序列表：换成不需要转义的圆点，缩进保留
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	if body, ok := splitBullet(trimmed); ok {
		return indent + "• " + convertInline(lang, body)
	}

	return indent + convertInline(lang, strings.TrimLeft(line, " \t"))
}

func isHR(s string) bool {
	if len(s) < 3 {
		return false
	}
	for _, set := range []string{"-", "*", "_"} {
		if strings.Trim(s, set) == "" {
			return true
		}
	}
	return false
}

// splitHeading 认 # 到 ###### 的标题。
func splitHeading(s string) (level int, rest string, ok bool) {
	i := 0
	for i < len(s) && s[i] == '#' {
		i++
	}
	if i == 0 || i > 6 || i >= len(s) || (s[i] != ' ' && s[i] != '\t') {
		return 0, "", false
	}
	return i, strings.TrimSpace(s[i:]), true
}

// splitBullet 认 -、*、+ 开头的无序列表项。有序列表不动：数字加点本来就读得通，
// 而把 "1." 认成列表再转回去只是多一次出错机会。
func splitBullet(s string) (string, bool) {
	if len(s) < 2 {
		return "", false
	}
	if (s[0] == '-' || s[0] == '*' || s[0] == '+') && (s[1] == ' ' || s[1] == '\t') {
		return strings.TrimSpace(s[1:]), true
	}
	return "", false
}

// stripEmphasis 去掉强调标记，只留文字。
func stripEmphasis(s string) string {
	for _, mark := range []string{"***", "**", "~~", "*", "__"} {
		s = strings.ReplaceAll(s, mark, "")
	}
	return s
}

// convertInline 逐字符扫描一行，识别行内代码、链接、图片、加粗、删除线、斜体，
// 其余一律按字面量转义。
//
// 刻意不认下划线斜体（_x_）：snake_case 的标识符在技术对话里遍地都是，认了以后
// 半句话会莫名其妙变成斜体，还可能因为找不到配对而破坏整条消息。
func convertInline(lang, s string) string {
	var b strings.Builder
	b.Grow(len(s) + 32)
	r := []rune(s)

	for i := 0; i < len(r); {
		switch {
		case r[i] == '`':
			if end := indexRune(r, '`', i+1); end > i+1 {
				b.WriteString("`" + escapeCode(string(r[i+1:end])) + "`")
				i = end + 1
				continue
			}
		case r[i] == '!' && i+1 < len(r) && r[i+1] == '[':
			// 图片降级成链接：Telegram 的文本消息不渲染 markdown 图片
			if text, url, next, ok := matchLink(r, i+1); ok {
				if text == "" {
					text = i18n.T(lang, "plugin.telegram_bot.md.image", "图片")
				}
				b.WriteString("[" + escapeMDV2(text) + "](" + escapeURL(url) + ")")
				i = next
				continue
			}
		case r[i] == '[':
			if text, url, next, ok := matchLink(r, i); ok {
				b.WriteString("[" + escapeMDV2(text) + "](" + escapeURL(url) + ")")
				i = next
				continue
			}
		case hasPrefixAt(r, i, "**"):
			if inner, next, ok := matchDelimited(r, i, "**"); ok {
				b.WriteString("*" + escapeMDV2(stripEmphasis(inner)) + "*")
				i = next
				continue
			}
		case hasPrefixAt(r, i, "__"):
			if inner, next, ok := matchDelimited(r, i, "__"); ok {
				b.WriteString("*" + escapeMDV2(stripEmphasis(inner)) + "*")
				i = next
				continue
			}
		case hasPrefixAt(r, i, "~~"):
			if inner, next, ok := matchDelimited(r, i, "~~"); ok {
				b.WriteString("~" + escapeMDV2(stripEmphasis(inner)) + "~")
				i = next
				continue
			}
		case r[i] == '*':
			if inner, next, ok := matchDelimited(r, i, "*"); ok {
				b.WriteString("_" + escapeMDV2(stripEmphasis(inner)) + "_")
				i = next
				continue
			}
		}

		// 认不出来的就是字面量
		if r[i] < 128 && strings.ContainsRune(mdv2Special, r[i]) {
			b.WriteByte('\\')
		}
		b.WriteRune(r[i])
		i++
	}
	return b.String()
}

// matchLink 从 r[i] == '[' 开始匹配 [文字](地址)，返回文字、地址与下一个位置。
func matchLink(r []rune, i int) (text, url string, next int, ok bool) {
	if i >= len(r) || r[i] != '[' {
		return "", "", 0, false
	}
	close := indexRune(r, ']', i+1)
	if close < 0 || close+1 >= len(r) || r[close+1] != '(' {
		return "", "", 0, false
	}
	end := indexRune(r, ')', close+2)
	if end < 0 {
		return "", "", 0, false
	}
	url = strings.TrimSpace(string(r[close+2 : end]))
	if url == "" {
		return "", "", 0, false
	}
	return string(r[i+1 : close]), url, end + 1, true
}

// matchDelimited 匹配同一行内的成对定界符，返回中间内容与下一个位置。
// 内容为空视为不匹配（`**` 这种孤立标记按字面量处理）。
func matchDelimited(r []rune, i int, delim string) (inner string, next int, ok bool) {
	d := []rune(delim)
	start := i + len(d)
	for j := start; j+len(d) <= len(r); j++ {
		if matchAt(r, j, d) {
			if j == start {
				return "", 0, false
			}
			return string(r[start:j]), j + len(d), true
		}
	}
	return "", 0, false
}

func hasPrefixAt(r []rune, i int, prefix string) bool { return matchAt(r, i, []rune(prefix)) }

func matchAt(r []rune, i int, want []rune) bool {
	if i+len(want) > len(r) {
		return false
	}
	for k, c := range want {
		if r[i+k] != c {
			return false
		}
	}
	return true
}

func indexRune(r []rune, target rune, from int) int {
	for i := from; i < len(r); i++ {
		if r[i] == target {
			return i
		}
	}
	return -1
}
